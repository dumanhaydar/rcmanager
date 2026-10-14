// Package jsonfile reads and writes rcmanager's state files.
//
// Writes are atomic, because a crash mid-write must never leave a truncated
// registry behind, and every write is fingerprinted so the daemon can tell a
// hand edit from its own save.
package jsonfile

import (
	"crypto/sha1"
	"encoding/hex"
	"encoding/json"
	"os"
	"path/filepath"
	"strconv"
	"sync"
)

// Read decodes file into v. A missing or unparseable file is not an error:
// the caller keeps whatever defaults v already holds, which is how rcmanager
// survives a half-saved config.
func Read(file string, v any) error {
	raw, err := os.ReadFile(file)
	if err != nil {
		return nil
	}
	if len(trimSpace(raw)) == 0 {
		return nil
	}
	if err := json.Unmarshal(raw, v); err != nil {
		return nil
	}
	return nil
}

// ReadStrict decodes file into v and reports a malformed document, for the
// cases where silently ignoring an edit would be worse than refusing it.
func ReadStrict(file string, v any) error {
	raw, err := os.ReadFile(file)
	if err != nil {
		return err
	}
	return json.Unmarshal(raw, v)
}

func trimSpace(b []byte) []byte {
	i, j := 0, len(b)
	for i < j && isSpace(b[i]) {
		i++
	}
	for j > i && isSpace(b[j-1]) {
		j--
	}
	return b[i:j]
}

func isSpace(c byte) bool { return c == ' ' || c == '\t' || c == '\n' || c == '\r' }

var (
	mu          sync.Mutex
	lastWritten = map[string]string{}
)

// IsOwnWrite reports whether a watched file's current bytes are exactly what
// rcmanager last wrote. Without it the daemon would reload on its own writes, so
// every `rcmanager start` would reconcile the change it had just made. Comparing
// content rather than mtime makes an editor saving an identical file a no-op.
func IsOwnWrite(file string) bool {
	mu.Lock()
	expected, ok := lastWritten[file]
	mu.Unlock()
	if !ok {
		return false
	}
	raw, err := os.ReadFile(file)
	if err != nil {
		return false
	}
	return hash(raw) == expected
}

func hash(b []byte) string {
	sum := sha1.Sum(b)
	return hex.EncodeToString(sum[:])
}

// Write serialises v to file through a temporary file and rename(2), which is
// atomic on the same filesystem.
func Write(file string, v any, mode os.FileMode) error {
	text, err := json.MarshalIndent(v, "", "\t")
	if err != nil {
		return err
	}
	text = append(text, '\n')

	if err := os.MkdirAll(filepath.Dir(file), 0o700); err != nil {
		return err
	}
	tmp := file + "." + strconv.Itoa(os.Getpid()) + ".tmp"
	if err := os.WriteFile(tmp, text, mode); err != nil {
		return err
	}
	if err := os.Rename(tmp, file); err != nil {
		os.Remove(tmp)
		return err
	}

	mu.Lock()
	lastWritten[file] = hash(text)
	mu.Unlock()
	return nil
}
