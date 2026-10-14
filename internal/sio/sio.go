// Package sio gives a managed session stdio that outlives the daemon.
//
// A supervised agent must survive `systemctl restart rcmanager`. Piping its stdio
// to the daemon makes that impossible: when the daemon exits, the read ends of
// those pipes close and the child dies of SIGPIPE on its next write. So the
// pipes have to go.
//
//	stdout/stderr -> plain files, opened O_APPEND. A descriptor on a file has
//	                 no peer to lose, so the child neither notices nor cares
//	                 that the daemon went away. The daemon *tails* those files
//	                 instead of reading a pipe, which means a restarted daemon
//	                 resumes capture exactly where the old one stopped.
//
//	stdin         -> a fifo, opened O_RDWR. This is the load-bearing trick: the
//	                 child inherits a descriptor that is simultaneously a
//	                 reader and a writer, so the fifo never reaches
//	                 end-of-file even when every other writer is gone. A TUI
//	                 that would exit on EOF simply keeps waiting, and a new
//	                 daemon can reopen the write end later to answer a prompt.
//
// The cost is that the raw files need draining and truncating in place rather
// than rotating — see Tailer.
package sio

import (
	"os"
	"sync"
	"syscall"
	"time"

	"github.com/dumanhaydar/rcmanager/internal/paths"
)

// TruncateAfter is how much consumed raw capture is allowed to accumulate
// before the file is truncated back to nothing.
const TruncateAfter int64 = 1024 * 1024

// Offsets is where capture of each raw stream has got to.
type Offsets struct {
	Out int64 `json:"out"`
	Err int64 `json:"err"`
}

// IO is the three descriptors a managed session runs on.
type IO struct {
	Name  string
	Files paths.IOFiles

	mu       sync.Mutex
	stdin    *os.File // O_RDWR on the fifo
	out      *os.File // O_APPEND on the raw stdout file
	err      *os.File
	usesFifo bool
}

// New describes a session's stdio without opening anything.
func New(name string) *IO {
	return &IO{Name: name, Files: paths.IOPaths(name)}
}

// Open opens everything a spawn needs. With fresh set, the raw files are
// truncated first so a newly started session does not replay the previous
// run's output. Safe to call for a session that is already running: it reuses
// the existing fifo and appends to the existing raw files.
func (io *IO) Open(fresh bool) error {
	io.mu.Lock()
	defer io.mu.Unlock()

	if err := paths.EnsureHome(); err != nil {
		return err
	}

	if !isFifo(io.Files.Stdin) {
		_ = os.Remove(io.Files.Stdin)
		if err := syscall.Mkfifo(io.Files.Stdin, 0o600); err != nil && !os.IsExist(err) {
			io.usesFifo = false
		}
	}
	if isFifo(io.Files.Stdin) {
		// O_RDWR, never O_WRONLY: opening a fifo for writing alone blocks until
		// a reader arrives, and the reader here is the child we have not
		// spawned yet.
		fd, err := os.OpenFile(io.Files.Stdin, os.O_RDWR, 0o600)
		if err == nil {
			io.stdin = fd
			io.usesFifo = true
		}
	}

	if fresh {
		_ = os.WriteFile(io.Files.Out, nil, 0o600)
		_ = os.WriteFile(io.Files.Err, nil, 0o600)
	}
	out, err := os.OpenFile(io.Files.Out, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o600)
	if err != nil {
		return err
	}
	errf, err := os.OpenFile(io.Files.Err, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o600)
	if err != nil {
		_ = out.Close()
		return err
	}
	io.out, io.err = out, errf
	return nil
}

// UsesFifo reports whether stdin is a fifo. Without one, a session cannot be
// prompted and does not survive a daemon restart; rcmanager says so rather than
// pretending otherwise.
func (io *IO) UsesFifo() bool {
	io.mu.Lock()
	defer io.mu.Unlock()
	return io.usesFifo
}

// Stdio returns the three files to hand to a child process.
func (io *IO) Stdio() (stdin, stdout, stderr *os.File) {
	io.mu.Lock()
	defer io.mu.Unlock()
	return io.stdin, io.out, io.err
}

// WriteKeys sends keystrokes to the running child. It reports false when
// there is no fifo to write to.
func (io *IO) WriteKeys(keys string) bool {
	io.mu.Lock()
	fd := io.stdin
	io.mu.Unlock()
	if fd == nil {
		return false
	}
	_, err := fd.WriteString(keys)
	return err == nil
}

// ReopenStdin reattaches the write end after a daemon restart, without
// disturbing the child. It reports false when the fifo is gone (the session
// predates this layout, or the run directory was cleaned).
func (io *IO) ReopenStdin() bool {
	io.mu.Lock()
	defer io.mu.Unlock()
	if io.stdin != nil {
		return true
	}
	if !isFifo(io.Files.Stdin) {
		return false
	}
	fd, err := os.OpenFile(io.Files.Stdin, os.O_RDWR, 0o600)
	if err != nil {
		return false
	}
	io.stdin = fd
	io.usesFifo = true
	return true
}

// Close releases the descriptors, leaving the files and the fifo in place for
// the next daemon.
func (io *IO) Close() {
	io.mu.Lock()
	defer io.mu.Unlock()
	for _, fd := range []**os.File{&io.stdin, &io.out, &io.err} {
		if *fd != nil {
			_ = (*fd).Close()
			*fd = nil
		}
	}
}

