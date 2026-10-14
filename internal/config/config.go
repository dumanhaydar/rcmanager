// Package config holds rcmanager's global settings.
//
// Absent keys fall back to the defaults below, and every key here can be
// overridden per session in the registry, so a single noisy session can have a
// longer restart delay without changing the machine's policy.
package config

import (
	"bytes"
	"encoding/json"
	"fmt"
	"sort"
	"strconv"
	"strings"

	"github.com/dumanhaydar/rcmanager/internal/jsonfile"
	"github.com/dumanhaydar/rcmanager/internal/paths"
	"github.com/dumanhaydar/rcmanager/internal/rcerr"
)

// Config is the effective global configuration.
type Config struct {
	DefaultProvider string `json:"defaultProvider"`

	// Supervision, overridable per session in the ecosystem file.
	Autorestart     bool  `json:"autorestart"`
	MaxRestarts     int   `json:"maxRestarts"`
	RestartWindowMs int64 `json:"restartWindowMs"`
	RestartDelayMs  int64 `json:"restartDelayMs"`
	MinUptimeMs     int64 `json:"minUptimeMs"`
	StopTimeoutMs   int64 `json:"stopTimeoutMs"`

	MaxLogBytes     int64 `json:"maxLogBytes"`
	LogRetain       int   `json:"logRetain"`
	StripAnsiInLogs bool  `json:"stripAnsiInLogs"`

	// Resilience.
	WatchConfig     bool   `json:"watchConfig"`
	RestartOnChange bool   `json:"restartOnChange"`
	IsolateSessions string `json:"isolateSessions"` // auto | off
}

// Defaults returns the configuration rcmanager uses when nothing is set.
func Defaults() Config {
	return Config{
		DefaultProvider: "claude",

		Autorestart:     true,
		MaxRestarts:     15,
		RestartWindowMs: 60_000,
		RestartDelayMs:  2_000,
		MinUptimeMs:     5_000,
		StopTimeoutMs:   15_000,

		MaxLogBytes:     10 * 1024 * 1024,
		LogRetain:       3,
		StripAnsiInLogs: true,

		WatchConfig:     true,
		RestartOnChange: false,
		IsolateSessions: "auto",
	}
}

// Kind is the type of a settable config value.
type Kind int

const (
	KindString Kind = iota
	KindBool
	KindNumber
	KindEnum
)

// Spec describes one settable key.
type Spec struct {
	Kind   Kind
	Doc    string
	Values []string // KindEnum only
}

// Schema is every key `rcmanager config set` accepts, in display order.
var Schema = map[string]Spec{
	"defaultProvider": {KindString, "Provider used when --provider is omitted.", nil},
	"autorestart":     {KindBool, "Restart managed sessions that exit unexpectedly.", nil},
	"maxRestarts":     {KindNumber, "Max restarts inside restartWindowMs before a session is marked errored.", nil},
	"restartWindowMs": {KindNumber, "Sliding window used to detect crash loops (ms).", nil},
	"restartDelayMs":  {KindNumber, "Delay before a crashed session is restarted (ms).", nil},
	"minUptimeMs":     {KindNumber, "A session exiting sooner than this counts toward the crash-loop budget (ms).", nil},
	"stopTimeoutMs":   {KindNumber, "Grace period between SIGTERM and SIGKILL (ms).", nil},
	"maxLogBytes":     {KindNumber, "Rotate a log file once it exceeds this size (bytes).", nil},
	"logRetain":       {KindNumber, "Number of rotated log files to keep per stream.", nil},
	"stripAnsiInLogs": {KindBool, "Strip ANSI escapes before writing session logs.", nil},
	"watchConfig":     {KindBool, "Reload config.json and registry.json automatically when they change on disk.", nil},
	"restartOnChange": {KindBool, "Restart a session when an edit changes how it launches. Off: rcmanager flags it and waits.", nil},
	"isolateSessions": {KindEnum, "Run each session in its own cgroup scope so a daemon restart cannot sweep it up (systemd only).", []string{"auto", "off"}},
}

