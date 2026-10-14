package cli

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"time"

	"github.com/dumanhaydar/rcmanager/internal/ansi"
	"github.com/dumanhaydar/rcmanager/internal/client"
	"github.com/dumanhaydar/rcmanager/internal/config"
	"github.com/dumanhaydar/rcmanager/internal/format"
	"github.com/dumanhaydar/rcmanager/internal/ipc"
	"github.com/dumanhaydar/rcmanager/internal/paths"
	"github.com/dumanhaydar/rcmanager/internal/provider"
	"github.com/dumanhaydar/rcmanager/internal/rcerr"
	"github.com/dumanhaydar/rcmanager/internal/registry"
	"github.com/dumanhaydar/rcmanager/internal/service"
	"github.com/dumanhaydar/rcmanager/internal/session"
	"github.com/dumanhaydar/rcmanager/internal/ui"
)

const setupSteps = 5

// setup is the guided first run: pick a provider, register for boot, start the
// supervisor, add the first sessions.
//
// Every step is skippable and nothing is installed or trusted without an
// explicit yes — a first run is exactly the moment not to surprise anybody.
func setup(_ []string, flags Flags) error {
	if !ui.Interactive() {
		return rcerr.New("`rcmanager setup` needs a terminal to ask questions in.",
			"In a script, use `rcmanager config set`, `rcmanager service install` and `rcmanager start` directly.")
	}

	fmt.Printf("\n%s\n", ansi.Bold("rcmanager setup"))
	fmt.Println(ansi.Grey("  Your service manager manages rcmanager; rcmanager manages agent sessions."))
	fmt.Println(ansi.Grey("  Nothing is installed, trusted or started without you saying so."))

	providerID, err := stepProvider()
	if err != nil {
		return err
	}
	stepService(flags)
	if err := stepDaemon(flags); err != nil {
		return err
	}
	started := stepSessions(providerID, flags)
	return stepSummary(started, flags)
}

func stepProvider() (string, error) {
	ui.Step(1, setupSteps, "Which agent CLI should rcmanager drive?")

	type found struct {
		p provider.Provider
		d provider.Detection
	}
	var all []found
	for _, p := range provider.List() {
		all = append(all, found{p, p.Detect()})
	}

	var available []ui.Choice
	for _, f := range all {
		version := f.d.Version
		if version == "" {
			version = "installed"
		}
		if f.d.Available {
			ui.Info(fmt.Sprintf("%s %s · %s", ansi.Green("✔"), f.p.DisplayName(), version))
			available = append(available, ui.Choice{Value: f.p.ID(), Label: f.p.DisplayName(), Hint: version})
			continue
		}
		ui.Info(fmt.Sprintf("%s %s · %s", ansi.Grey("·"), f.p.DisplayName(), ansi.Grey("not found")))
	}

	cfg := config.Load()
	chosen := cfg.DefaultProvider

	switch len(available) {
	case 0:
		ui.Warn("no provider CLI was found on PATH.")
		for _, f := range all {
			inst, ok := f.p.(provider.Installable)
			if !ok {
				continue
			}
			for _, opt := range inst.InstallOptions() {
				ui.Info(fmt.Sprintf("%s: %s", f.p.DisplayName(), opt.Display))
			}
		}
		ui.Info("install one, then run `rcmanager setup` again")
	case 1:
		chosen = available[0].Value
	default:
		def := 0
		for i, c := range available {
			if c.Value == chosen {
				def = i
			}
		}
		chosen = ui.Select("Default provider, used when --provider is omitted:", available, def)
	}

	if chosen != cfg.DefaultProvider {
		if _, err := config.Set("defaultProvider", chosen); err != nil {
			return "", err
		}
		ui.OK("defaultProvider = " + chosen)
	} else {
		ui.Info("defaultProvider = " + chosen)
	}

	// A session that starts without a login gets as far as a login prompt
	// nobody can answer, so it is worth knowing now rather than later.
	if p, err := provider.Get(chosen); err == nil {
		ensureLoggedIn(p)
	}
	return chosen, nil
}

func ensureLoggedIn(p provider.Provider) {
	inst, ok := p.(provider.Installable)
	if !ok {
		return
	}
	auth := inst.AuthStatus()
	if auth.LoggedIn == nil {
		ui.Info("login state: " + auth.Detail)
		return
	}
	if *auth.LoggedIn {
		ui.Info("signed in · " + auth.Detail)
		return
	}

	ui.Warn(p.DisplayName() + " is not signed in on this machine (" + auth.Detail + ")")
	login := inst.LoginCommand()
	if login == nil {
		return
	}
	if !ui.Confirm(fmt.Sprintf("Run `%s` now?", login.Display), true) {
		ui.Info("skipped — sessions will stall at the login prompt until you do")
		return
	}
	runInteractive(login.Command, login.Args)
}

// runInteractive hands the terminal to another command, which is the only way
// a login or an installer can ask its own questions.
func runInteractive(command string, args []string) {
	cmd := exec.Command(command, args...)
	cmd.Stdin, cmd.Stdout, cmd.Stderr = os.Stdin, os.Stdout, os.Stderr
	if err := cmd.Run(); err != nil {
		ui.Warn(command + " exited with " + err.Error())
	}
}

