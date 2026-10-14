package session

import (
	"errors"
	"fmt"
	"os"
	"os/exec"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/dumanhaydar/rcmanager/internal/claim"
	"github.com/dumanhaydar/rcmanager/internal/isolate"
	"github.com/dumanhaydar/rcmanager/internal/paths"
	"github.com/dumanhaydar/rcmanager/internal/procs"
	"github.com/dumanhaydar/rcmanager/internal/provider"
	"github.com/dumanhaydar/rcmanager/internal/ptywrap"
	"github.com/dumanhaydar/rcmanager/internal/registry"
	"github.com/dumanhaydar/rcmanager/internal/sio"
)

// Start brings the session up.
//
// It adopts a process rcmanager started earlier where it can, asks the provider
// whether a start is possible at all, and only then spawns. force restarts a
// session that is already running.
func (m *Managed) Start(force bool) Result {
	m.mu.Lock()
	if m.status == Online || m.status == Launching {
		if !force {
			res := Result{OK: true, AlreadyRunning: true, Status: m.viewLocked()}
			m.mu.Unlock()
			return res
		}
		m.mu.Unlock()
		m.Stop("restart")
		m.mu.Lock()
	}

	// A process rcmanager started earlier (before a daemon restart) may still be
	// alive — adopt rather than duplicate. Either way the saved pid has now
	// been used up; keeping it would mean re-testing a dead pid on every later
	// restart.
	saved := m.savedState
	m.savedState = nil
	m.mu.Unlock()

	// Before anything is supervised — adopting is supervising — make sure no
	// other daemon is already doing it. Two supervisors on one conversation
	// end with two agents appending to one transcript.
	if res, ok := m.claimConversation(); !ok {
		return res
	}

	if m.tryAdopt(saved) {
		m.mu.Lock()
		res := Result{OK: true, Adopted: true, Status: m.viewLocked()}
		m.mu.Unlock()
		return res
	}

	// The daemon's veto inspects every session, this one included, so it runs
	// with no session lock held.
	if m.canStart != nil {
		if veto := m.canStart(m); veto != nil {
			m.mu.Lock()
			m.releaseClaimLocked()
			m.updateLocked(func() {
				m.status = Stopped
				m.message = veto.Error
			})
			m.logLocked(veto.Error)
			res := Result{Error: veto.Error, Hint: veto.Hint, Status: m.viewLocked()}
			m.mu.Unlock()
			return res
		}
	}

	m.mu.Lock()
	entry, prov := m.entry, m.prov
	m.mu.Unlock()

	// Provider preflight shells out, so it runs without the session lock held.
	pre := prov.Preflight(entry)
	if !pre.OK {
		m.mu.Lock()
		m.releaseClaimLocked()
		m.updateLocked(func() {
			m.status = Errored
			m.message = pre.Error
		})
		m.logLocked("preflight failed: " + pre.Error)
		res := Result{Error: pre.Error, Hint: pre.Hint, Status: m.viewLocked()}
		m.mu.Unlock()
		return res
	}

	if finder, ok := prov.(provider.OwnerFinder); ok {
		var exclude []int
		if pid := m.Pid(); pid > 0 {
			exclude = append(exclude, pid)
		}
		if owner := finder.FindExternalOwner(entry, exclude); owner != nil {
			msg := "refusing to start: " + owner.Reason
			m.mu.Lock()
			// Someone else's agent process owns this conversation, so this
			// daemon is not supervising it and must not keep the claim: that
			// would lock out the daemon whose child it actually is.
			m.releaseClaimLocked()
			m.updateLocked(func() {
				m.status = Stopped
				m.message = msg
			})
			m.logLocked(msg)
			res := Result{
				Error:  msg,
				Hint:   "Two processes on one session fork its history. Close the other one, then start it here.",
				Status: m.viewLocked(),
			}
			m.mu.Unlock()
			return res
		}
	}

	return m.spawnChild()
}

