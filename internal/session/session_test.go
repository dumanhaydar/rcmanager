package session

import (
	"os"
	"regexp"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/dumanhaydar/rcmanager/internal/config"
	"github.com/dumanhaydar/rcmanager/internal/paths"
	"github.com/dumanhaydar/rcmanager/internal/procs"
	"github.com/dumanhaydar/rcmanager/internal/provider"
	"github.com/dumanhaydar/rcmanager/internal/registry"
	"github.com/dumanhaydar/rcmanager/internal/sio"
)

// marker appears in the managed child's argv, so the signature interlock has
// something real to match against.
const marker = "rcmanager-session-test"

// fake is a provider whose "agent" is a shell script, which is enough to
// exercise everything rcmanager does to a session: prompts, URL announcements,
// clean exits and crash loops.
type fake struct {
	script string
	url    *regexp.Regexp
	prompt bool
	// nameInSignature mimics a provider that puts the session name on the
	// command line, the way a Claude environment or Codex session does.
	nameInSignature bool
	// preflight, when set, is what the provider reports instead of "ok".
	preflight *provider.Result
}

func (fake) ID() string                            { return "fake" }
func (fake) DisplayName() string                   { return "Fake Agent" }
func (fake) Limitations() []string                 { return nil }
func (fake) EnvDeny() []*regexp.Regexp             { return nil }
func (fake) Detect() provider.Detection            { return provider.Detection{Available: true} }
func (fake) Discover(string) []provider.Discovered { return nil }
func (f fake) Preflight(*registry.Entry) provider.Result {
	if f.preflight != nil {
		return *f.preflight
	}
	return provider.Result{OK: true}
}

func (f fake) Capabilities() provider.Capabilities {
	return provider.Capabilities{PublishesRemoteURL: f.url != nil, Headless: true}
}

func (f fake) BuildCommand(e *registry.Entry) (provider.Command, error) {
	signature := []string{"sh", marker}
	if f.nameInSignature {
		signature = append(signature, e.Name)
	}
	return provider.Command{
		Command:   "sh",
		Args:      []string{"-c", "# " + marker + " " + e.Name + "\n" + f.script},
		Cwd:       e.Cwd,
		Signature: signature,
	}, nil
}

func (f fake) ExtractRemoteURL(text string) string {
	if f.url == nil {
		return ""
	}
	m := f.url.FindStringSubmatch(text)
	if m == nil {
		return ""
	}
	return m[1]
}

func (f fake) Interactions() []provider.Interaction {
	if !f.prompt {
		return nil
	}
	return []provider.Interaction{{
		ID:       "ready",
		Once:     true,
		Match:    regexp.MustCompile(`(?i)areyouready`),
		Respond:  func(*registry.Entry) string { return "yes\n" },
		Answered: "answered the readiness prompt",
		Delay:    50 * time.Millisecond,
	}}
}

func withHome(t *testing.T) {
	t.Helper()
	t.Setenv("RCMANAGER_HOME", t.TempDir())
	paths.Reload()
	t.Setenv("RCMANAGER_CLAIM_DIR", t.TempDir())
	t.Cleanup(func() {
		os.Unsetenv("RCMANAGER_HOME")
		paths.Reload()
	})
}

func testConfig() config.Config {
	cfg := config.Defaults()
	cfg.MinUptimeMs = 300
	cfg.RestartDelayMs = 100
	cfg.RestartWindowMs = 10_000
	cfg.StopTimeoutMs = 2_000
	cfg.MaxRestarts = 2
	cfg.IsolateSessions = "off"
	return cfg
}

func newSession(t *testing.T, p provider.Provider, cfg config.Config) *Managed {
	t.Helper()
	entry, err := registry.Normalize(&registry.Entry{
		Name: "demo", Provider: "fake", Cwd: t.TempDir(), Enabled: true,
	}, "")
	if err != nil {
		t.Fatal(err)
	}
	s := New(entry, p, Options{Config: cfg})
	t.Cleanup(func() {
		s.Stop("test cleanup")
		s.Dispose()
	})
	return s
}

