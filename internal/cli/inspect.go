package cli

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
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
	"github.com/dumanhaydar/rcmanager/internal/session"
	"github.com/dumanhaydar/rcmanager/internal/ui"
)

// ---------------------------------------------------------------------- list

func list(_ []string, flags Flags) error {
	var sessions []session.View
	if err := client.Call("list", ipc.Args{}, &sessions, client.Options{Autostart: flags.Autostart()}); err != nil {
		return err
	}

	if flags.Bool("json") {
		return printJSON(sessions)
	}
	if len(sessions) == 0 {
		ui.Info("no sessions registered yet.")
		ui.Info("register one with:  rcmanager start <name> <path>")
		return nil
	}

	rows := make([][]string, 0, len(sessions))
	for _, s := range sessions {
		status := string(s.Status)
		if !s.Enabled {
			status = "disabled"
		}
		pid := "-"
		if s.Pid != 0 {
			pid = strconv.Itoa(s.Pid)
		}
		id := "-"
		if s.SessionID != "" {
			id = short(s.SessionID, 8)
		}
		uptime := "-"
		if s.UptimeMs > 0 {
			uptime = format.Duration(s.UptimeMs)
		}
		rows = append(rows, []string{
			ansi.Bold(s.Name), s.Provider, format.PaintStatus(status),
			pid, id, format.EllipsizeLeft(s.Cwd, 34), uptime,
		})
	}
	fmt.Println(format.Table(
		[]string{"Name", "Provider", "Status", "PID", "Session ID", "Directory", "Uptime"}, rows))

	for _, s := range sessions {
		if s.Message != "" {
			fmt.Println(ansi.Grey("  " + s.Name + ": " + s.Message))
		}
	}

	d := client.DaemonInfo()
	pid := "-"
	if d.Pid != 0 {
		pid = strconv.Itoa(d.Pid)
	}
	fmt.Println(ansi.Grey("  daemon pid " + pid + " · state " + paths.Get().Home))
	return nil
}

func short(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n]
}

// ----------------------------------------------------------------- status/info

func status(args []string, flags Flags) error {
	name, err := requireName(args, "status")
	if err != nil {
		return err
	}
	var s session.View
	if err := client.Call("describe", ipc.Args{Name: name}, &s, client.Options{Autostart: flags.Autostart()}); err != nil {
		return err
	}
	if flags.Bool("json") {
		return printJSON(s)
	}

	state := string(s.Status)
	if !s.Enabled {
		state = "disabled"
	}
	line := ansi.Bold(s.Name) + "  " + format.PaintStatus(state)
	if s.Pid != 0 {
		line += fmt.Sprintf("  pid %d", s.Pid)
	}
	if s.UptimeMs > 0 {
		line += "  up " + format.Duration(s.UptimeMs)
	}
	fmt.Println(line)
	if s.Message != "" {
		fmt.Println(ansi.Grey("  " + s.Message))
	}
	if s.RemoteURL != "" {
		fmt.Println("  " + ansi.Cyan(s.RemoteURL))
	}
	return nil
}

