// Package session supervises one managed agent process.
//
// A session owns exactly one child, its logs and its restart policy.
// Everything that can signal a pid goes through procs.Terminate, which refuses
// to act unless the live argv still matches the recorded signature.
//
// The child is deliberately *not* tied to the daemon's lifetime: its stdio
// lives in files and a fifo (package sio) and, where systemd allows it, in its
// own cgroup scope (package isolate). A daemon restart reattaches to a session
// that never noticed the daemon was gone.
package session

import (
	"os/exec"
	"sync"
	"sync/atomic"
	"time"

	"github.com/dumanhaydar/rcmanager/internal/claim"
	"github.com/dumanhaydar/rcmanager/internal/config"
	"github.com/dumanhaydar/rcmanager/internal/logstream"
	"github.com/dumanhaydar/rcmanager/internal/paths"
	"github.com/dumanhaydar/rcmanager/internal/provider"
	"github.com/dumanhaydar/rcmanager/internal/registry"
	"github.com/dumanhaydar/rcmanager/internal/sio"
)

// Status is where a session stands.
type Status string

const (
	Stopped   Status = "stopped"
	Launching Status = "launching"
	Online    Status = "online"
	// Restarting is waiting out the restart delay after an unexpected exit.
	// Distinct from Launching on purpose: Start refuses to act on a session
	// that is already coming up, so parking a crashed session in Launching
	// meant the restart timer fired into that guard and the session sat there
	// forever.
	Restarting Status = "restarting"
	Errored    Status = "errored"
	Disabled   Status = "disabled"
)

// urlWindow is how long after a launch a URL in the output is still plausibly
// the announcement.
const urlWindow = 5 * time.Minute

// urlWatchdog is how long to wait for the remote-control URL before saying so.
//
// "online" has to mean reachable, not merely alive. A session stalled on an
// unanswered startup prompt keeps its process, passes minUptime, and reports
// online forever while never appearing in the app — which is worse than an
// error, because nothing suggests looking.
const urlWatchdog = 90 * time.Second

// Veto is the daemon's refusal to start a session (a provider singleton).
type Veto struct {
	Error string
	Hint  string
}

// Exit records how a child went away.
type Exit struct {
	Code     int    `json:"code"`
	Signal   string `json:"signal,omitempty"`
	At       string `json:"at"`
	UptimeMs int64  `json:"uptimeMs"`
}

// Persisted is the subset written to state.json so a fresh daemon can
// re-adopt a session that outlived its predecessor.
type Persisted struct {
	Name      string      `json:"name"`
	Pid       int         `json:"pid"`
	Signature []string    `json:"signature,omitempty"`
	StartedAt int64       `json:"startedAt,omitempty"`
	Status    Status      `json:"status"`
	Restarts  int         `json:"restarts"`
	RemoteURL string      `json:"remoteUrl,omitempty"`
	Scope     string      `json:"scope,omitempty"`
	Capture   sio.Offsets `json:"capture"`
}

// View is everything rcmanager reports about a session over IPC.
//
// No field is omitempty on purpose: "pid 0" and "no message" are answers, and
// leaving them out lets a client that decodes into a reused struct keep the
// previous session's values — a stale pid being the worst of them.
type View struct {
	Name         string         `json:"name"`
	Provider     string         `json:"provider"`
	SessionID    string         `json:"sessionId"`
	Cwd          string         `json:"cwd"`
	Enabled      bool           `json:"enabled"`
	Status       Status         `json:"status"`
	Pid          int            `json:"pid"`
	StartedAt    string         `json:"startedAt"`
	UptimeMs     int64          `json:"uptimeMs"`
	Restarts     int            `json:"restarts"`
	Adopted      bool           `json:"adopted"`
	RemoteURL    string         `json:"remoteUrl"`
	Message      string         `json:"message"`
	NeedsRestart []string       `json:"needsRestart"`
	LastExit     *Exit          `json:"lastExit"`
	Signature    []string       `json:"signature"`
	Scope        string         `json:"scope"`
	Logs         paths.LogFiles `json:"logs"`
	Note         string         `json:"note"`
	Source       string         `json:"source"`
}

// Result is the outcome of a lifecycle request.
type Result struct {
	OK             bool
	Adopted        bool
	AlreadyRunning bool
	AlreadyStopped bool
	Error          string
	Hint           string
	Status         View
}

// Managed is one supervised agent process.
type Managed struct {
	mu sync.Mutex

	entry    *registry.Entry
	cfg      config.Config
	prov     provider.Provider
	onChange func()
	canStart func(*Managed) *Veto

	cmd       *exec.Cmd
	pid       int
	signature []string
	startedAt time.Time
	// urlWatchFrom is when *this* daemon started reading this session's
	// output. The announcement window is measured from here, not from the
	// session's start: an adopted session announced its URL long before this
	// daemon existed, and the TUI reprints the banner on every repaint, so
	// measuring from the original start meant never looking at all.
	urlWatchFrom time.Time
	status       Status
	message      string
	remoteURL    string
	urlLocked    bool
	lastExit     *Exit
	restarts     int
	// restartTimes is the crash-loop budget: only short-lived exits are
	// recorded, so a session that ran happily for hours always gets a fresh
	// allowance.
	restartTimes []time.Time
	adopted      bool
	needsRestart []string
	scope        string
	savedState   *Persisted
	// launchedWith is the baseline a later edit is compared against. It has to
	// be what this process was launched with, not the last thing the document
	// said, or reverting an edit re-flags it and the flag never clears.
	launchedWith *registry.Entry

	stopping     bool
	restartTimer *time.Timer
	urlTimer     *time.Timer
	minUpTimer   *time.Timer
	adoptStop    chan struct{}

	claim     *claim.Claim
	io        *sio.IO
	tailers   []*sio.Tailer
	captureAt sio.Offsets
	logFiles  paths.LogFiles
	out       *logstream.Stream
	err       *logstream.Stream

	ingestCh chan chunk
	pending  map[string]bool
	// needAnalysis is read by the capture sinks, which run on the tailers'
	// goroutines and must not touch the session lock. Once the URL is known
	// and every one-shot prompt has been answered, there is nothing left to
	// look for in a session's output — and an agent TUI repaints several times
	// a second for the rest of its life. Logs are still written; the matching
	// is what stops.
	needAnalysis atomic.Bool
	tailBuf      string
	urlBuf       string
	blocked      string
	usesPty      bool
}

