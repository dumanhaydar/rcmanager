// Package client is how the CLI talks to the daemon, starting one if needed.
package client

import (
	"fmt"
	"os"
	"os/exec"
	"syscall"
	"time"

	"github.com/dumanhaydar/rcmanager/internal/ansi"
	"github.com/dumanhaydar/rcmanager/internal/daemon"
	"github.com/dumanhaydar/rcmanager/internal/ipc"
	"github.com/dumanhaydar/rcmanager/internal/jsonfile"
	"github.com/dumanhaydar/rcmanager/internal/paths"
	"github.com/dumanhaydar/rcmanager/internal/procs"
	"github.com/dumanhaydar/rcmanager/internal/rcerr"
	"github.com/dumanhaydar/rcmanager/internal/version"
)

// Info is what the pid file and a liveness check say about the daemon.
type Info struct {
	Pid       int    `json:"pid"`
	Alive     bool   `json:"alive"`
	StartedAt string `json:"startedAt,omitempty"`
	Socket    string `json:"socket"`
}

type pidRecord struct {
	Pid       int    `json:"pid"`
	StartedAt string `json:"startedAt"`
}

// DaemonInfo reads the daemon's pid file and checks the process.
func DaemonInfo() Info {
	var rec pidRecord
	_ = jsonfile.Read(paths.Get().DaemonPid, &rec)
	return Info{
		Pid:       rec.Pid,
		Alive:     rec.Pid != 0 && procs.IsAlive(rec.Pid),
		StartedAt: rec.StartedAt,
		Socket:    paths.Get().Sock,
	}
}

// Handshake is the running daemon's own account of itself, or nil when
// nothing answers. This is the ping every command makes anyway, with the
// reply kept.
func Handshake(timeout time.Duration) *daemon.Pong {
	var pong daemon.Pong
	if err := ipc.Call(paths.Get().Sock, "ping", ipc.Args{}, &pong, ipc.CallOptions{Timeout: timeout}); err != nil {
		return nil
	}
	return &pong
}

var warnedAboutSkew bool

// warnOnSkew says so when the CLI and the daemon come from different installs.
//
// Replacing the binary makes the CLI the new version from that moment; the
// daemon is a running process and keeps executing the old code until it is
// restarted. Nothing about that is visible — you get a new client driving an
// old server over an IPC contract that may have changed underneath it, which
// is a bad way to find out.
//
// Once per process, and never fatal: a version difference is usually
// harmless, and refusing to work would be worse than saying so.
func warnOnSkew(pong *daemon.Pong) {
	if warnedAboutSkew || pong == nil || pong.Version == "" || pong.Version == version.Version {
		return
	}
	warnedAboutSkew = true
	fmt.Fprintf(os.Stderr, "%s rcmanager %s is talking to a daemon running %s\n",
		ansi.Yellow("!"), ansi.Bold(version.Version), ansi.Bold(pong.Version))
	fmt.Fprintf(os.Stderr, "  %s\n", ansi.Grey("the daemon keeps running the code it started with — restart it to catch up:"))
	fmt.Fprintf(os.Stderr, "  %s   %s\n", ansi.Grey("rcmanager upgrade"), ansi.Grey("(your sessions keep running)"))
}

// EnsureDaemon starts the daemon if nothing is listening, so a bare command
// works without a separate "start the supervisor first" step. Under a service
// manager the unit owns it; this is the fallback for a plain shell.
func EnsureDaemon(autostart bool) (Info, error) {
	if pong := Handshake(1500 * time.Millisecond); pong != nil {
		warnOnSkew(pong)
		return DaemonInfo(), nil
	}
	if !autostart {
		return Info{}, rcerr.New("The rcmanager daemon is not running.",
			"Start it with `rcmanager daemon --detached` (or start the installed service).")
	}

	if err := paths.EnsureHome(); err != nil {
		return Info{}, err
	}
	// Clear a socket left behind by a daemon that died hard.
	if _, err := os.Stat(paths.Get().Sock); err == nil {
		_ = os.Remove(paths.Get().Sock)
	}

	self, err := os.Executable()
	if err != nil {
		return Info{}, err
	}
	logFile, err := os.OpenFile(paths.Get().DaemonLog, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o600)
	if err != nil {
		return Info{}, err
	}
	defer logFile.Close()

	cmd := exec.Command(self, "daemon", "--foreground")
	cmd.Stdout = logFile
	cmd.Stderr = logFile
	cmd.Env = os.Environ()
	// Its own session, so the daemon outlives the shell that started it.
	cmd.SysProcAttr = &syscall.SysProcAttr{Setsid: true}
	if err := cmd.Start(); err != nil {
		return Info{}, err
	}
	// The daemon is deliberately orphaned; reaping it here would kill it when
	// the CLI exits.
	go func() { _ = cmd.Process.Release() }()

	deadline := time.Now().Add(20 * time.Second)
	for time.Now().Before(deadline) {
		time.Sleep(150 * time.Millisecond)
		if Handshake(1500*time.Millisecond) != nil {
			return DaemonInfo(), nil
		}
	}
	return Info{}, rcerr.New("Timed out waiting for the rcmanager daemon to start.",
		"See "+paths.Get().DaemonLog+" for details.")
}

// Options tunes one call.
type Options struct {
	Autostart  bool
	Timeout    time.Duration
	OnProgress func(ipc.Progress)
}

// Call sends one command to the daemon, starting it first when allowed.
func Call(cmd string, args ipc.Args, out any, opts Options) error {
	if _, err := EnsureDaemon(opts.Autostart); err != nil {
		return err
	}
	timeout := opts.Timeout
	if timeout <= 0 {
		timeout = 60 * time.Second
	}
	return ipc.Call(paths.Get().Sock, cmd, args, out, ipc.CallOptions{
		Timeout:    timeout,
		OnProgress: opts.OnProgress,
	})
}