func waitFor(t *testing.T, what string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(15 * time.Second)
	for time.Now().Before(deadline) {
		if cond() {
			return
		}
		time.Sleep(50 * time.Millisecond)
	}
	t.Fatalf("timed out waiting for %s", what)
}

// The happy path: the session comes up, its prompt is answered through the
// fifo, and the URL it announces is recorded.
func TestStartAnswersPromptsAndCapturesTheURL(t *testing.T) {
	withHome(t)
	p := fake{
		script: `echo "Are you ready?"; read answer; echo "answer=$answer"; echo "URL=https://example.test/session_1"; while true; do sleep 1; done`,
		url:    regexp.MustCompile(`URL=(\S+)`),
		prompt: true,
	}
	s := newSession(t, p, testConfig())

	r := s.Start(false)
	if !r.OK {
		t.Fatalf("Start() = %+v", r)
	}
	waitFor(t, "the session to come online", func() bool { return s.Status() == Online })
	waitFor(t, "the URL to be captured", func() bool { return s.View().RemoteURL != "" })

	if got, want := s.View().RemoteURL, "https://example.test/session_1"; got != want {
		t.Errorf("RemoteURL = %q, want %q", got, want)
	}

	log, err := os.ReadFile(paths.LogPaths("demo").Out)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(log), "answer=yes") {
		t.Errorf("the prompt was never answered on the fifo; log:\n%s", log)
	}

	// Starting again is a no-op rather than a second process.
	if again := s.Start(false); !again.AlreadyRunning {
		t.Errorf("Start() on a running session = %+v", again)
	}

	pid := s.Pid()
	if stop := s.Stop("test"); !stop.OK {
		t.Fatalf("Stop() = %+v", stop)
	}
	if s.Status() != Stopped {
		t.Errorf("status after Stop() = %q", s.Status())
	}
	waitFor(t, "the child to go away", func() bool { return !procs.IsAlive(pid) })
}

// Only short-lived exits count against the budget, and once it is spent rcmanager
// stops burning restarts and says why.
func TestCrashLoopBudgetGivesUp(t *testing.T) {
	withHome(t)
	s := newSession(t, fake{script: `exit 3`}, testConfig())

	s.Start(false)
	waitFor(t, "the crash loop to be given up on", func() bool { return s.Status() == Errored })

	view := s.View()
	if !strings.Contains(view.Message, "crash loop") {
		t.Errorf("message = %q, want it to name the crash loop", view.Message)
	}
	if view.LastExit == nil || view.LastExit.Code != 3 {
		t.Errorf("last exit = %+v, want code 3", view.LastExit)
	}
	if view.Restarts == 0 {
		t.Error("no restarts were attempted before giving up")
	}
}

func TestAutorestartOffLeavesItStopped(t *testing.T) {
	withHome(t)
	cfg := testConfig()
	cfg.Autorestart = false
	s := newSession(t, fake{script: `exit 0`}, cfg)

	s.Start(false)
	waitFor(t, "the session to settle", func() bool { return s.Status() == Stopped })
	if msg := s.View().Message; !strings.Contains(msg, "autorestart disabled") {
		t.Errorf("message = %q", msg)
	}
}

// A new daemon adopts a process its predecessor started: same pid, no second
// process, capture resumed from the recorded offset.
func TestAdoptionTakesOverARunningChild(t *testing.T) {
	withHome(t)
	p := fake{
		script: `echo "URL=https://example.test/session_2"; while true; do sleep 1; done`,
		url:    regexp.MustCompile(`URL=(\S+)`),
	}
	first := newSession(t, p, testConfig())
	first.Start(false)
	waitFor(t, "the session to come online", func() bool { return first.Status() == Online })
	waitFor(t, "the URL to be captured", func() bool { return first.View().RemoteURL != "" })

	pid := first.Pid()
	saved := first.Persisted()
	// Detach is what a daemon shutdown does: the child keeps running.
	first.Detach()
	if !procs.IsAlive(pid) {
		t.Fatal("Detach() killed the session it was supposed to leave running")
	}

	second := newSession(t, p, testConfig())
	second.SetSavedState(&saved)
	r := second.Start(false)
	if !r.Adopted {
		t.Fatalf("Start() = %+v, want an adoption", r)
	}
	view := second.View()
	if view.Pid != pid {
		t.Errorf("adopted pid = %d, want %d", view.Pid, pid)
	}
	if view.RemoteURL != "https://example.test/session_2" {
		t.Errorf("the URL did not survive the handover: %q", view.RemoteURL)
	}
	if !view.Adopted || view.Status != Online {
		t.Errorf("unexpected view after adoption: %+v", view)
	}
}