func (m *Managed) spawnChild() Result {
	m.mu.Lock()
	entry, prov := m.entry, m.prov
	cmdSpec, err := prov.BuildCommand(entry)
	if err != nil {
		m.releaseClaimLocked()
		m.updateLocked(func() {
			m.status = Errored
			m.message = err.Error()
		})
		m.logLocked(err.Error())
		res := Result{Error: err.Error(), Status: m.viewLocked()}
		m.mu.Unlock()
		return res
	}
	m.openLogsLocked()

	// A provider may need a real terminal (see package ptywrap). Either way the
	// child leads its own process group, so stopping it reaches the pty wrapper
	// and the agent inside it together.
	launch := ptywrap.Launch{Command: cmdSpec.Command, Args: cmdSpec.Args}
	if cmdSpec.Pty {
		launch = ptywrap.Wrap(cmdSpec.Command, cmdSpec.Args)
	}

	env := buildEnv(cmdSpec, prov)

	// ...and, where systemd offers it, its own cgroup scope, so
	// `systemctl stop rcmanager` cannot sweep the session up with the supervisor.
	m.scope = ""
	if m.cfg.IsolateSessions != "off" && isolate.Available() {
		if scoped := isolate.Wrap(launch.Command, launch.Args, entry.Name, env["PATH"]); scoped != nil {
			m.scope = scoped.Unit
			launch = ptywrap.Launch{Command: scoped.Command, Args: scoped.Args}
		} else {
			m.logLocked("cannot resolve \"" + launch.Command + "\" on PATH — starting without a cgroup scope; " +
				"this session will not survive `systemctl stop rcmanager`")
		}
	}

	m.usesPty = cmdSpec.Pty
	m.pending = map[string]bool{}
	for _, i := range prov.Interactions() {
		m.pending[i.ID] = true
	}
	m.tailBuf = ""
	m.urlBuf = ""
	m.urlLocked = false
	m.blocked = ""
	m.needAnalysis.Store(true)

	if err := m.io.Open(true); err != nil {
		msg := "could not open session stdio: " + err.Error()
		m.releaseClaimLocked()
		m.updateLocked(func() {
			m.status = Errored
			m.message = msg
		})
		m.logLocked(msg)
		res := Result{Error: msg, Status: m.viewLocked()}
		m.mu.Unlock()
		return res
	}

	stdin, stdout, stderr := m.io.Stdio()
	child := exec.Command(launch.Command, launch.Args...)
	child.Dir = cmdSpec.Cwd
	child.Env = flattenEnv(env)
	// Files and a fifo, never pipes: see package sio for why the child must not
	// hold the other end of anything the daemon owns.
	child.Stdin = stdin
	child.Stdout = stdout
	child.Stderr = stderr
	child.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}

	if err := child.Start(); err != nil {
		msg := "spawn failed: " + err.Error()
		m.releaseClaimLocked()
		m.updateLocked(func() {
			m.status = Errored
			m.message = msg
		})
		m.logLocked(msg)
		m.io.Close()
		res := Result{Error: msg, Status: m.viewLocked()}
		m.mu.Unlock()
		return res
	}

	m.cmd = child
	m.pid = child.Process.Pid
	m.signature = cmdSpec.Signature
	m.launchedWith = entry
	m.startedAt = time.Now()
	m.urlWatchFrom = m.startedAt
	m.adopted = false
	m.stopping = false
	m.updateLocked(func() {
		m.status = Launching
		m.message = ""
		m.remoteURL = ""
		m.needsRestart = nil
	})

	suffix := ""
	if cmdSpec.Pty {
		suffix += " (pty)"
	}
	if m.scope != "" {
		suffix += " (scope " + m.scope + ")"
	}
	m.logLocked(fmt.Sprintf("started pid %d%s: %s %s (cwd %s)",
		m.pid, suffix, cmdSpec.Command, strings.Join(cmdSpec.Args, " "), cmdSpec.Cwd))
	if !m.io.UsesFifo() {
		m.logLocked("no fifo available — this session will not survive a daemon restart")
	}

	m.startCaptureLocked(sio.Offsets{})
	m.armURLWatchdogLocked()

	// Anything still alive after minUptime is considered up.
	minUptime := m.settingDuration("minUptimeMs")
	m.minUpTimer = time.AfterFunc(minUptime, func() {
		m.mu.Lock()
		if m.cmd == child && m.status == Launching {
			m.updateLocked(func() { m.status = Online })
		}
		m.mu.Unlock()
	})

	view := m.viewLocked()
	m.mu.Unlock()

	go m.wait(child)
	return Result{OK: true, Status: view}
}

