package cli

import (
	"encoding/json"
	"fmt"
	"os"
	"os/signal"
	"regexp"
	"strconv"
	"syscall"
	"time"

	"github.com/dumanhaydar/rcmanager/internal/procs"
	"github.com/dumanhaydar/rcmanager/internal/provider"
	codexprov "github.com/dumanhaydar/rcmanager/internal/provider/codex"
)

// codexSupervisor is the managed child of a Codex session.
//
// `codex remote-control start` self-backgrounds, so there is no long-lived
// process for rcmanager to watch. This is that process: it starts the machine-wide
// app-server daemon with remote control enabled, stays in the foreground
// reporting health, and stops the daemon again on SIGTERM or SIGINT.
//
// It is deliberately a hidden subcommand of rcmanager rather than a separate
// script: rcmanager is one binary, and re-executing itself keeps the managed
// child's argv — and therefore the signature interlock — under rcmanager's own
// control.
func codexSupervisor(_ []string, flags Flags) error {
	bin := flags.Str("bin", codexprov.Bin())
	name := flags.Str("name", "codex")
	poll := 30 * time.Second
	if v := flags.Str("poll-ms", ""); v != "" {
		if ms, err := strconv.Atoi(v); err == nil && ms > 0 {
			poll = time.Duration(ms) * time.Millisecond
		}
	}

	s := &codexSup{bin: bin, name: name, poll: poll}
	return s.run()
}

type codexSup struct {
	bin  string
	name string
	poll time.Duration

	stopping bool
}

func (s *codexSup) log(msg string) {
	fmt.Fprintf(os.Stdout, "[rcmanager:codex:%s] %s\n", s.name, msg)
}

func (s *codexSup) warn(msg string) {
	fmt.Fprintf(os.Stderr, "[rcmanager:codex:%s] %s\n", s.name, msg)
}

// daemonPid reads the pid Codex recorded for its app-server daemon.
func (s *codexSup) daemonPid() int {
	raw, err := os.ReadFile(codexprov.FilePaths().PidFile)
	if err != nil {
		return 0
	}
	var rec struct {
		Pid int `json:"pid"`
	}
	if err := json.Unmarshal(raw, &rec); err != nil {
		return 0
	}
	return rec.Pid
}

var connectFailed = regexp.MustCompile(`(?i)failed to connect`)

// healthy reports whether the control socket answers, which is the only real
// proof the daemon is usable.
func (s *codexSup) healthy() bool {
	r := provider.Probe(s.bin, []string{"app-server", "daemon", "version"}, 20*time.Second)
	return r.OK && !connectFailed.MatchString(r.Out+r.Err)
}

func (s *codexSup) ensureRunning() bool {
	if s.healthy() {
		return true
	}

	s.log("daemon not responding, starting it with remote control enabled…")
	start := provider.Probe(s.bin, []string{"remote-control", "start"}, 2*time.Minute)
	if start.Out != "" {
		s.log(start.Out)
	}
	if start.Err != "" {
		s.warn(start.Err)
	}

	// `remote-control start` already implies remote control, but re-asserting
	// is cheap and keeps a daemon that someone started plainly in the right
	// mode.
	enable := provider.Probe(s.bin, []string{"app-server", "daemon", "enable-remote-control"}, time.Minute)
	if !enable.OK && enable.Err != "" {
		s.warn(enable.Err)
	}

	ok := s.healthy()
	if ok {
		s.log(fmt.Sprintf("daemon online (pid %d)", s.daemonPid()))
	} else {
		s.log("daemon still not responding")
	}
	return ok
}

func (s *codexSup) shutdown(signal string) {
	if s.stopping {
		return
	}
	s.stopping = true
	s.log("received " + signal + ", stopping the Codex remote-control daemon…")
	r := provider.Probe(s.bin, []string{"remote-control", "stop"}, time.Minute)
	if r.Out != "" {
		s.log(r.Out)
	}
	if r.Err != "" {
		s.warn(r.Err)
	}
	os.Exit(0)
}

func (s *codexSup) run() error {
	signals := make(chan os.Signal, 1)
	signal.Notify(signals, syscall.SIGTERM, syscall.SIGINT)
	go func() {
		sig := <-signals
		s.shutdown(sig.String())
	}()

	s.log("supervising the machine-wide Codex remote-control daemon (" + s.bin + ")")
	s.log("note: Codex remote control is machine-wide — it cannot be bound to one thread.")

	if !s.ensureRunning() {
		s.warn("could not bring the Codex app-server daemon online; exiting so rcmanager can retry.")
		os.Exit(1)
	}

	lastPid := s.daemonPid()
	s.log(fmt.Sprintf("ready · daemon pid %d · pair with: %s remote-control pair", lastPid, s.bin))

	// Foreground health loop. Exiting non-zero hands the retry decision, and
	// the backoff, to rcmanager.
	for {
		time.Sleep(s.poll)
		if s.stopping {
			return nil
		}

		pid := s.daemonPid()
		if pid != lastPid {
			s.log(fmt.Sprintf("daemon pid changed %d -> %d", lastPid, pid))
			lastPid = pid
		}
		if procs.IsAlive(pid) && s.healthy() {
			continue
		}

		s.warn("daemon health check failed; attempting to restart it.")
		if !s.ensureRunning() {
			s.warn("restart failed; exiting so rcmanager can apply its own backoff.")
			os.Exit(1)
		}
		lastPid = s.daemonPid()
	}
}
