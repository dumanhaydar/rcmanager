package cli

import (
	"fmt"
	"os"
	"strconv"
	"time"

	"github.com/dumanhaydar/rcmanager/internal/ansi"
	"github.com/dumanhaydar/rcmanager/internal/client"
	"github.com/dumanhaydar/rcmanager/internal/format"
	"github.com/dumanhaydar/rcmanager/internal/ipc"
	"github.com/dumanhaydar/rcmanager/internal/paths"
	"github.com/dumanhaydar/rcmanager/internal/rcerr"
	"github.com/dumanhaydar/rcmanager/internal/service"
	"github.com/dumanhaydar/rcmanager/internal/session"
	"github.com/dumanhaydar/rcmanager/internal/ui"
)

// serviceCmd registers rcmanager with whatever boot-time service manager this
// machine actually has, without the user needing to know which one that is.
func serviceCmd(args []string, flags Flags) error {
	action := "status"
	if len(args) > 0 {
		action = args[0]
	}
	switch action {
	case "install":
		return serviceInstall(flags)
	case "uninstall", "remove":
		return serviceUninstall(flags)
	case "status":
		return serviceStatus(flags)
	case "print":
		return servicePrint(flags)
	case "detect", "list":
		return serviceDetect(flags)
	case "start", "stop", "restart":
		return serviceControl(action, flags)
	}
	return rcerr.Newf("Use: install | uninstall | status | print | detect | start | stop | restart",
		"Unknown service action %q.", action)
}

// resolveManager picks the manager to act on: --manager wins, otherwise the
// best one this machine offers.
func resolveManager(flags Flags) (service.Manager, error) {
	if id := flags.Str("manager", ""); id != "" {
		m, err := service.Get(id)
		if err != nil {
			return nil, err
		}
		if probe := m.Detect(); !probe.Available && !flags.Bool("force") {
			return nil, rcerr.Newf("Pass --force to install anyway, or run `rcmanager service detect`.",
				"%s is not usable here: %s", m.DisplayName(), probe.Reason)
		}
		return m, nil
	}
	best := service.Detect()
	if best == nil {
		return nil, rcerr.New("No supported service manager was found on this machine.",
			"Run `rcmanager service detect` to see what was checked, then start the daemon manually with `rcmanager daemon --detached`.")
	}
	return best.Manager, nil
}

func serviceInstall(flags Flags) error {
	manager, err := resolveManager(flags)
	if err != nil {
		return err
	}
	ctx := service.NewContext()
	ui.Info(fmt.Sprintf("detected %s (%s)", ansi.Bold(manager.DisplayName()), manager.ID()))

	// Installing under a different manager than the one already registered
	// would leave two things racing to start the same daemon.
	for _, other := range service.List() {
		if other.ID() == manager.ID() {
			continue
		}
		if other.Status().Installed {
			ui.Warn(fmt.Sprintf("%s also has rcmanager registered — remove it with: rcmanager service uninstall --manager %s",
				other.DisplayName(), other.ID()))
		}
	}

	installed, err := manager.Install(ctx)
	if err != nil {
		return err
	}
	ui.OK("installed " + installed.Path)
	for _, n := range installed.Notes {
		ui.Info(n)
	}

	if started := manager.Start(); !started.OK && started.Err != "" {
		ui.Warn("could not start it: " + started.Err)
	}

	// Give the daemon a moment to bind its socket before reporting.
	time.Sleep(1500 * time.Millisecond)
	s := manager.Status()
	d := client.DaemonInfo()

	fmt.Println()
	ui.OK(fmt.Sprintf("%s: installed=%t enabled=%t running=%t",
		manager.DisplayName(), s.Installed, s.Enabled, s.Running))
	if d.Alive {
		ui.Info(fmt.Sprintf("daemon running (pid %d)", d.Pid))
	} else {
		ui.Info("daemon not running yet")
	}
	if hint := manager.LogsHint(); hint != "" {
		ui.Info("logs: " + hint)
	}

	// Report whatever was registered before, so an install is never a
	// downgrade in visible state.
	if d.Alive {
		var sessions []session.View
		if err := client.Call("list", ipc.Args{}, &sessions, client.Options{Autostart: false}); err == nil {
			ui.Info(fmt.Sprintf("%d session(s) registered · rcmanager list", len(sessions)))
		}
	}
	return nil
}