// wait turns the child's exit into a supervision decision.
func (m *Managed) wait(child *exec.Cmd) {
	err := child.Wait()

	code, signal := 0, ""
	if err != nil {
		var exitErr *exec.ExitError
		if errors.As(err, &exitErr) {
			code = exitErr.ExitCode()
			if ws, ok := exitErr.Sys().(syscall.WaitStatus); ok && ws.Signaled() {
				signal = ws.Signal().String()
			}
		} else {
			code = -1
		}
	}

	m.mu.Lock()
	if m.cmd != child {
		// Already detached or replaced: this exit is history.
		m.mu.Unlock()
		return
	}
	m.mu.Unlock()
	m.handleExit(code, signal)
}

// handleExit applies the restart policy to a child that has gone away.
func (m *Managed) handleExit(code int, signal string) {
	m.mu.Lock()
	m.stopTimersLocked()
	uptime := int64(0)
	if !m.startedAt.IsZero() {
		uptime = time.Since(m.startedAt).Milliseconds()
	}
	m.lastExit = &Exit{Code: code, Signal: signal, At: time.Now().UTC().Format(time.RFC3339), UptimeMs: uptime}
	m.cmd = nil
	m.pid = 0
	sig := signal
	if sig == "" {
		sig = "-"
	}
	m.logLocked(fmt.Sprintf("exited code=%d signal=%s after %ds", code, sig, uptime/1000))

	tailers := m.tailers
	stopping := m.stopping
	enabled := m.entry.Enabled
	blocked := m.blocked
	autorestart := m.settingBool("autorestart")
	window := m.settingDuration("restartWindowMs")
	minUptime := m.settingDuration("minUptimeMs")
	maxRestarts := m.settingInt("maxRestarts")
	delay := m.settingDuration("restartDelayMs")
	m.mu.Unlock()

	// Drain whatever the child wrote on its way out before capture stops. This
	// happens outside the lock: a tailer callback must never wait on it.
	for _, t := range tailers {
		t.Poll()
	}
	m.stopCapture()
	m.io.Close()

	m.mu.Lock()
	defer m.mu.Unlock()

	if stopping {
		m.stopping = false
		m.releaseClaimLocked()
		m.updateLocked(func() { m.status = stoppedStatus(enabled) })
		return
	}
	if !enabled {
		m.releaseClaimLocked()
		m.updateLocked(func() { m.status = Disabled })
		return
	}
	if !autorestart {
		m.releaseClaimLocked()
		m.updateLocked(func() {
			m.status = Stopped
			m.message = "exited (autorestart disabled)"
		})
		return
	}

	// A blocking condition rcmanager is not allowed to resolve on its own (a trust
	// or permission prompt) will not fix itself, so restarting just burns the
	// budget and floods the log. Fail fast with the reason instead.
	if blocked != "" {
		m.updateLocked(func() {
			m.status = Errored
			m.message = blocked
		})
		m.logLocked("not restarting: " + blocked)
		return
	}

	now := time.Now()
	if time.Duration(uptime)*time.Millisecond < minUptime {
		m.restartTimes = append(m.restartTimes, now)
	}
	kept := m.restartTimes[:0]
	for _, t := range m.restartTimes {
		if now.Sub(t) < window {
			kept = append(kept, t)
		}
	}
	m.restartTimes = kept

	if len(m.restartTimes) > maxRestarts {
		msg := fmt.Sprintf("crash loop: %d fast exits within %ds", len(m.restartTimes), int(window.Seconds()))
		m.updateLocked(func() {
			m.status = Errored
			m.message = msg
		})
		m.logLocked(msg + " — giving up. Fix the cause and run: rcmanager restart " + m.entry.Name)
		m.releaseClaimLocked()
		return
	}

	m.restarts++
	m.updateLocked(func() {
		m.status = Restarting
		m.message = fmt.Sprintf("restarting in %ds", int(delay.Seconds()))
	})
	m.logLocked(fmt.Sprintf("restarting in %ds (restart #%d)", int(delay.Seconds()), m.restarts))

	if m.restartTimer != nil {
		m.restartTimer.Stop()
	}
	m.restartTimer = time.AfterFunc(delay, func() {
		m.mu.Lock()
		ready := m.entry.Enabled && m.cmd == nil
		m.mu.Unlock()
		if ready {
			m.Start(false)
		}
	})
}

