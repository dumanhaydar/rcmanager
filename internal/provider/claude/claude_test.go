package claude

import (
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"

	"github.com/dumanhaydar/rcmanager/internal/registry"
)

const liveID = "session_ABCDEFGHIJKLMNOPQRSTUVWX" // 24 characters, like the real thing

func TestExtractRemoteURLPrefersTheAnnouncement(t *testing.T) {
	// A session that echoes a neighbour's link must not adopt it: the URL is
	// taken from the announcement, which only the CLI prints.
	text := "look at https://claude.ai/code/session_ZZZZZZZZZZZZZZZZZZZZZZZZ later · " +
		"remote-control is active · Continue here, on your phone, or at https://claude.ai/code/" + liveID
	if got, want := (Provider{}).ExtractRemoteURL(text), "https://claude.ai/code/"+liveID; got != want {
		t.Fatalf("ExtractRemoteURL() = %q, want %q", got, want)
	}
}

// The TUI repaints with absolute cursor positioning and sometimes damages the
// host it printed: only the id is trustworthy, so the URL is rebuilt from it.
func TestExtractRemoteURLSurvivesAMangledHost(t *testing.T) {
	for _, text := range []string{
		"claude.a/code/" + liveID,
		"claude.ai/coe/" + liveID,
		"remote-controlisactive at claude.ai/code/" + liveID,
	} {
		if got, want := (Provider{}).ExtractRemoteURL(text), "https://claude.ai/code/"+liveID; got != want {
			t.Errorf("ExtractRemoteURL(%q) = %q, want %q", text, got, want)
		}
	}
}

// A truncated id yields a link that resolves to nothing while looking
// plausible, so anything short of 24 characters is treated as damage.
func TestExtractRemoteURLRejectsATruncatedID(t *testing.T) {
	if got := (Provider{}).ExtractRemoteURL("claude.ai/code/session_TOOSHORT"); got != "" {
		t.Errorf("ExtractRemoteURL() = %q, want no URL", got)
	}
}

func TestExtractRemoteURLReportsAnEnvironment(t *testing.T) {
	got := (Provider{}).ExtractRemoteURL("open https://claude.ai/code?environment=env_0123456789abcd")
	if want := "https://claude.ai/code?environment=env_0123456789abcd"; got != want {
		t.Fatalf("ExtractRemoteURL() = %q, want %q", got, want)
	}
}

func TestBuildCommandCreateUsesAFixedSessionID(t *testing.T) {
	t.Setenv("CLAUDE_CONFIG_DIR", t.TempDir())
	e := &registry.Entry{Name: "demo", Provider: "claude", SessionID: "uuid-1", Cwd: t.TempDir()}

	cmd, err := (Provider{}).BuildCommand(e)
	if err != nil {
		t.Fatal(err)
	}
	joined := strings.Join(cmd.Args, " ")
	if !strings.Contains(joined, "--session-id uuid-1") || !strings.Contains(joined, "--remote-control demo") {
		t.Errorf("create args = %v", cmd.Args)
	}
	if !cmd.Pty {
		t.Error("the create path needs a pty: the flag form is the interactive UI")
	}
	for _, token := range []string{"claude", "uuid-1", "remote-control"} {
		if !strings.Contains(strings.Join(append(cmd.Signature, ""), " "), token) {
			t.Errorf("signature %v is missing %q, so rcmanager could not safely signal it", cmd.Signature, token)
		}
	}
}

