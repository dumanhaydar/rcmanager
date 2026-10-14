// Package service registers rcmanager with whatever boot-time service manager a
// machine actually has, so the user does not need to know which one that is.
//
// A service adapter is the only place that knows about unit files, plists or
// crontabs. Everything an adapter needs about *this* installation comes from
// Context, so nothing about the host is hardcoded anywhere.
package service

import (
	"os"
	"os/exec"
	"os/user"
	"path/filepath"
	"sort"
	"strings"

	"github.com/dumanhaydar/rcmanager/internal/paths"
	"github.com/dumanhaydar/rcmanager/internal/rcerr"
)

// Context is everything an adapter needs to describe this installation.
type Context struct {
	Entrypoint  string // the rcmanager binary
	RcmanHome   string
	Path        string            // PATH for the service
	Env         map[string]string // pinned provider binaries
	User        string
	Home        string
	Label       string
	Description string
}

// Status is what a manager reports about rcmanager.
type Status struct {
	Installed bool   `json:"installed"`
	Running   bool   `json:"running"`
	Enabled   bool   `json:"enabled"`
	Detail    string `json:"detail"`
}

// Availability is whether a manager can be used here, and why.
type Availability struct {
	Available bool   `json:"available"`
	Reason    string `json:"reason"`
}

// Installed is the outcome of registering rcmanager.
type Installed struct {
	Path  string
	Notes []string
}

// Removed is the outcome of deregistering rcmanager.
type Removed struct {
	Removed bool
	Notes   []string
}

// Manager teaches rcmanager how to register itself for boot on one kind of
// machine.
type Manager interface {
	ID() string
	DisplayName() string
	// Priority decides which manager wins when several are available.
	Priority() int

	Detect() Availability
	UnitPath() string
	Render(ctx Context) string
	Install(ctx Context) (Installed, error)
	Uninstall() Removed
	Status() Status
	Start() Run
	Stop() Run
	LogsHint() string
}

// Run is the result of a command an adapter shelled out to.
type Run struct {
	OK  bool
	Out string
	Err string
}

// Which resolves a binary on PATH, or returns "".
func Which(bin string) string {
	path, err := exec.LookPath(bin)
	if err != nil {
		return ""
	}
	return path
}

// Exec runs a command, never returning an error the caller has to unwrap.
func Exec(cmd string, args []string, input string) Run {
	c := exec.Command(cmd, args...)
	if input != "" {
		c.Stdin = strings.NewReader(input)
	}
	var out, errOut strings.Builder
	c.Stdout = &out
	c.Stderr = &errOut
	err := c.Run()
	return Run{
		OK:  err == nil,
		Out: strings.TrimSpace(out.String()),
		Err: strings.TrimSpace(firstNonEmpty(errOut.String(), errString(err))),
	}
}

func errString(err error) string {
	if err == nil {
		return ""
	}
	return err.Error()
}

func firstNonEmpty(values ...string) string {
	for _, v := range values {
		if strings.TrimSpace(v) != "" {
			return strings.TrimSpace(v)
		}
	}
	return ""
}

// AsRoot runs a command as root, directly when already root and through
// passwordless sudo otherwise.
func AsRoot(args []string, input string) Run {
	if os.Geteuid() == 0 {
		return Exec(args[0], args[1:], input)
	}
	return Exec("sudo", append([]string{"-n"}, args...), input)
}

// ProviderBins is the list of provider CLIs a service needs to find.
var ProviderBins = []string{"claude", "codex"}

