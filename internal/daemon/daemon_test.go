package daemon

import (
	"os"
	"path/filepath"
	"regexp"
	"testing"
	"time"

	"github.com/dumanhaydar/rcmanager/internal/ipc"
	"github.com/dumanhaydar/rcmanager/internal/paths"
	"github.com/dumanhaydar/rcmanager/internal/procs"
	"github.com/dumanhaydar/rcmanager/internal/provider"
	"github.com/dumanhaydar/rcmanager/internal/registry"
	"github.com/dumanhaydar/rcmanager/internal/session"
)

// fake is a provider whose "agent" is a shell loop, so the daemon can be
// driven end to end without either provider CLI installed.
type fake struct{ singleton bool }

func (f fake) ID() string {
	if f.singleton {
		return "fake-singleton"
	}
	return "fake"
}
func (fake) DisplayName() string                       { return "Fake Agent" }
func (fake) Limitations() []string                     { return nil }
func (fake) EnvDeny() []*regexp.Regexp                 { return nil }
func (fake) Detect() provider.Detection                { return provider.Detection{Available: true} }
func (fake) Discover(string) []provider.Discovered     { return nil }
func (fake) Preflight(*registry.Entry) provider.Result { return provider.Result{OK: true} }
func (fake) ExtractRemoteURL(string) string            { return "" }
func (fake) Interactions() []provider.Interaction      { return nil }

func (f fake) Capabilities() provider.Capabilities {
	return provider.Capabilities{Headless: true, Singleton: f.singleton}
}

func (fake) BuildCommand(e *registry.Entry) (provider.Command, error) {
	return provider.Command{
		Command:   "sh",
		Args:      []string{"-c", "# rcmanager-daemon-test " + e.Name + "\nwhile true; do sleep 1; done"},
		Cwd:       e.Cwd,
		Signature: []string{"sh", "rcmanager-daemon-test " + e.Name},
	}, nil
}

func init() {
	provider.Register(fake{})
	provider.Register(fake{singleton: true})
}

func start(t *testing.T) (*Daemon, string) {
	t.Helper()
	home := t.TempDir()
	t.Setenv("RCMANAGER_HOME", home)
	// A unix socket path is limited to about a hundred characters, and a test
	// temporary directory is nowhere near that short — which is exactly what
	// RCMANAGER_SOCK exists for.
	sockDir, err := os.MkdirTemp("", "rcs")
	if err != nil {
		t.Fatal(err)
	}
	t.Setenv("RCMANAGER_SOCK", filepath.Join(sockDir, "d.sock"))
	t.Cleanup(func() { os.RemoveAll(sockDir) })
	paths.Reload()

	d := New()
	// Keep the window short so a test does not wait on real supervision
	// timings.
	d.cfg.MinUptimeMs = 200
	d.cfg.RestartDelayMs = 100
	d.cfg.StopTimeoutMs = 2000
	d.cfg.IsolateSessions = "off"

	if err = d.Start(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		d.Shutdown("test", true, false)
		os.Unsetenv("RCMANAGER_HOME")
		os.Unsetenv("RCMANAGER_SOCK")
		paths.Reload()
	})
	return d, paths.Get().Sock
}