// A pid that no longer looks like what rcmanager started is never adopted, which
// is what stops a recycled pid being mistaken for a session.
func TestAdoptionRefusesAMismatchedSignature(t *testing.T) {
	withHome(t)
	s := newSession(t, fake{script: `while true; do sleep 1; done`}, testConfig())
	saved := Persisted{Name: "demo", Pid: os.Getpid(), Signature: []string{"definitely-not-this-process"}}
	s.SetSavedState(&saved)

	if r := s.Start(false); r.Adopted {
		t.Error("a process with a different command line was adopted")
	}
	s.Stop("test")
}

// Supervision limits apply to a live session in place; a change to how it
// launches is flagged and waits for an explicit restart.
func TestApplyEntryFlagsLaunchChangesOnly(t *testing.T) {
	withHome(t)
	s := newSession(t, fake{script: `while true; do sleep 1; done`}, testConfig())
	s.Start(false)
	waitFor(t, "the session to come online", func() bool { return s.Status() == Online })

	original := s.Entry().Clone()

	noted := s.Entry().Clone()
	noted.Note = "just a note"
	if restart, _ := s.ApplyEntry(noted); restart {
		t.Error("a note change asked for a restart")
	}

	moved := s.Entry().Clone()
	moved.Args = []string{"--new-flag"}
	restart, changed := s.ApplyEntry(moved)
	if !restart || len(changed) != 1 || changed[0] != "args" {
		t.Fatalf("ApplyEntry() = %v, %v", restart, changed)
	}
	if view := s.View(); len(view.NeedsRestart) != 1 || !strings.Contains(view.Message, "rcmanager restart") {
		t.Errorf("the pending change was not reported: %+v", view)
	}

	// Undoing the edit must clear the flag rather than re-raise it.
	if restart, _ := s.ApplyEntry(original); restart {
		t.Error("reverting the edit still asked for a restart")
	}
	if view := s.View(); len(view.NeedsRestart) != 0 {
		t.Errorf("needsRestart survived the revert: %v", view.NeedsRestart)
	}
}

// A provider singleton is enforced wherever a session can start, not just in
// the one command that used to check it.
func TestVetoBlocksStart(t *testing.T) {
	withHome(t)
	entry, err := registry.Normalize(&registry.Entry{
		Name: "demo", Provider: "fake", Cwd: t.TempDir(), Enabled: true,
	}, "")
	if err != nil {
		t.Fatal(err)
	}
	s := New(entry, fake{script: `while true; do sleep 1; done`}, Options{
		Config:   testConfig(),
		CanStart: func(*Managed) *Veto { return &Veto{Error: "only one of these may run", Hint: "stop the other"} },
	})
	defer s.Dispose()

	r := s.Start(false)
	if r.OK || r.Error != "only one of these may run" {
		t.Fatalf("Start() = %+v", r)
	}
	if s.Pid() != 0 {
		t.Error("a vetoed session still spawned a process")
	}
}

// Output already sitting in the raw capture file is the case that deadlocked
// the supervisor: Tailer.Start polls once synchronously, under the session
// lock, so a sink that took that lock hung the very session it was starting.
// It only showed up on Linux, where the child had written something before the
// first poll ran; macOS kept finding an empty file and getting away with it.
func TestStartCaptureWithOutputAlreadyPresentDoesNotDeadlock(t *testing.T) {
	withHome(t)
	if err := paths.EnsureHome(); err != nil {
		t.Fatal(err)
	}
	s := newSession(t, fake{script: `exit 0`}, testConfig())
	if err := os.WriteFile(s.io.Files.Out, []byte("output from before capture started\n"), 0o600); err != nil {
		t.Fatal(err)
	}

	done := make(chan struct{})
	go func() {
		defer close(done)
		s.mu.Lock()
		s.startCaptureLocked(sio.Offsets{})
		s.mu.Unlock()
	}()

	select {
	case <-done:
	case <-time.After(10 * time.Second):
		t.Fatal("startCapture deadlocked on output that was already in the raw file")
	}
	s.stopCapture()

	// The pre-existing bytes still reach the log: the fix must not have simply
	// stopped reading them.
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if log, err := os.ReadFile(paths.LogPaths("demo").Out); err == nil &&
			strings.Contains(string(log), "output from before capture started") {
			return
		}
		time.Sleep(50 * time.Millisecond)
	}
	t.Error("the output that was already in the raw file never reached the log")
}

