// Package daemon is rcmanager's supervisor: one long-lived process that owns every
// managed session and answers CLI requests over a unix socket. The OS service
// manager supervises this; this supervises the agents.
//
// The daemon is deliberately disposable. Sessions run on stdio that outlives
// it (package sio) in cgroup scopes it does not own (package isolate), so
// stopping, restarting or crashing the daemon does not touch a running agent —
// a successor adopts them and resumes log capture mid-stream. Only an explicit
// `rcmanager stop` ends a session.
package daemon

import (
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"os"
	"runtime"
	"strings"
	"sync"
	"time"

	"github.com/dumanhaydar/rcmanager/internal/config"
	"github.com/dumanhaydar/rcmanager/internal/ipc"
	"github.com/dumanhaydar/rcmanager/internal/jsonfile"
	"github.com/dumanhaydar/rcmanager/internal/logstream"
	"github.com/dumanhaydar/rcmanager/internal/paths"
	"github.com/dumanhaydar/rcmanager/internal/procs"
	"github.com/dumanhaydar/rcmanager/internal/provider"
	"github.com/dumanhaydar/rcmanager/internal/rcerr"
	"github.com/dumanhaydar/rcmanager/internal/registry"
	"github.com/dumanhaydar/rcmanager/internal/session"
	"github.com/dumanhaydar/rcmanager/internal/version"
	"github.com/dumanhaydar/rcmanager/internal/watch"
)

// state is what the daemon persists so a successor can re-adopt.
type state struct {
	DaemonPid int                 `json:"daemonPid"`
	UpdatedAt string              `json:"updatedAt"`
	Sessions  []session.Persisted `json:"sessions"`
}

type pidRecord struct {
	Pid       int    `json:"pid"`
	StartedAt string `json:"startedAt"`
}

// Pong is the daemon's own account of itself. The version is what lets the CLI
// notice it is talking to a daemon from a different install.
type Pong struct {
	Pid        int    `json:"pid"`
	Sessions   int    `json:"sessions"`
	Socket     string `json:"socket"`
	Version    string `json:"version"`
	Ready      bool   `json:"ready"`
	Entrypoint string `json:"entrypoint"`
	StartedAt  string `json:"startedAt"`
}

// BulkResult is one session's outcome in a startAll, stopAll or restartAll.
type BulkResult struct {
	Name   string       `json:"name"`
	OK     bool         `json:"ok"`
	Error  string       `json:"error,omitempty"`
	Status session.View `json:"status"`
}

// ReloadReport is what changed when the registry was re-read.
type ReloadReport struct {
	Started   []string `json:"started,omitempty"`
	Stopped   []string `json:"stopped,omitempty"`
	Removed   []string `json:"removed,omitempty"`
	Restarted []string `json:"restarted,omitempty"`
	Flagged   []string `json:"flagged,omitempty"`
	Unchanged int      `json:"unchanged"`
}

// Daemon supervises every registered session.
type Daemon struct {
	socketPath string

	mu       sync.Mutex
	sessions map[string]*session.Managed
	order    []string
	cfg      config.Config

	server  *ipc.Server
	log     *logstream.Stream
	watches []*watch.Watcher

	shuttingDown bool
	startedAt    string
	// ready distinguishes "listening" from "has taken over its sessions". The
	// socket is bound before adoption so the CLI can answer promptly, which
	// means an early `list` can catch a session mid-handover and report it as
	// not running. Anything comparing before and after a restart needs to wait
	// for this, not just for the socket.
	ready bool

	saveTimer   *time.Timer
	stopPersist chan struct{}
	stopped     chan struct{}
}

// New builds a daemon bound to the configured socket.
func New() *Daemon {
	return &Daemon{
		socketPath: paths.Get().Sock,
		sessions:   map[string]*session.Managed{},
		cfg:        config.Load(),
		log: logstream.New(paths.Get().DaemonLog, logstream.Options{
			MaxBytes: 5 * 1024 * 1024, Retain: 3, Strip: true,
		}),
		stopped: make(chan struct{}),
	}
}

func (d *Daemon) line(msg string) { d.log.Line(msg) }

