// Package claude teaches rcmanager how to supervise Claude Code sessions.
//
// Verified against Claude Code 2.1.252.
//
// Remote control comes in two shapes, and the distinction is the whole reason
// this file is careful:
//
//	claude remote-control --name <name>   an *environment*: one persistent
//	                                      server hosting up to --capacity
//	                                      conversations, publishing
//	                                      claude.ai/code?environment=env_…
//	claude --remote-control               one *session*, publishing
//	                                      claude.ai/code/session_…
//
// rcmanager supervises sessions, so it wants the flag, not the subcommand.
//
// Session metadata lives in ~/.claude/projects/<slugified-cwd>/<uuid>.jsonl.
// The head of each file carries the records rcmanager needs:
//
//	{"type":"custom-title","customTitle":"..."}   display name
//	{"type":"agent-name","agentName":"..."}       display name (alias)
//	{...,"cwd":"/abs/path",...}                   authoritative cwd
//
// and the file's mtime is the last-active time.
package claude

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"time"

	"github.com/dumanhaydar/rcmanager/internal/jsonfile"
	"github.com/dumanhaydar/rcmanager/internal/procs"
	"github.com/dumanhaydar/rcmanager/internal/provider"
	"github.com/dumanhaydar/rcmanager/internal/registry"
)

// Provider is the Claude Code adapter.
type Provider struct{}

func init() { provider.Register(Provider{}) }

func bin() string {
	if b := os.Getenv("RCMANAGER_CLAUDE_BIN"); b != "" {
		return b
	}
	return "claude"
}

// sessionURL is the canonical url for a session id, rebuilt rather than
// scraped — see ExtractRemoteURL.
func sessionURL(id string) string { return "https://claude.ai/code/" + id }

func projectsDir() string {
	if dir := os.Getenv("CLAUDE_CONFIG_DIR"); dir != "" {
		return filepath.Join(dir, "projects")
	}
	home, _ := os.UserHomeDir()
	return filepath.Join(home, ".claude", "projects")
}

// configFile is Claude's own settings file, where per-directory trust lives.
func configFile() string {
	if dir := os.Getenv("CLAUDE_CONFIG_DIR"); dir != "" {
		return filepath.Join(dir, ".claude.json")
	}
	home, _ := os.UserHomeDir()
	return filepath.Join(home, ".claude.json")
}

func (Provider) ID() string          { return "claude" }
func (Provider) DisplayName() string { return "Claude Code" }

func (Provider) Capabilities() provider.Capabilities {
	return provider.Capabilities{
		ResumeBySessionID: true,
		Headless:          true,
		Singleton:         false,
		NamedSessions:     true,
		CreateNew:         true,
		// A working session always announces a claude.ai/code URL. Its absence
		// is therefore evidence, not noise — see the remote-url watchdog in
		// package session.
		PublishesRemoteURL: true,
	}
}

func (Provider) Limitations() []string {
	return []string{
		"`claude remote-control` (the subcommand) is an *environment*, not a session: one persistent server hosting up to --capacity conversations created on demand, publishing a `claude.ai/code?environment=env_…` url. rcmanager supervises sessions, so it uses `claude --remote-control` (the flag on the main command) instead, which is one session publishing `claude.ai/code/session_…`. The environment is available as providerOptions.attach = \"environment\" for anyone who wants one.",
		"`claude remote-control --session-id <id>` only reattaches a *bridge* session (a cse_…/session_… id that already has an environment). Given a local transcript uuid it fails with \"has no environment_id\", so it is not the way to resume an ordinary local session. rcmanager exposes it as providerOptions.attach = \"bridge\".",
		"A new session gets its uuid assigned by rcmanager up front (`--session-id`), so every later start resumes that same conversation rather than beginning a new one.",
		"Binding remote control to one exact local session therefore uses `claude --resume <uuid> --remote-control`, which is the interactive UI and refuses to run on a pipe. rcmanager allocates a pty with script(1) instead of using tmux.",
		"Resuming a large or old session shows a blocking \"resume from summary / full\" modal. rcmanager answers it automatically (summary by default; set providerOptions.resumeMode = \"full\" to change that).",
		"The workspace must be trusted before a session can run there. `claude remote-control` hard-errors on an untrusted directory rather than prompting, and the resume path shows a modal that stalls forever, so rcmanager checks ~/.claude.json up front and refuses to start with a fix. Grant it with `rcmanager trust <path>`; it is never granted implicitly by starting a session.",
		"Requires a logged-in Claude account with a subscription (`claude auth`).",
		"Reattaching mints a fresh bridge, so the claude.ai/code URL changes on every restart; read the current one from `rcmanager info <name>`.",
		"Discovery matches the display name recorded by `-n` / `--remote-control <name>` (exact, then case-insensitive, then ignoring separators). A session that was never named cannot be found by name.",
	}
}