// Keys returns the schema keys in the order they are declared for display.
func Keys() []string {
	out := []string{
		"defaultProvider", "autorestart", "maxRestarts", "restartWindowMs",
		"restartDelayMs", "minUptimeMs", "stopTimeoutMs", "maxLogBytes",
		"logRetain", "stripAnsiInLogs", "watchConfig", "restartOnChange",
		"isolateSessions",
	}
	// Guard against a key being added to Schema and forgotten here.
	if len(out) != len(Schema) {
		seen := map[string]bool{}
		for _, k := range out {
			seen[k] = true
		}
		extra := []string{}
		for k := range Schema {
			if !seen[k] {
				extra = append(extra, k)
			}
		}
		sort.Strings(extra)
		out = append(out, extra...)
	}
	return out
}

// Load reads config.json over the defaults. Absent keys keep their default,
// which is the whole merge semantics in one json.Unmarshal.
func Load() Config {
	cfg := Defaults()
	_ = jsonfile.Read(paths.Get().Config, &cfg)
	if cfg.IsolateSessions == "" {
		cfg.IsolateSessions = "auto"
	}
	return cfg
}

// raw returns config.json as a map, so `set` and `unset` touch one key and
// leave everything else — including keys a newer rcmanager wrote — alone.
func raw() map[string]any {
	doc := map[string]any{}
	_ = jsonfile.Read(paths.Get().Config, &doc)
	if doc == nil {
		doc = map[string]any{}
	}
	return doc
}

func saveRaw(doc map[string]any) error {
	if err := paths.EnsureHome(); err != nil {
		return err
	}
	return jsonfile.Write(paths.Get().Config, doc, 0o600)
}

// ValueOf returns one effective setting, as it would be printed.
func ValueOf(cfg Config, key string) (any, error) {
	if _, ok := Schema[key]; !ok {
		return nil, rcerr.New(fmt.Sprintf("Unknown config key %q.", key),
			"Known keys: "+strings.Join(Keys(), ", "))
	}
	blob, err := json.Marshal(cfg)
	if err != nil {
		return nil, err
	}
	m := map[string]any{}
	dec := json.NewDecoder(bytes.NewReader(blob))
	// UseNumber keeps 10485760 printable as itself: decoded as float64 it
	// would be reported to the user as 1.048576e+07.
	dec.UseNumber()
	if err := dec.Decode(&m); err != nil {
		return nil, err
	}
	return m[key], nil
}

// Set validates and stores one setting, returning the stored value.
func Set(key, value string) (any, error) {
	spec, ok := Schema[key]
	if !ok {
		return nil, rcerr.New(fmt.Sprintf("Unknown config key %q.", key),
			"Known keys: "+strings.Join(Keys(), ", "))
	}
	coerced, err := coerce(key, value, spec)
	if err != nil {
		return nil, err
	}
	doc := raw()
	doc[key] = coerced
	if err := saveRaw(doc); err != nil {
		return nil, err
	}
	return coerced, nil
}

// Unset removes a setting, returning the default that now applies.
func Unset(key string) (any, error) {
	if _, ok := Schema[key]; !ok {
		return nil, rcerr.New(fmt.Sprintf("Unknown config key %q.", key), "")
	}
	doc := raw()
	delete(doc, key)
	if err := saveRaw(doc); err != nil {
		return nil, err
	}
	return ValueOf(Defaults(), key)
}

func coerce(key, value string, spec Spec) (any, error) {
	switch spec.Kind {
	case KindString:
		return value, nil
	case KindEnum:
		v := strings.ToLower(value)
		for _, allowed := range spec.Values {
			if v == allowed {
				return v, nil
			}
		}
		return nil, rcerr.New(fmt.Sprintf("Config %q expects one of %s, got %q.",
			key, strings.Join(spec.Values, ", "), value), "")
	case KindNumber:
		n, err := strconv.ParseFloat(value, 64)
		if err != nil {
			return nil, rcerr.New(fmt.Sprintf("Config %q expects a number, got %q.", key, value), "")
		}
		return n, nil
	case KindBool:
		switch strings.ToLower(value) {
		case "true", "1", "yes", "on":
			return true, nil
		case "false", "0", "no", "off":
			return false, nil
		}
		return nil, rcerr.New(fmt.Sprintf("Config %q expects a boolean, got %q.", key, value), "")
	}
	return value, nil
}

// IsDefault reports whether the effective value of key is still the default.
func IsDefault(cfg Config, key string) bool {
	a, _ := ValueOf(cfg, key)
	b, _ := ValueOf(Defaults(), key)
	return fmt.Sprint(a) == fmt.Sprint(b)
}

// File is where the settings live, for printing.
func File() string { return paths.Get().Config }
