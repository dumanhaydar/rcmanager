// Package watch notices when someone edits a file rcmanager reads.
//
// There is no filesystem-event dependency here: a 1s stat poll on two files is
// free, works identically on Linux, macOS and in a container with no inotify
// budget left, and survives the atomic rename that every editor worth using
// performs on save (which would make an inode-level watch go deaf).
package watch

import (
	"os"
	"sync"
	"time"
)

// Watcher polls one file and reports edits made by anyone but rcmanager.
type Watcher struct {
	stop chan struct{}
	once sync.Once
}

// Interval is how often a watched file is stat'ed.
const Interval = time.Second

// File watches file and calls onChange when its content changes. isOwn lets
// the caller ignore the writes rcmanager itself just made — without it the daemon
// would reload on its own saves, so every `rcmanager start` would reconcile the
// change it had just made.
func File(file string, onChange func(), isOwn func() bool) *Watcher {
	w := &Watcher{stop: make(chan struct{})}
	last := fingerprint(file)

	go func() {
		ticker := time.NewTicker(Interval)
		defer ticker.Stop()
		for {
			select {
			case <-w.stop:
				return
			case <-ticker.C:
				current := fingerprint(file)
				if current == last {
					continue
				}
				last = current
				if isOwn != nil && isOwn() {
					continue
				}
				// A bad edit must not kill the daemon.
				func() {
					defer func() { _ = recover() }()
					onChange()
				}()
			}
		}
	}()
	return w
}

// Active reports whether the watcher is running. Kept for symmetry with
// event-based backends, which can fail to start.
func (w *Watcher) Active() bool { return w != nil && w.stop != nil }

// Close stops watching.
func (w *Watcher) Close() {
	if w == nil {
		return
	}
	w.once.Do(func() { close(w.stop) })
}

// fingerprint is size plus modification time: enough to notice an edit, and
// cheaper than hashing a file on every tick. Content equality is settled by
// the caller's isOwn check.
func fingerprint(file string) string {
	st, err := os.Stat(file)
	if err != nil {
		return "absent"
	}
	return st.ModTime().UTC().Format(time.RFC3339Nano) + ":" + itoa(st.Size())
}

func itoa(n int64) string {
	if n == 0 {
		return "0"
	}
	var digits []byte
	for n > 0 {
		digits = append([]byte{byte('0' + n%10)}, digits...)
		n /= 10
	}
	return string(digits)
}