// EnvDeny lists variables that must never reach a managed child.
//
// CLAUDE_CODE_CHILD_SESSION in particular marks the process as a nested child
// and disables transcript saving — inheriting it would silently stop the
// resumed conversation from being written to disk. The messaging socket,
// bridge id and sse port are equally session-scoped. ANTHROPIC_* and
// CLAUDE_CONFIG_DIR are deliberately kept: those are real configuration.
func (Provider) EnvDeny() []*regexp.Regexp {
	return []*regexp.Regexp{
		regexp.MustCompile(`^CLAUDECODE$`),
		regexp.MustCompile(`^CLAUDE_PID$`),
		regexp.MustCompile(`^CLAUDE_EFFORT$`),
		regexp.MustCompile(`^CLAUDE_CODE_(CHILD_SESSION|SESSION_ID|BRIDGE_SESSION_ID|ENTRYPOINT|EXECPATH|SSE_PORT|MESSAGING_SOCKET|MESSAGING_TOKEN)$`),
	}
}

func (Provider) Detect() provider.Detection {
	r := provider.Probe(bin(), []string{"--version"}, 10*time.Second)
	d := provider.Detection{Available: r.OK, Bin: bin()}
	if r.OK {
		d.Version = strings.TrimSpace(strings.SplitN(r.Out, "\n", 2)[0])
	} else {
		d.Error = firstNonEmpty(r.Err, r.Out, "claude CLI not found on PATH")
	}
	return d
}

// InstallOptions lists ways to install the CLI, most self-contained first.
//
// rcmanager never runs one of these on its own — the command is printed and
// confirmed first. A pipe-to-shell installer in particular is something you
// should read before agreeing to.
func (Provider) InstallOptions() []provider.InstallOption {
	return []provider.InstallOption{
		{
			ID:      "native",
			Label:   "Official installer",
			Note:    "no Node required; installs to ~/.local/bin",
			Command: "sh",
			Args:    []string{"-c", "curl -fsSL https://claude.ai/install.sh | bash"},
			Display: "curl -fsSL https://claude.ai/install.sh | bash",
		},
		{
			ID:      "npm",
			Label:   "npm (global)",
			Note:    "if you would rather manage it with npm",
			Command: "npm",
			Args:    []string{"install", "-g", "@anthropic-ai/claude-code"},
			Display: "npm install -g @anthropic-ai/claude-code",
		},
	}
}

// AuthStatus reads whether this machine is signed in.
//
// `claude auth status` reports JSON, so this reads a fact rather than scraping
// prose that changes between releases. A session that starts without a login
// gets as far as a login prompt nobody can answer, so it is worth knowing
// before rather than after.
func (Provider) AuthStatus() provider.Auth {
	r := provider.Probe(bin(), []string{"auth", "status"}, 20*time.Second)
	no := false
	if !r.OK && r.Out == "" {
		return provider.Auth{LoggedIn: &no, Detail: firstNonEmpty(r.Err, "could not run `claude auth status`")}
	}

	var doc struct {
		LoggedIn   bool   `json:"loggedIn"`
		Email      string `json:"email"`
		AuthMethod string `json:"authMethod"`
	}
	if err := json.Unmarshal([]byte(r.Out), &doc); err == nil {
		in := doc.LoggedIn
		detail := "not signed in"
		if in {
			parts := []string{}
			for _, p := range []string{doc.Email, doc.AuthMethod} {
				if p != "" {
					parts = append(parts, p)
				}
			}
			detail = strings.Join(parts, " · ")
		}
		return provider.Auth{LoggedIn: &in, Detail: detail}
	}

	// Older builds printed prose. Fall back to the shape of it.
	in := regexp.MustCompile(`(?i)logged\s*in|signed\s*in`).MatchString(r.Out)
	return provider.Auth{LoggedIn: &in, Detail: strings.SplitN(r.Out, "\n", 2)[0]}
}