func stoppedStatus(enabled bool) Status {
	if enabled {
		return Stopped
	}
	return Disabled
}

// Stop ends the session: SIGTERM to the process group, then SIGKILL.
func (m *Managed) Stop(reason string) Result {
	m.mu.Lock()
	m.stopTimersLocked()
	m.stopAdoptPollLocked()

	pid := m.pid
	if pid == 0 || !procs.IsAlive(pid) {
		m.cmd = nil
		m.pid = 0
		enabled := m.entry.Enabled
		m.releaseClaimLocked()
		m.mu.Unlock()

		m.stopCapture()
		m.io.Close()

		m.mu.Lock()
		m.updateLocked(func() {
			m.status = stoppedStatus(enabled)
			m.message = ""
		})
		res := Result{OK: true, AlreadyStopped: true, Status: m.viewLocked()}
		m.mu.Unlock()
		return res
	}

	m.stopping = true
	m.logLocked(fmt.Sprintf("stopping (%s) pid %d", reason, pid))
	signature := m.signature
	timeout := m.settingDuration("stopTimeoutMs")
	tailers := m.tailers
	enabled := m.entry.Enabled
	m.mu.Unlock()

	outcome := procs.Terminate(pid, signature, timeout, true)

	// Capture whatever it printed while shutting down, then let go.
	for _, t := range tailers {
		t.Poll()
	}
	m.stopCapture()
	m.io.Close()

	m.mu.Lock()
	defer m.mu.Unlock()

	if outcome == procs.Refused {
		// The pid no longer looks like the process we started: never signal it.
		msg := fmt.Sprintf("pid %d no longer matches this session's command; leaving it alone", pid)
		m.logLocked(msg)
		m.cmd = nil
		m.pid = 0
		m.stopping = false
		// The pid is not ours to signal, so it is not ours to claim either.
		m.releaseClaimLocked()
		m.updateLocked(func() {
			m.status = Stopped
			m.message = msg
		})
		return Result{Error: msg, Status: m.viewLocked()}
	}

	m.cmd = nil
	m.pid = 0
	m.stopping = false
	m.adopted = false
	m.restartTimes = nil
	m.releaseClaimLocked()
	m.updateLocked(func() {
		m.status = stoppedStatus(enabled)
		m.message = ""
		m.needsRestart = nil
	})
	m.logLocked("stopped (" + string(outcome) + ")")
	return Result{OK: true, Status: m.viewLocked()}
}

// Restart stops and starts, clearing the crash-loop budget: an explicit
// restart is a fresh start, not a continuation of whatever was failing.
func (m *Managed) Restart() Result {
	m.Stop("restart")
	time.Sleep(250 * time.Millisecond)
	m.mu.Lock()
	m.restartTimes = nil
	m.mu.Unlock()
	return m.Start(false)
}

// Detach lets go of a running session without stopping it.
//
// This is what a daemon shutdown does. Everything the successor needs to take
// over — pid, signature, capture offsets — is in state.json; the child keeps
// writing to its raw files the whole time and never learns that anything
// happened.
func (m *Managed) Detach() {
	m.mu.Lock()
	m.stopTimersLocked()
	m.stopAdoptPollLocked()
	tailers := m.tailers
	// Drop our own exit bookkeeping first: closing the fifo must not look like
	// the session died.
	m.cmd = nil
	// Hand the conversation over: the successor daemon has to be able to claim
	// what it is about to adopt.
	m.releaseClaimLocked()
	m.mu.Unlock()

	for _, t := range tailers {
		t.Poll()
	}
	m.stopCapture()
	m.io.Close()

	m.mu.Lock()
	if m.out != nil {
		m.out.Close()
		m.out = nil
	}
	if m.err != nil {
		m.err.Close()
		m.err = nil
	}
	m.mu.Unlock()
}

// Dispose releases everything, for a session being unregistered.
func (m *Managed) Dispose() {
	m.mu.Lock()
	m.stopTimersLocked()
	m.stopAdoptPollLocked()
	m.releaseClaimLocked()
	m.mu.Unlock()

	m.stopCapture()
	m.stopIngest()
	m.io.Close()

	m.mu.Lock()
	if m.out != nil {
		m.out.Close()
		m.out = nil
	}
	if m.err != nil {
		m.err.Close()
		m.err = nil
	}
	m.mu.Unlock()
}

