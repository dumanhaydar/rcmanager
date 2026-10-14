// Package claim stops two supervisors from driving the same conversation.
//
// rcmanager already refuses to attach on top of an agent process someone else
// is running — that check is the provider's, and it looks at agent processes.
// This is the other half: two rcmanager daemons, started with different
// RCMANAGER_HOME values, each with the session in its own registry, each
// perfectly entitled to supervise it as far as it can tell. Both then restart
// it, and two agents end up appending to one transcript.
//
// The guard is an advisory lock per conversation, held by the daemon for as
// long as it supervises that session:
//
//   - It lives outside RCMANAGER_HOME, because a guard kept inside the home
//     being guarded against is no guard at all. That is the one thing
//     rcmanager writes elsewhere, and it writes nothing but an empty file and
//     a line saying who holds it.
//   - It is flock(2), not a pid file, because a lock dies with the process
//     that held it. A daemon that is SIGKILLed, or whose machine loses power,
//     leaves nothing to clean up and nothing stale to misread.
//   - It keys on the conversation, not the session name: the same transcript
//     under two different names in two registries is exactly the collision
//     worth preventing.
package claim

import (
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"syscall"
	"time"
)

// Holder describes the daemon that already supervises a session.
type Holder struct {
	Pid  int    `json:"pid"`
	Home string `json:"home"`
	Name string `json:"name"`
	At   string `json:"at"`
}

// Describe renders a holder for a refusal message.
func (h Holder) Describe() string {
	parts := []string{}
	if h.Pid != 0 {
		parts = append(parts, "pid "+strconv.Itoa(h.Pid))
	}
	if h.Home != "" {
		parts = append(parts, "state "+h.Home)
	}
	if h.Name != "" {
		parts = append(parts, "as \""+h.Name+"\"")
	}
	if len(parts) == 0 {
		return "another rcmanager daemon"
	}
	return "another rcmanager daemon (" + strings.Join(parts, " · ") + ")"
}

// Dir is where the locks live: per user, and independent of any daemon's home.
//
// RCMANAGER_CLAIM_DIR overrides it, which is what the test suite uses so that
// two test runs on one machine do not contend over the same conversation
// names. Otherwise it is the user's cache directory, which every service unit
// rcmanager generates can reach because they all set HOME.
func Dir() string {
	if dir := strings.TrimSpace(os.Getenv("RCMANAGER_CLAIM_DIR")); dir != "" {
		return dir
	}
	if cache := strings.TrimSpace(os.Getenv("XDG_CACHE_HOME")); cache != "" {
		return filepath.Join(cache, "rcmanager", "claims")
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return filepath.Join(os.TempDir(), "rcmanager-claims")
	}
	return filepath.Join(home, ".cache", "rcmanager", "claims")
}

// Key identifies the thing that must not be supervised twice.
//
// A session id is the conversation itself, so it wins when there is one. A
// provider that allows only one managed session per machine is keyed on the
// provider alone, which makes that rule hold across daemons too. Failing
// both, the name is all there is to go on.
func Key(providerID, sessionID, name string, singleton bool) string {
	switch {
	case singleton:
		return providerID + "/singleton"
	case sessionID != "":
		return providerID + "/" + sessionID
	default:
		return providerID + "/name:" + name
	}
}

var unsafeKey = regexp.MustCompile(`[^A-Za-z0-9._-]`)

// Claim is a held lock. Release it when the session stops being supervised.
type Claim struct {
	key  string
	file string
	fd   *os.File
}

// Key returns the conversation this claim covers.
func (c *Claim) Key() string {
	if c == nil {
		return ""
	}
	return c.key
}

// Acquire takes the lock for key.
//
// It returns (claim, zero, nil) when the caller may supervise the session,
// (nil, holder, nil) when another daemon already does, and (nil, zero, err)
// when the lock could not be used at all — the caller decides what to do with
// that, and rcmanager chooses to supervise anyway rather than refuse to work
// on a machine where the lock directory is unusable.
func Acquire(key string, self Holder) (*Claim, Holder, error) {
	dir := Dir()
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return nil, Holder{}, err
	}
	file := filepath.Join(dir, unsafeKey.ReplaceAllString(key, "_")+".lock")

	fd, err := os.OpenFile(file, os.O_CREATE|os.O_RDWR, 0o600)
	if err != nil {
		return nil, Holder{}, err
	}

	if err := syscall.Flock(int(fd.Fd()), syscall.LOCK_EX|syscall.LOCK_NB); err != nil {
		_ = fd.Close()
		if errors.Is(err, syscall.EWOULDBLOCK) || errors.Is(err, syscall.EAGAIN) {
			// Reading needs no lock, so the loser can say who won.
			return nil, read(file), nil
		}
		return nil, Holder{}, err
	}

	self.At = time.Now().UTC().Format(time.RFC3339)
	if blob, err := json.Marshal(self); err == nil {
		_ = fd.Truncate(0)
		_, _ = fd.WriteAt(blob, 0)
		_ = fd.Sync()
	}
	return &Claim{key: key, file: file, fd: fd}, Holder{}, nil
}

// Release hands the conversation back, so a successor daemon can take it.
func (c *Claim) Release() {
	if c == nil || c.fd == nil {
		return
	}
	// Truncating first means a reader never attributes the lock to a daemon
	// that has already let go of it.
	_ = c.fd.Truncate(0)
	_ = syscall.Flock(int(c.fd.Fd()), syscall.LOCK_UN)
	_ = c.fd.Close()
	c.fd = nil
}

func read(file string) Holder {
	raw, err := os.ReadFile(file)
	if err != nil || len(raw) == 0 {
		return Holder{}
	}
	var h Holder
	if err := json.Unmarshal(raw, &h); err != nil {
		return Holder{}
	}
	return h
}