// Renaming a running session must move everything named after it without the
// agent noticing: same process, same conversation, and capture that carries on
// into the renamed log.
func TestRenameKeepsTheSessionRunningAndCapturing(t *testing.T) {
	withHome(t)
	p := fake{
		script: `echo "before the rename"; sleep 0.4; echo "MARKER after the rename"; while true; do sleep 1; done`,
	}
	s := newSession(t, p, testConfig())
	s.Start(false)
	waitFor(t, "the session to come online", func() bool { return s.Status() == Online })

	pid := s.Pid()
	oldLog := paths.LogPaths("demo").Out
	oldFifo := s.io.Files.Stdin

	entry := s.Entry().Clone()
	entry.Name = "renamed"
	normalized, err := registry.Normalize(entry, "")
	if err != nil {
		t.Fatal(err)
	}
	if err := s.Rename(normalized); err != nil {
		t.Fatal(err)
	}

	if s.Name() != "renamed" {
		t.Errorf("Name() = %q after rename", s.Name())
	}
	if s.Pid() != pid {
		t.Errorf("the agent was restarted: pid %d → %d", pid, s.Pid())
	}
	if !procs.IsAlive(pid) {
		t.Fatal("the agent was killed by the rename")
	}
	if s.View().SessionID != normalized.SessionID {
		t.Error("the session id changed, so this would be a different conversation")
	}

	// The files moved rather than being left behind under the old name.
	if _, err := os.Stat(oldLog); !os.IsNotExist(err) {
		t.Errorf("the old log file is still there: %v", err)
	}
	if _, err := os.Stat(oldFifo); !os.IsNotExist(err) {
		t.Errorf("the old stdin fifo is still there: %v", err)
	}
	newLog := paths.LogPaths("renamed").Out
	if _, err := os.Stat(newLog); err != nil {
		t.Fatalf("no log under the new name: %v", err)
	}

	// Output written after the rename still reaches the renamed log, which is
	// what proves capture followed the files rather than tailing a dead path.
	deadline := time.Now().Add(15 * time.Second)
	for time.Now().Before(deadline) {
		if raw, err := os.ReadFile(newLog); err == nil && strings.Contains(string(raw), "MARKER after the rename") {
			if !strings.Contains(string(raw), "before the rename") {
				t.Error("the history from before the rename was lost")
			}
			return
		}
		time.Sleep(100 * time.Millisecond)
	}
	t.Fatal("output written after the rename never reached the renamed log")
}

// A provider that carries the session name in its command line is launching
// something the new name no longer describes, so the session says so instead
// of pretending the rename took full effect.
func TestRenameFlagsAProviderThatNamesItsProcess(t *testing.T) {
	withHome(t)
	s := newSession(t, fake{script: `while true; do sleep 1; done`, nameInSignature: true}, testConfig())
	s.Start(false)
	waitFor(t, "the session to come online", func() bool { return s.Status() == Online })

	entry := s.Entry().Clone()
	entry.Name = "renamed"
	normalized, _ := registry.Normalize(entry, "")
	if err := s.Rename(normalized); err != nil {
		t.Fatal(err)
	}

	view := s.View()
	if len(view.NeedsRestart) != 1 || view.NeedsRestart[0] != "name" {
		t.Errorf("NeedsRestart = %v, want [name]", view.NeedsRestart)
	}
	if !strings.Contains(view.Message, "restart to apply") {
		t.Errorf("message = %q", view.Message)
	}

	// The signature still describes the live process, or rcmanager would have
	// lost the right to signal its own child.
	if r := s.Stop("test"); !r.OK {
		t.Errorf("Stop() after a rename = %+v — the interlock stopped matching", r)
	}
}