func (Provider) LoginCommand() *provider.LoginCmd {
	return &provider.LoginCmd{Command: bin(), Args: []string{"auth", "login"}, Display: "claude auth login"}
}

type localSession struct {
	SessionID    string
	Name         string
	Cwd          string
	LastActive   string
	LastActiveMs int64
	Bytes        int64
	File         string
}

// readHead reads the first n bytes of a session file, so a multi-hundred-MB
// transcript is never slurped to find its title.
func readHead(file string, n int) string {
	fd, err := os.Open(file)
	if err != nil {
		return ""
	}
	defer fd.Close()
	buf := make([]byte, n)
	read, err := fd.Read(buf)
	if read <= 0 && err != nil {
		return ""
	}
	return string(buf[:read])
}

var headChunks = []int{64 * 1024, 512 * 1024}

func readSessionMeta(file string) (name, cwd string) {
	for _, size := range headChunks {
		head := readHead(file, size)
		// Drop the trailing partial line so the decoder never sees a truncated
		// record.
		lines := strings.Split(head, "\n")
		if len(lines) > 0 {
			lines = lines[:len(lines)-1]
		}
		for _, line := range lines {
			if line == "" || line[0] != '{' {
				continue
			}
			var rec struct {
				Type        string `json:"type"`
				CustomTitle string `json:"customTitle"`
				AgentName   string `json:"agentName"`
				Cwd         string `json:"cwd"`
			}
			if err := json.Unmarshal([]byte(line), &rec); err != nil {
				continue
			}
			if name == "" {
				if rec.Type == "custom-title" && rec.CustomTitle != "" {
					name = rec.CustomTitle
				} else if rec.Type == "agent-name" && rec.AgentName != "" {
					name = rec.AgentName
				}
			}
			if cwd == "" && rec.Cwd != "" {
				cwd = rec.Cwd
			}
			if name != "" && cwd != "" {
				return name, cwd
			}
		}
		if len(head) < size {
			break // whole file already read
		}
	}
	return name, cwd
}

// listSessions enumerates local Claude sessions from Claude's own metadata
// only. Project directories are never searched — this reads
// ~/.claude/projects and nothing inside the user's repositories.
func listSessions() []localSession {
	root := projectsDir()
	projects, err := os.ReadDir(root)
	if err != nil {
		return nil
	}

	var out []localSession
	for _, p := range projects {
		if !p.IsDir() {
			continue
		}
		dir := filepath.Join(root, p.Name())
		entries, err := os.ReadDir(dir)
		if err != nil {
			continue
		}
		for _, f := range entries {
			if !strings.HasSuffix(f.Name(), ".jsonl") {
				continue
			}
			full := filepath.Join(dir, f.Name())
			st, err := os.Stat(full)
			if err != nil || !st.Mode().IsRegular() || st.Size() == 0 {
				continue
			}
			name, cwd := readSessionMeta(full)
			out = append(out, localSession{
				SessionID:    strings.TrimSuffix(f.Name(), ".jsonl"),
				Name:         name,
				Cwd:          cwd,
				LastActive:   st.ModTime().UTC().Format(time.RFC3339),
				LastActiveMs: st.ModTime().UnixMilli(),
				Bytes:        st.Size(),
				File:         full,
			})
		}
	}
	return out
}

