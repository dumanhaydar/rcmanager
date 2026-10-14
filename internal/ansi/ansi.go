// Package ansi paints terminal output and strips escapes back out of it.
//
// Agent CLIs repaint a TUI continuously; without stripping, a session log is
// megabytes of escape codes per hour and no prompt pattern ever matches.
package ansi

import (
	"os"
	"regexp"
	"strconv"
	"strings"
)

const esc = "\x1b"

var enabled = IsTerminal(os.Stdout) && os.Getenv("NO_COLOR") == ""

// IsTerminal reports whether f is a character device, which is as much as
// rcmanager needs to know before animating anything. Keeping it here keeps the
// whole binary dependency-free.
func IsTerminal(f *os.File) bool {
	if f == nil {
		return false
	}
	st, err := f.Stat()
	if err != nil {
		return false
	}
	return st.Mode()&os.ModeCharDevice != 0
}

// SetEnabled forces colour on or off. Tests and `--json` callers use it.
func SetEnabled(v bool) { enabled = v }

// Enabled reports whether colour is being emitted.
func Enabled() bool { return enabled }

func wrap(open, close int) func(string) string {
	return func(s string) string {
		if !enabled {
			return s
		}
		return esc + "[" + strconv.Itoa(open) + "m" + s + esc + "[" + strconv.Itoa(close) + "m"
	}
}

var (
	Bold    = wrap(1, 22)
	Dim     = wrap(2, 22)
	Red     = wrap(31, 39)
	Green   = wrap(32, 39)
	Yellow  = wrap(33, 39)
	Blue    = wrap(34, 39)
	Magenta = wrap(35, 39)
	Cyan    = wrap(36, 39)
	Grey    = wrap(90, 39)
)

var (
	// CSI sequences, plus OSC 8 hyperlinks — agent CLIs emit both.
	csiRe = regexp.MustCompile(esc + `\[[0-?]*[ -/]*[@-~]`)
	oscRe = regexp.MustCompile(esc + `\][^` + esc + `\x07]*(?:\x07|` + esc + `\\)`)
	// Cursor-forward. TUIs pad with ESC[<n>C instead of literal spaces, so
	// deleting it outright glues words together ("Resume from summary" ->
	// "Resumefromsummary") and breaks both readability and any matching.
	cufRe = regexp.MustCompile(esc + `\[([0-9]*)C`)
)

const maxPad = 200

// Strip removes ANSI escapes, turning cursor-forward padding back into spaces.
func Strip(s string) string {
	// Plain output is the common case on the write-the-log path, and three
	// regexes over text with nothing to strip is pure cost.
	if !strings.Contains(s, esc) {
		return s
	}
	s = oscRe.ReplaceAllString(s, "")
	s = cufRe.ReplaceAllStringFunc(s, func(m string) string {
		digits := cufRe.FindStringSubmatch(m)[1]
		n := 1
		if digits != "" {
			if v, err := strconv.Atoi(digits); err == nil {
				n = v
			}
		}
		if n > maxPad {
			n = maxPad
		}
		return strings.Repeat(" ", n)
	})
	return csiRe.ReplaceAllString(s, "")
}

// VisibleLength is the rune count once escapes are removed, for table padding.
func VisibleLength(s string) int { return len([]rune(Strip(s))) }