// DestroyIO removes a session's fifo and raw capture files.
func (m *Managed) DestroyIO() { m.io.Destroy() }

func (m *Managed) stopTimersLocked() {
	for _, t := range []**time.Timer{&m.restartTimer, &m.urlTimer, &m.minUpTimer} {
		if *t != nil {
			(*t).Stop()
			*t = nil
		}
	}
}

func (m *Managed) stopAdoptPollLocked() {
	if m.adoptStop != nil {
		close(m.adoptStop)
		m.adoptStop = nil
	}
}

// ------------------------------------------------------------------ adoption

// tryAdopt reattaches to a process rcmanager started before the daemon was
// restarted. It only ever adopts a pid whose live argv still matches the
// signature, so a recycled pid can never be mistaken for ours.
func (m *Managed) tryAdopt(saved *Persisted) bool {
	if saved == nil || saved.Pid <= 0 || len(saved.Signature) == 0 {
		return false
	}
	if !procs.IsAlive(saved.Pid) || !procs.MatchesSignature(saved.Pid, saved.Signature) {
		return false
	}

	m.mu.Lock()
	m.pid = saved.Pid
	m.signature = saved.Signature
	if saved.StartedAt > 0 {
		m.startedAt = time.UnixMilli(saved.StartedAt)
	} else {
		m.startedAt = time.Now()
	}
	m.adopted = true
	// Whatever this session announced, it announced before this daemon could
	// read it; the window starts now.
	m.urlWatchFrom = time.Now()
	m.remoteURL = saved.RemoteURL
	m.restarts = saved.Restarts
	m.scope = saved.Scope
	m.launchedWith = m.entry

	m.pending = map[string]bool{}
	for _, i := range m.prov.Interactions() {
		m.pending[i.ID] = true
	}
	m.tailBuf = ""
	m.urlBuf = ""
	// Replayed output is history, not an announcement: if the URL survived the
	// handover, do not let old lines overwrite it.
	m.urlLocked = saved.RemoteURL != ""
	m.blocked = ""
	m.needAnalysis.Store(true)
	m.openLogsLocked()
	m.mu.Unlock()

	// Take the stdio back: the raw files are still being appended to, and
	// reopening the fifo restores the ability to answer a prompt.
	_ = m.io.Open(false)
	stdinBack := m.io.ReopenStdin()

	m.mu.Lock()
	m.startCaptureLocked(saved.Capture)
	m.armURLWatchdogLocked()
	message := ""
	if !stdinBack {
		message = "adopted, but its stdin fifo is gone — prompts cannot be answered until it restarts"
	}
	m.updateLocked(func() {
		m.status = Online
		m.message = message
	})
	m.logLocked(fmt.Sprintf("adopted running pid %d — capture resumed at out:%d err:%d",
		saved.Pid, saved.Capture.Out, saved.Capture.Err))

	m.stopAdoptPollLocked()
	stop := make(chan struct{})
	m.adoptStop = stop
	pid, signature := m.pid, m.signature
	m.mu.Unlock()

	// An adopted process is not our child, so there is no Wait to block on:
	// poll the signature instead and treat its disappearance as an exit.
	go func() {
		ticker := time.NewTicker(5 * time.Second)
		defer ticker.Stop()
		for {
			select {
			case <-stop:
				return
			case <-ticker.C:
				if procs.IsAlive(pid) && procs.MatchesSignature(pid, signature) {
					continue
				}
				m.mu.Lock()
				if m.adoptStop == stop {
					m.adoptStop = nil
					m.adopted = false
				}
				m.mu.Unlock()
				m.handleExit(-1, "ADOPTED_EXIT")
				return
			}
		}
	}()
	return true
}