// Discover finds local sessions whose recorded display name matches, in three
// tiers: exact, case-insensitive, then squashed (case- and
// separator-insensitive). The first tier that matches anything wins; within a
// tier, the most-recently-active session comes first.
func (Provider) Discover(name string) []provider.Discovered {
	all := listSessions()
	sort.SliceStable(all, func(i, j int) bool { return all[i].LastActiveMs > all[j].LastActiveMs })

	if name == "" {
		out := make([]provider.Discovered, 0, len(all))
		for _, s := range all {
			out = append(out, toDiscovered(s))
		}
		return out
	}

	lower := strings.ToLower(name)
	target := provider.Squash(name)

	var exact, loose, fuzzy []localSession
	for _, s := range all {
		if s.Name == name {
			exact = append(exact, s)
		}
		if s.Name != "" && strings.ToLower(s.Name) == lower {
			loose = append(loose, s)
		}
		if s.Name != "" && provider.Squash(s.Name) == target {
			fuzzy = append(fuzzy, s)
		}
	}
	pool := exact
	if len(pool) == 0 {
		pool = loose
	}
	if len(pool) == 0 {
		pool = fuzzy
	}

	out := make([]provider.Discovered, 0, len(pool))
	for _, s := range pool {
		// A session with no recorded cwd cannot be resumed safely.
		if s.Cwd == "" {
			continue
		}
		out = append(out, toDiscovered(s))
	}
	return out
}

func toDiscovered(s localSession) provider.Discovered {
	return provider.Discovered{
		SessionID:    s.SessionID,
		Name:         s.Name,
		Cwd:          s.Cwd,
		LastActive:   s.LastActive,
		LastActiveMs: s.LastActiveMs,
		Detail:       fmt.Sprintf("%.1f MB transcript", float64(s.Bytes)/1024/1024),
	}
}

// attachMode decides which mechanism binds remote control for this entry.
func (p Provider) attachMode(e *registry.Entry) string {
	if v, ok := e.Option("attach"); ok {
		return v
	}
	if e.SessionID == "" {
		return "create"
	}
	// A recorded id whose transcript does not exist yet is one rcmanager minted and
	// has not started: create it rather than fail to resume it.
	if p.sessionExists(e.SessionID) {
		return "resume"
	}
	return "create"
}

func (Provider) sessionExists(sessionID string) bool {
	root := projectsDir()
	dirs, err := os.ReadDir(root)
	if err != nil {
		return false
	}
	for _, d := range dirs {
		if _, err := os.Stat(filepath.Join(root, d.Name(), sessionID+".jsonl")); err == nil {
			return true
		}
	}
	return false
}

func (p Provider) BuildCommand(e *registry.Entry) (provider.Command, error) {
	attach := p.attachMode(e)
	env := map[string]string{}
	for k, v := range e.Env {
		env[k] = v
	}

	switch attach {
	// --- reattach one exact existing local session ------------------------
	// `claude remote-control --session-id` only accepts a *bridge* id
	// (cse_…/session_…) that already has an environment; handed a local
	// transcript uuid it fails with "has no environment_id". The mechanism
	// that binds remote control to a specific local session is the resume flag
	// pair, which is interactive and therefore needs a pty.
	case "resume":
		args := []string{"--resume", e.SessionID, "--remote-control"}
		args = appendOption(args, e, "permissionMode", "--permission-mode")
		args = appendOption(args, e, "model", "--model")
		args = append(args, e.Args...)
		return provider.Command{
			Command:   bin(),
			Args:      args,
			Cwd:       e.Cwd,
			Env:       env,
			Pty:       true,
			Signature: []string{"claude", e.SessionID, "remote-control"},
		}, nil

	// --- reattach a remote-control bridge session --------------------------
	case "bridge":
		id := e.SessionID
		if v, ok := e.Option("bridgeSessionId"); ok {
			id = v
		}
		args := append([]string{"remote-control", "--session-id", id}, e.Args...)
		return provider.Command{
			Command:   bin(),
			Args:      args,
			Cwd:       e.Cwd,
			Env:       env,
			Signature: []string{"remote-control", id},
		}, nil

	// --- an environment: a server hosting many sessions ---------------------
	// Explicit opt-in only. It is a different thing from a session — one
	// process, up to --capacity conversations created on demand from the app,
	// and a `?environment=env_…` url. rcmanager cannot supervise those
	// individually, which is why it is not the default.
	case "environment":
		args := []string{"remote-control", "--name", e.Name}
		args = appendOption(args, e, "spawn", "--spawn")
		args = appendOption(args, e, "capacity", "--capacity")
		args = appendOption(args, e, "permissionMode", "--permission-mode")
		args = append(args, e.Args...)
		return provider.Command{
			Command:   bin(),
			Args:      args,
			Cwd:       e.Cwd,
			Env:       env,
			Signature: []string{"remote-control", e.Name},
		}, nil
	}

	// --- create one new session, under remote control -----------------------
	if e.SessionID == "" {
		return provider.Command{}, errors.New("session \"" + e.Name + "\" has no session id to create; " +
			"this is assigned at registration — re-register it with `rcmanager start`")
	}

	// `--session-id` fixes the id up front so the very same conversation is
	// resumed on every later start, instead of a fresh one each time. The flag
	// form is interactive, so it needs a pty.
	args := []string{"--session-id", e.SessionID, "--remote-control", e.Name}
	args = appendOption(args, e, "permissionMode", "--permission-mode")
	args = appendOption(args, e, "model", "--model")
	args = append(args, e.Args...)
	return provider.Command{
		Command:   bin(),
		Args:      args,
		Cwd:       e.Cwd,
		Env:       env,
		Pty:       true,
		Signature: []string{"claude", e.SessionID, "remote-control"},
	}, nil
}

