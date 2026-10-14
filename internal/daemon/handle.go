package daemon

import (
	"fmt"
	"os"
	"strings"
	"sync"
	"time"

	"github.com/dumanhaydar/rcmanager/internal/config"
	"github.com/dumanhaydar/rcmanager/internal/ipc"
	"github.com/dumanhaydar/rcmanager/internal/paths"
	"github.com/dumanhaydar/rcmanager/internal/procs"
	"github.com/dumanhaydar/rcmanager/internal/rcerr"
	"github.com/dumanhaydar/rcmanager/internal/registry"
	"github.com/dumanhaydar/rcmanager/internal/session"
	"github.com/dumanhaydar/rcmanager/internal/version"
)

// handle answers one CLI request.
func (d *Daemon) handle(req ipc.Request, progress func(ipc.Progress)) (any, error) {
	switch req.Cmd {
	case "ping":
		d.mu.Lock()
		ready, count := d.ready, len(d.sessions)
		d.mu.Unlock()
		entrypoint, _ := os.Executable()
		return Pong{
			Pid:        os.Getpid(),
			Sessions:   count,
			Socket:     d.socketPath,
			Version:    version.Version,
			Ready:      ready,
			Entrypoint: entrypoint,
			StartedAt:  d.startedAt,
		}, nil

	case "list":
		sessions := d.list()
		out := make([]session.View, 0, len(sessions))
		for _, s := range sessions {
			out = append(out, s.View())
		}
		return out, nil

	case "describe":
		s, err := d.get(req.Args.Name)
		if err != nil {
			return nil, err
		}
		return s.View(), nil

	case "register":
		if req.Args.Entry == nil {
			return nil, rcerr.New("register needs a session entry.", "")
		}
		entry, err := registry.Upsert(req.Args.Entry)
		if err != nil {
			return nil, err
		}
		s, err := d.attach(entry)
		if err != nil {
			return nil, err
		}
		return s.View(), nil

	case "start":
		s, err := d.get(req.Args.Name)
		if err != nil {
			return nil, err
		}
		result := s.Start(req.Args.Force)
		d.persist()
		if !result.OK {
			return nil, &rcerr.Error{Message: result.Error, Hint: result.Hint, Code: "ERROR"}
		}
		return result.Status, nil

	case "stop":
		s, err := d.get(req.Args.Name)
		if err != nil {
			return nil, err
		}
		reason := req.Args.Reason
		if reason == "" {
			reason = "requested"
		}
		result := s.Stop(reason)
		d.persist()
		if !result.OK {
			return nil, rcerr.New(result.Error, "")
		}
		return result.Status, nil

	case "restart":
		s, err := d.get(req.Args.Name)
		if err != nil {
			return nil, err
		}
		result := s.Restart()
		d.persist()
		if !result.OK {
			return nil, &rcerr.Error{Message: result.Error, Hint: result.Hint, Code: "ERROR"}
		}
		return result.Status, nil

	case "rename":
		return d.rename(req.Args.Name, req.Args.NewName)

	case "enable", "disable":
		return d.toggle(req.Args.Name, req.Cmd == "enable")

	case "remove":
		return d.remove(req.Args.Name)

	case "startAll", "stopAll", "restartAll":
		return d.bulk(req.Cmd, progress), nil

	case "reload":
		return d.reload()

	case "shutdown":
		stopSessions := req.Args.StopSessions
		time.AfterFunc(50*time.Millisecond, func() { d.Shutdown("request", stopSessions, true) })
		return map[string]any{"stopping": true, "stopSessions": stopSessions}, nil
	}

	return nil, rcerr.Newf("", "Unknown daemon command %q.", req.Cmd)
}

func (d *Daemon) toggle(name string, on bool) (any, error) {
	s, err := d.get(name)
	if err != nil {
		return nil, err
	}
	entry := s.Entry().Clone()
	entry.Enabled = on
	stored, err := registry.Upsert(entry)
	if err != nil {
		return nil, err
	}
	s.SetEntry(stored)

	if on {
		if s.Status() != session.Online {
			s.Start(false)
		}
	} else {
		s.Stop("disabled")
	}
	d.persist()
	return s.View(), nil
}

