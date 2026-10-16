package cli

import (
	"os"
	"reflect"
	"testing"

	"github.com/dumanhaydar/rcmanager/internal/paths"
	_ "github.com/dumanhaydar/rcmanager/internal/provider/claude"
	"github.com/dumanhaydar/rcmanager/internal/registry"
)

func TestParse(t *testing.T) {
	cases := []struct {
		argv  []string
		args  []string
		flags Flags
	}{
		{[]string{"start", "demo", "/tmp/x"}, []string{"start", "demo", "/tmp/x"}, Flags{}},
		{[]string{"start", "demo", "--provider", "claude"}, []string{"start", "demo"}, Flags{"provider": "claude"}},
		{[]string{"start", "demo", "--provider=claude"}, []string{"start", "demo"}, Flags{"provider": "claude"}},
		// A boolean flag must not swallow the argument after it.
		{[]string{"list", "--json", "x"}, []string{"list", "x"}, Flags{"json": "true"}},
		{[]string{"logs", "demo", "-f", "-n", "5"}, []string{"logs", "demo"}, Flags{"f": "true", "n": "5"}},
		{[]string{"daemon", "--stop", "--with-sessions"}, []string{"daemon"}, Flags{"stop": "true", "with-sessions": "true"}},
	}
	for _, c := range cases {
		args, flags := Parse(c.argv)
		if !reflect.DeepEqual(args, c.args) {
			t.Errorf("Parse(%v) args = %v, want %v", c.argv, args, c.args)
		}
		if !reflect.DeepEqual(flags, c.flags) {
			t.Errorf("Parse(%v) flags = %v, want %v", c.argv, flags, c.flags)
		}
	}
}

func TestFlagsHelpers(t *testing.T) {
	flags := Flags{"json": "true", "provider": "codex", "manager": "true"}
	if !flags.Bool("json") || flags.Bool("force") {
		t.Error("Bool() is wrong")
	}
	if got := flags.Str("provider", "claude"); got != "codex" {
		t.Errorf("Str() = %q", got)
	}
	// A flag given without a value is not a value.
	if got := flags.Str("manager", ""); got != "" {
		t.Errorf("Str() returned the placeholder %q", got)
	}
	if !flags.Autostart() {
		t.Error("Autostart() should default to on")
	}
	if (Flags{"no-autostart": "true"}).Autostart() {
		t.Error("--no-autostart was ignored")
	}
}

func TestUnknownCommandExitsNonZero(t *testing.T) {
	if code := Main([]string{"nonsense-command"}); code != 1 {
		t.Errorf("Main() = %d, want 1", code)
	}
	if code := Main([]string{"--version"}); code != 0 {
		t.Errorf("Main(--version) = %d, want 0", code)
	}
	if code := Main([]string{"help"}); code != 0 {
		t.Errorf("Main(help) = %d, want 0", code)
	}
	// No command at all is a usage error, which is what a shell script checks.
	if code := Main(nil); code != 1 {
		t.Errorf("Main(nil) = %d, want 1", code)
	}
}

func TestStartWithAPathKeepsTheRegisteredSessionID(t *testing.T) {
	t.Setenv("RCMANAGER_HOME", t.TempDir())
	paths.Reload()
	t.Cleanup(func() {
		os.Unsetenv("RCMANAGER_HOME")
		paths.Reload()
	})
	dir := t.TempDir()

	first, err := resolveTarget("demo", dir, "claude", true)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := registry.Upsert(first.entry); err != nil {
		t.Fatal(err)
	}

	// The same `start <name> <path>` again is the same session: a new id
	// here would point the registry at a conversation that does not exist.
	again, err := resolveTarget("demo", dir, "claude", true)
	if err != nil {
		t.Fatal(err)
	}
	if again.entry.SessionID != first.entry.SessionID {
		t.Errorf("session id changed from %s to %s", first.entry.SessionID, again.entry.SessionID)
	}
	if again.origin != "registry" {
		t.Errorf("origin = %q, want registry", again.origin)
	}

	// A different directory is a different session.
	moved, err := resolveTarget("demo", t.TempDir(), "claude", true)
	if err != nil {
		t.Fatal(err)
	}
	if moved.entry.SessionID == first.entry.SessionID {
		t.Error("a new directory kept the old session id")
	}
}