func appendOption(args []string, e *registry.Entry, option, flag string) []string {
	if v, ok := e.Option(option); ok && v != "" {
		return append(args, flag, v)
	}
	return args
}

// Interactions are the startup modals that would otherwise stall an
// unattended session forever.
//
// Patterns are written WITHOUT whitespace: the TUI paints with absolute cursor
// positioning, so the words arrive glued together (see the match
// normalisation in package session).
//
// The list is built once. It is consulted on every chunk of a session's
// output, and compiling four patterns per chunk showed up in the supervisor's
// profile for no reason at all.
var interactions = []provider.Interaction{
	{
		ID:    "resume-prompt",
		Once:  true,
		Match: regexp.MustCompile(`(?i)resumefromsummary|resumefullsessionas-is`),
		// resumeMode picks the answer: "summary" (default, far cheaper on
		// usage limits) or "full".
		Respond: func(e *registry.Entry) string {
			if v, _ := e.Option("resumeMode"); v == "full" {
				return "2"
			}
			return "1"
		},
		Answered: "answered the resume summary/full prompt",
		Describe: "the resume summary/full prompt is unanswered",
		Delay:    1200 * time.Millisecond,
	},
	{
		// A brand-new remote-control session asks this before it publishes
		// anything. Unanswered, the process sits at the prompt: alive, so
		// rcmanager called it online, but with remote control never switched on
		// and no URL ever printed — invisible in the app, for exactly as
		// long as you care to wait.
		//
		// Answering is not a judgement call the way workspace trust is. The
		// session was started by `rcmanager start` on a remote-control
		// provider; enabling remote control is the thing that was asked for.
		ID:    "enable-remote-control",
		Once:  true,
		Match: regexp.MustCompile(`(?i)enableremotecontrol`),
		// Written on a fifo rather than a tty, so the line terminator has to
		// be \n; a bare \r would never end the line.
		Respond:  func(*registry.Entry) string { return "y\n" },
		Answered: `answered the "Enable Remote Control?" prompt`,
		Describe: `waiting at the "Enable Remote Control?" prompt`,
		Delay:    500 * time.Millisecond,
	},
	{
		ID:    "trust-dialog",
		Once:  true,
		Match: regexp.MustCompile(`(?i)isthisaprojectyoucreatedoroneyoutrust|yes,itrustthisfolder|doyoutrustthefiles|workspacenottrusted`),
		// Accepting a workspace-trust prompt is a security decision, so
		// rcmanager only does it when a human has opted in per session with
		// providerOptions.trustWorkspace. Otherwise it reports the block.
		// The menu preselects "No, exit"; down-then-enter picks "Yes".
		Respond: func(e *registry.Entry) string {
			if e.BoolOption("trustWorkspace") {
				return "\x1b[B\r"
			}
			return ""
		},
		Fatal:    true,
		Answered: "accepted the workspace trust dialog (providerOptions.trustWorkspace is set for this session)",
		Describe: "blocked by the workspace trust dialog — run `claude` in this directory once and accept it (for a resumed session, providerOptions.trustWorkspace = true also works; a brand-new session has no dialog to answer and must be trusted interactively)",
		Delay:    1200 * time.Millisecond,
	},
	{
		ID:       "browser-prompt",
		Once:     true,
		Match:    regexp.MustCompile(`(?i)claudewantstouseyourbrowser`),
		Fatal:    true,
		Describe: "blocked by the Claude-in-Chrome permission prompt — answer it once interactively, or add \"--no-chrome\" to the session args",
	},
}

