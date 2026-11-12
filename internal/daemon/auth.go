package daemon

import (
	"fmt"
	"os"
	"os/exec"
	"sync"
	"time"

	"github.com/dumanhaydar/rcmanager/internal/provider"
	"github.com/dumanhaydar/rcmanager/internal/session"
)

// authCacheTTL bounds how often the daemon asks a provider CLI whether it is
// signed in. `start all` over a dozen sessions is one question, not twelve.
// A command the user types drops the cache first, so fixing the login and
// running `rcmanager restart all` is never refused on a stale answer.
const authCacheTTL = 30 * time.Second

// authGate remembers each provider's last login answer, and what was last
// written to the daemon log about it, so a provider that is signed out says so
// once rather than once per session per restart.
type authGate struct {
	mu       sync.Mutex
	cache    map[string]authAnswer
	reported map[string]string
}

type authAnswer struct {
	auth provider.Auth
	at   time.Time
}

func newAuthGate() *authGate {
	return &authGate{cache: map[string]authAnswer{}, reported: map[string]string{}}
}

func (g *authGate) invalidate() {
	g.mu.Lock()
	g.cache = map[string]authAnswer{}
	g.mu.Unlock()
}

// check returns the provider's login state as the daemon sees it. The lock is
// held across the probe on purpose: concurrent starts wait for one answer
// instead of each running the CLI.
func (g *authGate) check(p provider.Provider) (provider.Auth, bool) {
	inst, ok := p.(provider.Installable)
	if !ok {
		return provider.Auth{}, false
	}
	g.mu.Lock()
	defer g.mu.Unlock()
	if a, ok := g.cache[p.ID()]; ok && time.Since(a.at) < authCacheTTL {
		return a.auth, true
	}
	auth := inst.AuthStatus()
	g.cache[p.ID()] = authAnswer{auth: auth, at: time.Now()}
	return auth, true
}

// changed reports whether state differs from what was last logged for this
// provider, and records it.
func (g *authGate) changed(id, state string) bool {
	g.mu.Lock()
	defer g.mu.Unlock()
	if g.reported[id] == state {
		return false
	}
	g.reported[id] = state
	return true
}

// checkAuth asks the provider, writes a line to the daemon log when the answer
// is new, and returns it. Only Auth.Summary is ever logged: whether, how and
// until when — never the account, never a token.
func (d *Daemon) checkAuth(p provider.Provider) (provider.Auth, bool) {
	auth, ok := d.auth.check(p)
	if !ok {
		return auth, false
	}
	state := auth.Summary()
	if !d.auth.changed(p.ID(), state) {
		return auth, true
	}
	switch {
	case auth.LoggedIn == nil:
		d.line(fmt.Sprintf("%s: could not read its login status from the daemon; starting sessions anyway", p.ID()))
	case *auth.LoggedIn:
		d.line(fmt.Sprintf("%s: signed in for the daemon (%s)", p.ID(), state))
	default:
		d.line(fmt.Sprintf("ERROR %s: NOT signed in for the daemon (%s) — its sessions will not be started until it is. "+
			"It may still be signed in from your terminal: run `rcmanager doctor`", p.ID(), state))
	}
	return auth, true
}

// authVeto refuses to start a session whose provider is not signed in where
// the daemon runs it. Such a session comes up, draws its UI, says "Not logged
// in", and is never reachable — and rcmanager used to call that online.
//
// Only a definite "not signed in" refuses. A status command that failed says
// nothing either way, and blocking on it would turn a slow CLI into an outage.
func (d *Daemon) authVeto(s *session.Managed) *session.Veto {
	auth, ok := d.checkAuth(s.Provider())
	if !ok || auth.LoggedIn == nil || *auth.LoggedIn {
		return nil
	}
	return &session.Veto{
		Status: session.AuthRequired,
		Error: fmt.Sprintf("%s is not signed in where rcmanager runs it (%s), so the session was not started — run `rcmanager doctor`",
			s.Provider().DisplayName(), auth.Summary()),
		Hint: "Diagnose with:  rcmanager doctor",
	}
}

// canStart is every veto the daemon holds over a start, in order.
func (d *Daemon) canStart(s *session.Managed) *session.Veto {
	if v := d.singletonVeto(s); v != nil {
		return v
	}
	return d.authVeto(s)
}

// AuthReport is one provider's login state as seen from one context, for
// `rcmanager doctor`. It carries no account details.
type AuthReport struct {
	Provider  string `json:"provider"`
	Known     bool   `json:"known"`
	LoggedIn  bool   `json:"loggedIn"`
	Method    string `json:"authMethod,omitempty"`
	ExpiresAt string `json:"expiresAt,omitempty"`
	Bin       string `json:"bin,omitempty"`
	Version   string `json:"version,omitempty"`
}

// DaemonContext is the environment the daemon runs provider CLIs in. The
// whole point of `doctor` is that this can differ from the user's shell.
type DaemonContext struct {
	Pid       int          `json:"pid"`
	Home      string       `json:"home"`
	User      string       `json:"user"`
	Path      string       `json:"path"`
	ConfigDir string       `json:"claudeConfigDir,omitempty"`
	Providers []AuthReport `json:"providers"`
}

// CurrentContext describes the calling process's own environment.
func CurrentContext() DaemonContext {
	return DaemonContext{
		Pid:       os.Getpid(),
		Home:      os.Getenv("HOME"),
		User:      os.Getenv("USER"),
		Path:      os.Getenv("PATH"),
		ConfigDir: os.Getenv("CLAUDE_CONFIG_DIR"),
	}
}

// ReportAuth asks every installed provider whether it is signed in, from the
// calling process's own context — the daemon's, or the shell's for the CLI.
func ReportAuth() []AuthReport {
	var out []AuthReport
	for _, p := range provider.List() {
		if r, ok := reportOne(p, nil); ok {
			out = append(out, r)
		}
	}
	return out
}

func reportOne(p provider.Provider, ask func(provider.Provider) (provider.Auth, bool)) (AuthReport, bool) {
	det := p.Detect()
	if !det.Available {
		return AuthReport{}, false
	}
	var auth provider.Auth
	if ask != nil {
		a, ok := ask(p)
		if !ok {
			return AuthReport{}, false
		}
		auth = a
	} else {
		inst, ok := p.(provider.Installable)
		if !ok {
			return AuthReport{}, false
		}
		auth = inst.AuthStatus()
	}
	r := AuthReport{Provider: p.ID(), Method: auth.Method, ExpiresAt: auth.ExpiresAt, Bin: det.Bin, Version: det.Version}
	// Two contexts with different PATHs can run two different installs, which
	// looks exactly like a login problem from the outside.
	if full, err := exec.LookPath(det.Bin); err == nil {
		r.Bin = full
	}
	if auth.LoggedIn != nil {
		r.Known, r.LoggedIn = true, *auth.LoggedIn
	}
	return r, true
}

// authReport asks every provider afresh from the daemon's context, and lets
// the daemon log record the answer as a start would.
func (d *Daemon) authReport() DaemonContext {
	d.auth.invalidate()
	out := CurrentContext()
	for _, p := range provider.List() {
		if r, ok := reportOne(p, d.checkAuth); ok {
			out.Providers = append(out.Providers, r)
		}
	}
	return out
}