// ServicePath is the PATH for the service.
//
// A boot-time service starts with a minimal environment, so the provider CLIs
// have to be findable. This keeps the directory holding the rcmanager binary, the
// directories holding each provider binary, and the standard system paths —
// and drops the ephemeral, version-hashed entries an IDE remote server injects
// into an interactive shell, which would be dangling after its next update.
func ServicePath() string {
	home, _ := os.UserHomeDir()
	self, _ := os.Executable()

	dirs := []string{
		filepath.Dir(self),
		filepath.Join(home, ".local", "bin"),
		filepath.Join(home, "bin"),
	}
	for _, bin := range ProviderBins {
		if found := Which(bin); found != "" {
			dirs = append(dirs, filepath.Dir(found))
		}
	}
	// The package-manager prefixes, when they exist. An agent session runs
	// commands of its own — git, node, a formatter — and on a Mac those live
	// under /opt/homebrew, which no standard path list mentions; a session
	// started at boot would be missing most of the tools it had in a terminal.
	// These are fixed locations, not the version-hashed directories an IDE
	// remote server injects, so pinning them stays correct across upgrades.
	for _, dir := range []string{"/opt/homebrew/bin", "/opt/homebrew/sbin", "/home/linuxbrew/.linuxbrew/bin"} {
		if st, err := os.Stat(dir); err == nil && st.IsDir() {
			dirs = append(dirs, dir)
		}
	}
	dirs = append(dirs, "/usr/local/sbin", "/usr/local/bin", "/usr/sbin", "/usr/bin", "/sbin", "/bin")

	seen := map[string]bool{}
	var unique []string
	for _, d := range dirs {
		if d == "" || d == "." || seen[d] {
			continue
		}
		seen[d] = true
		unique = append(unique, d)
	}
	return strings.Join(unique, ":")
}

// PinnedProviderBins records each provider CLI by absolute path, as
// RCMANAGER_<ID>_BIN.
//
// PATH alone is not enough. A boot service starts with almost nothing, and a
// provider installed under a version manager (nvm, asdf) lives in a directory
// no fixed list would guess. Worse, the failure is silent until something is
// actually launched: systemd resolves executables against its own environment,
// so a bare name that worked interactively dies as "Failed to find
// executable". Recording the resolved path at install time removes the
// guessing. PATH is still set, because the agent CLI needs one for its own
// subprocesses.
func PinnedProviderBins() map[string]string {
	out := map[string]string{}
	for _, bin := range ProviderBins {
		if found := Which(bin); found != "" {
			out["RCMANAGER_"+strings.ToUpper(bin)+"_BIN"] = found
		}
	}
	return out
}

// NewContext describes this installation.
func NewContext() Context {
	home, _ := os.UserHomeDir()
	self, err := os.Executable()
	if err != nil {
		self = "rcmanager"
	}
	username := ""
	if u, err := user.Current(); err == nil {
		username = u.Username
	}
	return Context{
		Entrypoint:  self,
		RcmanHome:   paths.Get().Home,
		Path:        ServicePath(),
		Env:         PinnedProviderBins(),
		User:        username,
		Home:        home,
		Label:       "rcmanager",
		Description: "rcmanager - Remote Control Manager for AI coding-agent sessions",
	}
}

// EnvLines renders the pinned environment in a stable order, so a re-install
// does not rewrite the unit with the same values in a different sequence.
func (c Context) EnvLines(format func(k, v string) string) []string {
	keys := make([]string, 0, len(c.Env))
	for k := range c.Env {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	out := make([]string, 0, len(keys))
	for _, k := range keys {
		out = append(out, format(k, c.Env[k]))
	}
	return out
}

// managers is ordered by preference, highest priority first, so
// `rcmanager service install` works the same on a systemd server, a Mac, an Alpine
// box, or a container that only has cron.
var managers []Manager

func register(m Manager) {
	managers = append(managers, m)
	sort.SliceStable(managers, func(i, j int) bool { return managers[i].Priority() > managers[j].Priority() })
}

// List returns every manager, best first.
func List() []Manager { return managers }

// Get returns one manager by id.
func Get(id string) (Manager, error) {
	for _, m := range managers {
		if m.ID() == strings.ToLower(id) {
			return m, nil
		}
	}
	var ids []string
	for _, m := range managers {
		ids = append(ids, m.ID())
	}
	return nil, rcerr.Newf("Available: "+strings.Join(ids, ", "), "Unknown service manager %q.", id)
}

// Probe is a manager with its availability.
type Probe struct {
	Manager Manager
	Availability
}

// ProbeAll returns every manager with its availability, best first.
func ProbeAll() []Probe {
	out := make([]Probe, 0, len(managers))
	for _, m := range managers {
		out = append(out, Probe{Manager: m, Availability: m.Detect()})
	}
	return out
}

// Detect returns the best usable manager, or nil when the machine has none.
func Detect() *Probe {
	for _, p := range ProbeAll() {
		if p.Available {
			probe := p
			return &probe
		}
	}
	return nil
}
