package cli

import (
	"reflect"
	"testing"
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
