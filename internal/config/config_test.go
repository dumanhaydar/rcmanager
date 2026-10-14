package config

import (
	"os"
	"testing"

	"github.com/dumanhaydar/rcmanager/internal/paths"
)

func withHome(t *testing.T) {
	t.Helper()
	t.Setenv("RCMANAGER_HOME", t.TempDir())
	paths.Reload()
	t.Cleanup(func() {
		os.Unsetenv("RCMANAGER_HOME")
		paths.Reload()
	})
}

// An absent key keeps its default: that is the whole merge semantics, and a
// half-written config must not silently zero every setting.
func TestLoadMergesOverDefaults(t *testing.T) {
	withHome(t)
	if err := os.WriteFile(paths.Get().Config, []byte(`{"restartDelayMs":500}`), 0o600); err != nil {
		t.Fatal(err)
	}
	cfg := Load()
	if cfg.RestartDelayMs != 500 {
		t.Errorf("RestartDelayMs = %d, want 500", cfg.RestartDelayMs)
	}
	if cfg.MaxRestarts != Defaults().MaxRestarts || !cfg.Autorestart {
		t.Errorf("absent keys did not fall back to defaults: %+v", cfg)
	}
}

func TestSetCoercesAndValidates(t *testing.T) {
	withHome(t)

	if _, err := Set("autorestart", "off"); err != nil {
		t.Fatal(err)
	}
	if Load().Autorestart {
		t.Error("autorestart=off was not stored")
	}
	if _, err := Set("autorestart", "maybe"); err == nil {
		t.Error("a non-boolean was accepted")
	}
	if _, err := Set("isolateSessions", "sometimes"); err == nil {
		t.Error("a value outside the enum was accepted")
	}
	if _, err := Set("maxRestarts", "abc"); err == nil {
		t.Error("a non-number was accepted")
	}
	if _, err := Set("nonsense", "1"); err == nil {
		t.Error("an unknown key was accepted")
	}
}

// `set` and `unset` touch one key and leave everything else alone, including
// keys a newer rcmanager wrote.
func TestSetPreservesUnknownKeys(t *testing.T) {
	withHome(t)
	if err := os.WriteFile(paths.Get().Config, []byte(`{"futureSetting":"keep me"}`), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := Set("logRetain", "7"); err != nil {
		t.Fatal(err)
	}
	raw, _ := os.ReadFile(paths.Get().Config)
	if !contains(string(raw), "futureSetting") {
		t.Errorf("an unrecognised key was dropped: %s", raw)
	}

	value, err := Unset("logRetain")
	if err != nil {
		t.Fatal(err)
	}
	if Load().LogRetain != Defaults().LogRetain {
		t.Error("unset did not restore the default")
	}
	if got := itoaAny(value); got != "3" {
		t.Errorf("Unset() reported %q as the new default", got)
	}
}

func TestValueOfPrintsIntegersAsIntegers(t *testing.T) {
	withHome(t)
	// Decoded as a float64, 10485760 would be reported as 1.048576e+07.
	v, err := ValueOf(Load(), "maxLogBytes")
	if err != nil {
		t.Fatal(err)
	}
	if got := itoaAny(v); got != "10485760" {
		t.Errorf("maxLogBytes printed as %q", got)
	}
}

func TestKeysCoversTheWholeSchema(t *testing.T) {
	if len(Keys()) != len(Schema) {
		t.Errorf("Keys() lists %d of %d schema entries", len(Keys()), len(Schema))
	}
}

func contains(haystack, needle string) bool {
	return len(haystack) >= len(needle) && (haystack == needle || indexOf(haystack, needle) >= 0)
}

func indexOf(haystack, needle string) int {
	for i := 0; i+len(needle) <= len(haystack); i++ {
		if haystack[i:i+len(needle)] == needle {
			return i
		}
	}
	return -1
}

func itoaAny(v any) string {
	type stringer interface{ String() string }
	if s, ok := v.(stringer); ok {
		return s.String()
	}
	return ""
}
