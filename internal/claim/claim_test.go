package claim

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func withDir(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	t.Setenv("RCMANAGER_CLAIM_DIR", dir)
	return dir
}

func TestSecondAcquireReportsTheHolder(t *testing.T) {
	withDir(t)

	first, holder, err := Acquire("claude/abc", Holder{Pid: 101, Home: "/home/a/.rcmanager", Name: "api"})
	if err != nil || first == nil {
		t.Fatalf("Acquire() = %v, %+v, %v", first, holder, err)
	}

	second, holder, err := Acquire("claude/abc", Holder{Pid: 202, Home: "/home/a/.other"})
	if err != nil {
		t.Fatal(err)
	}
	if second != nil {
		t.Fatal("two daemons both claimed the same conversation")
	}
	if holder.Pid != 101 || !strings.Contains(holder.Describe(), "/home/a/.rcmanager") {
		t.Errorf("holder = %+v, want the first daemon with its home", holder)
	}

	// Letting go hands it over.
	first.Release()
	third, holder, err := Acquire("claude/abc", Holder{Pid: 202})
	if err != nil || third == nil {
		t.Fatalf("the conversation was not released: %v %+v", err, holder)
	}
	third.Release()
}

// A different conversation is a different lock: the guard must not serialise
// unrelated sessions.
func TestDifferentConversationsDoNotContend(t *testing.T) {
	withDir(t)

	a, _, err := Acquire("claude/one", Holder{Pid: 1})
	if err != nil || a == nil {
		t.Fatal(err)
	}
	defer a.Release()
	b, holder, err := Acquire("claude/two", Holder{Pid: 2})
	if err != nil || b == nil {
		t.Fatalf("a second conversation was refused: %v %+v", err, holder)
	}
	b.Release()
}

// The key is what makes this guard work across daemons, so it must not depend
// on anything a daemon configures for itself.
func TestKeyIgnoresTheDaemonsHome(t *testing.T) {
	t.Setenv("RCMANAGER_HOME", "/somewhere/else")
	before := Key("claude", "abc", "api", false)
	t.Setenv("RCMANAGER_HOME", "/somewhere/different")
	if after := Key("claude", "abc", "api", false); after != before {
		t.Errorf("Key() changed with RCMANAGER_HOME: %q → %q", before, after)
	}

	if got := Key("claude", "", "api", false); got != "claude/name:api" {
		t.Errorf("Key() with no session id = %q", got)
	}
	// A machine-wide singleton provider collapses to one key, so the rule
	// holds across daemons as well as within one.
	if got := Key("codex", "abc", "api", true); got != "codex/singleton" {
		t.Errorf("Key() for a singleton = %q", got)
	}
}

// A name that is fine for a session is not necessarily fine for a filename.
func TestAwkwardKeysBecomeOneFile(t *testing.T) {
	dir := withDir(t)
	c, _, err := Acquire("claude/name:some.session-1", Holder{Pid: 1})
	if err != nil || c == nil {
		t.Fatal(err)
	}
	defer c.Release()

	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 1 {
		t.Fatalf("expected one lock file, got %v", entries)
	}
	if filepath.Ext(entries[0].Name()) != ".lock" {
		t.Errorf("lock file is named %q", entries[0].Name())
	}
}