// ApplyEntry applies an edited registry entry to a live session.
//
// Supervision limits and notes take effect immediately. A change to how the
// session *launches* cannot, so it is flagged and left to the user rather than
// acted on: interrupting a working agent is a decision, not a consequence of
// saving a file.
func (m *Managed) ApplyEntry(entry *registry.Entry) (restartNeeded bool, changed []string) {
	m.mu.Lock()
	defer m.mu.Unlock()

	m.entry = entry
	if entry.Provider != m.prov.ID() {
		if p, err := provider.Get(entry.Provider); err == nil {
			m.prov = p
		}
	}

	running := m.status == Online || m.status == Launching
	if m.launchedWith != nil {
		changed = registry.LaunchDiff(m.launchedWith, entry)
	}
	if len(changed) == 0 || !running {
		// Back in agreement with the running process — including after an edit
		// was undone, which must clear the flag rather than re-raise it.
		if len(m.needsRestart) > 0 {
			m.updateLocked(func() {
				m.needsRestart = nil
				m.message = ""
			})
		}
		return false, nil
	}

	msg := "registry changed " + strings.Join(changed, ", ") + " — restart to apply: rcmanager restart " + m.entry.Name
	m.updateLocked(func() {
		m.needsRestart = changed
		m.message = msg
	})
	m.logLocked(msg)
	return true, changed
}

// -------------------------------------------------------------------- output

// View is everything rcmanager reports about this session.
func (m *Managed) View() View {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.viewLocked()
}

func (m *Managed) viewLocked() View {
	v := View{
		Name:         m.entry.Name,
		Provider:     m.entry.Provider,
		SessionID:    m.entry.SessionID,
		Cwd:          m.entry.Cwd,
		Enabled:      m.entry.Enabled,
		Status:       m.status,
		Pid:          m.pid,
		Restarts:     m.restarts,
		Adopted:      m.adopted,
		RemoteURL:    m.remoteURL,
		Message:      m.message,
		NeedsRestart: m.needsRestart,
		LastExit:     m.lastExit,
		Signature:    m.signature,
		Scope:        m.scope,
		Logs:         m.logFiles,
		Note:         m.entry.Note,
		Source:       m.entry.Source,
	}
	if !m.startedAt.IsZero() {
		v.StartedAt = m.startedAt.UTC().Format(time.RFC3339)
		if m.status == Online || m.status == Launching {
			v.UptimeMs = time.Since(m.startedAt).Milliseconds()
		}
	}
	return v
}

// Persisted is the subset written to state.json so a fresh daemon can
// re-adopt this session.
func (m *Managed) Persisted() Persisted {
	m.mu.Lock()
	defer m.mu.Unlock()
	p := Persisted{
		Name:      m.entry.Name,
		Pid:       m.pid,
		Signature: m.signature,
		Status:    m.status,
		Restarts:  m.restarts,
		RemoteURL: m.remoteURL,
		Scope:     m.scope,
		Capture:   m.captureOffsetsLocked(),
	}
	if !m.startedAt.IsZero() {
		p.StartedAt = m.startedAt.UnixMilli()
	}
	return p
}

// buildEnv is the environment for a managed child.
//
// rcmanager is often launched from inside an agent session (a terminal, an IDE, or
// another agent's shell), and those sessions export markers that tell a nested
// CLI it is a child process. Claude Code's CLAUDE_CODE_CHILD_SESSION is the
// dangerous one: inherited, it silently turns transcript saving off, so the
// resumed conversation stops being written to disk. Providers declare which
// variables must not leak in; everything else is passed through so real
// credentials and PATH still work.
func buildEnv(cmd provider.Command, prov provider.Provider) map[string]string {
	env := map[string]string{}
	for _, kv := range os.Environ() {
		if i := strings.IndexByte(kv, '='); i > 0 {
			env[kv[:i]] = kv[i+1:]
		}
	}
	for _, deny := range prov.EnvDeny() {
		for key := range env {
			if deny.MatchString(key) {
				delete(env, key)
			}
		}
	}
	if cmd.Pty {
		for k, v := range ptywrap.Env() {
			env[k] = v
		}
	}
	for k, v := range cmd.Env {
		env[k] = v
	}
	return env
}

func flattenEnv(env map[string]string) []string {
	out := make([]string, 0, len(env))
	for k, v := range env {
		out = append(out, k+"="+v)
	}
	return out
}

