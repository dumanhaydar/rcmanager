package ansi

import "testing"

func TestStripRemovesCSIAndOSC(t *testing.T) {
	in := "\x1b[32mgreen\x1b[39m \x1b]8;;https://example.com\x07link\x1b]8;;\x07"
	if got, want := Strip(in), "green link"; got != want {
		t.Fatalf("Strip() = %q, want %q", got, want)
	}
}

// A TUI pads with cursor-forward instead of spaces. Deleting it outright would
// glue words together and break every prompt pattern.
func TestStripTurnsCursorForwardIntoSpaces(t *testing.T) {
	if got, want := Strip("Resume\x1b[1Cfrom\x1b[1Csummary"), "Resume from summary"; got != want {
		t.Fatalf("Strip() = %q, want %q", got, want)
	}
	if got := Strip("a\x1b[9999Cb"); len(got) != 1+maxPad+1 {
		t.Fatalf("padding was not clamped: got %d chars", len(got))
	}
}

func TestVisibleLengthIgnoresEscapes(t *testing.T) {
	SetEnabled(true)
	defer SetEnabled(false)
	if got, want := VisibleLength(Bold("abc")), 3; got != want {
		t.Fatalf("VisibleLength() = %d, want %d", got, want)
	}
}
