package format

import "testing"

func TestDuration(t *testing.T) {
	cases := map[int64]string{
		-1: "-", 0: "0s", 5_000: "5s", 90_000: "1m",
		3_600_000: "1h", 5_400_000: "1h30m", 86_400_000: "1d", 93_600_000: "1d2h",
	}
	for ms, want := range cases {
		if got := Duration(ms); got != want {
			t.Errorf("Duration(%d) = %q, want %q", ms, got, want)
		}
	}
}

// Paths read better from the right, so the head is what gets dropped.
func TestEllipsizeLeftKeepsTheTail(t *testing.T) {
	if got, want := EllipsizeLeft("/home/user/projects/app", 10), "…cts/app"; len(got) == 0 || got[len(got)-7:] != want[len(want)-7:] {
		t.Errorf("EllipsizeLeft() = %q", got)
	}
	if got := EllipsizeLeft("short", 10); got != "short" {
		t.Errorf("EllipsizeLeft() shortened a string that fits: %q", got)
	}
}

func TestTableAlignsColumns(t *testing.T) {
	out := Table([]string{"A", "Long header"}, [][]string{{"x", "y"}})
	lines := splitLines(out)
	if len(lines) != 5 {
		t.Fatalf("table has %d lines:\n%s", len(lines), out)
	}
	// Box-drawing characters are multi-byte, so compare rune counts.
	width := len([]rune(lines[0]))
	for _, l := range lines {
		if len([]rune(l)) != width {
			t.Errorf("ragged table:\n%s", out)
			break
		}
	}
}

func splitLines(s string) []string {
	var out []string
	current := ""
	for _, r := range s {
		if r == '\n' {
			out = append(out, current)
			current = ""
			continue
		}
		current += string(r)
	}
	if current != "" {
		out = append(out, current)
	}
	return out
}