func stepService(flags Flags) {
	ui.Step(2, setupSteps, "Start rcmanager at boot?")

	for _, m := range service.List() {
		if m.Status().Installed {
			ui.Info("already registered with " + ansi.Bold(m.DisplayName()))
			if !ui.Confirm("Re-install it? (refreshes the unit with current paths)", false) {
				return
			}
			if err := serviceInstall(Flags{"manager": m.ID()}); err != nil {
				ui.Warn(err.Error())
			}
			return
		}
	}

	best := service.Detect()
	if best == nil {
		ui.Warn("no supported service manager here — start the daemon yourself with `rcmanager daemon --detached`")
		return
	}
	if !ui.Confirm(fmt.Sprintf("Register rcmanager with %s?", ansi.Bold(best.Manager.DisplayName())), true) {
		ui.Info("skipped — rcmanager will not start at boot")
		return
	}
	if err := serviceInstall(Flags{"manager": best.Manager.ID()}); err != nil {
		ui.Warn(err.Error())
	}
}

func stepDaemon(flags Flags) error {
	ui.Step(3, setupSteps, "Supervisor")

	if d := client.DaemonInfo(); d.Alive {
		ui.Info(fmt.Sprintf("daemon already running (pid %d)", d.Pid))
		return nil
	}
	if !ui.Confirm("Start it now?", true) {
		ui.Info("skipped")
		return nil
	}
	d, err := client.EnsureDaemon(true)
	if err != nil {
		return err
	}
	ui.OK(fmt.Sprintf("daemon running (pid %d)", d.Pid))
	return nil
}

func stepSessions(providerID string, flags Flags) []string {
	ui.Step(4, setupSteps, "Sessions")

	p, err := provider.Get(providerID)
	if err != nil {
		ui.Warn(err.Error())
		return nil
	}

	var started []string
	for {
		mode := ui.Select("How would you like to add a session?", []ui.Choice{
			{Value: "discovered", Label: "Adopt an existing local session", Hint: "found from the provider's own metadata"},
			{Value: "new", Label: "Create a new session in a directory"},
			{Value: "done", Label: "Nothing for now"},
		}, 0)

		switch mode {
		case "discovered":
			started = append(started, addDiscovered(p, flags)...)
		case "new":
			started = append(started, addNew(p, flags)...)
		default:
			return started
		}
		if !ui.Confirm("Add another?", false) {
			return started
		}
	}
}

func addDiscovered(p provider.Provider, flags Flags) []string {
	found := p.Discover("")
	if len(found) == 0 {
		ui.Info("no local " + p.DisplayName() + " sessions were found")
		return nil
	}

	limit := len(found)
	if limit > 15 {
		limit = 15
	}
	choices := make([]ui.Choice, 0, limit)
	for _, s := range found[:limit] {
		label := s.Name
		if label == "" {
			label = "(unnamed) " + short(s.SessionID, 8)
		}
		choices = append(choices, ui.Choice{
			Value: s.SessionID,
			Label: label,
			Hint:  format.EllipsizeLeft(s.Cwd, 40) + " · " + format.Ago(s.LastActive),
		})
	}

	picked := ui.MultiSelect("Which sessions should rcmanager supervise?", choices)
	byID := map[string]provider.Discovered{}
	for _, s := range found {
		byID[s.SessionID] = s
	}

	var started []string
	for _, id := range picked {
		s := byID[id]
		if s.Cwd == "" {
			ui.Warn("session " + short(id, 8) + " has no recorded directory — skipped")
			continue
		}
		name := s.Name
		if name == "" || registry.AssertValidName(name) != nil {
			name = ui.Ask("  a name for "+short(id, 8)+" (letters, digits, . _ -)",
				"session-"+short(id, 6))
		}
		entry := &registry.Entry{
			Name: name, Provider: p.ID(), SessionID: id, Cwd: s.Cwd, Enabled: true,
		}
		if startOne(entry, flags) {
			started = append(started, name)
		}
	}
	return started
}

func addNew(p provider.Provider, flags Flags) []string {
	cwdDefault, _ := os.Getwd()
	dir, err := filepath.Abs(ui.Ask("  directory for the new session", cwdDefault))
	if err != nil {
		ui.Warn(err.Error())
		return nil
	}
	if st, err := os.Stat(dir); err != nil || !st.IsDir() {
		ui.Warn("not a directory: " + dir)
		return nil
	}
	name := ui.Ask("  a name for it", filepath.Base(dir))

	t, err := resolveTarget(name, dir, p.ID(), true)
	if err != nil {
		ui.Warn(err.Error())
		return nil
	}
	if startOne(t.entry, flags) {
		return []string{name}
	}
	return nil
}

func startOne(entry *registry.Entry, flags Flags) bool {
	granted, err := ensureTrusted(entry, flags)
	if err != nil {
		ui.Warn(err.Error())
		return false
	}
	if !granted {
		return false
	}
	if err := client.Call("register", ipc.Args{Entry: entry}, nil, client.Options{Autostart: true}); err != nil {
		ui.Warn(err.Error())
		return false
	}
	status, err := ui.WithSpinner("starting "+entry.Name, func() (session.View, error) {
		var v session.View
		err := client.Call("start", ipc.Args{Name: entry.Name}, &v,
			client.Options{Autostart: true, Timeout: 180 * time.Second})
		return v, err
	})
	if err != nil {
		return false
	}
	ui.OK(fmt.Sprintf("%s started (pid %d)", ansi.Bold(entry.Name), status.Pid))
	printRemoteHint(status)
	return true
}

func stepSummary(started []string, flags Flags) error {
	ui.Step(5, setupSteps, "Done")

	if len(started) > 0 {
		fmt.Println()
		if err := list(nil, flags); err != nil {
			return err
		}
	}
	fmt.Println()
	ui.Info("state and logs live in " + paths.Get().Home)
	ui.Info("rcmanager list            what is running")
	ui.Info("rcmanager logs <name> -f  follow one session")
	ui.Info("rcmanager info <name>     session id, directory, remote URL")
	return nil
}
