// Package registry is rcmanager's source of truth for which sessions exist.
//
// It is an ecosystem document — the sessions a machine is expected to be
// running, in a file a human can read and edit:
//
//	{"sessions": [{name, provider, sessionId, cwd, enabled, env, args, ...}]}
//
// It never contains transcripts and never points at anything rcmanager may delete.
package registry

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"time"

	"github.com/dumanhaydar/rcmanager/internal/jsonfile"
	"github.com/dumanhaydar/rcmanager/internal/paths"
	"github.com/dumanhaydar/rcmanager/internal/rcerr"
)

var validName = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._-]*$`)

// AssertValidName rejects a name that could not be used as a filename or a
// unit name.
func AssertValidName(name string) error {
	if !validName.MatchString(name) {
		return rcerr.New(fmt.Sprintf("Invalid session name %q.", name),
			"Names may contain letters, digits, dot, underscore and dash, and must start with a letter or digit.")
	}
	return nil
}

// Entry is one registered session.
//
// The supervision fields are pointers so that "unset" is distinguishable from
// "set to zero": an unset override falls back to the global config, while
// maxRestarts: 0 genuinely means no restarts.
type Entry struct {
	Name            string            `json:"name"`
	Provider        string            `json:"provider"`
	SessionID       string            `json:"sessionId,omitempty"`
	Cwd             string            `json:"cwd"`
	Enabled         bool              `json:"enabled"`
	Args            []string          `json:"args,omitempty"`
	Env             map[string]string `json:"env,omitempty"`
	ProviderOptions map[string]any    `json:"providerOptions,omitempty"`

	// Per-session supervision overrides; unset keys fall back to the global
	// config. Every key a session reads through its setting lookup must be
	// here, or the override is silently dropped on the way in.
	Autorestart     *bool  `json:"autorestart,omitempty"`
	MaxRestarts     *int   `json:"maxRestarts,omitempty"`
	RestartDelayMs  *int64 `json:"restartDelayMs,omitempty"`
	RestartWindowMs *int64 `json:"restartWindowMs,omitempty"`
	MinUptimeMs     *int64 `json:"minUptimeMs,omitempty"`
	StopTimeoutMs   *int64 `json:"stopTimeoutMs,omitempty"`

	Note         string `json:"note,omitempty"`
	Source       string `json:"source,omitempty"`
	RegisteredAt string `json:"registeredAt,omitempty"`
}

// rawEntry exists only to accept the two spellings of one flag: `autostart`,
// which reads better in a hand-written file, and `enabled`, which is what the
// runtime calls it. An absent flag means the session wants to be running.
type rawEntry struct {
	Entry
	EnabledPtr   *bool `json:"enabled"`
	AutostartPtr *bool `json:"autostart"`
}

// Option returns a providerOptions value as a string, and whether it was set.
func (e *Entry) Option(key string) (string, bool) {
	if e.ProviderOptions == nil {
		return "", false
	}
	v, ok := e.ProviderOptions[key]
	if !ok || v == nil {
		return "", false
	}
	if s, ok := v.(string); ok {
		return s, s != ""
	}
	return fmt.Sprint(v), true
}

// BoolOption returns a providerOptions value as a boolean.
func (e *Entry) BoolOption(key string) bool {
	if e.ProviderOptions == nil {
		return false
	}
	switch v := e.ProviderOptions[key].(type) {
	case bool:
		return v
	case string:
		return v == "true" || v == "1" || v == "yes" || v == "on"
	}
	return false
}

// Clone returns a deep copy, so a live session and the registry can never
// share the maps and slices they each mutate.
func (e *Entry) Clone() *Entry {
	if e == nil {
		return nil
	}
	out := *e
	// append to an empty slice, not to nil: an empty []string{} must not come
	// back as null, or LaunchDiff reports a change where none was made.
	if e.Args != nil {
		out.Args = append([]string{}, e.Args...)
	}
	if e.Env != nil {
		out.Env = make(map[string]string, len(e.Env))
		for k, v := range e.Env {
			out.Env[k] = v
		}
	}
	if e.ProviderOptions != nil {
		out.ProviderOptions = make(map[string]any, len(e.ProviderOptions))
		for k, v := range e.ProviderOptions {
			out.ProviderOptions[k] = v
		}
	}
	return &out
}

// Normalize validates an entry and fills in the derived fields.
func Normalize(raw *Entry, source string) (*Entry, error) {
	if raw == nil {
		return nil, rcerr.New("Session entry must be an object.", "")
	}
	if err := AssertValidName(raw.Name); err != nil {
		return nil, err
	}
	if raw.Provider == "" {
		return nil, rcerr.Newf("", "Session %q is missing \"provider\".", raw.Name)
	}
	if raw.Cwd == "" {
		return nil, rcerr.Newf("", "Session %q is missing \"cwd\".", raw.Name)
	}

	out := raw.Clone()
	out.Cwd, _ = filepath.Abs(expandHome(out.Cwd))
	if out.Args == nil {
		out.Args = []string{}
	}
	if out.Env == nil {
		out.Env = map[string]string{}
	}
	if out.ProviderOptions == nil {
		out.ProviderOptions = map[string]any{}
	}
	if source != "" && out.Source == "" {
		out.Source = source
	}
	if out.RegisteredAt == "" {
		out.RegisteredAt = time.Now().UTC().Format(time.RFC3339)
	}
	return out, nil
}

// expandHome makes "~/projects/foo" usable in a hand-written ecosystem file.
func expandHome(p string) string {
	if p == "~" || len(p) > 1 && p[:2] == "~/" {
		if home, err := os.UserHomeDir(); err == nil {
			return filepath.Join(home, p[1:])
		}
	}
	return p
}

// Load reads every registered session.
func Load() ([]*Entry, error) {
	doc := struct {
		Sessions []rawEntry `json:"sessions"`
	}{}
	_ = jsonfile.Read(paths.Get().Registry, &doc)

	out := make([]*Entry, 0, len(doc.Sessions))
	for i := range doc.Sessions {
		e := resolveEnabled(&doc.Sessions[i])
		normalized, err := Normalize(e, "")
		if err != nil {
			return nil, err
		}
		out = append(out, normalized)
	}
	return out, nil
}

func resolveEnabled(r *rawEntry) *Entry {
	e := r.Entry
	switch {
	case r.EnabledPtr != nil:
		e.Enabled = *r.EnabledPtr
	case r.AutostartPtr != nil:
		e.Enabled = *r.AutostartPtr
	default:
		e.Enabled = true
	}
	return &e
}

type document struct {
	Version   int      `json:"version"`
	UpdatedAt string   `json:"updatedAt"`
	Sessions  []*Entry `json:"sessions"`
}

// Save replaces the registry document.
func Save(entries []*Entry) error {
	if err := paths.EnsureHome(); err != nil {
		return err
	}
	if entries == nil {
		entries = []*Entry{}
	}
	return jsonfile.Write(paths.Get().Registry, document{
		Version:   1,
		UpdatedAt: time.Now().UTC().Format(time.RFC3339),
		Sessions:  entries,
	}, 0o600)
}

// Get returns one entry by name, or nil.
func Get(name string) *Entry {
	all, err := Load()
	if err != nil {
		return nil
	}
	for _, e := range all {
		if e.Name == name {
			return e
		}
	}
	return nil
}

// Upsert inserts or merge-updates by name, keeping the original
// registeredAt, and returns the stored entry.
func Upsert(entry *Entry) (*Entry, error) {
	normalized, err := Normalize(entry, "")
	if err != nil {
		return nil, err
	}
	all, err := Load()
	if err != nil {
		return nil, err
	}
	replaced := false
	for i, e := range all {
		if e.Name != normalized.Name {
			continue
		}
		merged := normalized.Clone()
		merged.RegisteredAt = e.RegisteredAt
		all[i] = merged
		replaced = true
		break
	}
	if !replaced {
		all = append(all, normalized)
	}
	if err := Save(all); err != nil {
		return nil, err
	}
	return Get(normalized.Name), nil
}

// Remove unregisters a session. The provider's underlying conversation is
// never touched.
func Remove(name string) (bool, error) {
	all, err := Load()
	if err != nil {
		return false, err
	}
	next := make([]*Entry, 0, len(all))
	for _, e := range all {
		if e.Name != name {
			next = append(next, e)
		}
	}
	if len(next) == len(all) {
		return false, nil
	}
	return true, Save(next)
}

// ParseEcosystem accepts a bare array or a {"sessions": [...]} document, so a
// file can be as short as the one session it declares.
func ParseEcosystem(data []byte, source string) ([]*Entry, error) {
	var asArray []rawEntry
	if err := json.Unmarshal(data, &asArray); err == nil && asArray != nil {
		return normalizeAll(asArray, source)
	}

	var asDoc struct {
		Sessions []rawEntry `json:"sessions"`
	}
	if err := json.Unmarshal(data, &asDoc); err != nil {
		return nil, rcerr.Newf("", "Could not parse ecosystem file %s: %v", source, err)
	}
	if asDoc.Sessions == nil {
		return nil, rcerr.Newf("", "Ecosystem file %s must contain an array, or a \"sessions\" array.", source)
	}
	list := asDoc.Sessions
	return normalizeAll(list, source)
}

func normalizeAll(list []rawEntry, source string) ([]*Entry, error) {
	out := make([]*Entry, 0, len(list))
	for i := range list {
		e, err := Normalize(resolveEnabled(&list[i]), source)
		if err != nil {
			return nil, err
		}
		out = append(out, e)
	}
	return out, nil
}

// LaunchKeys are the fields that decide *how* a session launches. A change to
// any of them means the running process no longer matches what the registry
// asks for; a change to anything else (a supervision limit, a note) can be
// applied to a live session without touching it.
var LaunchKeys = []string{"provider", "sessionId", "cwd", "args", "env", "providerOptions"}

// LaunchDiff reports which launch-affecting fields differ between two entries.
func LaunchDiff(a, b *Entry) []string {
	if a == nil || b == nil {
		return nil
	}
	var changed []string
	for _, key := range LaunchKeys {
		if encode(field(a, key)) != encode(field(b, key)) {
			changed = append(changed, key)
		}
	}
	return changed
}

func field(e *Entry, key string) any {
	switch key {
	case "provider":
		return e.Provider
	case "sessionId":
		return e.SessionID
	case "cwd":
		return e.Cwd
	case "args":
		return e.Args
	case "env":
		return e.Env
	case "providerOptions":
		return e.ProviderOptions
	}
	return nil
}

func encode(v any) string {
	b, err := json.Marshal(v)
	if err != nil {
		return fmt.Sprint(v)
	}
	return string(b)
}
