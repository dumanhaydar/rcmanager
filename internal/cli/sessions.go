package cli

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/dumanhaydar/rcmanager/internal/ansi"
	"github.com/dumanhaydar/rcmanager/internal/client"
	"github.com/dumanhaydar/rcmanager/internal/format"
	"github.com/dumanhaydar/rcmanager/internal/ipc"
	"github.com/dumanhaydar/rcmanager/internal/provider"
	"github.com/dumanhaydar/rcmanager/internal/rcerr"
	"github.com/dumanhaydar/rcmanager/internal/registry"
	"github.com/dumanhaydar/rcmanager/internal/session"
	"github.com/dumanhaydar/rcmanager/internal/ui"
)

func requireName(args []string, cmd string) (string, error) {
	if len(args) == 0 || args[0] == "" {
		return "", rcerr.New("Usage: rcmanager "+cmd+" <name>", "")
	}
	return args[0], nil
}

// --------------------------------------------------------------------- start

func start(args []string, flags Flags) error {
	if len(args) == 0 {
		return rcerr.New("Usage: rcmanager start <name> [path] [--provider <provider>]",
			"Or point it at an ecosystem file: rcmanager start ./rcmanager.json")
	}
	first := args[0]
	second := ""
	if len(args) > 1 {
		second = args[1]
	}

	// `rcmanager start <file.json>` — bulk register and start from an ecosystem file.
	if looksLikeEcosystemFile(first) {
		abs, err := filepath.Abs(first)
		if err != nil {
			return err
		}
		return startEcosystem(abs, flags)
	}
	if first == "all" {
		return startAll(flags)
	}

	t, err := resolveTarget(first, second, flags.Str("provider", ""), false)
	if err != nil {
		return err
	}

	if t.origin == "discovered" {
		p, _ := provider.Get(t.entry.Provider)
		ui.Info(fmt.Sprintf("discovered %s session %s", p.DisplayName(), ansi.Cyan(t.discovered.SessionID)))
		extra := ""
		if len(t.duplicates) > 0 {
			extra = fmt.Sprintf(" · %d older duplicate(s) ignored", len(t.duplicates))
		}
		ui.Info(fmt.Sprintf("  cwd %s · last active %s%s", t.entry.Cwd, format.Ago(t.discovered.LastActive), extra))
	}

	// Declining is a decision, not a detour: registering a session that cannot
	// start would leave a broken entry behind and an error to read later.
	granted, err := ensureTrusted(t.entry, flags)
	if err != nil {
		return err
	}
	if !granted {
		return nil
	}

	var registered session.View
	if err := client.Call("register", ipc.Args{Entry: t.entry}, &registered, client.Options{Autostart: flags.Autostart()}); err != nil {
		return err
	}
	if t.origin != "registry" {
		ui.OK("registered " + ansi.Bold(t.entry.Name))
	}

	status, err := ui.WithSpinner("starting "+t.entry.Name, func() (session.View, error) {
		var v session.View
		err := client.Call("start", ipc.Args{Name: t.entry.Name, Force: flags.Bool("force")}, &v,
			client.Options{Autostart: flags.Autostart(), Timeout: 180 * time.Second})
		return v, err
	})
	if err != nil {
		return err
	}

	if status.Adopted {
		ui.OK(fmt.Sprintf("%s was already running (pid %d) — adopted", ansi.Bold(t.entry.Name), status.Pid))
	} else {
		ui.OK(fmt.Sprintf("%s started (pid %d)", ansi.Bold(t.entry.Name), status.Pid))
	}
	printRemoteHint(status)
	return nil
}