func info(args []string, flags Flags) error {
	name, err := requireName(args, "info")
	if err != nil {
		return err
	}
	var s session.View
	if err := client.Call("describe", ipc.Args{Name: name}, &s, client.Options{Autostart: flags.Autostart()}); err != nil {
		return err
	}
	if flags.Bool("json") {
		return printJSON(s)
	}

	p, err := provider.Get(s.Provider)
	if err != nil {
		return err
	}
	detect := p.Detect()

	state := string(s.Status)
	if !s.Enabled {
		state = "disabled"
	}
	providerLine := fmt.Sprintf("%s (%s)", p.DisplayName(), s.Provider)
	if detect.Version != "" {
		providerLine += " · " + detect.Version
	}
	started := "-"
	if s.StartedAt != "" {
		started = s.StartedAt + " (" + format.Duration(s.UptimeMs) + ")"
	}
	remote := s.RemoteURL
	if remote == "" {
		remote = "(not seen yet — check the log)"
	}
	registered := "via CLI"
	if s.Source != "" {
		registered = "from " + s.Source
	}

	rows := [][2]string{
		{"name", s.Name},
		{"provider", providerLine},
		{"status", format.PaintStatus(state)},
		{"enabled", strconv.FormatBool(s.Enabled)},
		{"pid", dash(s.Pid)},
		{"session id", orDash(s.SessionID)},
		{"directory", s.Cwd},
		{"started", started},
		{"restarts", strconv.Itoa(s.Restarts)},
		{"remote url", remote},
		{"log (out)", s.Logs.Out},
		{"log (err)", s.Logs.Err},
		{"registered", registered},
	}
	if s.Note != "" {
		rows = append(rows, [2]string{"note", s.Note})
	}
	if s.Scope != "" {
		rows = append(rows, [2]string{"cgroup scope", s.Scope})
	}
	if s.Adopted {
		rows = append(rows, [2]string{"adopted", "yes — running since before this daemon started"})
	}
	if len(s.NeedsRestart) > 0 {
		rows = append(rows, [2]string{"pending",
			strings.Join(s.NeedsRestart, ", ") + " changed in the registry — rcmanager restart " + s.Name})
	}
	if s.LastExit != nil {
		sig := s.LastExit.Signal
		if sig == "" {
			sig = "-"
		}
		rows = append(rows, [2]string{"last exit",
			fmt.Sprintf("code=%d signal=%s at %s", s.LastExit.Code, sig, s.LastExit.At)})
	}
	fmt.Println(format.Pairs(rows))

	fmt.Printf("\n%s\n", ansi.Bold(p.DisplayName()+" limitations"))
	for _, l := range p.Limitations() {
		fmt.Println("  " + ansi.Grey("-") + " " + l)
	}
	return nil
}

func dash(n int) string {
	if n == 0 {
		return "-"
	}
	return strconv.Itoa(n)
}

func orDash(s string) string {
	if s == "" {
		return "-"
	}
	return s
}

// ---------------------------------------------------------------------- logs

func logs(args []string, flags Flags) error {
	name := ""
	if len(args) > 0 {
		name = args[0]
	}
	lines := 30
	if v := flags.Str("lines", flags.Str("n", "")); v != "" {
		if n, err := strconv.Atoi(v); err == nil && n > 0 {
			lines = n
		}
	}
	follow := flags.Bool("follow") || flags.Bool("f")

	type target struct {
		label string
		file  string
	}
	var candidates []target

	if name != "" {
		lp := paths.LogPaths(name)
		if registry.Get(name) == nil {
			if _, err := os.Stat(lp.Out); err != nil {
				return rcerr.Newf("Run `rcmanager list` to see registered sessions.", "No logs for %q.", name)
			}
		}
		candidates = append(candidates,
			target{name, lp.Out}, target{name + ":stderr", lp.Err})
	} else {
		entries, err := registry.Load()
		if err != nil {
			return err
		}
		if len(entries) == 0 {
			return rcerr.New("No sessions registered, so there are no logs yet.", "")
		}
		for _, e := range entries {
			lp := paths.LogPaths(e.Name)
			candidates = append(candidates,
				target{e.Name, lp.Out}, target{e.Name + ":stderr", lp.Err})
		}
	}

	var files []target
	for _, c := range candidates {
		if st, err := os.Stat(c.file); err == nil && st.Size() > 0 {
			files = append(files, c)
		}
	}
	if len(files) == 0 {
		ui.Info("no log output yet.")
		return nil
	}

	if !follow {
		for _, f := range files {
			fmt.Printf("\n%s\n", ansi.Bold("── "+f.label+" "+ansi.Grey(f.file)))
			text, err := tail(f.file, lines)
			if err != nil {
				continue
			}
			fmt.Print(text)
		}
		return nil
	}

	// Delegate following to `tail -F`: it handles rotation and is already there.
	argv := []string{"-n", strconv.Itoa(lines), "-F"}
	for _, f := range files {
		argv = append(argv, f.file)
	}
	cmd := exec.Command("tail", argv...)
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr
	cmd.Stdin = os.Stdin
	return cmd.Run()
}

