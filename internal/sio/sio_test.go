package sio

import (
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"

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

// The fifo is opened O_RDWR so it never reaches end-of-file: that is what lets
// a session survive the daemon, and what lets a later daemon answer a prompt.
func TestStdinFifoSurvivesReopening(t *testing.T) {
	withHome(t)
	io := New("demo")
	if err := io.Open(true); err != nil {
		t.Fatal(err)
	}
	if !io.UsesFifo() {
		t.Fatal("no fifo was created, so this session could not be prompted")
	}

	stdin, _, _ := io.Stdio()
	read := make(chan string, 1)
	go func() {
		buf := make([]byte, 16)
		n, _ := stdin.Read(buf)
		read <- string(buf[:n])
	}()
	if !io.WriteKeys("y\n") {
		t.Fatal("WriteKeys() failed")
	}
	select {
	case got := <-read:
		if got != "y\n" {
			t.Errorf("read %q, want %q", got, "y\n")
		}
	case <-time.After(2 * time.Second):
		t.Fatal("the fifo never delivered the keystrokes")
	}

	// A daemon handover: close the write end and take it back.
	io.Close()
	if !io.ReopenStdin() {
		t.Fatal("ReopenStdin() failed, so prompts could no longer be answered")
	}
	io.Destroy()
	if _, err := os.Stat(io.Files.Stdin); !os.IsNotExist(err) {
		t.Error("Destroy() left the fifo behind")
	}
}

func TestOpenFreshTruncatesRawFiles(t *testing.T) {
	withHome(t)
	if err := paths.EnsureHome(); err != nil {
		t.Fatal(err)
	}
	io := New("demo")
	if err := os.WriteFile(io.Files.Out, []byte("old output"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := io.Open(true); err != nil {
		t.Fatal(err)
	}
	defer io.Destroy()
	if st, _ := os.Stat(io.Files.Out); st.Size() != 0 {
		t.Errorf("a new session replayed %d bytes of the previous run", st.Size())
	}
}

func TestTailerResumesFromAnOffset(t *testing.T) {
	file := filepath.Join(t.TempDir(), "raw")
	if err := os.WriteFile(file, []byte("already captured\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	first, _ := os.Stat(file)

	var mu sync.Mutex
	var seen string
	tailer := NewTailer(file, func(b []byte) {
		mu.Lock()
		seen += string(b)
		mu.Unlock()
	})
	// Starting at the current size is how a successor daemon avoids replaying
	// old output — and avoids re-answering prompts by typing into a live agent.
	tailer.Start(first.Size())
	defer tailer.Stop()

	fd, err := os.OpenFile(file, os.O_APPEND|os.O_WRONLY, 0o600)
	if err != nil {
		t.Fatal(err)
	}
	_, _ = fd.WriteString("new line\n")
	_ = fd.Close()

	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		mu.Lock()
		done := seen == "new line\n"
		mu.Unlock()
		if done {
			if pos := tailer.Position(); pos != first.Size()+9 {
				t.Errorf("Position() = %d, want %d", pos, first.Size()+9)
			}
			return
		}
		time.Sleep(50 * time.Millisecond)
	}
	mu.Lock()
	defer mu.Unlock()
	t.Fatalf("tailer captured %q", seen)
}

// Truncated underneath us (by the drain, or by hand) the tailer has to start
// over rather than seek past the end and capture nothing forever.
func TestTailerRecoversFromTruncation(t *testing.T) {
	file := filepath.Join(t.TempDir(), "raw")
	if err := os.WriteFile(file, []byte("aaaa"), 0o600); err != nil {
		t.Fatal(err)
	}
	var mu sync.Mutex
	var seen string
	tailer := NewTailer(file, func(b []byte) {
		mu.Lock()
		seen += string(b)
		mu.Unlock()
	})
	tailer.Start(4)
	defer tailer.Stop()

	if err := os.WriteFile(file, []byte("b"), 0o600); err != nil {
		t.Fatal(err)
	}
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		mu.Lock()
		got := seen
		mu.Unlock()
		if got == "b" {
			return
		}
		time.Sleep(50 * time.Millisecond)
	}
	t.Fatal("the tailer never recovered after the file was truncated")
}