// rename moves a session to a new name, running or not.
//
// The registry entry moves with everything named after it — logs, raw capture
// files, the stdin fifo — and the session keeps its id, so this renames the
// handle rcmanager knows it by and nothing about the conversation underneath.
func (d *Daemon) rename(oldName, newName string) (any, error) {
	if newName == "" {
		return nil, rcerr.New("rename needs a new name.", "Usage: rcmanager rename <name> <new-name>")
	}
	if err := registry.AssertValidName(newName); err != nil {
		return nil, err
	}
	s, err := d.get(oldName)
	if err != nil {
		return nil, err
	}
	if newName == oldName {
		return s.View(), nil
	}

	// Refuse rather than merge: two sessions quietly becoming one would lose
	// whichever registry entry was overwritten.
	d.mu.Lock()
	_, taken := d.sessions[newName]
	d.mu.Unlock()
	if taken || registry.Get(newName) != nil {
		return nil, rcerr.Newf("Pick another name, or remove that session first.",
			"A session named %q is already registered.", newName)
	}

	entry := s.Entry().Clone()
	entry.Name = newName
	entry, err = registry.Normalize(entry, "")
	if err != nil {
		return nil, err
	}

	if err := s.Rename(entry); err != nil {
		return nil, rcerr.Newf("", "could not rename %q: %v", oldName, err)
	}

	// The registry is only touched once the session itself has moved, so a
	// failure above leaves the document describing what is really on disk.
	if _, err := registry.Upsert(entry); err != nil {
		return nil, err
	}
	if _, err := registry.Remove(oldName); err != nil {
		return nil, err
	}

	d.mu.Lock()
	delete(d.sessions, oldName)
	d.sessions[newName] = s
	for i, n := range d.order {
		if n == oldName {
			d.order[i] = newName
			break
		}
	}
	d.mu.Unlock()

	d.line(oldName + " renamed to " + newName)
	d.persist()
	return s.View(), nil
}

func (d *Daemon) remove(name string) (any, error) {
	d.mu.Lock()
	s, exists := d.sessions[name]
	d.mu.Unlock()

	// Unregister only: the provider's conversation is never touched.
	if exists {
		s.Stop("removed")
		s.DestroyIO()
		s.Dispose()
		d.forget(name)
	}
	removed, err := registry.Remove(name)
	if err != nil {
		return nil, err
	}
	d.persist()
	if !removed && !exists {
		return nil, rcerr.Newf("", "No session named %q is registered.", name)
	}
	return map[string]string{"removed": name}, nil
}

// bulk works through every session one at a time, streaming where it has got
// to. Without the stream the CLI simply blocks for as long as the whole thing
// takes and then prints the result, which is indistinguishable from a hang.
func (d *Daemon) bulk(cmd string, progress func(ipc.Progress)) []BulkResult {
	var targets []*session.Managed
	for _, s := range d.list() {
		if cmd == "stopAll" || s.Entry().Enabled {
			targets = append(targets, s)
		}
	}

	// Tell the caller what is coming before doing any of it: a list that
	// appears all at once and then fills in reads as progress, where a line at
	// a time reads as a stall between each.
	names := make([]string, 0, len(targets))
	for _, s := range targets {
		names = append(names, s.Name())
	}
	progress(ipc.Progress{Phase: "plan", Names: names})

	out := make([]BulkResult, 0, len(targets))
	for _, s := range targets {
		progress(ipc.Progress{Phase: "begin", Name: s.Name()})

		var r session.Result
		switch cmd {
		case "stopAll":
			r = s.Stop("stop all")
		case "restartAll":
			r = s.Restart()
		default:
			r = s.Start(false)
		}

		view := s.View()
		out = append(out, BulkResult{Name: s.Name(), OK: r.OK, Error: r.Error, Status: view})
		progress(ipc.Progress{
			Phase: "end", Name: s.Name(), OK: r.OK, Error: r.Error,
			Pid: view.Pid, Adopted: r.Adopted,
		})

		if cmd != "stopAll" {
			time.Sleep(500 * time.Millisecond)
		}
	}
	d.persist()
	return out
}