// Rename moves a live session to a new name.
//
// Everything named after the session moves with it: its logs, its raw capture
// files and its stdin fifo. They are renamed rather than recreated, so a
// running agent keeps writing to the same inodes and never notices — and
// capture resumes at the offset it had reached, so no output is replayed or
// skipped.
//
// What does *not* change is the recorded signature. It describes the process
// that is actually running, which was launched under the old name, and the
// interlock in package procs has to keep matching it or rcmanager would lose
// the right to signal its own child. When the new name would change how the
// session launches — a provider that puts the name in its command line — the
// session is flagged as needing a restart, the same as any other
// launch-affecting edit.
func (m *Managed) Rename(entry *registry.Entry) error {
	oldName := m.Name()

	m.mu.Lock()
	offsets := m.captureOffsetsLocked()
	m.stopCaptureLocked()

	if m.out != nil {
		m.out.Close()
	}
	if m.err != nil {
		m.err.Close()
	}
	m.out, m.err = nil, nil

	if err := renameLogs(m.logFiles, paths.LogPaths(entry.Name), m.cfg.LogRetain); err != nil {
		m.mu.Unlock()
		return err
	}
	if err := m.io.Rename(entry.Name); err != nil {
		m.mu.Unlock()
		return err
	}

	m.entry = entry
	m.logFiles = paths.LogPaths(entry.Name)
	m.openLogsLocked()
	m.logLocked("renamed from " + oldName)

	running := m.status == Online || m.status == Launching
	if running {
		m.startCaptureLocked(offsets)
	} else {
		m.captureAt = offsets
	}

	// A provider that carries the session name in its argv is launching
	// something the new name no longer describes.
	if running && len(m.signature) > 0 {
		if cmd, err := m.prov.BuildCommand(entry); err == nil && !sameSignature(cmd.Signature, m.signature) {
			msg := "renamed, but the running process was launched as " + oldName +
				" — restart to apply: rcmanager restart " + entry.Name
			m.updateLocked(func() {
				m.needsRestart = []string{"name"}
				m.message = msg
			})
			m.logLocked(msg)
		}
	}
	m.mu.Unlock()

	m.onChange()
	return nil
}

func sameSignature(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

// renameLogs moves the human-readable logs, rotations included, so `rcmanager
// logs <new-name>` shows the history the session actually has.
func renameLogs(from, to paths.LogFiles, retain int) error {
	if retain < 1 {
		retain = 1
	}
	for _, pair := range [][2]string{{from.Out, to.Out}, {from.Err, to.Err}} {
		if pair[0] == pair[1] {
			continue
		}
		for i := 0; i <= retain; i++ {
			suffix := ""
			if i > 0 {
				suffix = "." + strconv.Itoa(i)
			}
			src, dst := pair[0]+suffix, pair[1]+suffix
			if _, err := os.Stat(src); err != nil {
				continue
			}
			if err := os.Rename(src, dst); err != nil {
				return err
			}
		}
	}
	return nil
}

// claimConversation takes the machine-wide lock on this session's
// conversation, so no second daemon can supervise it at the same time.
//
// A lock that cannot be used at all — an unwritable cache directory, an
// exotic filesystem — is reported on the session and then ignored: refusing
// to supervise anything would be a worse failure than the one this guards
// against, and saying so beats being silent about it.
func (m *Managed) claimConversation() (Result, bool) {
	m.mu.Lock()
	if m.claim != nil {
		m.mu.Unlock()
		return Result{}, true
	}
	entry, prov := m.entry, m.prov
	m.mu.Unlock()

	key := claim.Key(prov.ID(), entry.SessionID, entry.Name, prov.Capabilities().Singleton)
	held, holder, err := claim.Acquire(key, claim.Holder{
		Pid:  os.Getpid(),
		Home: paths.Get().Home,
		Name: entry.Name,
	})

	m.mu.Lock()
	defer m.mu.Unlock()

	if err != nil {
		msg := "could not take the single-supervisor lock (" + err.Error() + ") — supervising anyway"
		m.updateLocked(func() { m.message = msg })
		m.logLocked(msg)
		return Result{}, true
	}
	if held == nil {
		what := "this session"
		if entry.SessionID != "" {
			what = "session " + entry.SessionID
		}
		msg := "refusing to start: " + what + " is already supervised by " + holder.Describe()
		m.updateLocked(func() {
			m.status = Stopped
			m.message = msg
		})
		m.logLocked(msg)
		return Result{
			Error: msg,
			Hint: "Two supervisors on one conversation fork its history. Stop that daemon " +
				"(`rcmanager daemon --stop`), or manage this session from it.",
			Status: m.viewLocked(),
		}, false
	}

	m.claim = held
	return Result{}, true
}

func (m *Managed) releaseClaimLocked() {
	if m.claim == nil {
		return
	}
	m.claim.Release()
	m.claim = nil
}
