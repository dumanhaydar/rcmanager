// Package provider is the adapter contract between rcmanager and one agent CLI.
//
// Adapters are the only place that knows CLI flags or on-disk session layouts;
// everything else in rcmanager is provider-agnostic. Adding a provider is one file
// plus one line in Register.
package provider

import (
	"fmt"
	"os/exec"
	"regexp"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/dumanhaydar/rcmanager/internal/rcerr"
	"github.com/dumanhaydar/rcmanager/internal/registry"
)

// Capabilities is what rcmanager may assume about a provider.
type Capabilities struct {
	// ResumeBySessionID: remote control can be bound to one specific
	// pre-existing session id.
	ResumeBySessionID bool `json:"resumeBySessionId"`
	// Headless: the managed process runs without a TTY of its own.
	Headless bool `json:"headless"`
	// Singleton: only one managed process of this provider per machine.
	Singleton bool `json:"singleton"`
	// NamedSessions: sessions carry a user-visible name discovery can match.
	NamedSessions bool `json:"namedSessions"`
	// CreateNew: a brand-new session can be created from scratch by rcmanager.
	CreateNew bool `json:"createNew"`
	// PublishesRemoteURL: a healthy session announces a remote-control URL, so
	// silence is a symptom rather than noise.
	PublishesRemoteURL bool `json:"publishesRemoteUrl"`
}

// Names lists the capabilities that are on, for display.
func (c Capabilities) Names() []string {
	var out []string
	for name, on := range map[string]bool{
		"resumeBySessionId":  c.ResumeBySessionID,
		"headless":           c.Headless,
		"singleton":          c.Singleton,
		"namedSessions":      c.NamedSessions,
		"createNew":          c.CreateNew,
		"publishesRemoteUrl": c.PublishesRemoteURL,
	} {
		if on {
			out = append(out, name)
		}
	}
	sort.Strings(out)
	return out
}

// Command is how a session launches.
//
// Signature is the list of argv substrings that must all be present in a live
// process's command line before rcmanager is willing to signal it. It is the
// interlock that stops rcmanager from ever killing an unrelated agent process.
type Command struct {
	Command   string
	Args      []string
	Cwd       string
	Env       map[string]string
	Pty       bool
	Signature []string
}

// Detection is whether a provider's CLI is usable here.
type Detection struct {
	Available bool   `json:"available"`
	Version   string `json:"version"`
	Bin       string `json:"bin"`
	Error     string `json:"error"`
}

// Discovered is a session the provider knows about locally.
type Discovered struct {
	SessionID    string `json:"sessionId"`
	Name         string `json:"name"`
	Cwd          string `json:"cwd"`
	LastActive   string `json:"lastActive"`
	LastActiveMs int64  `json:"-"`
	Detail       string `json:"detail,omitempty"`
}

// Result is a pass/fail with an actionable hint.
type Result struct {
	OK    bool
	Error string
	Hint  string
}

// Interaction is a startup modal rcmanager knows how to answer.
//
// Unattended supervision is the whole point of rcmanager, and a modal waiting for
// a keypress stalls a session forever. Providers declare exactly which prompts
// they can answer; anything else is left alone, and a prompt marked Fatal (a
// trust decision, say) is reported rather than clicked through — rcmanager must
// never accept a security prompt on the user's behalf.
type Interaction struct {
	ID    string
	Once  bool
	Match *regexp.Regexp
	// Respond returns the keystrokes to send, or "" to report the block
	// instead of guessing.
	Respond  func(e *registry.Entry) string
	Fatal    bool
	Answered string
	Describe string
	Delay    time.Duration
}

// Owner is another process already driving a session.
type Owner struct {
	Pid    int
	Argv   []string
	Reason string
}

// InstallOption is one way to install a provider CLI. rcmanager never runs one on
// its own: the command is printed and confirmed first.
type InstallOption struct {
	ID      string
	Label   string
	Note    string
	Command string
	Args    []string
	Display string
}

// Auth is whether this machine is signed in to a provider.
type Auth struct {
	// LoggedIn is nil when the provider cannot report it.
	LoggedIn *bool
	Detail   string
}

// LoginCmd is the command that signs a user in.
type LoginCmd struct {
	Command string
	Args    []string
	Display string
}