// tail returns the last n lines without reading the whole file: a session log
// runs to maxLogBytes (10 MB by default) and `rcmanager logs` should not pull all
// of it into memory to show thirty lines.
func tail(file string, lines int) (string, error) {
	fd, err := os.Open(file)
	if err != nil {
		return "", err
	}
	defer fd.Close()

	st, err := fd.Stat()
	if err != nil {
		return "", err
	}
	size := st.Size()
	want := int64(max(64*1024, lines*512))
	if want > size {
		want = size
	}
	buf := make([]byte, want)
	if _, err := fd.ReadAt(buf, size-want); err != nil && err.Error() != "EOF" {
		return "", err
	}

	// The window may start mid-line; drop that fragment unless we read it all.
	parts := strings.Split(string(buf), "\n")
	if want < size && len(parts) > 1 {
		parts = parts[1:]
	}
	if len(parts) > lines {
		parts = parts[len(parts)-lines:]
	}
	return strings.Join(parts, "\n") + "\n", nil
}

// -------------------------------------------------------------------- config

func configCmd(args []string, _ Flags) error {
	action := ""
	if len(args) > 0 {
		action = args[0]
	}
	key := ""
	if len(args) > 1 {
		key = args[1]
	}

	switch action {
	case "", "list", "ls":
		cfg := config.Load()
		rows := [][2]string{}
		for _, k := range config.Keys() {
			value, _ := config.ValueOf(cfg, k)
			text := fmt.Sprint(value)
			if config.IsDefault(cfg, k) {
				text += ansi.Grey("  (default)")
			}
			rows = append(rows, [2]string{k, text})
		}
		fmt.Println(format.Pairs(rows))
		fmt.Println(ansi.Grey("\nstored in " + config.File()))
		return nil

	case "get":
		if key == "" {
			return rcerr.New("Usage: rcmanager config get <key>", "")
		}
		value, err := config.ValueOf(config.Load(), key)
		if err != nil {
			return err
		}
		fmt.Println(value)
		return nil

	case "set":
		if key == "" || len(args) < 3 {
			return rcerr.New("Usage: rcmanager config set <key> <value>", "")
		}
		value, err := config.Set(key, strings.Join(args[2:], " "))
		if err != nil {
			return err
		}
		if key == "defaultProvider" {
			// Fail loudly on a typo, after storing nothing invalid silently.
			if _, err := provider.Get(fmt.Sprint(value)); err != nil {
				return err
			}
		}
		ui.OK(fmt.Sprintf("%s = %v", key, value))
		// A missing daemon is fine here: the next one loads the file anyway.
		_ = client.Call("reload", ipc.Args{}, nil, client.Options{Autostart: false, Timeout: 30 * time.Second})
		return nil

	case "unset":
		if key == "" {
			return rcerr.New("Usage: rcmanager config unset <key>", "")
		}
		value, err := config.Unset(key)
		if err != nil {
			return err
		}
		ui.OK(fmt.Sprintf("%s reset to default (%v)", key, value))
		return nil
	}
	return rcerr.Newf("Use: list | get | set | unset", "Unknown config action %q.", action)
}

// ---------------------------------------------------------------------- trust

func trust(args []string, flags Flags) error   { return setTrust(args, flags, true) }
func untrust(args []string, flags Flags) error { return setTrust(args, flags, false) }

