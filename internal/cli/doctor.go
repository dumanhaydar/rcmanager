package cli

import (
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"time"

	"github.com/dumanhaydar/rcmanager/internal/ansi"
	"github.com/dumanhaydar/rcmanager/internal/client"
	"github.com/dumanhaydar/rcmanager/internal/daemon"
	"github.com/dumanhaydar/rcmanager/internal/format"
	"github.com/dumanhaydar/rcmanager/internal/ipc"
	"github.com/dumanhaydar/rcmanager/internal/provider"
	"github.com/dumanhaydar/rcmanager/internal/session"
	"github.com/dumanhaydar/rcmanager/internal/ui"
)

// doctor asks each provider CLI whether it is signed in twice — from this
// shell and from the daemon — and explains a difference.
//
// The two can disagree, and when they do every symptom points elsewhere: the
// session starts, the process is alive, and it never shows up in the app.
// Observed on macOS under launchd, where Claude Code read a stale Keychain
// entry while the shell used a valid ~/.claude/.credentials.json.
//
// It reports and suggests. It never touches the Keychain or any credentials:
// those are the user's to change, and the commands are printed to be run by
// hand.
func doctor(_ []string, flags Flags) error {
	shell := daemon.CurrentContext()
	shell.Providers = daemon.ReportAuth()

	// Never autostart here. A daemon started from this shell inherits this
	// shell's environment, and comparing the shell with itself would hide the
	// very difference this command exists to find.
	var remote daemon.DaemonContext
	daemonErr := client.Call("authStatus", ipc.Args{}, &remote, client.Options{Autostart: false, Timeout: 90 * time.Second})

	if flags.Bool("json") {
		out := map[string]any{"shell": shell}
		if daemonErr == nil {
			out["daemon"] = remote
		} else {
			out["daemonError"] = daemonErr.Error()
		}
		return printJSON(out)
	}

	rows := [][]string{}
	for _, r := range shell.Providers {
		rows = append(rows, authRow("this shell", r))
	}
	if daemonErr == nil {
		for _, r := range remote.Providers {
			rows = append(rows, authRow(fmt.Sprintf("daemon (pid %d)", remote.Pid), r))
		}
	}
	if len(rows) == 0 {
		ui.Warn("no provider CLI found on PATH — see `rcmanager providers`")
		return nil
	}
	fmt.Println(format.Table([]string{"Context", "Provider", "Signed in", "Method", "Login expires", "CLI"}, rows))
	fmt.Println()

	if daemonErr != nil {
		if info := client.DaemonInfo(); info.Alive {
			ui.Warn(fmt.Sprintf("the daemon (pid %d) could not answer, so only this shell was checked: %v", info.Pid, daemonErr))
			ui.Info("if it predates this rcmanager, run:  rcmanager upgrade")
			return nil
		}
		ui.Warn("the daemon is not running, so only this shell was checked.")
		ui.Info("start it the way it normally runs (rcmanager service install, or your service manager), then run doctor again.")
		return nil
	}

	waiting := authRequiredSessions()
	problems := 0
	for _, d := range remote.Providers {
		s, inShell := findReport(shell.Providers, d.Provider)
		problems += diagnose(d, s, inShell, shell, remote)
	}
	if problems == 0 {
		ui.OK("every provider the daemon can see is signed in")
	}
	if len(waiting) > 0 {
		fmt.Println()
		ui.Info(fmt.Sprintf("%d session(s) are waiting for a sign-in. Once it is fixed:  rcmanager restart all", len(waiting)))
	}
	return nil
}

func authRow(context string, r daemon.AuthReport) []string {
	signed := ansi.Grey("unknown")
	if r.Known {
		signed = ansi.Red("no")
		if r.LoggedIn {
			signed = ansi.Green("yes")
		}
	}
	cli := r.Bin
	if r.Version != "" {
		cli += " (" + r.Version + ")"
	}
	return []string{context, ansi.Bold(r.Provider), signed, orDash(r.Method), orDash(r.ExpiresAt), cli}
}

func findReport(list []daemon.AuthReport, id string) (daemon.AuthReport, bool) {
	for _, r := range list {
		if r.Provider == id {
			return r, true
		}
	}
	return daemon.AuthReport{}, false
}