func (Provider) Interactions() []provider.Interaction { return interactions }

// TrustState reads workspace trust from Claude's own record rather than
// discovering it by running into the modal.
func (Provider) TrustState(cwd string) provider.Trust {
	doc := map[string]any{}
	_ = jsonfile.Read(configFile(), &doc)

	abs, _ := filepath.Abs(cwd)
	projects, _ := doc["projects"].(map[string]any)
	rec, known := projects[abs].(map[string]any)
	trusted := false
	if known {
		trusted, _ = rec["hasTrustDialogAccepted"].(bool)
	}
	return provider.Trust{Known: known, Trusted: trusted, File: configFile()}
}

// SetTrust records trust for a directory.
//
// Deliberately a separate, named act rather than something a session start can
// do on its own: this is the decision that lets an agent run commands against
// those files. Claude keeps a lot in this file, so the read-modify-write is
// atomic and takes a backup the first time.
func (Provider) SetTrust(cwd string, grant bool) provider.TrustResult {
	file := configFile()
	dir, _ := filepath.Abs(cwd)

	doc := map[string]any{}
	if err := jsonfile.ReadStrict(file, &doc); err != nil {
		return provider.TrustResult{Error: "Claude has no settings file at " + file + ". Run `claude` once first."}
	}

	backup := file + ".rcmanager-backup"
	if _, err := os.Stat(backup); err != nil {
		if raw, err := os.ReadFile(file); err == nil {
			_ = os.WriteFile(backup, raw, 0o600)
		}
	}

	projects, _ := doc["projects"].(map[string]any)
	if projects == nil {
		projects = map[string]any{}
	}
	rec, _ := projects[dir].(map[string]any)
	if rec == nil {
		rec = map[string]any{}
	}
	rec["hasTrustDialogAccepted"] = grant
	projects[dir] = rec
	doc["projects"] = projects

	if err := jsonfile.Write(file, doc, 0o600); err != nil {
		return provider.TrustResult{Error: "could not write " + file + ": " + err.Error()}
	}
	return provider.TrustResult{OK: true, Dir: dir, File: file, Backup: backup, Trusted: grant}
}

func (p Provider) Preflight(e *registry.Entry) provider.Result {
	if st, err := os.Stat(e.Cwd); err != nil || !st.IsDir() {
		return provider.Result{Error: "Directory does not exist: " + e.Cwd}
	}

	// Only a *resume* needs the transcript to exist. A create is the thing that
	// brings it into being, and an id rcmanager minted has no transcript until the
	// first start.
	if e.SessionID != "" && p.attachMode(e) == "resume" {
		found := false
		for _, s := range listSessions() {
			if s.SessionID == e.SessionID {
				found = true
				break
			}
		}
		if !found {
			return provider.Result{
				Error: "Claude has no local session " + e.SessionID + ".",
				Hint:  "It may have been deleted or archived. Re-run discovery with `rcmanager start <name> --provider claude` after removing the stale registration.",
			}
		}
	}
	return p.TrustPreflight(e)
}