// Options configures a managed session.
type Options struct {
	Config   config.Config
	OnChange func()
	// CanStart is the daemon's veto, checked on every path that starts a
	// session — autorestart and boot restore included, not only the CLI's.
	CanStart func(*Managed) *Veto
}

// New builds a managed session from a registry entry.
func New(entry *registry.Entry, p provider.Provider, opts Options) *Managed {
	status := Stopped
	if !entry.Enabled {
		status = Disabled
	}
	onChange := opts.OnChange
	if onChange == nil {
		onChange = func() {}
	}
	return &Managed{
		entry:    entry,
		cfg:      opts.Config,
		prov:     p,
		onChange: onChange,
		canStart: opts.CanStart,
		status:   status,
		io:       sio.New(entry.Name),
		logFiles: paths.LogPaths(entry.Name),
		pending:  map[string]bool{},
	}
}

// Name is the session's registered name.
func (m *Managed) Name() string {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.entry.Name
}

// Entry returns the current registry entry.
func (m *Managed) Entry() *registry.Entry {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.entry
}

// Provider returns the adapter driving this session.
func (m *Managed) Provider() provider.Provider {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.prov
}

// Status is the current state.
func (m *Managed) Status() Status {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.status
}

// Pid is the managed process, or 0.
func (m *Managed) Pid() int {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.pid
}

// SetConfig applies new global settings.
func (m *Managed) SetConfig(cfg config.Config) {
	m.mu.Lock()
	m.cfg = cfg
	m.mu.Unlock()
}

// SetEntry replaces the entry without reconciling anything. The daemon uses
// it when only the registered flags changed.
func (m *Managed) SetEntry(e *registry.Entry) {
	m.mu.Lock()
	m.entry = e
	m.mu.Unlock()
	m.onChange()
}

// SetSavedState hands the session the pid and offsets a previous daemon
// recorded, so Start can adopt rather than duplicate.
func (m *Managed) SetSavedState(p *Persisted) {
	m.mu.Lock()
	m.savedState = p
	m.mu.Unlock()
}

// ---------------------------------------------------------------- settings

func (m *Managed) settingBool(name string) bool {
	switch name {
	case "autorestart":
		if m.entry.Autorestart != nil {
			return *m.entry.Autorestart
		}
		return m.cfg.Autorestart
	}
	return false
}

func (m *Managed) settingInt(name string) int {
	if name == "maxRestarts" {
		if m.entry.MaxRestarts != nil {
			return *m.entry.MaxRestarts
		}
		return m.cfg.MaxRestarts
	}
	return 0
}

func (m *Managed) settingDuration(name string) time.Duration {
	ms := func(override *int64, global int64) time.Duration {
		if override != nil {
			return time.Duration(*override) * time.Millisecond
		}
		return time.Duration(global) * time.Millisecond
	}
	switch name {
	case "restartDelayMs":
		return ms(m.entry.RestartDelayMs, m.cfg.RestartDelayMs)
	case "restartWindowMs":
		return ms(m.entry.RestartWindowMs, m.cfg.RestartWindowMs)
	case "minUptimeMs":
		return ms(m.entry.MinUptimeMs, m.cfg.MinUptimeMs)
	case "stopTimeoutMs":
		return ms(m.entry.StopTimeoutMs, m.cfg.StopTimeoutMs)
	}
	return 0
}

// -------------------------------------------------------------------- logs

func (m *Managed) openLogsLocked() {
	opts := logstream.Options{
		MaxBytes: m.cfg.MaxLogBytes,
		Retain:   m.cfg.LogRetain,
		Strip:    m.cfg.StripAnsiInLogs,
	}
	if m.out == nil {
		m.out = logstream.New(m.logFiles.Out, opts)
	}
	if m.err == nil {
		m.err = logstream.New(m.logFiles.Err, opts)
	}
}

// ReopenLogs reopens the human-readable logs so changed log settings take
// effect on a live session.
func (m *Managed) ReopenLogs() {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.out != nil {
		m.out.Close()
	}
	if m.err != nil {
		m.err.Close()
	}
	m.out, m.err = nil, nil
	m.openLogsLocked()
}

// HasLogs reports whether this session has ever opened its logs.
func (m *Managed) HasLogs() bool {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.out != nil
}

func (m *Managed) logLocked(text string) {
	m.openLogsLocked()
	m.out.Line("rcmanager | " + text)
}

// Log writes a line to the session's own log, for the things the daemon does
// to a session rather than with it.
func (m *Managed) Log(text string) { m.log(text) }

func (m *Managed) log(text string) {
	m.mu.Lock()
	m.logLocked(text)
	m.mu.Unlock()
}

// updateLocked mutates state and notifies the daemon once the lock is free.
func (m *Managed) updateLocked(apply func()) {
	apply()
	go m.onChange()
}