// ensureTrusted settles workspace trust before the daemon is asked to start
// anything.
//
// The daemon cannot do this: it has no terminal, and a supervisor blocking on
// a modal nobody can see is the failure rcmanager exists to prevent. So the
// question is asked here, once, and the daemon only ever receives a decided
// answer.
//
// Answering is still the human's — rcmanager offers, it does not assume. With no
// terminal to ask in, this stays quiet and lets preflight refuse with its hint.
func ensureTrusted(entry *registry.Entry, flags Flags) (bool, error) {
	p, err := provider.Get(entry.Provider)
	if err != nil {
		return false, err
	}
	keeper, ok := p.(provider.TrustKeeper)
	if !ok {
		return true, nil
	}

	verdict := keeper.TrustPreflight(entry)
	if verdict.OK {
		return true, nil
	}
	if !ui.Interactive() {
		return false, nil
	}

	ui.Warn(verdict.Error)
	granted := flags.Bool("trust") ||
		ui.Confirm(fmt.Sprintf("Trust %s for %s?", ansi.Bold(entry.Cwd), p.DisplayName()), false)
	if !granted {
		ui.Info("left untrusted — nothing was registered")
		if verdict.Hint != "" {
			ui.Info(verdict.Hint)
		}
		return false, nil
	}

	result := keeper.SetTrust(entry.Cwd, true)
	if !result.OK {
		return false, rcerr.New(result.Error, "")
	}
	ui.OK("trusted " + entry.Cwd)
	ui.Info("recorded in " + result.File + backupNote(result.Backup))
	return true, nil
}

func backupNote(backup string) string {
	if backup == "" {
		return ""
	}
	return " (backup at " + backup + ")"
}

func looksLikeEcosystemFile(value string) bool {
	if !strings.HasSuffix(value, ".json") {
		return false
	}
	_, err := os.Stat(value)
	return err == nil
}

func startEcosystem(file string, flags Flags) error {
	raw, err := os.ReadFile(file)
	if err != nil {
		return rcerr.New("Could not read ecosystem file "+file+".", "")
	}
	entries, err := registry.ParseEcosystem(raw, file)
	if err != nil {
		return err
	}

	ui.Info(fmt.Sprintf("%d session(s) from %s", len(entries), file))
	if _, err := client.EnsureDaemon(flags.Autostart()); err != nil {
		return err
	}

	// The CLI drives this loop, so it already knows the plan — draw it up front
	// and fill it in, rather than emitting a line whenever something finishes.
	names := make([]string, 0, len(entries))
	for _, e := range entries {
		names = append(names, e.Name)
	}
	tasks := ui.NewTaskList(names)

	for _, entry := range entries {
		tasks.Set(entry.Name, ui.Active, "registering")
		if err := client.Call("register", ipc.Args{Entry: entry}, nil, client.Options{Autostart: false}); err != nil {
			tasks.Set(entry.Name, ui.Failed, err.Error())
			continue
		}
		if !entry.Enabled {
			tasks.Set(entry.Name, ui.Skipped, "disabled — not started")
			continue
		}
		tasks.Set(entry.Name, ui.Active, "starting")
		var status session.View
		if err := client.Call("start", ipc.Args{Name: entry.Name, Force: flags.Bool("force")}, &status,
			client.Options{Autostart: false, Timeout: 180 * time.Second}); err != nil {
			tasks.Set(entry.Name, ui.Failed, err.Error())
			continue
		}
		verb := "started"
		if status.Adopted {
			verb = "adopted"
		}
		tasks.Set(entry.Name, ui.Done, fmt.Sprintf("%s · pid %d", verb, status.Pid))
	}
	tasks.Stop()
	fmt.Println()
	return list(nil, flags)
}

// bulk runs a daemon-side bulk operation with a live line per session.
func bulk(cmd, verb string, flags Flags) error {
	var tasks *ui.TaskList
	var results []map[string]any

	err := client.Call(cmd, ipc.Args{}, &results, client.Options{
		Autostart: flags.Autostart(),
		Timeout:   10 * time.Minute,
		OnProgress: func(p ipc.Progress) {
			switch p.Phase {
			case "plan":
				if len(p.Names) == 0 {
					return
				}
				tasks = ui.NewTaskList(p.Names)
			case "begin":
				tasks.Set(p.Name, ui.Active, verb)
			case "end":
				if p.OK {
					detail := "stopped"
					if p.Pid != 0 {
						detail = fmt.Sprintf("pid %d", p.Pid)
						if p.Adopted {
							detail += " (adopted)"
						}
					}
					tasks.Set(p.Name, ui.Done, detail)
					return
				}
				detail := p.Error
				if detail == "" {
					detail = "failed"
				}
				tasks.Set(p.Name, ui.Failed, detail)
			}
		},
	})
	tasks.Stop()
	if err != nil {
		return err
	}
	if len(results) == 0 {
		ui.Info("no sessions to act on.")
	}
	return nil
}