// A recorded id whose transcript exists is resumed; one rcmanager minted and never
// started is created instead of failing to resume.
func TestAttachModeFollowsTheTranscript(t *testing.T) {
	cfg := t.TempDir()
	t.Setenv("CLAUDE_CONFIG_DIR", cfg)
	project := filepath.Join(cfg, "projects", "-tmp-work")
	if err := os.MkdirAll(project, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(project, "known.jsonl"), []byte(`{"cwd":"/tmp/work"}`+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}

	p := Provider{}
	if got := p.attachMode(&registry.Entry{Name: "a", SessionID: "known", Cwd: "/tmp/work"}); got != "resume" {
		t.Errorf("attachMode(existing) = %q, want resume", got)
	}
	if got := p.attachMode(&registry.Entry{Name: "a", SessionID: "minted", Cwd: "/tmp/work"}); got != "create" {
		t.Errorf("attachMode(unstarted) = %q, want create", got)
	}
	opts := map[string]any{"attach": "environment"}
	if got := p.attachMode(&registry.Entry{Name: "a", SessionID: "known", ProviderOptions: opts}); got != "environment" {
		t.Errorf("providerOptions.attach was ignored: %q", got)
	}
}

func TestResumeAndEnvironmentArgs(t *testing.T) {
	cfg := t.TempDir()
	t.Setenv("CLAUDE_CONFIG_DIR", cfg)
	project := filepath.Join(cfg, "projects", "p")
	_ = os.MkdirAll(project, 0o755)
	_ = os.WriteFile(filepath.Join(project, "sid.jsonl"), []byte("{}\n"), 0o600)

	resume, err := (Provider{}).BuildCommand(&registry.Entry{
		Name: "demo", SessionID: "sid", Cwd: t.TempDir(),
		ProviderOptions: map[string]any{"model": "opus", "permissionMode": "plan"},
	})
	if err != nil {
		t.Fatal(err)
	}
	if got := strings.Join(resume.Args, " "); got != "--resume sid --remote-control --permission-mode plan --model opus" {
		t.Errorf("resume args = %q", got)
	}

	env, err := (Provider{}).BuildCommand(&registry.Entry{
		Name: "demo", SessionID: "sid", Cwd: t.TempDir(),
		ProviderOptions: map[string]any{"attach": "environment", "capacity": "3"},
	})
	if err != nil {
		t.Fatal(err)
	}
	if got := strings.Join(env.Args, " "); got != "remote-control --name demo --capacity 3" {
		t.Errorf("environment args = %q", got)
	}
	if env.Pty {
		t.Error("an environment is a headless server and needs no pty")
	}
}

// Trust is a security decision: a directory Claude has never seen cannot be
// started unattended, while an existing session in a known directory runs
// without ever showing the dialog.
func TestTrustPreflight(t *testing.T) {
	cfg := t.TempDir()
	t.Setenv("CLAUDE_CONFIG_DIR", cfg)
	work := t.TempDir()
	p := Provider{}

	unknown := p.TrustPreflight(&registry.Entry{Name: "a", Cwd: work})
	if unknown.OK || !strings.Contains(unknown.Hint, "rcmanager trust") {
		t.Errorf("an unknown directory was allowed: %+v", unknown)
	}

	if err := os.WriteFile(filepath.Join(cfg, ".claude.json"),
		[]byte(`{"projects":{"`+work+`":{"hasTrustDialogAccepted":false}}}`), 0o600); err != nil {
		t.Fatal(err)
	}
	if r := p.TrustPreflight(&registry.Entry{Name: "a", Cwd: work, SessionID: "x"}); !r.OK {
		t.Errorf("resuming a session in a known directory was refused: %+v", r)
	}
	if r := p.TrustPreflight(&registry.Entry{Name: "a", Cwd: work}); r.OK {
		t.Error("a brand-new session was allowed in an untrusted directory")
	}

	// Opting in per session is the documented escape hatch.
	opted := &registry.Entry{Name: "a", Cwd: work, ProviderOptions: map[string]any{"trustWorkspace": true}}
	if r := p.TrustPreflight(opted); !r.OK {
		t.Errorf("providerOptions.trustWorkspace was ignored: %+v", r)
	}
}

func TestSetTrustTakesABackup(t *testing.T) {
	cfg := t.TempDir()
	t.Setenv("CLAUDE_CONFIG_DIR", cfg)
	work := t.TempDir()
	file := filepath.Join(cfg, ".claude.json")

	if r := (Provider{}).SetTrust(work, true); r.OK {
		t.Error("trust was recorded although Claude has no settings file")
	}
	if err := os.WriteFile(file, []byte(`{"keepMe":1,"projects":{}}`), 0o600); err != nil {
		t.Fatal(err)
	}

	r := (Provider{}).SetTrust(work, true)
	if !r.OK || !r.Trusted {
		t.Fatalf("SetTrust() = %+v", r)
	}
	if _, err := os.Stat(r.Backup); err != nil {
		t.Errorf("no backup was taken: %v", err)
	}
	if !(Provider{}).TrustState(work).Trusted {
		t.Error("TrustState() does not see the grant")
	}
	raw, _ := os.ReadFile(file)
	if !strings.Contains(string(raw), "keepMe") {
		t.Error("the rest of Claude's settings were lost")
	}
}

// Inheriting CLAUDE_CODE_CHILD_SESSION silently disables transcript saving,
// which is the one variable that must never leak into a managed child.
func TestEnvDenyCoversNestedSessionMarkers(t *testing.T) {
	deny := (Provider{}).EnvDeny()
	matches := func(key string) bool {
		for _, re := range deny {
			if re.MatchString(key) {
				return true
			}
		}
		return false
	}
	for _, key := range []string{"CLAUDECODE", "CLAUDE_CODE_CHILD_SESSION", "CLAUDE_CODE_SSE_PORT", "CLAUDE_PID"} {
		if !matches(key) {
			t.Errorf("%s is allowed to leak into a managed session", key)
		}
	}
	for _, key := range []string{"ANTHROPIC_API_KEY", "CLAUDE_CONFIG_DIR", "PATH"} {
		if matches(key) {
			t.Errorf("%s is real configuration and must be passed through", key)
		}
	}
}

// Prompt patterns are matched against output with whitespace removed, because
// the TUI paints with cursor positioning rather than spaces.
func TestInteractionPatternsMatchFlattenedOutput(t *testing.T) {
	flatten := func(s string) string {
		return strings.ToLower(regexp.MustCompile(`\s+`).ReplaceAllString(s, ""))
	}
	byID := map[string]func(*registry.Entry) string{}
	patterns := map[string]*regexp.Regexp{}
	for _, i := range (Provider{}).Interactions() {
		byID[i.ID] = i.Respond
		patterns[i.ID] = i.Match
	}

	if !patterns["enable-remote-control"].MatchString(flatten("Enable Remote Control?")) {
		t.Error("the enable-remote-control prompt would not be answered")
	}
	if !patterns["resume-prompt"].MatchString(flatten("Resume from summary")) {
		t.Error("the resume prompt would not be answered")
	}

	// The fifo is not a tty, so the answer has to end the line with \n.
	if got := byID["enable-remote-control"](&registry.Entry{}); got != "y\n" {
		t.Errorf("enable-remote-control answer = %q", got)
	}
	if got := byID["resume-prompt"](&registry.Entry{}); got != "1" {
		t.Errorf("resume answer = %q, want summary", got)
	}
	full := &registry.Entry{ProviderOptions: map[string]any{"resumeMode": "full"}}
	if got := byID["resume-prompt"](full); got != "2" {
		t.Errorf("resumeMode=full answer = %q", got)
	}

	// Workspace trust is never clicked through without an explicit opt-in.
	if got := byID["trust-dialog"](&registry.Entry{}); got != "" {
		t.Errorf("the trust dialog was answered on the user's behalf: %q", got)
	}
}

// Observed in the field: a session that was live and reachable the whole time,
// whose announcement came out of the TUI with characters missing from the word
// "session" — and which therefore had no URL recorded at all, while the
// watchdog complained about it forever.
func TestExtractRemoteURLSurvivesADamagedSessionPrefix(t *testing.T) {
	text := "remote-control is active · Continue here, on your phone, or at " +
		"https://claude.ai/code/ssion_01RDLippg3f1VTL2o8xpydE8"
	want := "https://claude.ai/code/session_01RDLippg3f1VTL2o8xpydE8"
	if got := (Provider{}).ExtractRemoteURL(text); got != want {
		t.Fatalf("ExtractRemoteURL() = %q, want %q", got, want)
	}

	// Any remnant of the word, in order, is accepted.
	for _, prefix := range []string{"session", "ssion", "sesion", "sion", "sn", "s"} {
		text := "claude.ai/code/" + prefix + "_01RDLippg3f1VTL2o8xpydE8 "
		if got := (Provider{}).ExtractRemoteURL(text); got != want {
			t.Errorf("prefix %q: ExtractRemoteURL() = %q", prefix, got)
		}
	}
}

// Loose on the prefix must not mean loose on what counts as an id.
func TestExtractRemoteURLStillRejectsUnrelatedTokens(t *testing.T) {
	cases := []string{
		// Not a remnant of "session": the letters are out of order.
		"claude.ai/code/noise_01RDLippg3f1VTL2o8xpydE8 ",
		"remote-control is active at claude.ai/code/xyz_01RDLippg3f1VTL2o8xpydE8 ",
		// 23 characters: a damaged payload, which would resolve to nothing.
		"claude.ai/code/session_01RDLippg3f1VTL2o8xpydE ",
		// 25 characters: not an id either.
		"claude.ai/code/session_01RDLippg3f1VTL2o8xpydE8X ",
		// An id with no url and no announcement around it.
		"the user pasted session_01RDLippg3f1VTL2o8xpydE8 into a message ",
	}
	for _, text := range cases {
		if got := (Provider{}).ExtractRemoteURL(text); got != "" {
			t.Errorf("ExtractRemoteURL(%q) = %q, want no URL", text, got)
		}
	}
}

// The interaction list is consulted on every chunk of a session's output, so
// it must not be rebuilt — and in particular its patterns must not be
// recompiled — per call.
func TestInteractionsAreBuiltOnce(t *testing.T) {
	first := (Provider{}).Interactions()
	second := (Provider{}).Interactions()

	if len(first) == 0 {
		t.Fatal("no interactions declared")
	}
	if &first[0] != &second[0] {
		t.Error("Interactions() rebuilds its list on every call")
	}
	for _, action := range first {
		if action.Match == nil {
			t.Errorf("interaction %q has no pattern", action.ID)
		}
	}
}