// TrustPreflight refuses a start that workspace trust is going to block
// anyway — but only that one, because the two entry points behave differently
// and a preflight stricter than the CLI would refuse sessions that
// demonstrably run.
//
// Observed against 2.1.252-2.1.257:
//
//	directory absent from ~/.claude.json
//	  `remote-control --name` exits with a trust error; `--resume` puts up the
//	  modal. Neither can proceed unattended -> refuse, with the fix.
//
//	directory present, hasTrustDialogAccepted false
//	  `--resume` runs normally and never shows the dialog: trust is settled
//	  when a session is created, not when an existing one is resumed. A managed
//	  session in exactly this state has been online for hours, so blocking it
//	  would be a regression, not a safeguard. A *new* session still needs real
//	  trust.
func (p Provider) TrustPreflight(e *registry.Entry) provider.Result {
	trust := p.TrustState(e.Cwd)
	if trust.Trusted || e.BoolOption("trustWorkspace") {
		return provider.Result{OK: true}
	}

	attach, _ := e.Option("attach")
	if attach == "" {
		if e.SessionID != "" {
			attach = "resume"
		} else {
			attach = "new"
		}
	}
	if attach == "resume" && trust.Known {
		return provider.Result{OK: true}
	}

	msg := "Claude has never been run in " + e.Cwd + ", so it will refuse to start there or stall on the trust prompt."
	if trust.Known {
		msg = "Claude does not trust " + e.Cwd + ", and a new session there cannot get past the trust prompt unattended."
	}
	return provider.Result{Error: msg, Hint: "Grant it once:  rcmanager trust " + e.Cwd}
}

var claudeExe = regexp.MustCompile(`(^|/)claude(\.js)?$`)

// FindExternalOwner detects a Claude process outside rcmanager that already owns
// this session.
//
// Two writers on one transcript fork it, so rcmanager refuses to attach on top of
// a session a terminal, an IDE or another supervisor is already driving.
//
// Claude appends to the transcript and closes it again rather than holding the
// descriptor, so ownership has to be read from argv. A live session is
// launched as one of:
//
//	claude --resume <uuid|name> [--remote-control]
//	claude remote-control --session-id <uuid>
//	claude --session-id <uuid> ...
//
// so matching the id or the name in a value position covers every form.
func (Provider) FindExternalOwner(e *registry.Entry, excludePids []int) *provider.Owner {
	needles := map[string]bool{}
	for _, n := range []string{e.SessionID, e.Name} {
		if n != "" {
			needles[n] = true
		}
	}
	flags := map[string]bool{
		"--resume": true, "-r": true, "--session-id": true,
		"--name": true, "-n": true, "--teleport": true, "remote-control": true,
	}

	for _, proc := range procs.ListProcesses(excludePids...) {
		if len(proc.Argv) == 0 {
			continue
		}
		exe := filepath.Base(proc.Argv[0])
		isClaude := exe == "claude"
		if !isClaude {
			for _, a := range proc.Argv {
				if claudeExe.MatchString(a) {
					isClaude = true
					break
				}
			}
		}
		if !isClaude {
			continue
		}

		hit := false
		for i, tok := range proc.Argv {
			if i == 0 || !needles[tok] {
				continue
			}
			prev := proc.Argv[i-1]
			if flags[prev] || prev == "claude" || exe == "claude" {
				hit = true
				break
			}
		}
		if !hit {
			continue
		}

		head := proc.Argv
		if len(head) > 4 {
			head = head[:4]
		}
		return &provider.Owner{
			Pid:    proc.Pid,
			Argv:   proc.Argv,
			Reason: fmt.Sprintf("pid %d (%s) is already running this Claude session", proc.Pid, strings.Join(head, " ")),
		}
	}
	return nil
}