// Renaming a stopped session is just bookkeeping, and must still move its logs.
func TestRenameAStoppedSession(t *testing.T) {
	withHome(t)
	s := newSession(t, fake{script: `exit 0`}, testConfig())
	s.log("a line from before the rename")

	entry := s.Entry().Clone()
	entry.Name = "renamed"
	normalized, _ := registry.Normalize(entry, "")
	if err := s.Rename(normalized); err != nil {
		t.Fatal(err)
	}

	raw, err := os.ReadFile(paths.LogPaths("renamed").Out)
	if err != nil {
		t.Fatalf("no log under the new name: %v", err)
	}
	if !strings.Contains(string(raw), "a line from before the rename") {
		t.Error("the log history did not follow the rename")
	}
	if s.View().Status != Stopped {
		t.Errorf("status = %q, want it left alone", s.View().Status)
	}
}

// The collision this exists for: two daemons, each with the session in its own
// registry, both entitled to supervise it as far as either can tell. The
// second one is refused, and told who holds it.
func TestASecondSupervisorIsRefused(t *testing.T) {
	withHome(t)
	script := `while true; do sleep 1; done`

	entry, err := registry.Normalize(&registry.Entry{
		Name: "demo", Provider: "fake", SessionID: "shared-conversation",
		Cwd: t.TempDir(), Enabled: true,
	}, "")
	if err != nil {
		t.Fatal(err)
	}

	first := New(entry, fake{script: script}, Options{Config: testConfig()})
	defer first.Dispose()
	if r := first.Start(false); !r.OK {
		t.Fatalf("the first supervisor could not start it: %+v", r)
	}
	waitFor(t, "the session to come online", func() bool { return first.Status() == Online })

	// A second daemon's view of the same conversation, registered under
	// another name entirely — which is exactly how the collision happens.
	otherEntry := entry.Clone()
	otherEntry.Name = "same-conversation-other-name"
	second := New(otherEntry, fake{script: script}, Options{Config: testConfig()})
	defer second.Dispose()

	r := second.Start(false)
	if r.OK {
		t.Fatal("two supervisors were allowed on one conversation")
	}
	if !strings.Contains(r.Error, "already supervised by") {
		t.Errorf("error = %q, want it to name the other supervisor", r.Error)
	}
	if !strings.Contains(r.Error, strconv.Itoa(os.Getpid())) {
		t.Errorf("error = %q, want the holder's pid", r.Error)
	}
	if second.Pid() != 0 {
		t.Error("the refused session spawned a process anyway")
	}
	if first.Status() != Online || !procs.IsAlive(first.Pid()) {
		t.Error("the refusal disturbed the session that rightfully held it")
	}

	// Once the holder lets go, the conversation is available again.
	first.Stop("test")
	if r := second.Start(false); !r.OK {
		t.Fatalf("the conversation was not released: %+v", r)
	}
	second.Stop("test")
}

// Handing over is not giving up: a daemon that detaches must release the
// conversation so its successor can adopt it.
func TestDetachReleasesTheConversation(t *testing.T) {
	withHome(t)
	script := `while true; do sleep 1; done`
	entry, _ := registry.Normalize(&registry.Entry{
		Name: "demo", Provider: "fake", SessionID: "handover", Cwd: t.TempDir(), Enabled: true,
	}, "")

	first := New(entry, fake{script: script}, Options{Config: testConfig()})
	defer first.Dispose()
	first.Start(false)
	waitFor(t, "the session to come online", func() bool { return first.Status() == Online })

	pid := first.Pid()
	saved := first.Persisted()
	first.Detach()

	successor := New(entry, fake{script: script}, Options{Config: testConfig()})
	defer func() {
		successor.Stop("test")
		successor.Dispose()
	}()
	successor.SetSavedState(&saved)

	r := successor.Start(false)
	if !r.OK || !r.Adopted {
		t.Fatalf("the successor could not adopt what was handed to it: %+v", r)
	}
	if successor.Pid() != pid {
		t.Errorf("adopted pid = %d, want %d", successor.Pid(), pid)
	}
}

