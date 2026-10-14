// Package logstream writes session logs that stay readable.
//
// Agent CLIs redraw a status line several times a second, so ANSI is stripped
// and repaint-only writes are dropped; without that a session log grows by
// megabytes of escape codes an hour and says nothing.
package logstream

import (
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/dumanhaydar/rcmanager/internal/ansi"
)

// Options configures a log sink. The zero value is replaced by sane defaults.
type Options struct {
	MaxBytes int64
	Retain   int
	Strip    bool
}

// isDecoration reports whether r is one of the spinner glyphs, box-drawing
// characters and progress marks an agent TUI repaints continuously. Left in,
// they are the overwhelming majority of a session log.
//
// Written as a range check rather than a character class: this runs on every
// line of every session's output, and the regexp engine showed up in the
// supervisor's profile for exactly this.
func isDecoration(r rune) bool {
	switch {
	case r >= 0x2500 && r <= 0x257f, // box drawing
		r >= 0x2580 && r <= 0x259f, // block elements
		r >= 0x25a0 && r <= 0x25ff, // geometric shapes
		r >= 0x2600 && r <= 0x27bf, // symbols, dingbats, braille spinners
		r >= 0x2b00 && r <= 0x2bff: // arrows and symbols
		return true
	}
	switch r {
	case '*', '|', '/', '\\', '_', '.', '·', '╌', '─', '━':
		return true
	}
	return false
}

// informative reports whether a line carries anything but repaint: once
// whitespace and decoration are discounted, fewer than three characters left
// means it said nothing. Real output — words, paths, urls — always survives.
func informative(line string) bool {
	kept := 0
	for _, r := range line {
		if r == ' ' || r == '\t' || r == '\n' || r == '\r' || r == '\v' || r == '\f' {
			continue
		}
		if isDecoration(r) {
			continue
		}
		if kept++; kept >= 3 {
			return true
		}
	}
	return false
}

// denoise drops repaint-only lines.
func denoise(text string) string {
	var kept []string
	for _, line := range strings.Split(text, "\n") {
		if informative(line) {
			kept = append(kept, line)
		}
	}
	if len(kept) == 0 {
		return ""
	}
	return strings.Join(kept, "\n") + "\n"
}

// Stream is an append-only, size-rotating log sink, safe for concurrent use.
type Stream struct {
	mu     sync.Mutex
	file   string
	opts   Options
	fd     *os.File
	size   int64
	writes int
}

// New opens (and creates) a log file.
func New(file string, opts Options) *Stream {
	if opts.MaxBytes <= 0 {
		opts.MaxBytes = 10 * 1024 * 1024
	}
	if opts.Retain <= 0 {
		opts.Retain = 3
	}
	s := &Stream{file: file, opts: opts}
	s.open()
	return s
}

func (s *Stream) open() {
	_ = os.MkdirAll(filepath.Dir(s.file), 0o700)
	fd, err := os.OpenFile(s.file, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o600)
	if err != nil {
		s.fd = nil
		return
	}
	s.fd = fd
	if st, err := fd.Stat(); err == nil {
		s.size = st.Size()
	} else {
		s.size = 0
	}
}

// Write appends a chunk, stripping and denoising it first when configured.
func (s *Stream) Write(chunk []byte) {
	text := string(chunk)
	if s.opts.Strip {
		text = denoise(strings.ReplaceAll(ansi.Strip(text), "\r", "\n"))
		if strings.TrimSpace(text) == "" {
			return
		}
	}

	s.mu.Lock()
	defer s.mu.Unlock()
	if s.fd == nil {
		s.open()
		if s.fd == nil {
			return
		}
	}

	// If the file was deleted or rotated underneath us (logrotate, `rm`), the
	// descriptor still points at an unlinked inode and the output silently
	// vanishes.
	s.writes++
	if s.writes%200 == 0 {
		if _, err := os.Stat(s.file); err != nil {
			s.reopenLocked()
		}
	}

	n, err := s.fd.WriteString(text)
	if err != nil {
		// A full or read-only disk must never take the daemon down.
		return
	}
	s.size += int64(n)
	if s.size > s.opts.MaxBytes {
		s.rotateLocked()
	}
}

// Line writes one timestamped line.
func (s *Stream) Line(text string) {
	s.Write([]byte("[" + time.Now().UTC().Format(time.RFC3339) + "] " + text + "\n"))
}

func (s *Stream) reopenLocked() {
	if s.fd != nil {
		_ = s.fd.Close()
		s.fd = nil
	}
	s.open()
}

func (s *Stream) rotateLocked() {
	if s.fd != nil {
		_ = s.fd.Close()
		s.fd = nil
	}
	for i := s.opts.Retain - 1; i >= 1; i-- {
		from := s.file + "." + strconv.Itoa(i)
		to := s.file + "." + strconv.Itoa(i+1)
		if _, err := os.Stat(from); err == nil {
			_ = os.Rename(from, to)
		}
	}
	_ = os.Rename(s.file, s.file+".1")
	s.open()
	s.size = 0
}

// Close releases the descriptor. A closed stream can be written to again; it
// reopens on demand.
func (s *Stream) Close() {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.fd != nil {
		_ = s.fd.Close()
		s.fd = nil
	}
}