// reload re-reads config.json and registry.json and reconciles, without
// interrupting healthy work.
//
// The rule is that saving a file is not permission to restart an agent
// mid-thought. Supervision settings and notes are applied to live sessions in
// place; an edit that changes how a session *launches* is reported as
// needsRestart and waits for `rcmanager restart` — unless restartOnChange is on,
// which is the explicit way to ask for the old behaviour. Only sessions that
// were added, removed, enabled or disabled actually move.
func (d *Daemon) reload() ([]session.View, error) {
	before := map[string]*registry.Entry{}
	for _, s := range d.list() {
		before[s.Name()] = s.Entry()
	}

	oldCfg := d.cfg
	newCfg := config.Load()
	d.mu.Lock()
	d.cfg = newCfg
	d.mu.Unlock()

	logSettingsChanged := oldCfg.MaxLogBytes != newCfg.MaxLogBytes ||
		oldCfg.LogRetain != newCfg.LogRetain ||
		oldCfg.StripAnsiInLogs != newCfg.StripAnsiInLogs

	report := ReloadReport{}

	entries, err := registry.Load()
	if err != nil {
		return nil, err
	}
	names := map[string]bool{}
	for _, entry := range entries {
		names[entry.Name] = true
	}

	for _, entry := range entries {
		previous, existed := before[entry.Name]
		s, err := d.attach(entry)
		if err != nil {
			d.line("reload " + entry.Name + ": " + err.Error())
			continue
		}
		s.SetConfig(newCfg)
		if logSettingsChanged && s.HasLogs() {
			s.ReopenLogs()
		}

		// Brand new in the registry: start it if it wants to be running.
		if !existed {
			if !entry.Enabled {
				continue
			}
			r := s.Start(false)
			report.Started = append(report.Started, entry.Name)
			if !r.OK {
				d.line("reload start " + entry.Name + ": " + r.Error)
			}
			continue
		}

		// enabled flipped — an explicit instruction either way.
		if previous.Enabled != entry.Enabled {
			if entry.Enabled {
				r := s.Start(false)
				report.Started = append(report.Started, entry.Name)
				if !r.OK {
					d.line("reload start " + entry.Name + ": " + r.Error)
				}
			} else {
				s.Stop("disabled in the registry")
				report.Stopped = append(report.Stopped, entry.Name)
			}
			continue
		}

		// The session knows what it was launched with; the registry's previous
		// state is not the same thing once an edit has been undone.
		restartNeeded, _ := s.ApplyEntry(entry)
		if !restartNeeded {
			report.Unchanged++
			continue
		}
		if newCfg.RestartOnChange {
			s.Restart()
			report.Restarted = append(report.Restarted, entry.Name)
		} else {
			report.Flagged = append(report.Flagged, entry.Name)
		}
	}

	for _, s := range d.list() {
		if names[s.Name()] {
			continue
		}
		// Dropped from the registry: that is an explicit deregistration.
		s.Stop("removed from the registry")
		s.Dispose()
		d.forget(s.Name())
		report.Removed = append(report.Removed, s.Name())
	}

	d.line("reloaded: " + summarize(report))
	d.persist()

	sessions := d.list()
	out := make([]session.View, 0, len(sessions))
	for _, s := range sessions {
		out = append(out, s.View())
	}
	return out, nil
}

func summarize(c ReloadReport) string {
	var parts []string
	if len(c.Started) > 0 {
		parts = append(parts, "started "+strings.Join(c.Started, ", "))
	}
	if len(c.Stopped) > 0 {
		parts = append(parts, "stopped "+strings.Join(c.Stopped, ", "))
	}
	if len(c.Removed) > 0 {
		parts = append(parts, "removed "+strings.Join(c.Removed, ", "))
	}
	if len(c.Restarted) > 0 {
		parts = append(parts, "restarted "+strings.Join(c.Restarted, ", "))
	}
	if len(c.Flagged) > 0 {
		parts = append(parts, strings.Join(c.Flagged, ", ")+" need a restart to pick up changes")
	}
	if c.Unchanged > 0 {
		parts = append(parts, fmt.Sprintf("%d untouched", c.Unchanged))
	}
	if len(parts) == 0 {
		return "no changes"
	}
	return strings.Join(parts, "; ")
}

// Shutdown stands the supervisor down.
//
// By default this *detaches*: managed sessions keep running, and the next
// daemon adopts them. That is what makes `systemctl restart rcmanager`, an upgrade
// or a crash survivable for an agent that is mid-task. Ending the work is a
// separate, explicit request.
func (d *Daemon) Shutdown(reason string, stopSessions, exit bool) {
	d.mu.Lock()
	if d.shuttingDown {
		d.mu.Unlock()
		return
	}
	d.shuttingDown = true
	watches := d.watches
	stopPersist := d.stopPersist
	d.stopPersist = nil
	d.mu.Unlock()

	if stopPersist != nil {
		close(stopPersist)
	}
	for _, w := range watches {
		w.Close()
	}

	sessions := d.list()
	if stopSessions {
		d.line(fmt.Sprintf("shutting down (%s); stopping %d session(s) as requested", reason, len(sessions)))
		var wg sync.WaitGroup
		for _, s := range sessions {
			wg.Add(1)
			go func(s *session.Managed) {
				defer wg.Done()
				s.Stop("daemon " + reason)
			}(s)
		}
		wg.Wait()
		d.persist()
		for _, s := range sessions {
			s.Dispose()
		}
	} else {
		live := 0
		for _, s := range sessions {
			if pid := s.Pid(); pid > 0 && procs.IsAlive(pid) {
				live++
			}
		}
		d.line(fmt.Sprintf("shutting down (%s); leaving %d session(s) running", reason, live))
		// Persist before letting go: pid, signature and capture offsets are how
		// the successor picks each session back up mid-stream.
		for _, s := range sessions {
			s.Detach()
		}
		d.persist()
	}

	if d.server != nil {
		d.server.Close()
	}
	_ = os.Remove(d.socketPath)
	_ = os.Remove(paths.Get().DaemonPid)

	d.line("shutdown complete")
	d.log.Close()
	close(d.stopped)

	// exit false lets a test drive a full daemon handover in one process.
	if exit {
		os.Exit(0)
	}
}
