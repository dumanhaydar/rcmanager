// Package codex teaches rcmanager how to supervise the OpenAI Codex CLI.
//
// Verified against codex-cli 0.148.0-alpha.2.
//
// Codex's remote control is architecturally different from Claude's. There is
// no per-session remote-control process. Instead there is ONE machine-wide
// app-server daemon:
//
//	codex remote-control start | stop | pair
//	codex app-server daemon start | stop | restart
//	                 | enable-remote-control | disable-remote-control | version
//
// The daemon self-backgrounds (double-fork) and records its pid in
// ~/.codex/app-server-daemon/app-server.pid, with a control socket at
// ~/.codex/app-server-control/app-server-control.sock. Threads are then
// created and attached *through* that daemon from the Codex app — the CLI
// cannot bind remote control to one specific thread id.
//
// `codex resume <id>` does exist and does take a session id or name, but it is
// the interactive TUI: it needs a real terminal and is not a remote-control
// surface, so rcmanager cannot supervise it headlessly.
//
// Consequently this adapter models Codex as a SINGLETON session whose managed
// child is rcmanager itself, re-executed as a small supervisor (see the
// codex-supervisor subcommand) that keeps the machine-wide daemon alive with
// remote control enabled, and dies with it. Thread discovery is still
// implemented, because `rcmanager info` reporting which thread a name refers to is
// useful even though rcmanager cannot bind to it.
package codex

import (
	"encoding/json"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"time"

	"github.com/dumanhaydar/rcmanager/internal/provider"
	"github.com/dumanhaydar/rcmanager/internal/registry"
)

// Provider is the Codex CLI adapter.
type Provider struct{}

func init() { provider.Register(Provider{}) }

// Bin is the codex executable rcmanager drives.
func Bin() string {
	if b := os.Getenv("RCMANAGER_CODEX_BIN"); b != "" {
		return b
	}
	return "codex"
}

// Home is Codex's state directory.
func Home() string {
	if h := os.Getenv("CODEX_HOME"); h != "" {
		return h
	}
	home, _ := os.UserHomeDir()
	return filepath.Join(home, ".codex")
}

// Paths are the Codex files rcmanager reads.
type Paths struct {
	Home        string
	PidFile     string
	Settings    string
	ControlSock string
	Index       string
	Sessions    string
}

// FilePaths resolves Codex's own layout.
func FilePaths() Paths {
	home := Home()
	return Paths{
		Home:        home,
		PidFile:     filepath.Join(home, "app-server-daemon", "app-server.pid"),
		Settings:    filepath.Join(home, "app-server-daemon", "settings.json"),
		ControlSock: filepath.Join(home, "app-server-control", "app-server-control.sock"),
		Index:       filepath.Join(home, "session_index.jsonl"),
		Sessions:    filepath.Join(home, "sessions"),
	}
}

func (Provider) ID() string          { return "codex" }
func (Provider) DisplayName() string { return "OpenAI Codex CLI" }

func (Provider) Capabilities() provider.Capabilities {
	return provider.Capabilities{
		ResumeBySessionID: false,
		Headless:          true,
		Singleton:         true,
		NamedSessions:     true,
		CreateNew:         false,
	}
}

func (Provider) Limitations() []string {
	return []string{
		"Codex remote control is a single machine-wide app-server daemon, not a per-session process. rcmanager can therefore supervise exactly one Codex session per machine; a second one is refused.",
		"There is no `--session-id` equivalent: remote control cannot be bound to one specific thread. Threads are created and attached from the Codex app through the daemon.",
		"`codex resume <id|name>` resumes a specific thread, but it is the interactive TUI and requires a real terminal, so rcmanager cannot supervise it headlessly (rcmanager does not use tmux).",
		"The daemon self-backgrounds, so rcmanager supervises it indirectly: the managed child keeps the daemon alive and healthy, and stops it on `rcmanager stop`.",
		"A session `cwd` is recorded per thread, but the daemon itself is not directory-scoped — the cwd registered with rcmanager only sets where the supervisor runs.",
		"Pairing is interactive: run `codex remote-control pair` once to link the machine to your account.",
	}
}