// Provider teaches rcmanager how to supervise one agent CLI.
type Provider interface {
	ID() string
	DisplayName() string
	Capabilities() Capabilities
	Limitations() []string

	// EnvDeny lists variables that must never reach a managed child. rcmanager is
	// often launched from inside an agent session, and those sessions export
	// markers that tell a nested CLI it is a child process — inherited, some of
	// them silently disable transcript saving.
	EnvDeny() []*regexp.Regexp

	Detect() Detection
	Discover(name string) []Discovered
	BuildCommand(e *registry.Entry) (Command, error)
	ExtractRemoteURL(text string) string
	Preflight(e *registry.Entry) Result
	Interactions() []Interaction
}

// Installable is implemented by providers whose CLI rcmanager can offer to install.
type Installable interface {
	InstallOptions() []InstallOption
	AuthStatus() Auth
	LoginCommand() *LoginCmd
}

// TrustKeeper is implemented by providers that keep a per-directory trust
// record. Trust is what lets an agent act on those files, so rcmanager only ever
// changes it through an explicit command.
type TrustKeeper interface {
	TrustState(cwd string) Trust
	SetTrust(cwd string, grant bool) TrustResult
	TrustPreflight(e *registry.Entry) Result
}

// Trust is a provider's recorded verdict on a directory.
type Trust struct {
	Known   bool
	Trusted bool
	File    string
}

// TrustResult is the outcome of changing trust.
type TrustResult struct {
	OK      bool
	Error   string
	Dir     string
	File    string
	Backup  string
	Trusted bool
}

// OwnerFinder is implemented by providers that can tell whether something
// outside rcmanager is already driving a session. Two writers on one transcript
// fork it, so rcmanager refuses to attach on top.
type OwnerFinder interface {
	FindExternalOwner(e *registry.Entry, excludePids []int) *Owner
}

var (
	mu        sync.RWMutex
	providers = map[string]Provider{}
	order     []string
)

// Register adds a provider. Embedders can plug in an adapter without forking
// rcmanager.
func Register(p Provider) Provider {
	mu.Lock()
	defer mu.Unlock()
	if _, exists := providers[p.ID()]; !exists {
		order = append(order, p.ID())
	}
	providers[p.ID()] = p
	return p
}

// List returns every registered provider, in registration order.
func List() []Provider {
	mu.RLock()
	defer mu.RUnlock()
	out := make([]Provider, 0, len(order))
	for _, id := range order {
		out = append(out, providers[id])
	}
	return out
}

// Get returns one provider by id.
func Get(id string) (Provider, error) {
	mu.RLock()
	defer mu.RUnlock()
	p, ok := providers[strings.ToLower(id)]
	if !ok {
		ids := make([]string, len(order))
		copy(ids, order)
		return nil, rcerr.New(fmt.Sprintf("Unknown provider %q.", id),
			"Available providers: "+strings.Join(ids, ", "))
	}
	return p, nil
}

// Has reports whether a provider id is known.
func Has(id string) bool {
	mu.RLock()
	defer mu.RUnlock()
	_, ok := providers[strings.ToLower(id)]
	return ok
}

// Probe runs a CLI and captures its output, without ever returning an error
// the caller has to care about.
type ProbeResult struct {
	OK   bool
	Out  string
	Err  string
	Code int
}

// Probe runs bin with args under a timeout.
func Probe(bin string, args []string, timeout time.Duration) ProbeResult {
	if timeout <= 0 {
		timeout = 10 * time.Second
	}
	cmd := exec.Command(bin, args...)
	var out, errOut strings.Builder
	cmd.Stdout = &out
	cmd.Stderr = &errOut

	if err := cmd.Start(); err != nil {
		return ProbeResult{Out: "", Err: err.Error(), Code: -1}
	}
	done := make(chan error, 1)
	go func() { done <- cmd.Wait() }()

	select {
	case err := <-done:
		code := cmd.ProcessState.ExitCode()
		return ProbeResult{
			OK:   err == nil,
			Out:  strings.TrimSpace(out.String()),
			Err:  strings.TrimSpace(errOut.String()),
			Code: code,
		}
	case <-time.After(timeout):
		_ = cmd.Process.Kill()
		<-done
		return ProbeResult{Out: strings.TrimSpace(out.String()), Err: "timed out", Code: -1}
	}
}

// Squash makes a name case- and separator-insensitive, so "ShopifyAppCreator"
// can find a session named "Shopify App Creator".
func Squash(s string) string {
	var b strings.Builder
	for _, r := range strings.ToLower(s) {
		if r >= 'a' && r <= 'z' || r >= '0' && r <= '9' {
			b.WriteRune(r)
		}
	}
	return b.String()
}