// Rename moves a session's stdio files to the ones a new name implies.
//
// The files are renamed, never recreated: a running child holds descriptors on
// those inodes, and rename(2) leaves an inode alone, so the agent keeps
// writing to the same place without ever learning it is called something else
// now. Recreating them would strand its output in an unlinked file.
func (io *IO) Rename(newName string) error {
	io.mu.Lock()
	defer io.mu.Unlock()

	if err := paths.EnsureHome(); err != nil {
		return err
	}
	next := paths.IOPaths(newName)

	for _, move := range [][2]string{
		{io.Files.Stdin, next.Stdin},
		{io.Files.Out, next.Out},
		{io.Files.Err, next.Err},
	} {
		if move[0] == move[1] {
			continue
		}
		if _, err := os.Stat(move[0]); err != nil {
			continue // nothing there yet: the new name simply starts clean
		}
		if err := os.Rename(move[0], move[1]); err != nil {
			return err
		}
	}

	io.Name = newName
	io.Files = next
	return nil
}

// Destroy removes the fifo and raw files. Only for a session being
// unregistered.
func (io *IO) Destroy() {
	io.Close()
	for _, f := range []string{io.Files.Stdin, io.Files.Out, io.Files.Err} {
		_ = os.Remove(f)
	}
}

func isFifo(file string) bool {
	st, err := os.Stat(file)
	if err != nil {
		return false
	}
	return st.Mode()&os.ModeNamedPipe != 0
}

// Tailer reads new bytes out of a raw capture file and hands them on.
//
// Polling rather than filesystem events: a handful of files at 200ms is free,
// and watch events on append-heavy files are unreliable enough across
// platforms that the fallback would have to exist anyway.
type Tailer struct {
	file     string
	onData   func([]byte)
	interval time.Duration

	mu       sync.Mutex
	offset   int64
	consumed int64
	stop     chan struct{}
	done     chan struct{}
}

// NewTailer prepares a tailer for file.
func NewTailer(file string, onData func([]byte)) *Tailer {
	return &Tailer{file: file, onData: onData, interval: 200 * time.Millisecond}
}

// Start begins reading at offset. Pass the persisted offset so a successor
// daemon resumes mid-stream rather than replaying or skipping output.
func (t *Tailer) Start(offset int64) *Tailer {
	t.mu.Lock()
	if offset < 0 {
		offset = 0
	}
	t.offset = offset
	t.stop = make(chan struct{})
	t.done = make(chan struct{})
	stop := t.stop
	done := t.done
	t.mu.Unlock()

	t.Poll()
	go func() {
		defer close(done)
		ticker := time.NewTicker(t.interval)
		defer ticker.Stop()
		for {
			select {
			case <-stop:
				return
			case <-ticker.C:
				t.Poll()
			}
		}
	}()
	return t
}

// Position is where capture has got to, so a successor daemon can resume from
// here.
func (t *Tailer) Position() int64 {
	t.mu.Lock()
	defer t.mu.Unlock()
	return t.offset
}

// Poll reads whatever is new, right now.
func (t *Tailer) Poll() {
	t.mu.Lock()
	defer t.mu.Unlock()

	st, err := os.Stat(t.file)
	if err != nil {
		return
	}
	size := st.Size()

	// Truncated underneath us (by maybeTruncate, or by hand): start over
	// rather than seek past the end and capture nothing forever.
	if size < t.offset {
		t.offset = 0
		t.consumed = 0
	}
	if size == t.offset {
		t.maybeTruncateLocked(size)
		return
	}

	fd, err := os.Open(t.file)
	if err != nil {
		return
	}
	defer fd.Close()

	length := size - t.offset
	buf := make([]byte, length)
	n, err := fd.ReadAt(buf, t.offset)
	if n > 0 {
		t.offset += int64(n)
		t.consumed += int64(n)
		chunk := buf[:n]
		// The callback writes logs and may answer a prompt; do not hold the
		// tailer's lock across it.
		t.mu.Unlock()
		t.onData(chunk)
		t.mu.Lock()
	}
	_ = err // a partial read is retried on the next tick

	t.maybeTruncateLocked(t.offset)
}

// maybeTruncateLocked keeps the raw file from growing without bound.
//
// It cannot be rotated by rename: the child holds an open descriptor and would
// keep writing into the unlinked inode. Truncating in place works because that
// descriptor is O_APPEND, so the next write goes to the new end of file. We
// only do it once fully drained *and* the size still matches what we read,
// which closes the window where a write lands between the read and the
// truncate. A write inside the remaining microscopic window costs one line of
// a log; nothing depends on it.
func (t *Tailer) maybeTruncateLocked(sizeAtRead int64) {
	if t.consumed < TruncateAfter {
		return
	}
	st, err := os.Stat(t.file)
	if err != nil || st.Size() != sizeAtRead {
		return
	}
	if err := os.Truncate(t.file, 0); err != nil {
		return
	}
	t.offset = 0
	t.consumed = 0
}

// Stop ends the polling goroutine.
func (t *Tailer) Stop() {
	t.mu.Lock()
	stop, done := t.stop, t.done
	t.stop = nil
	t.mu.Unlock()
	if stop == nil {
		return
	}
	close(stop)
	<-done
}
