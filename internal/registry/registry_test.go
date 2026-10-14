package registry

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/dumanhaydar/rcmanager/internal/paths"
)

func withHome(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	t.Setenv("RCMANAGER_HOME", dir)
	paths.Reload()
	t.Cleanup(func() {
		os.Unsetenv("RCMANAGER_HOME")
		paths.Reload()
	})
	return dir
}

func TestNamesAreValidated(t *testing.T) {
	for _, bad := range []string{"", ".hidden", "has space", "slash/name", "-leading"} {
		if err := AssertValidName(bad); err == nil {
			t.Errorf("AssertValidName(%q) accepted an unusable name", bad)
		}
	}
	for _, good := range []string{"demo", "a.b_c-d", "9lives"} {
		if err := AssertValidName(good); err != nil {
			t.Errorf("AssertValidName(%q) = %v", good, err)
		}
	}
}

func TestUpsertKeepsRegisteredAt(t *testing.T) {
	withHome(t)

	first, err := Upsert(&Entry{Name: "demo", Provider: "claude", Cwd: t.TempDir(), Enabled: true})
	if err != nil {
		t.Fatal(err)
	}
	second, err := Upsert(&Entry{Name: "demo", Provider: "claude", Cwd: t.TempDir(), Enabled: false, Note: "edited"})
	if err != nil {
		t.Fatal(err)
	}
	if second.RegisteredAt != first.RegisteredAt {
		t.Errorf("registeredAt changed on update: %q -> %q", first.RegisteredAt, second.RegisteredAt)
	}
	if second.Note != "edited" || second.Enabled {
		t.Errorf("update did not apply: %+v", second)
	}
	all, _ := Load()
	if len(all) != 1 {
		t.Errorf("upsert duplicated the entry: %d entries", len(all))
	}
}

func TestRemoveLeavesOthersAlone(t *testing.T) {
	withHome(t)
	dir := t.TempDir()
	for _, name := range []string{"one", "two"} {
		if _, err := Upsert(&Entry{Name: name, Provider: "claude", Cwd: dir, Enabled: true}); err != nil {
			t.Fatal(err)
		}
	}
	removed, err := Remove("one")
	if err != nil || !removed {
		t.Fatalf("Remove() = %v, %v", removed, err)
	}
	if again, _ := Remove("one"); again {
		t.Error("Remove() reported removing an absent session")
	}
	if all, _ := Load(); len(all) != 1 || all[0].Name != "two" {
		t.Errorf("unexpected registry contents: %+v", all)
	}
}

// `enabled` is the runtime spelling and `autostart` the ecosystem-file one;
// an absent flag means the session wants to run.
func TestEcosystemEnabledSpellings(t *testing.T) {
	dir := t.TempDir()
	doc := []byte(`{"sessions":[
		{"name":"a","provider":"claude","cwd":"` + dir + `"},
		{"name":"b","provider":"claude","cwd":"` + dir + `","autostart":false},
		{"name":"c","provider":"claude","cwd":"` + dir + `","enabled":false,"autostart":true}
	]}`)

	entries, err := ParseEcosystem(doc, "test.json")
	if err != nil {
		t.Fatal(err)
	}
	want := map[string]bool{"a": true, "b": false, "c": false}
	for _, e := range entries {
		if e.Enabled != want[e.Name] {
			t.Errorf("%s: enabled = %v, want %v", e.Name, e.Enabled, want[e.Name])
		}
		if e.Source != "test.json" {
			t.Errorf("%s: source = %q", e.Name, e.Source)
		}
	}
}

func TestEcosystemAcceptsArrayOrDocument(t *testing.T) {
	dir := t.TempDir()
	for _, doc := range []string{
		`[{"name":"a","provider":"claude","cwd":"` + dir + `"}]`,
		`{"sessions":[{"name":"a","provider":"claude","cwd":"` + dir + `"}]}`,
	} {
		entries, err := ParseEcosystem([]byte(doc), "x.json")
		if err != nil {
			t.Fatalf("%s: %v", doc, err)
		}
		if len(entries) != 1 || entries[0].Name != "a" {
			t.Errorf("%s parsed to %+v", doc, entries)
		}
	}
	if _, err := ParseEcosystem([]byte(`{"nope":1}`), "x.json"); err == nil {
		t.Error("a document with no session list was accepted")
	}
}

// Only a change to how a session launches should force a restart; a new note
// or a different restart delay applies to a live session in place.
func TestLaunchDiff(t *testing.T) {
	base := &Entry{Name: "a", Provider: "claude", SessionID: "1", Cwd: "/tmp", Args: []string{"-x"},
		Env: map[string]string{"A": "1"}, ProviderOptions: map[string]any{"resumeMode": "summary"}}

	same := base.Clone()
	if diff := LaunchDiff(base, same); len(diff) != 0 {
		t.Errorf("identical entries differ: %v", diff)
	}

	noted := base.Clone()
	noted.Note = "hello"
	delay := int64(9)
	noted.RestartDelayMs = &delay
	if diff := LaunchDiff(base, noted); len(diff) != 0 {
		t.Errorf("a note/limit change was treated as a launch change: %v", diff)
	}

	moved := base.Clone()
	moved.Cwd = "/other"
	moved.Args = []string{"-y"}
	diff := LaunchDiff(base, moved)
	if len(diff) != 2 {
		t.Fatalf("LaunchDiff() = %v, want cwd and args", diff)
	}
}

func TestNormalizeRequiresProviderAndCwd(t *testing.T) {
	if _, err := Normalize(&Entry{Name: "a", Cwd: "/tmp"}, ""); err == nil {
		t.Error("an entry with no provider was accepted")
	}
	if _, err := Normalize(&Entry{Name: "a", Provider: "claude"}, ""); err == nil {
		t.Error("an entry with no cwd was accepted")
	}
	e, err := Normalize(&Entry{Name: "a", Provider: "claude", Cwd: "."}, "src")
	if err != nil {
		t.Fatal(err)
	}
	if !filepath.IsAbs(e.Cwd) {
		t.Errorf("cwd was not made absolute: %q", e.Cwd)
	}
}