func startAll(flags Flags) error {
	if err := bulk("startAll", "starting", flags); err != nil {
		return err
	}
	fmt.Println()
	return list(nil, flags)
}

func printRemoteHint(status session.View) {
	if status.RemoteURL != "" {
		ui.Info("remote control: " + ansi.Cyan(status.RemoteURL))
		return
	}
	ui.Info("the remote-control URL appears in the log shortly: rcmanager logs " + status.Name)
}

// ---------------------------------------------------------------- stop/restart

func stop(args []string, flags Flags) error {
	name, err := requireName(args, "stop")
	if err != nil {
		return err
	}
	if name == "all" {
		return bulk("stopAll", "stopping", flags)
	}
	if _, err := ui.WithSpinner("stopping "+name, func() (session.View, error) {
		var v session.View
		err := client.Call("stop", ipc.Args{Name: name}, &v,
			client.Options{Autostart: flags.Autostart(), Timeout: 120 * time.Second})
		return v, err
	}); err != nil {
		return err
	}
	ui.OK(ansi.Bold(name) + " stopped")
	return nil
}

func restart(args []string, flags Flags) error {
	name, err := requireName(args, "restart")
	if err != nil {
		return err
	}
	if name == "all" {
		if err := bulk("restartAll", "restarting", flags); err != nil {
			return err
		}
		fmt.Println()
		return list(nil, flags)
	}
	status, err := ui.WithSpinner("restarting "+name, func() (session.View, error) {
		var v session.View
		err := client.Call("restart", ipc.Args{Name: name}, &v,
			client.Options{Autostart: flags.Autostart(), Timeout: 180 * time.Second})
		return v, err
	})
	if err != nil {
		return err
	}
	ui.OK(fmt.Sprintf("%s restarted (pid %d)", ansi.Bold(name), status.Pid))
	printRemoteHint(status)
	return nil
}

func remove(args []string, flags Flags) error {
	name, err := requireName(args, "remove")
	if err != nil {
		return err
	}
	if err := client.Call("remove", ipc.Args{Name: name}, nil, client.Options{Autostart: flags.Autostart()}); err != nil {
		return err
	}
	ui.OK(ansi.Bold(name) + " unregistered")
	ui.Info("the underlying agent conversation was not touched")
	return nil
}

func enable(args []string, flags Flags) error  { return toggle(args, flags, "enable") }
func disable(args []string, flags Flags) error { return toggle(args, flags, "disable") }

func toggle(args []string, flags Flags, cmd string) error {
	name, err := requireName(args, cmd)
	if err != nil {
		return err
	}
	var status session.View
	if err := client.Call(cmd, ipc.Args{Name: name}, &status,
		client.Options{Autostart: flags.Autostart(), Timeout: 120 * time.Second}); err != nil {
		return err
	}
	pid := ""
	if status.Pid != 0 {
		pid = fmt.Sprintf(" (pid %d)", status.Pid)
	}
	ui.OK(ansi.Bold(name) + " " + cmd + "d" + pid)
	if status.Message != "" {
		ui.Info(status.Message)
	}
	return nil
}

// --------------------------------------------------------------------- reload

func reload(_ []string, flags Flags) error {
	var sessions []session.View
	if err := client.Call("reload", ipc.Args{}, &sessions,
		client.Options{Autostart: flags.Autostart(), Timeout: 5 * time.Minute}); err != nil {
		return err
	}
	ui.OK("reloaded config.json and registry.json")

	flagged := 0
	for _, s := range sessions {
		if len(s.NeedsRestart) == 0 {
			continue
		}
		flagged++
		ui.Info(fmt.Sprintf("%s: %s changed — %s to apply",
			ansi.Bold(s.Name), strings.Join(s.NeedsRestart, ", "),
			ansi.Yellow("rcmanager restart "+s.Name)))
	}
	if flagged == 0 {
		ui.Info("running sessions were left untouched")
	}
	fmt.Println()
	return list(nil, flags)
}

func printJSON(v any) error {
	blob, err := json.MarshalIndent(v, "", "  ")
	if err != nil {
		return err
	}
	fmt.Println(string(blob))
	return nil
}
