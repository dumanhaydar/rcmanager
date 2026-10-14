// Package isolate keeps a managed session out of the daemon's cgroup.
//
// Under systemd every process the daemon spawns inherits rcmanager.service's
// cgroup, and `systemctl stop` kills a cgroup, not a process. KillMode=process
// in the unit stops systemd doing that — but only for a unit rcmanager installed
// itself. Launching each session in its own transient scope makes the
// guarantee unconditional: the session is no longer part of rcmanager.service at
// all, so nothing that happens to the supervisor can reach it.
//
// `systemd-run --scope` execs the command in place rather than staying as a
// parent, so the pid rcmanager records is still the real process and the argv
// signature check in package procs keeps working unchanged.
package isolate

import (
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"sync"
	"time"
)

var (
	once      sync.Once
	available bool
)

// Available reports whether transient scopes can be used on this machine.
func Available() bool {
	once.Do(func() { available = probe() })
	return available
}

// ResetProbe clears the cached answer. Tests only.
func ResetProbe() {
	once = sync.Once{}
	available = false
}

func probe() bool {
	if runtime.GOOS != "linux" {
		return false
	}
	if _, err := os.Stat("/run/systemd/system"); err != nil {
		return false
	}
	if _, err := exec.LookPath("systemd-run"); err != nil {
		return false
	}
	// A user manager has to actually answer; --user against a dead bus fails at
	// spawn time, which would look like a crash-looping session.
	args := append(scopeFlags(), "--unit", "rcmanager-probe-"+strconv.Itoa(os.Getpid()), "--", "true")
	cmd := exec.Command("systemd-run", args...)
	done := make(chan error, 1)
	if err := cmd.Start(); err != nil {
		return false
	}
	go func() { done <- cmd.Wait() }()
	select {
	case err := <-done:
		return err == nil
	case <-time.After(5 * time.Second):
		_ = cmd.Process.Kill()
		return false
	}
}

func scopeFlags() []string {
	flags := []string{}
	if os.Geteuid() != 0 {
		flags = append(flags, "--user")
	}
	return append(flags, "--scope", "--collect", "--quiet")
}

// ResolveExecutable finds command on PATH, the way exec would.
//
// systemd-run resolves the executable against its own environment rather than
// inheriting the caller's lookup, so a bare name that would have been found
// dies with "Failed to find executable <x>: No such file or directory". That
// is invisible until you launch something that is not already in /usr/bin.
func ResolveExecutable(command string, path string) string {
	if strings.Contains(command, "/") {
		return command
	}
	for _, dir := range filepath.SplitList(path) {
		if dir == "" {
			continue
		}
		candidate := filepath.Join(dir, command)
		st, err := os.Stat(candidate)
		if err != nil || st.IsDir() {
			continue
		}
		if st.Mode()&0o111 != 0 {
			return candidate
		}
	}
	return ""
}

// Scoped is a launch wrapped in its own transient cgroup scope.
type Scoped struct {
	Command string
	Args    []string
	Unit    string
}

// Wrap wraps a launch so it lands in its own scope. name only labels the unit
// and is sanitised, because systemd unit names have a narrower alphabet than
// session names do.
//
// It returns nil when the command cannot be resolved to an absolute path:
// running in the daemon's own cgroup is a far better outcome than not running
// at all, so the caller falls back rather than handing systemd a name it will
// reject.
func Wrap(command string, args []string, name, path string) *Scoped {
	exe := ResolveExecutable(command, path)
	if exe == "" {
		return nil
	}
	unit := "rcmanager-" + sanitiseUnit(name) + "-" + strconv.FormatInt(time.Now().UnixMilli(), 10)
	full := append(scopeFlags(), "--unit", unit, "--")
	full = append(full, exe)
	full = append(full, args...)
	return &Scoped{Command: "systemd-run", Args: full, Unit: unit}
}

func sanitiseUnit(name string) string {
	var b strings.Builder
	for _, r := range name {
		switch {
		case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r >= '0' && r <= '9', r == '_', r == '-':
			b.WriteRune(r)
		default:
			b.WriteRune('_')
		}
	}
	return b.String()
}