func call(t *testing.T, socket, cmd string, args ipc.Args, out any) error {
	t.Helper()
	return ipc.Call(socket, cmd, args, out, ipc.CallOptions{Timeout: 30 * time.Second})
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

func TestRegisterStartAndStopOverIPC(t *testing.T) {
	d, socket := start(t)
	work := t.TempDir()

	var pong Pong
	if err := call(t, socket, "ping", ipc.Args{}, &pong); err != nil {
		t.Fatal(err)
	}
	if !pong.Ready || pong.Pid != os.Getpid() {
		t.Errorf("ping = %+v", pong)
	}

	entry := &registry.Entry{Name: "demo", Provider: "fake", Cwd: work, Enabled: true}
	if err := call(t, socket, "register", ipc.Args{Entry: entry}, nil); err != nil {
		t.Fatal(err)
	}
	// Registering must persist, so a later daemon knows the session exists.
	if registry.Get("demo") == nil {
		t.Fatal("register did not write the registry")
	}

	var view session.View
	if err := call(t, socket, "start", ipc.Args{Name: "demo"}, &view); err != nil {
		t.Fatal(err)
	}
	if view.Pid == 0 {
		t.Fatalf("start returned no pid: %+v", view)
	}
	waitFor(t, "the session to come online", func() bool {
		s, _ := d.get("demo")
		return s.Status() == session.Online
	})

	var listed []session.View
	if err := call(t, socket, "list", ipc.Args{}, &listed); err != nil {
		t.Fatal(err)
	}
	if len(listed) != 1 || listed[0].Name != "demo" {
		t.Fatalf("list = %+v", listed)
	}

	// A disabled session is stopped and stays stopped.
	if err := call(t, socket, "disable", ipc.Args{Name: "demo"}, &view); err != nil {
		t.Fatal(err)
	}
	if view.Status != session.Disabled || view.Pid != 0 {
		t.Errorf("after disable: %+v", view)
	}
	if registry.Get("demo").Enabled {
		t.Error("disable was not persisted")
	}

	if err := call(t, socket, "remove", ipc.Args{Name: "demo"}, nil); err != nil {
		t.Fatal(err)
	}
	if registry.Get("demo") != nil {
		t.Error("remove left the session registered")
	}
	if err := call(t, socket, "describe", ipc.Args{Name: "demo"}, &view); err == nil {
		t.Error("describe still answers for a removed session")
	}
}

// Saving a file is not permission to restart an agent mid-thought: an edit
// that changes how a session launches is flagged and waits.
func TestReloadReconcilesWithoutInterrupting(t *testing.T) {
	d, socket := start(t)
	work := t.TempDir()

	live := &registry.Entry{Name: "live", Provider: "fake", Cwd: work, Enabled: true}
	if err := call(t, socket, "register", ipc.Args{Entry: live}, nil); err != nil {
		t.Fatal(err)
	}
	if err := call(t, socket, "start", ipc.Args{Name: "live"}, nil); err != nil {
		t.Fatal(err)
	}
	waitFor(t, "the session to come online", func() bool {
		s, _ := d.get("live")
		return s.Status() == session.Online
	})
	s, _ := d.get("live")
	pidBefore := s.Pid()

	// Hand-edit: one launch-affecting change, one new session.
	edited := live.Clone()
	edited.Args = []string{"--changed"}
	added := &registry.Entry{Name: "added", Provider: "fake", Cwd: work, Enabled: false}
	if err := registry.Save([]*registry.Entry{edited, added}); err != nil {
		t.Fatal(err)
	}

	var views []session.View
	if err := call(t, socket, "reload", ipc.Args{}, &views); err != nil {
		t.Fatal(err)
	}
	if len(views) != 2 {
		t.Fatalf("reload reported %d sessions", len(views))
	}
	byName := map[string]session.View{}
	for _, v := range views {
		byName[v.Name] = v
	}
	if got := byName["live"]; len(got.NeedsRestart) != 1 || got.NeedsRestart[0] != "args" {
		t.Errorf("the launch change was not flagged: %+v", got)
	}
	if s.Pid() != pidBefore {
		t.Error("reload restarted a healthy session")
	}
	if got := byName["added"]; got.Status != session.Disabled || got.Pid != 0 {
		t.Errorf("a disabled new session was started: %+v", got)
	}

	// Dropping a session from the registry is an explicit deregistration.
	if err := registry.Save([]*registry.Entry{added}); err != nil {
		t.Fatal(err)
	}
	if err := call(t, socket, "reload", ipc.Args{}, &views); err != nil {
		t.Fatal(err)
	}
	if len(views) != 1 || views[0].Name != "added" {
		t.Errorf("reload = %+v, want only the remaining session", views)
	}
	waitFor(t, "the removed session to stop", func() bool { return !processAlive(pidBefore) })
}

// A provider that exposes one machine-wide daemon may only have one managed
// session; the second is refused rather than left to fight the first.
func TestSingletonProviderRefusesASecondSession(t *testing.T) {
	d, socket := start(t)
	work := t.TempDir()

	for _, name := range []string{"first", "second"} {
		entry := &registry.Entry{Name: name, Provider: "fake-singleton", Cwd: work, Enabled: true}
		if err := call(t, socket, "register", ipc.Args{Entry: entry}, nil); err != nil {
			t.Fatal(err)
		}
	}
	if err := call(t, socket, "start", ipc.Args{Name: "first"}, nil); err != nil {
		t.Fatal(err)
	}
	waitFor(t, "the first session to come online", func() bool {
		s, _ := d.get("first")
		return s.Status() == session.Online
	})

	err := call(t, socket, "start", ipc.Args{Name: "second"}, nil)
	if err == nil {
		t.Fatal("a second singleton session was allowed to start")
	}
	if s, _ := d.get("second"); s.Pid() != 0 {
		t.Error("the refused session still spawned a process")
	}
}

// Session ids are minted at registration, so every later start resumes the
// same conversation instead of beginning a new one.
func TestSessionIDIsAssignedForResumeCapableProviders(t *testing.T) {
	_, socket := start(t)
	work := t.TempDir()

	entry := &registry.Entry{Name: "demo", Provider: "fake", Cwd: work, Enabled: true}
	if err := call(t, socket, "register", ipc.Args{Entry: entry}, nil); err != nil {
		t.Fatal(err)
	}
	// The fake provider does not resume by id, so none should be invented.
	if got := registry.Get("demo"); got.SessionID != "" {
		t.Errorf("a session id was assigned to a provider that cannot use one: %q", got.SessionID)
	}

	if id := NewUUID(); len(id) != 36 || id[14] != '4' {
		t.Errorf("NewUUID() = %q, want a version-4 UUID", id)
	}
}

func processAlive(pid int) bool { return procs.IsAlive(pid) }

// Renaming over IPC moves the registry entry and the daemon's own index of
// sessions, so the old name stops answering and the new one takes over.
func TestRenameOverIPC(t *testing.T) {
	d, socket := start(t)
	work := t.TempDir()

	entry := &registry.Entry{Name: "before", Provider: "fake", Cwd: work, Enabled: true}
	if err := call(t, socket, "register", ipc.Args{Entry: entry}, nil); err != nil {
		t.Fatal(err)
	}
	if err := call(t, socket, "start", ipc.Args{Name: "before"}, nil); err != nil {
		t.Fatal(err)
	}
	waitFor(t, "the session to come online", func() bool {
		s, _ := d.get("before")
		return s.Status() == session.Online
	})
	s, _ := d.get("before")
	pid := s.Pid()

	var view session.View
	if err := call(t, socket, "rename", ipc.Args{Name: "before", NewName: "after"}, &view); err != nil {
		t.Fatal(err)
	}
	if view.Name != "after" || view.Pid != pid {
		t.Fatalf("rename returned %+v, want name after and pid %d", view, pid)
	}

	if registry.Get("before") != nil {
		t.Error("the old entry is still in the registry")
	}
	if got := registry.Get("after"); got == nil {
		t.Error("the new entry is not in the registry")
	} else if got.SessionID != entry.SessionID {
		t.Error("the session id changed, so this is no longer the same conversation")
	}

	if _, err := d.get("before"); err == nil {
		t.Error("the daemon still answers for the old name")
	}
	var listed []session.View
	if err := call(t, socket, "list", ipc.Args{}, &listed); err != nil {
		t.Fatal(err)
	}
	if len(listed) != 1 || listed[0].Name != "after" {
		t.Errorf("list = %+v", listed)
	}

	// Renaming onto a name that is taken would quietly destroy one of the two.
	second := &registry.Entry{Name: "other", Provider: "fake", Cwd: work, Enabled: false}
	if err := call(t, socket, "register", ipc.Args{Entry: second}, nil); err != nil {
		t.Fatal(err)
	}
	if err := call(t, socket, "rename", ipc.Args{Name: "after", NewName: "other"}, nil); err == nil {
		t.Error("renaming onto an existing session was allowed")
	}
	if err := call(t, socket, "rename", ipc.Args{Name: "after", NewName: "not a valid name"}, nil); err == nil {
		t.Error("an unusable name was accepted")
	}
	if err := call(t, socket, "rename", ipc.Args{Name: "nope", NewName: "x"}, nil); err == nil {
		t.Error("renaming an unregistered session was allowed")
	}

	// After all that, the session is still itself.
	if s, err := d.get("after"); err != nil || s.Pid() != pid {
		t.Errorf("the session did not survive the refused renames: %v", err)
	}
}
