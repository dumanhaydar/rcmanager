package procs

import (
	"os"
	"os/exec"
	"strconv"
	"testing"
	"time"
)

// The interlock: rcmanager may only signal a process whose live command line
// still matches what it recorded. Everything else in the supervisor leans on
// this, including the decision to check an adopted session's argv once a
// minute rather than on every poll — a stale belief about a pid can never
// become a signal sent to the wrong process.
func TestTerminateRefusesAMismatchedSignature(t *testing.T) {
	cmd := exec.Command("sh", "-c", "# rcmanager-procs-test\nwhile true; do sleep 1; done")
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	pid := cmd.Process.Pid
	// Reap it as it exits: a zombie answers kill(pid, 0), so a child nobody
	// waits on looks alive forever — see the note on IsAlive.
	reaped := make(chan struct{})
	go func() {
		_, _ = cmd.Process.Wait()
		close(reaped)
	}()
	t.Cleanup(func() {
		_ = cmd.Process.Kill()
		<-reaped
	})

	// Give the process a moment to be visible to whatever reads argv.
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) && len(Cmdline(pid)) == 0 {
		time.Sleep(50 * time.Millisecond)
	}

	if !IsAlive(pid) {
		t.Fatal("the test process is not running")
	}
	if !MatchesSignature(pid, []string{"sh", "rcmanager-procs-test"}) {
		t.Fatalf("MatchesSignature() does not recognise %v", Cmdline(pid))
	}

	// A signature that does not describe this process must stop the kill
	// outright, not merely fail to match one token.
	if outcome := Terminate(pid, []string{"sh", "some-other-process"}, time.Second, false); outcome != Refused {
		t.Errorf("Terminate() = %q, want %q", outcome, Refused)
	}
	if !IsAlive(pid) {
		t.Error("Terminate() signalled a process it did not recognise")
	}

	// An empty signature is the same refusal: with nothing to compare, there
	// is no right to signal.
	if outcome := Terminate(pid, nil, time.Second, false); outcome != Refused {
		t.Errorf("Terminate() with no signature = %q, want %q", outcome, Refused)
	}
	if !IsAlive(pid) {
		t.Error("Terminate() signalled a process on an empty signature")
	}

	// With the right signature it goes down.
	if outcome := Terminate(pid, []string{"sh", "rcmanager-procs-test"}, 10*time.Second, true); outcome == Refused {
		t.Errorf("Terminate() refused its own process: %q", outcome)
	}
	select {
	case <-reaped:
	case <-time.After(5 * time.Second):
		t.Error("the process is still running")
	}
}

func TestIsAlive(t *testing.T) {
	if !IsAlive(os.Getpid()) {
		t.Error("IsAlive() says this process is not running")
	}
	for _, pid := range []int{0, -1, 1 << 30} {
		if IsAlive(pid) {
			t.Errorf("IsAlive(%d) = true", pid)
		}
	}
}

// Descendants are what a stop sweeps up after signalling a process group, so
// the tree has to be read correctly in the first place.
func TestDescendantsOf(t *testing.T) {
	cmd := exec.Command("sh", "-c", "sleep 30 & wait")
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	pid := cmd.Process.Pid
	t.Cleanup(func() {
		_ = cmd.Process.Kill()
		_, _ = cmd.Process.Wait()
	})

	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if len(DescendantsOf(pid)) > 0 {
			return
		}
		time.Sleep(100 * time.Millisecond)
	}
	t.Error("DescendantsOf() found no child of " + strconv.Itoa(pid))
}