// EnvDeny keeps session-scoped markers from a parent agent process out.
func (Provider) EnvDeny() []*regexp.Regexp {
	return []*regexp.Regexp{
		regexp.MustCompile(`^CLAUDECODE$`),
		regexp.MustCompile(`^CLAUDE_CODE_`),
		regexp.MustCompile(`^CODEX_(SESSION|THREAD|TURN)_ID$`),
	}
}

func (Provider) InstallOptions() []provider.InstallOption {
	return []provider.InstallOption{{
		ID:      "npm",
		Label:   "npm (global)",
		Command: "npm",
		Args:    []string{"install", "-g", "@openai/codex"},
		Display: "npm install -g @openai/codex",
	}}
}

func (Provider) AuthStatus() provider.Auth {
	r := provider.Probe(Bin(), []string{"login", "status"}, 20*time.Second)
	text := r.Out + r.Err
	no, yes := false, true
	if regexp.MustCompile(`(?i)not logged in|no credentials|logged out`).MatchString(text) {
		return provider.Auth{LoggedIn: &no, Detail: "not signed in"}
	}
	if r.OK {
		detail := strings.SplitN(r.Out, "\n", 2)[0]
		if detail == "" {
			detail = "signed in"
		}
		return provider.Auth{LoggedIn: &yes, Detail: detail}
	}
	detail := r.Err
	if detail == "" {
		detail = "could not determine login status"
	}
	return provider.Auth{LoggedIn: &no, Detail: detail}
}

func (Provider) LoginCommand() *provider.LoginCmd {
	return &provider.LoginCmd{Command: Bin(), Args: []string{"login"}, Display: "codex login"}
}

func (Provider) Detect() provider.Detection {
	r := provider.Probe(Bin(), []string{"--version"}, 10*time.Second)
	d := provider.Detection{Available: r.OK, Bin: Bin()}
	if r.OK {
		d.Version = strings.TrimSpace(strings.SplitN(r.Out, "\n", 2)[0])
	} else {
		d.Error = r.Err
		if d.Error == "" {
			d.Error = "codex CLI not found on PATH"
		}
	}
	return d
}

type threadMeta struct {
	Name      string
	UpdatedAt string
}

// readThreadIndex reads named threads from Codex's own index.
func readThreadIndex() map[string]threadMeta {
	out := map[string]threadMeta{}
	raw, err := os.ReadFile(FilePaths().Index)
	if err != nil {
		return out
	}
	for _, line := range strings.Split(string(raw), "\n") {
		if strings.TrimSpace(line) == "" {
			continue
		}
		var rec struct {
			ID         string `json:"id"`
			ThreadName string `json:"thread_name"`
			UpdatedAt  string `json:"updated_at"`
		}
		if err := json.Unmarshal([]byte(line), &rec); err != nil || rec.ID == "" {
			continue
		}
		out[rec.ID] = threadMeta{Name: rec.ThreadName, UpdatedAt: rec.UpdatedAt}
	}
	return out
}

type rollout struct {
	SessionID    string
	Cwd          string
	LastActive   string
	LastActiveMs int64
}