// Real ids are `session_` + exactly 24 characters.
//
// The payload is matched strictly: a repaint can drop a character out of the
// middle of the id, and a truncated id yields a link that resolves to nothing
// while looking entirely plausible, so only 24 characters followed by a
// boundary count. The TUI repaints often enough that an intact copy follows.
//
// The `session` prefix, on the other hand, is matched loosely, because the
// repaint damages it too. Observed in the field, for a session that was live
// and reachable the whole time:
//
//	https://claude.ai/code/ssion_01RDLippg3f1VTL2o8xpydE8
//
// — two characters gone from the word, nothing wrong with the id. Requiring
// the literal prefix meant reporting no URL at all for that session, and the
// watchdog complaining about it forever. So any prefix that is a subsequence
// of "session" is accepted, which is loose enough for dropped characters and
// far too tight to match an unrelated token.
const idPattern = `([A-Za-z]{1,7})_([A-Za-z0-9]{24})(?:[^A-Za-z0-9]|$)`

var (
	// urlBuf arrives with whitespace collapsed, and the repaint sometimes drops
	// the spaces between words altogether ("remote-controlisactive"), so
	// whitespace is optional here rather than required.
	announcedRe = regexp.MustCompile(`(?i)remote-?control\s*is\s*active[\s\S]{0,200}?` + idPattern)
	// Accept an id that still carries a recognisable url around it, allowing
	// for a mangled host and path (`claude.a/code/`, `claude.ai/coe/`).
	nearRe = regexp.MustCompile(`(?i)claude\.[a-z]{0,3}/[a-z]{1,5}/` + idPattern)
	envRe  = regexp.MustCompile(`(?i)claude\.a[a-z]{0,2}/code\?environment=(env_[A-Za-z0-9]{12,})`)
)

// isDamagedSessionPrefix reports whether prefix is what a repaint can leave of
// the word "session": its characters, in order, with some of them missing.
func isDamagedSessionPrefix(prefix string) bool {
	const word = "session"
	i := 0
	for _, r := range strings.ToLower(prefix) {
		for i < len(word) && word[i] != byte(r) {
			i++
		}
		if i == len(word) {
			return false
		}
		i++
	}
	return true
}

// ExtractRemoteURL pulls the remote-control URL out of the session's own
// output.
//
// Two things make this harder than a URL match.
//
// A managed session prints whatever its agent prints, so a bare match is not
// enough: an agent that echoes another session's link would have that link
// recorded as its own. Anchoring on the announcement takes the URL the CLI
// published.
//
// And the rendered text is not reliable character-for-character. The TUI
// repaints with absolute cursor positioning and wraps the URL across a line,
// and what comes out the far end is sometimes damaged — observed in practice
// as `https://claude.a/code/session_01ECZ…`, missing the `i`, for a session
// that was live and reachable the whole time. Matching the full host meant
// reporting no URL at all for a perfectly healthy session.
//
// So only the id is read out of the output, and the URL is rebuilt around it.
// The id is the payload; the host is a constant we already know.
func (Provider) ExtractRemoteURL(text string) string {
	if m := lastSessionID(announcedRe, text); m != "" {
		return sessionURL(m)
	}
	if m := lastSessionID(nearRe, text); m != "" {
		return sessionURL(m)
	}
	// An environment publishes a different shape. rcmanager does not manage
	// environments, but reporting nothing would look like a stall.
	if m := lastMatch(envRe, text); m != "" {
		return "https://claude.ai/code?environment=" + m
	}
	return ""
}

// lastSessionID returns the id from the last match whose prefix is a
// plausible remnant of "session", so the newest announcement wins.
func lastSessionID(re *regexp.Regexp, text string) string {
	all := re.FindAllStringSubmatch(text, -1)
	for i := len(all) - 1; i >= 0; i-- {
		if isDamagedSessionPrefix(all[i][1]) {
			return "session_" + all[i][2]
		}
	}
	return ""
}

func lastMatch(re *regexp.Regexp, text string) string {
	all := re.FindAllStringSubmatch(text, -1)
	if len(all) == 0 {
		return ""
	}
	return all[len(all)-1][1]
}

func firstNonEmpty(values ...string) string {
	for _, v := range values {
		if v != "" {
			return v
		}
	}
	return ""
}

// Compile-time proof that this adapter honours every contract it claims.
var (
	_ provider.Provider    = Provider{}
	_ provider.Installable = Provider{}
	_ provider.TrustKeeper = Provider{}
	_ provider.OwnerFinder = Provider{}
)