// Start claims the pid file, binds the socket and takes over every enabled
// session.
func (d *Daemon) Start() error {
	if err := paths.EnsureHome(); err != nil {
		return err
	}
	if err := d.claimPidFile(); err != nil {
		return err
	}
	d.startedAt = time.Now().UTC().Format(time.RFC3339)
	d.line(fmt.Sprintf("daemon starting (pid %d, rcmanager %s, %s/%s)",
		os.Getpid(), version.Version, runtime.GOOS, runtime.GOARCH))

	var saved state
	_ = jsonfile.Read(paths.Get().State, &saved)
	savedByName := map[string]session.Persisted{}
	for _, s := range saved.Sessions {
		savedByName[s.Name] = s
	}

	entries, err := registry.Load()
	if err != nil {
		d.line("registry is unreadable: " + err.Error())
		entries = nil
	}
	for _, entry := range entries {
		s, err := d.attach(entry)
		if err != nil {
			d.line("cannot attach " + entry.Name + ": " + err.Error())
			continue
		}
		if p, ok := savedByName[entry.Name]; ok {
			persisted := p
			s.SetSavedState(&persisted)
		}
	}

	server, err := ipc.Listen(d.socketPath, d.handle)
	if err != nil {
		return err
	}
	d.server = server
	d.line("listening on " + d.socketPath)

	d.restoreEnabled()
	d.mu.Lock()
	d.ready = true
	d.mu.Unlock()
	d.startWatching()

	stopPersist := make(chan struct{})
	d.mu.Lock()
	d.stopPersist = stopPersist
	d.mu.Unlock()
	// The channel is passed in rather than read from the struct, so shutdown
	// can retire it without racing the loop that watches it.
	go d.persistLoop(stopPersist)
	return nil
}

func (d *Daemon) claimPidFile() error {
	var existing pidRecord
	_ = jsonfile.Read(paths.Get().DaemonPid, &existing)
	if existing.Pid != 0 && existing.Pid != os.Getpid() && procs.IsAlive(existing.Pid) {
		return rcerr.Newf("Use `rcmanager daemon --stop` or stop the service before starting another.",
			"Another rcmanager daemon is already running (pid %d).", existing.Pid)
	}
	return jsonfile.Write(paths.Get().DaemonPid, pidRecord{
		Pid: os.Getpid(), StartedAt: time.Now().UTC().Format(time.RFC3339),
	}, 0o600)
}

// restoreEnabled brings every enabled session up. Most of the time this adopts
// processes that never stopped running, which is instant; only genuinely dead
// ones are spawned, and only those need staggering.
func (d *Daemon) restoreEnabled() {
	var enabled []*session.Managed
	for _, s := range d.list() {
		if s.Entry().Enabled {
			enabled = append(enabled, s)
		}
	}
	d.line(fmt.Sprintf("restoring %d enabled session(s)", len(enabled)))

	adopted := 0
	for _, s := range enabled {
		r := s.Start(false)
		if !r.OK {
			d.line("restore " + s.Name() + ": " + r.Error)
		}
		if r.Adopted {
			adopted++
			continue
		}
		// Stagger: a herd of agent CLIs starting at once hammers the box.
		time.Sleep(750 * time.Millisecond)
	}
	if adopted > 0 {
		d.line(fmt.Sprintf("adopted %d session(s) that outlived the previous daemon", adopted))
	}
	d.persist()
}

// startWatching watches the two files a user edits by hand. Reloading is cheap
// and non-disruptive, so both funnel into the same reconcile.
func (d *Daemon) startWatching() {
	if !d.cfg.WatchConfig {
		return
	}
	files := paths.Watched()
	live := 0
	for _, file := range files {
		f := file
		w := watch.File(f, func() {
			d.line(f + " changed on disk — reloading")
			if _, err := d.reload(); err != nil {
				d.line("reload failed: " + err.Error())
			}
		}, func() bool { return jsonfile.IsOwnWrite(f) })
		d.watches = append(d.watches, w)
		if w.Active() {
			live++
		}
	}
	if live == len(files) {
		d.line("watching " + strings.Join(files, " and ") + " for changes")
	} else {
		d.line("could not watch every config file; use `rcmanager reload` after editing them")
	}
}

// assignSessionID gives a session an id if its provider works in terms of ids
// and it has none.
//
// Entries registered before rcmanager minted ids up front have no sessionId, and
// "no id" used to mean "run the environment server" — which is how a request
// for a session produced something else entirely. Assigning one here, and
// persisting it, makes those entries resume a single conversation from now on
// instead of starting over on every launch.
func (d *Daemon) assignSessionID(entry *registry.Entry) *registry.Entry {
	p, err := provider.Get(entry.Provider)
	if err != nil {
		return entry
	}
	if !p.Capabilities().ResumeBySessionID || entry.SessionID != "" {
		return entry
	}
	withID := entry.Clone()
	withID.SessionID = NewUUID()
	stored, err := registry.Upsert(withID)
	if err != nil {
		return entry
	}
	d.line(entry.Name + ": assigned session id " + stored.SessionID + " (was unset)")
	return stored
}