func serviceUninstall(flags Flags) error {
	manager, err := managerToActOn(flags)
	if err != nil {
		return err
	}
	before := client.DaemonInfo().Alive
	removed := manager.Uninstall()

	if removed.Removed {
		ui.OK("removed rcmanager from " + manager.DisplayName())
	} else {
		ui.Info(manager.DisplayName() + " had no rcmanager registration")
	}
	for _, n := range removed.Notes {
		ui.Info(n)
	}

	// Only some managers actually own the running process; say what happened
	// rather than assuming.
	after := client.DaemonInfo()
	switch {
	case before && !after.Alive:
		ui.Info("the daemon and its sessions were stopped; the conversations are untouched")
	case after.Alive:
		ui.Info(fmt.Sprintf("the daemon is still running (pid %d) — stop it with `rcmanager daemon --stop`", after.Pid))
	}
	return nil
}

func managerToActOn(flags Flags) (service.Manager, error) {
	if id := flags.Str("manager", ""); id != "" {
		return service.Get(id)
	}
	for _, m := range service.List() {
		if m.Status().Installed {
			return m, nil
		}
	}
	return nil, rcerr.New("rcmanager is not registered with any service manager here.",
		"Install it with `rcmanager service install`, or name one with --manager <id>.")
}

func serviceStatus(flags Flags) error {
	type row struct {
		ID        string `json:"manager"`
		Name      string `json:"name"`
		Available bool   `json:"available"`
		service.Status
	}
	var data []row
	var rows [][]string

	for _, p := range service.ProbeAll() {
		st := p.Manager.Status()
		if st.Detail == "" {
			// A manager that cannot describe itself here still explains why it
			// is unusable, which is the more useful line anyway.
			st.Detail = p.Reason
		}
		data = append(data, row{p.Manager.ID(), p.Manager.DisplayName(), p.Available, st})
		rows = append(rows, []string{
			ansi.Bold(p.Manager.ID()), yesNo(p.Available), yesNo(st.Installed),
			plainYesNo(st.Enabled), yesNo(st.Running), st.Detail,
		})
	}

	if flags.Bool("json") {
		return printJSON(data)
	}
	fmt.Println(format.Table(
		[]string{"Manager", "Usable", "Installed", "Enabled", "Running", "Detail"}, rows))

	d := client.DaemonInfo()
	state := "not running"
	if d.Alive {
		state = "running (pid " + strconv.Itoa(d.Pid) + ")"
	}
	fmt.Println(ansi.Grey("  daemon " + state + " · state " + paths.Get().Home))

	installed := false
	for _, r := range data {
		installed = installed || r.Installed
	}
	if !installed {
		fmt.Println(ansi.Grey("  not registered for boot — run: rcmanager service install"))
	}
	return nil
}

func yesNo(v bool) string {
	if v {
		return ansi.Green("yes")
	}
	return ansi.Grey("no")
}

func plainYesNo(v bool) string {
	if v {
		return "yes"
	}
	return ansi.Grey("no")
}

func servicePrint(flags Flags) error {
	manager, err := resolveManager(flags)
	if err != nil {
		return err
	}
	fmt.Fprintln(os.Stderr, ansi.Grey("# "+manager.DisplayName()+" · "+manager.UnitPath()))
	fmt.Print(manager.Render(service.NewContext()))
	return nil
}

func serviceDetect(flags Flags) error {
	probes := service.ProbeAll()
	if flags.Bool("json") {
		type row struct {
			ID        string `json:"id"`
			Name      string `json:"name"`
			Available bool   `json:"available"`
			Reason    string `json:"reason"`
		}
		var data []row
		for _, p := range probes {
			data = append(data, row{p.Manager.ID(), p.Manager.DisplayName(), p.Available, p.Reason})
		}
		return printJSON(data)
	}

	rows := make([][]string, 0, len(probes))
	for _, p := range probes {
		rows = append(rows, []string{
			ansi.Bold(p.Manager.ID()), p.Manager.DisplayName(), yesNo(p.Available), p.Reason,
		})
	}
	fmt.Println(format.Table([]string{"Manager", "Name", "Usable", "Why"}, rows))

	for _, p := range probes {
		if p.Available {
			fmt.Printf("\n%s would use %s (%s)\n", ansi.Green("→"), ansi.Bold(p.Manager.DisplayName()), p.Manager.ID())
			return nil
		}
	}
	fmt.Printf("\n%s no usable service manager found\n", ansi.Red("✖"))
	return nil
}

func serviceControl(what string, flags Flags) error {
	manager, err := managerToActOn(flags)
	if err != nil {
		return err
	}
	if what == "restart" {
		manager.Stop()
	}
	r := manager.Start()
	if what == "stop" {
		r = manager.Stop()
	}
	if !r.OK && r.Err != "" {
		return rcerr.New(r.Err, "")
	}
	ui.OK(manager.DisplayName() + " " + what + "ed")
	return nil
}
