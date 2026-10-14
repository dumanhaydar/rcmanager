// Package procs is rcmanager's safety interlock around other people's processes.
//
// rcmanager must never signal a process it did not start, so every kill is gated
// on the live argv still matching the command rcmanager recorded. When argv cannot
// be read at all, the answer is "refuse", not "guess".
package procs

import (
	"strings"
	"syscall"
	"time"
)

// Proc is a live process and its full command line.
type Proc struct {
	Pid  int
	Argv []string
}

// IsAlive reports whether a pid exists and could be signalled. EPERM counts:
// the process exists, it just belongs to someone else.
//
// A zombie counts as alive, because signalling one succeeds. That is correct
// for every caller here: rcmanager reaps its own children as they exit, and an
// adopted process belongs to someone else's process tree, which reaps it. The
// one place it would mislead is code that waits for a process it does not own
// and never reaps — don't write that.
func IsAlive(pid int) bool {
	if pid <= 0 {
		return false
	}
	err := syscall.Kill(pid, 0)
	if err == nil {
		return true
	}
	return err == syscall.EPERM
}

// Cmdline is the full argv of a running process, or nil.
func Cmdline(pid int) []string { return readCmdline(pid) }

// MatchesSignature reports whether the live argv of pid still contains every
// recorded token. An empty signature never matches: refusing is the safe
// answer when there is nothing to compare against.
func MatchesSignature(pid int, signature []string) bool {
	if len(signature) == 0 {
		return false
	}
	argv := readCmdline(pid)
	if len(argv) == 0 {
		return false
	}
	haystack := strings.Join(argv, " ")
	for _, token := range signature {
		if token == "" {
			continue
		}
		if !strings.Contains(haystack, token) {
			return false
		}
	}
	return true
}

// SignalGroup signals a whole process group, falling back to the single pid.
// Children are started in their own group, so this reaches a pty wrapper and
// the agent inside it together — signalling only the wrapper would strand the
// agent.
func SignalGroup(pid int, sig syscall.Signal) bool {
	if err := syscall.Kill(-pid, sig); err == nil {
		return true
	}
	return syscall.Kill(pid, sig) == nil
}

// Outcome is what Terminate managed to do.
type Outcome string

const (
	// Exited means the process was already gone, or went down on SIGTERM.
	Exited Outcome = "exited"
	// Killed means it took a SIGKILL.
	Killed Outcome = "killed"
	// Refused means the pid no longer looks like the process rcmanager started, so
	// nothing was signalled at all.
	Refused Outcome = "refused"
)

// Terminate sends SIGTERM, waits for the process to go, then SIGKILLs it.
// The signature is mandatory: without a match nothing is signalled.
func Terminate(pid int, signature []string, timeout time.Duration, group bool) Outcome {
	if !IsAlive(pid) {
		return Exited
	}
	if !MatchesSignature(pid, signature) {
		return Refused
	}

	// A pty wrapper puts the agent in its own process group, so record the tree
	// before signalling: anything that outlives the parent is swept up after,
	// and only if it is BOTH a descendant we recorded and still a signature
	// match.
	var tree []int
	if group {
		tree = DescendantsOf(pid)
	}

	if group {
		SignalGroup(pid, syscall.SIGTERM)
	} else {
		_ = syscall.Kill(pid, syscall.SIGTERM)
	}

	const poll = 200 * time.Millisecond
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		time.Sleep(poll)
		if !IsAlive(pid) {
			SweepOrphans(tree, signature)
			return Exited
		}
	}

	if MatchesSignature(pid, signature) {
		if group {
			SignalGroup(pid, syscall.SIGKILL)
		} else {
			_ = syscall.Kill(pid, syscall.SIGKILL)
		}
	}
	time.Sleep(poll)
	SweepOrphans(tree, signature)
	if IsAlive(pid) {
		return Refused
	}
	return Killed
}

// SweepOrphans terminates leftovers from a stopped session.
//
// Deliberately narrow: a pid is only signalled when it was a descendant of the
// process rcmanager started AND its live argv still matches the recorded
// signature. Anything else — an agent the user started themselves, a recycled
// pid — is left completely alone.
func SweepOrphans(tree []int, signature []string) {
	for _, pid := range tree {
		if !IsAlive(pid) || !MatchesSignature(pid, signature) {
			continue
		}
		_ = syscall.Kill(pid, syscall.SIGTERM)
	}
}

// DescendantsOf returns every descendant pid of root, breadth-first.
func DescendantsOf(root int) []int {
	children := map[int][]int{}
	for _, rel := range relations() {
		children[rel.ppid] = append(children[rel.ppid], rel.pid)
	}
	var out []int
	queue := append([]int(nil), children[root]...)
	for len(queue) > 0 {
		pid := queue[0]
		queue = queue[1:]
		out = append(out, pid)
		queue = append(queue, children[pid]...)
	}
	return out
}

type relation struct{ pid, ppid int }