// A session in its restart backoff is still this daemon's to supervise;
// nothing else may step in while it waits.
func TestTheClaimIsHeldAcrossARestartBackoff(t *testing.T) {
	withHome(t)
	cfg := testConfig()
	cfg.RestartDelayMs = 3000
	entry, _ := registry.Normalize(&registry.Entry{
		Name: "demo", Provider: "fake", SessionID: "backoff", Cwd: t.TempDir(), Enabled: true,
	}, "")

	first := New(entry, fake{script: `exit 1`}, Options{Config: cfg})
	defer func() {
		first.Stop("test")
		first.Dispose()
	}()
	first.Start(false)
	waitFor(t, "the session to be waiting out its backoff", func() bool { return first.Status() == Restarting })

	other := New(entry.Clone(), fake{script: `exit 1`}, Options{Config: cfg})
	defer other.Dispose()
	if r := other.Start(false); r.OK {
		t.Error("another supervisor slipped in while the session was restarting")
	}
}

// A daemon that tried to start a session and could not is not supervising
// anything, so it must not keep the conversation locked away from the daemon
// that could.
func TestAFailedStartReleasesTheConversation(t *testing.T) {
	withHome(t)
	entry, _ := registry.Normalize(&registry.Entry{
		Name: "demo", Provider: "fake", SessionID: "contested", Cwd: t.TempDir(), Enabled: true,
	}, "")

	refused := New(entry, fake{script: `exit 0`, preflight: &provider.Result{
		Error: "the provider says no", Hint: "fix it",
	}}, Options{Config: testConfig()})
	defer refused.Dispose()

	if r := refused.Start(false); r.OK {
		t.Fatal("a failed preflight still started the session")
	}

	// The conversation is free for a daemon that can actually run it.
	willing := New(entry.Clone(), fake{script: `while true; do sleep 1; done`}, Options{Config: testConfig()})
	defer func() {
		willing.Stop("test")
		willing.Dispose()
	}()
	if r := willing.Start(false); !r.OK {
		t.Fatalf("the conversation was still locked by the failed start: %+v", r)
	}
}

// An adopted session announced its URL before this daemon existed, but the TUI
// reprints the banner on every repaint. Measuring the announcement window from
// the session's original start meant an adopted session whose URL was never
// recorded could never acquire one, and the watchdog complained about it for
// as long as it ran.
func TestAnAdoptedSessionCanStillLearnItsURL(t *testing.T) {
	withHome(t)
	p := fake{
		script: `while true; do echo "remote control is active at URL=https://example.test/session_adopted"; sleep 1; done`,
		url:    regexp.MustCompile(`URL=(\S+)`),
	}
	first := New(mustEntry(t, "demo", "learns-late", t.TempDir()), p, Options{Config: testConfig()})
	defer first.Dispose()
	first.Start(false)
	waitFor(t, "the session to come online", func() bool { return first.Status() == Online })

	saved := first.Persisted()
	// A session whose URL was never captured, and which started long enough
	// ago that a window measured from then would be closed.
	saved.RemoteURL = ""
	saved.StartedAt = time.Now().Add(-2 * urlWindow).UnixMilli()
	first.Detach()

	second := New(mustEntry(t, "demo", "learns-late", first.Entry().Cwd), p, Options{Config: testConfig()})
	defer func() {
		second.Stop("test")
		second.Dispose()
	}()
	second.SetSavedState(&saved)
	if r := second.Start(false); !r.Adopted {
		t.Fatalf("Start() = %+v, want an adoption", r)
	}

	waitFor(t, "the adopted session to learn its URL", func() bool { return second.View().RemoteURL != "" })
	if got, want := second.View().RemoteURL, "https://example.test/session_adopted"; got != want {
		t.Errorf("RemoteURL = %q, want %q", got, want)
	}
}

func mustEntry(t *testing.T, name, sessionID, cwd string) *registry.Entry {
	t.Helper()
	e, err := registry.Normalize(&registry.Entry{
		Name: name, Provider: "fake", SessionID: sessionID, Cwd: cwd, Enabled: true,
	}, "")
	if err != nil {
		t.Fatal(err)
	}
	return e
}
