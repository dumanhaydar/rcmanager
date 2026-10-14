package logstream

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// A TUI repaints a spinner several times a second; left in, those writes are
// the overwhelming majority of a session log.
func TestWriteDropsRepaintsAndKeepsOutput(t *testing.T) {
	file := filepath.Join(t.TempDir(), "out.log")
	s := New(file, Options{Strip: true})
	s.Write([]byte("\x1b[2K⠹ \x1b[32m\x1b[39m\n"))
	s.Write([]byte("\x1b[32mremote control URL: https://claude.ai/code/session_x\x1b[39m\n"))
	s.Close()

	raw, err := os.ReadFile(file)
	if err != nil {
		t.Fatal(err)
	}
	text := string(raw)
	if strings.Contains(text, "\x1b[") {
		t.Error("escapes reached the log")
	}
	if !strings.Contains(text, "remote control URL") {
		t.Error("real output was dropped")
	}
	if strings.Contains(text, "⠹") {
		t.Error("a spinner repaint was kept")
	}
}

func TestRotateKeepsTheConfiguredNumberOfFiles(t *testing.T) {
	file := filepath.Join(t.TempDir(), "out.log")
	s := New(file, Options{MaxBytes: 64, Retain: 2})
	for i := 0; i < 20; i++ {
		s.Line("a reasonably long line of session output")
	}
	s.Close()

	if _, err := os.Stat(file + ".1"); err != nil {
		t.Errorf("nothing was rotated: %v", err)
	}
	if _, err := os.Stat(file + ".3"); err == nil {
		t.Error("more rotations were kept than logRetain allows")
	}
}

// A full or read-only disk, or a log deleted underneath us, must never take
// the daemon down.
func TestWriteSurvivesTheFileDisappearing(t *testing.T) {
	file := filepath.Join(t.TempDir(), "out.log")
	s := New(file, Options{})
	_ = os.Remove(file)
	for i := 0; i < 250; i++ {
		s.Line("still writing")
	}
	s.Close()
	if _, err := os.Stat(file); err != nil {
		t.Errorf("the stream never reopened its file: %v", err)
	}
}