// diagnose explains one provider's result and returns how many problems it
// found.
func diagnose(d, s daemon.AuthReport, inShell bool, shell, remote daemon.DaemonContext) int {
	name := d.Provider
	if p, err := provider.Get(d.Provider); err == nil {
		name = p.DisplayName()
	}

	switch {
	case !d.Known:
		ui.Warn(name + ": the daemon could not read its login status (the status command failed or timed out).")
		ui.Info("sessions are still started; if they never appear in the app, run `rcmanager logs <name>`.")
		return 1

	case d.LoggedIn:
		if inShell && s.Known && !s.LoggedIn {
			ui.Info(name + ": signed in for the daemon, but not from this shell — harmless for rcmanager.")
		}
		return 0

	case inShell && s.Known && s.LoggedIn:
		ui.Warn(ansi.Bold(name+" is signed in from this shell, but NOT from the daemon.") +
			" The daemon's sessions cannot authenticate.")
		explainContextDifference(d, s, shell, remote)
		return 1

	default:
		ui.Warn(name + " is not signed in, from this shell or from the daemon.")
		if p, err := provider.Get(d.Provider); err == nil {
			if inst, ok := p.(provider.Installable); ok && inst.LoginCommand() != nil {
				fmt.Println("  sign in, then restart the sessions:")
				fmt.Println("    " + ansi.Cyan(inst.LoginCommand().Display))
				fmt.Println("    " + ansi.Cyan("rcmanager restart all"))
			}
		}
		return 1
	}
}

// explainContextDifference lists what differs between the two contexts, most
// likely cause first. Nothing here is run: it is printed.
func explainContextDifference(d, s daemon.AuthReport, shell, remote daemon.DaemonContext) {
	if d.Bin != s.Bin {
		fmt.Printf("  · the two run different CLIs: %s (daemon) and %s (shell)\n", d.Bin, s.Bin)
	}
	if remote.Home != shell.Home {
		fmt.Printf("  · HOME differs: %s (daemon) and %s (shell)\n", orDash(remote.Home), orDash(shell.Home))
	}
	if remote.User != shell.User {
		fmt.Printf("  · USER differs: %s (daemon) and %s (shell)\n", orDash(remote.User), orDash(shell.User))
	}
	if d.Provider == "claude" && remote.ConfigDir != shell.ConfigDir {
		fmt.Printf("  · CLAUDE_CONFIG_DIR differs: %s (daemon) and %s (shell)\n", orDash(remote.ConfigDir), orDash(shell.ConfigDir))
	}

	if d.Provider != "claude" {
		return
	}

	if runtime.GOOS == "darwin" {
		fmt.Println()
		fmt.Println("  Most likely on macOS: the Keychain and ~/.claude/.credentials.json disagree.")
		fmt.Println("  Under launchd, Claude Code reads the Keychain entry \"Claude Code-credentials\" first; a")
		fmt.Println("  stale entry there wins over a valid credentials file, and `claude auth login` may")
		fmt.Println("  refresh the file without touching the Keychain.")
		if file := claudeCredentialsFile(shell.ConfigDir); file != "" {
			if st, err := os.Stat(file); err == nil {
				fmt.Printf("  %s exists (modified %s).\n", file, st.ModTime().Format(time.RFC3339))
			} else {
				fmt.Printf("  %s does not exist, so removing the Keychain entry would leave no login at all.\n", file)
			}
		}
		fmt.Println()
		fmt.Println("  rcmanager will not change the Keychain or your credentials. To do it yourself:")
		fmt.Println("    " + ansi.Grey("# 1. look at the entry — its attributes and dates, not the secret (no -w):"))
		fmt.Println("    " + ansi.Cyan(`security find-generic-password -s "Claude Code-credentials"`))
		fmt.Println("    " + ansi.Grey("# 2. if the credentials file above is current, remove the stale entry:"))
		fmt.Println("    " + ansi.Cyan(`security delete-generic-password -s "Claude Code-credentials"`))
		fmt.Println("    " + ansi.Grey("# 3. check from the daemon again, then bring the sessions back:"))
		fmt.Println("    " + ansi.Cyan("rcmanager doctor"))
		fmt.Println("    " + ansi.Cyan("rcmanager restart all"))
		fmt.Println("  " + ansi.Grey("If the credentials file is missing or stale too, sign in again with `claude auth login` instead."))
		return
	}

	fmt.Println()
	fmt.Println("  The daemon reads its credentials from its own HOME / CLAUDE_CONFIG_DIR. Make the")
	fmt.Println("  service run as the user who signed in, or sign in as the user the service runs as,")
	fmt.Println("  then:  " + ansi.Cyan("rcmanager restart all"))
}

// claudeCredentialsFile is where Claude Code keeps its file-based login. Only
// its existence and modification time are ever read — never its content.
func claudeCredentialsFile(configDir string) string {
	if configDir != "" {
		return filepath.Join(configDir, ".credentials.json")
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return ""
	}
	return filepath.Join(home, ".claude", ".credentials.json")
}

func authRequiredSessions() []string {
	var sessions []session.View
	if err := client.Call("list", ipc.Args{}, &sessions, client.Options{Autostart: false}); err != nil {
		return nil
	}
	var out []string
	for _, s := range sessions {
		if s.Status == session.AuthRequired {
			out = append(out, s.Name)
		}
	}
	return out
}