// listRollouts walks ~/.codex/sessions/YYYY/MM/DD/rollout-*.jsonl and reads
// each header record.
func listRollouts() []rollout {
	root := FilePaths().Sessions
	var files []struct {
		path  string
		mtime time.Time
	}

	var walk func(dir string, depth int)
	walk = func(dir string, depth int) {
		entries, err := os.ReadDir(dir)
		if err != nil {
			return
		}
		for _, e := range entries {
			full := filepath.Join(dir, e.Name())
			switch {
			case e.IsDir() && depth < 3:
				walk(full, depth+1)
			case !e.IsDir() && strings.HasPrefix(e.Name(), "rollout-") && strings.HasSuffix(e.Name(), ".jsonl"):
				if st, err := os.Stat(full); err == nil {
					files = append(files, struct {
						path  string
						mtime time.Time
					}{full, st.ModTime()})
				}
			}
		}
	}
	walk(root, 0)

	var out []rollout
	for _, f := range files {
		fd, err := os.Open(f.path)
		if err != nil {
			continue
		}
		buf := make([]byte, 16*1024)
		n, _ := fd.Read(buf)
		_ = fd.Close()
		if n <= 0 {
			continue
		}
		first := strings.SplitN(string(buf[:n]), "\n", 2)[0]
		var rec struct {
			Payload struct {
				SessionID string `json:"session_id"`
				Cwd       string `json:"cwd"`
			} `json:"payload"`
		}
		if err := json.Unmarshal([]byte(first), &rec); err != nil || rec.Payload.SessionID == "" {
			continue
		}
		out = append(out, rollout{
			SessionID:    rec.Payload.SessionID,
			Cwd:          rec.Payload.Cwd,
			LastActive:   f.mtime.UTC().Format(time.RFC3339),
			LastActiveMs: f.mtime.UnixMilli(),
		})
	}
	return out
}

// Discover resolves a thread name to its id and cwd from Codex's own index
// plus the rollout headers.
func (Provider) Discover(name string) []provider.Discovered {
	index := readThreadIndex()
	byID := map[string]rollout{}
	for _, r := range listRollouts() {
		byID[r.SessionID] = r
	}

	merged := make([]provider.Discovered, 0, len(index))
	for id, meta := range index {
		r := byID[id]
		lastActive := meta.UpdatedAt
		var lastMs int64
		if lastActive != "" {
			if t, err := time.Parse(time.RFC3339, lastActive); err == nil {
				lastMs = t.UnixMilli()
			}
		}
		if lastActive == "" {
			lastActive = r.LastActive
			lastMs = r.LastActiveMs
		}
		merged = append(merged, provider.Discovered{
			SessionID:    id,
			Name:         meta.Name,
			Cwd:          r.Cwd,
			LastActive:   lastActive,
			LastActiveMs: lastMs,
			Detail:       "thread (remote control cannot bind to a specific thread)",
		})
	}
	sort.SliceStable(merged, func(i, j int) bool { return merged[i].LastActiveMs > merged[j].LastActiveMs })

	if name == "" {
		return merged
	}
	target := provider.Squash(name)
	out := merged[:0:0]
	for _, s := range merged {
		if s.Name != "" && provider.Squash(s.Name) == target {
			out = append(out, s)
		}
	}
	return out
}

// BuildCommand re-executes rcmanager as the Codex supervisor: a foreground
// process that owns the machine-wide daemon's lifecycle, so rcmanager has a real
// child to watch.
func (Provider) BuildCommand(e *registry.Entry) (provider.Command, error) {
	self, err := os.Executable()
	if err != nil {
		self = "rcmanager"
	}
	env := map[string]string{}
	for k, v := range e.Env {
		env[k] = v
	}
	return provider.Command{
		Command:   self,
		Args:      []string{"codex-supervisor", "--bin", Bin(), "--name", e.Name},
		Cwd:       e.Cwd,
		Env:       env,
		Signature: []string{"codex-supervisor", e.Name},
	}, nil
}

func (Provider) Preflight(e *registry.Entry) provider.Result {
	if st, err := os.Stat(e.Cwd); err != nil || !st.IsDir() {
		return provider.Result{Error: "Directory does not exist: " + e.Cwd}
	}
	return provider.Result{OK: true}
}

func (Provider) Interactions() []provider.Interaction { return nil }

var urlRe = regexp.MustCompile(`https://(?:chatgpt\.com|codex\.[^\s/]+)/[^\s"']+`)

func (Provider) ExtractRemoteURL(text string) string {
	all := urlRe.FindAllString(text, -1)
	if len(all) == 0 {
		return ""
	}
	return all[len(all)-1]
}

var (
	_ provider.Provider    = Provider{}
	_ provider.Installable = Provider{}
)
