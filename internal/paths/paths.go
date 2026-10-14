// Package paths resolves every file rcmanager owns.
//
// All runtime state lives outside the binary, under RCMANAGER_HOME (default
// ~/.rcmanager). Nothing is ever written next to the executable.
package paths

import (
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"sync"
)

// P holds the absolute location of each file rcmanager reads or writes.
type P struct {
	Home      string // RCMANAGER_HOME
	Config    string // global settings (defaultProvider, ...)
	Registry  string // registered sessions (persisted)
	State     string // last known runtime state (pid, uptime, ...)
	Logs      string // per-session logs
	Run       string // stdin fifos and raw capture files
	Sock      string // daemon control socket
	DaemonPid string
	DaemonLog string
}

// The resolved paths are read from every goroutine in the daemon and
// recomputed when the environment changes, so they are guarded rather than
// merely initialised once.
var (
	mu    sync.RWMutex
	paths P
)

func init() { Reload() }

// Reload recomputes the paths from the environment. Tests use it after
// pointing RCMANAGER_HOME somewhere temporary.
func Reload() {
	mu.Lock()
	defer mu.Unlock()

	home := strings.TrimSpace(os.Getenv("RCMANAGER_HOME"))
	if home != "" {
		home, _ = filepath.Abs(home)
	} else {
		h, err := os.UserHomeDir()
		if err != nil {
			h = "."
		}
		home = filepath.Join(h, ".rcmanager")
	}

	sock := os.Getenv("RCMANAGER_SOCK")
	if sock == "" {
		sock = filepath.Join(home, "daemon.sock")
	}

	paths = P{
		Home:      home,
		Config:    filepath.Join(home, "config.json"),
		Registry:  filepath.Join(home, "registry.json"),
		State:     filepath.Join(home, "state.json"),
		Logs:      filepath.Join(home, "logs"),
		Run:       filepath.Join(home, "run"),
		Sock:      sock,
		DaemonPid: filepath.Join(home, "daemon.pid"),
		DaemonLog: filepath.Join(home, "daemon.log"),
	}
}

// Get returns the resolved paths.
func Get() P {
	mu.RLock()
	defer mu.RUnlock()
	return paths
}

// Watched are the two files a user edits by hand and the daemon watches.
func Watched() []string {
	p := Get()
	return []string{p.Config, p.Registry}
}

// EnsureHome creates the state directories, owner-only.
func EnsureHome() error {
	p := Get()
	for _, dir := range []string{p.Home, p.Logs, p.Run} {
		if err := os.MkdirAll(dir, 0o700); err != nil {
			return err
		}
	}
	return nil
}

// Logs holds the two human-readable log files of one session.
type LogFiles struct {
	Out string `json:"out"`
	Err string `json:"err"`
}

// LogPaths returns the rotated, ANSI-stripped logs of a session.
func LogPaths(name string) LogFiles {
	safe := SafeName(name)
	logs := Get().Logs
	return LogFiles{
		Out: filepath.Join(logs, safe+"-out.log"),
		Err: filepath.Join(logs, safe+"-err.log"),
	}
}

// IOFiles are the descriptors a session runs on between daemon lifetimes:
// Stdin is a fifo the daemon reopens to answer prompts; Out and Err are the
// raw files the child writes to directly. All three outlive the daemon on
// purpose — see package sio.
type IOFiles struct {
	Stdin string
	Out   string
	Err   string
}

// IOPaths returns a session's stdio locations.
func IOPaths(name string) IOFiles {
	safe := SafeName(name)
	run := Get().Run
	return IOFiles{
		Stdin: filepath.Join(run, safe+".in"),
		Out:   filepath.Join(run, safe+".out.raw"),
		Err:   filepath.Join(run, safe+".err.raw"),
	}
}

var unsafeName = regexp.MustCompile(`[^a-zA-Z0-9._-]`)

// SafeName makes a session name usable as a filename component.
func SafeName(name string) string { return unsafeName.ReplaceAllString(name, "_") }