// NewUUID mints a version-4 UUID, the shape every provider expects.
func NewUUID() string {
	var b [16]byte
	if _, err := rand.Read(b[:]); err != nil {
		return hex.EncodeToString(b[:])
	}
	b[6] = (b[6] & 0x0f) | 0x40
	b[8] = (b[8] & 0x3f) | 0x80
	h := hex.EncodeToString(b[:])
	return h[0:8] + "-" + h[8:12] + "-" + h[12:16] + "-" + h[16:20] + "-" + h[20:32]
}

// attach registers a session with the daemon, creating it if new.
func (d *Daemon) attach(entry *registry.Entry) (*session.Managed, error) {
	entry = d.assignSessionID(entry)

	d.mu.Lock()
	existing, ok := d.sessions[entry.Name]
	cfg := d.cfg
	d.mu.Unlock()

	if ok {
		existing.SetEntry(entry)
		existing.SetConfig(cfg)
		return existing, nil
	}

	p, err := provider.Get(entry.Provider)
	if err != nil {
		return nil, err
	}
	s := session.New(entry, p, session.Options{
		Config:   cfg,
		OnChange: d.schedulePersist,
		CanStart: d.singletonVeto,
	})

	d.mu.Lock()
	d.sessions[entry.Name] = s
	d.order = append(d.order, entry.Name)
	d.mu.Unlock()
	return s, nil
}

// singletonVeto refuses a second managed session of a provider that exposes
// one machine-wide remote-control daemon (Codex), where a second session would
// just fight the first.
//
// Returned rather than raised, and consulted by the session itself, so the
// rule holds on every path — autorestart and boot restore included, not only
// the one command that used to check it.
func (d *Daemon) singletonVeto(s *session.Managed) *session.Veto {
	if !s.Provider().Capabilities().Singleton {
		return nil
	}
	for _, other := range d.list() {
		if other == s || other.Entry().Provider != s.Entry().Provider {
			continue
		}
		if st := other.Status(); st == session.Online || st == session.Launching {
			return &session.Veto{
				Error: fmt.Sprintf("%s allows only one managed session per machine, and %q is already running.",
					s.Provider().DisplayName(), other.Name()),
				Hint: "Stop it first:  rcmanager stop " + other.Name(),
			}
		}
	}
	return nil
}

// list returns the sessions in registration order.
func (d *Daemon) list() []*session.Managed {
	d.mu.Lock()
	defer d.mu.Unlock()
	out := make([]*session.Managed, 0, len(d.order))
	for _, name := range d.order {
		if s, ok := d.sessions[name]; ok {
			out = append(out, s)
		}
	}
	return out
}

func (d *Daemon) get(name string) (*session.Managed, error) {
	d.mu.Lock()
	s, ok := d.sessions[name]
	d.mu.Unlock()
	if !ok {
		return nil, rcerr.Newf("Run `rcmanager list` to see registered sessions.",
			"No session named %q is registered.", name)
	}
	return s, nil
}

func (d *Daemon) forget(name string) {
	d.mu.Lock()
	delete(d.sessions, name)
	next := d.order[:0]
	for _, n := range d.order {
		if n != name {
			next = append(next, n)
		}
	}
	d.order = next
	d.mu.Unlock()
}

// ------------------------------------------------------------------ persist

func (d *Daemon) schedulePersist() {
	d.mu.Lock()
	defer d.mu.Unlock()
	if d.saveTimer != nil {
		return
	}
	d.saveTimer = time.AfterFunc(500*time.Millisecond, func() {
		d.mu.Lock()
		d.saveTimer = nil
		d.mu.Unlock()
		d.persist()
	})
}

func (d *Daemon) persist() {
	sessions := d.list()
	out := make([]session.Persisted, 0, len(sessions))
	for _, s := range sessions {
		out = append(out, s.Persisted())
	}
	if err := jsonfile.Write(paths.Get().State, state{
		DaemonPid: os.Getpid(),
		UpdatedAt: time.Now().UTC().Format(time.RFC3339),
		Sessions:  out,
	}, 0o600); err != nil {
		d.line("persist failed: " + err.Error())
	}
}

func (d *Daemon) persistLoop(stop <-chan struct{}) {
	ticker := time.NewTicker(10 * time.Second)
	defer ticker.Stop()
	for {
		select {
		case <-stop:
			return
		case <-ticker.C:
			d.persist()
		}
	}
}

// Wait blocks until the daemon has shut down.
func (d *Daemon) Wait() { <-d.stopped }