// setTrust grants or revokes the provider's workspace trust for a directory.
//
// Trust is what lets an agent act on those files, so rcmanager never infers it
// from a session start — it is only ever this command, typed on purpose.
func setTrust(args []string, flags Flags, grant bool) error {
	verb := "untrust"
	if grant {
		verb = "trust"
	}
	if len(args) == 0 {
		return rcerr.New("Usage: rcmanager "+verb+" <path> [--provider <provider>]", "")
	}
	dir, err := filepath.Abs(args[0])
	if err != nil {
		return err
	}
	st, err := os.Stat(dir)
	if err != nil {
		return rcerr.New("Directory does not exist: "+dir, "")
	}
	if !st.IsDir() {
		return rcerr.New("Not a directory: "+dir, "")
	}

	id := flags.Str("provider", config.Load().DefaultProvider)
	p, err := provider.Get(id)
	if err != nil {
		return err
	}
	keeper, ok := p.(provider.TrustKeeper)
	if !ok {
		return rcerr.Newf("Only providers that keep per-directory trust support this.",
			"%s has no workspace-trust record for rcmanager to change.", p.DisplayName())
	}

	if before := keeper.TrustState(dir); before.Trusted == grant {
		word := "does not trust"
		if grant {
			word = "already trusts"
		}
		ui.Info(fmt.Sprintf("%s %s %s", p.DisplayName(), word, dir))
		return nil
	}

	result := keeper.SetTrust(dir, grant)
	if !result.OK {
		return rcerr.New(result.Error, "")
	}
	word := "does not trust"
	if grant {
		word = "trusts"
	}
	ui.OK(fmt.Sprintf("%s now %s %s", p.DisplayName(), word, ansi.Bold(dir)))
	ui.Info("recorded in " + result.File + backupNote(result.Backup))
	if grant {
		ui.Info("this lets an agent read and run against everything in that directory")
	}
	return nil
}

// ----------------------------------------------------------------- providers

func providers(_ []string, flags Flags) error {
	type row struct {
		ID           string             `json:"id"`
		Name         string             `json:"name"`
		Detection    provider.Detection `json:"cli"`
		Capabilities []string           `json:"capabilities"`
		Limitations  []string           `json:"limitations"`
	}
	var data []row
	var rows [][]string

	for _, p := range provider.List() {
		d := p.Detect()
		cli := ansi.Red("not found")
		if d.Available {
			label := d.Version
			if label == "" {
				label = "yes"
			}
			cli = ansi.Green(label)
		}
		caps := p.Capabilities().Names()
		data = append(data, row{p.ID(), p.DisplayName(), d, caps, p.Limitations()})
		rows = append(rows, []string{ansi.Bold(p.ID()), p.DisplayName(), cli, strings.Join(caps, ", ")})
	}

	if flags.Bool("json") {
		return printJSON(data)
	}
	fmt.Println(format.Table([]string{"Provider", "Name", "CLI", "Capabilities"}, rows))
	for _, p := range provider.List() {
		fmt.Printf("\n%s\n", ansi.Bold(p.DisplayName()+" limitations"))
		for _, l := range p.Limitations() {
			fmt.Println("  " + ansi.Grey("-") + " " + l)
		}
	}
	return nil
}

// -------------------------------------------------------------------- discover

func discover(args []string, flags Flags) error {
	name := ""
	if len(args) > 0 {
		name = args[0]
	}
	p, err := provider.Get(flags.Str("provider", config.Load().DefaultProvider))
	if err != nil {
		return err
	}
	found := p.Discover(name)

	if flags.Bool("json") {
		return printJSON(found)
	}
	if len(found) == 0 {
		suffix := ""
		if name != "" {
			suffix = fmt.Sprintf(" named %q", name)
		}
		ui.Info(fmt.Sprintf("no %s sessions%s found locally.", p.DisplayName(), suffix))
		return nil
	}

	limit := len(found)
	if !flags.Bool("all") && limit > 25 {
		limit = 25
	}
	rows := make([][]string, 0, limit)
	for _, s := range found[:limit] {
		label := s.Name
		if label == "" {
			label = ansi.Grey("(unnamed)")
		}
		cwd := s.Cwd
		if cwd == "" {
			cwd = "-"
		}
		rows = append(rows, []string{
			short(s.SessionID, 8), label, format.EllipsizeLeft(cwd, 40), format.Ago(s.LastActive),
		})
	}
	fmt.Println(format.Table([]string{"Session ID", "Name", "Directory", "Last active"}, rows))
	return nil
}
