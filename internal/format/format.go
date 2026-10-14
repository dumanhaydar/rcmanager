// Package format renders rcmanager's tables and durations.
package format

import (
	"fmt"
	"strings"
	"time"

	"github.com/dumanhaydar/rcmanager/internal/ansi"
)

// Duration renders a millisecond count the way a status table wants it.
func Duration(ms int64) string {
	if ms < 0 {
		return "-"
	}
	s := ms / 1000
	if s < 60 {
		return fmt.Sprintf("%ds", s)
	}
	m := s / 60
	if m < 60 {
		return fmt.Sprintf("%dm", m)
	}
	h := m / 60
	if h < 24 {
		if m%60 != 0 {
			return fmt.Sprintf("%dh%dm", h, m%60)
		}
		return fmt.Sprintf("%dh", h)
	}
	d := h / 24
	if h%24 != 0 {
		return fmt.Sprintf("%dd%dh", d, h%24)
	}
	return fmt.Sprintf("%dd", d)
}

// Ago renders how long ago an RFC3339 timestamp was.
func Ago(ts string) string {
	if ts == "" {
		return "-"
	}
	t, err := time.Parse(time.RFC3339, ts)
	if err != nil {
		return "-"
	}
	return Duration(time.Since(t).Milliseconds()) + " ago"
}

// PaintStatus colours a session status.
func PaintStatus(status string) string {
	switch status {
	case "online":
		return ansi.Green(status)
	case "launching":
		return ansi.Cyan(status)
	case "restarting":
		return ansi.Yellow(status)
	case "errored":
		return ansi.Red(status)
	case "stopped", "disabled":
		return ansi.Grey(status)
	}
	return status
}

// EllipsizeLeft truncates to max visible runes, keeping the tail — paths read
// better from the right.
func EllipsizeLeft(s string, max int) string {
	runes := []rune(s)
	if len(runes) <= max || max < 2 {
		return s
	}
	return "…" + string(runes[len(runes)-(max-1):])
}

// Table renders a minimal box-drawing table.
func Table(headers []string, rows [][]string) string {
	width := make([]int, len(headers))
	for i, h := range headers {
		width[i] = ansi.VisibleLength(h)
	}
	for _, row := range rows {
		for i := range headers {
			if i < len(row) {
				if w := ansi.VisibleLength(row[i]); w > width[i] {
					width[i] = w
				}
			}
		}
	}

	rule := func(l, m, r string) string {
		parts := make([]string, len(width))
		for i, w := range width {
			parts[i] = strings.Repeat("─", w+2)
		}
		return l + strings.Join(parts, m) + r
	}
	line := func(cells []string) string {
		padded := make([]string, len(width))
		for i := range width {
			cell := ""
			if i < len(cells) {
				cell = cells[i]
			}
			padded[i] = cell + strings.Repeat(" ", max(0, width[i]-ansi.VisibleLength(cell)))
		}
		return "│ " + strings.Join(padded, " │ ") + " │"
	}

	bold := make([]string, len(headers))
	for i, h := range headers {
		bold[i] = ansi.Bold(h)
	}

	out := []string{rule("┌", "┬", "┐"), line(bold), rule("├", "┼", "┤")}
	for _, row := range rows {
		out = append(out, line(row))
	}
	out = append(out, rule("└", "┴", "┘"))
	return strings.Join(out, "\n")
}

// Pairs renders a key/value block with the keys aligned.
func Pairs(rows [][2]string) string {
	width := 0
	for _, r := range rows {
		if len(r[0]) > width {
			width = len(r[0])
		}
	}
	var b strings.Builder
	for _, r := range rows {
		b.WriteString(ansi.Grey(r[0] + strings.Repeat(" ", width-len(r[0]))))
		b.WriteString("  ")
		b.WriteString(r[1])
		b.WriteByte('\n')
	}
	return strings.TrimRight(b.String(), "\n")
}
