// Package cli is rcmanager's command-line surface.
package cli

import (
	"fmt"
	"os"
	"strings"

	"github.com/dumanhaydar/rcmanager/internal/ansi"
	"github.com/dumanhaydar/rcmanager/internal/paths"
	"github.com/dumanhaydar/rcmanager/internal/ptywrap"
	"github.com/dumanhaydar/rcmanager/internal/rcerr"
	"github.com/dumanhaydar/rcmanager/internal/version"
)

// booleanFlags are the flags that take no value, so `--json` is not mistaken
// for `--json <next-argument>`.
var booleanFlags = map[string]bool{
	"json": true, "follow": true, "f": true, "force": true, "all": true,
	"foreground": true, "detached": true, "stop": true, "status": true,
	"help": true, "h": true, "version": true, "v": true, "no-autostart": true,
	"print": true, "detect": true, "trust": true, "yes": true,
	"with-sessions": true,
}

// Flags are the parsed options of one invocation.
type Flags map[string]string

// Bool reports whether a flag was set.
func (f Flags) Bool(name string) bool {
	v, ok := f[name]
	return ok && v != "false"
}

// Str returns a flag's value, or fallback.
func (f Flags) Str(name, fallback string) string {
	if v, ok := f[name]; ok && v != "" && v != "true" {
		return v
	}
	return fallback
}

// Autostart reports whether the CLI may spawn a daemon (--no-autostart off).
func (f Flags) Autostart() bool { return !f.Bool("no-autostart") }

// Parse splits argv into positional arguments and flags, accepting
// `--flag`, `--key value`, `--key=value` and `-f`.
func Parse(argv []string) ([]string, Flags) {
	var args []string
	flags := Flags{}

	for i := 0; i < len(argv); i++ {
		token := argv[i]
		if !strings.HasPrefix(token, "-") || token == "-" {
			args = append(args, token)
			continue
		}

		raw := strings.TrimLeft(token, "-")
		if eq := strings.IndexByte(raw, '='); eq != -1 {
			flags[raw[:eq]] = raw[eq+1:]
			continue
		}
		if booleanFlags[raw] {
			flags[raw] = "true"
			continue
		}
		if i+1 >= len(argv) || strings.HasPrefix(argv[i+1], "--") {
			flags[raw] = "true"
			continue
		}
		flags[raw] = argv[i+1]
		i++
	}
	return args, flags
}

type handler func(args []string, flags Flags) error

func commands() map[string]handler {
	return map[string]handler{
		"setup":   setup,
		"upgrade": upgrade,

		"start":   start,
		"stop":    stop,
		"restart": restart,
		"remove":  remove,
		"delete":  remove,
		"rm":      remove,
		"enable":  enable,
		"disable": disable,

		"list":     list,
		"ls":       list,
		"status":   status,
		"info":     info,
		"show":     info,
		"describe": info,
		"logs":     logs,
		"log":      logs,

		"config":    configCmd,
		"reload":    reload,
		"trust":     trust,
		"untrust":   untrust,
		"providers": providers,
		"discover":  discover,

		"daemon":  daemonCmd,
		"service": serviceCmd,

		"codex-supervisor": codexSupervisor,
	}
}

func helpText() string {
	g := ansi.Grey
	b := ansi.Bold
	return `
` + b("rcmanager") + ` — Remote Control Manager  ` + g("v"+version.Version) + `

  A supervisor for AI coding-agent remote-control sessions.
  Your service manager manages rcmanager; rcmanager manages individual agent sessions.

` + b("USAGE") + `
  rcmanager setup                          ` + g("first time here? start with this") + `
  rcmanager start <name> [path] [--provider <provider>]
  rcmanager start <ecosystem.json>
  rcmanager <command> [args]

` + b("SETUP") + `
  setup                      Guided: provider, boot service, first sessions
  upgrade                    After replacing the binary: refresh the unit and
                             restart the daemon ` + g("(sessions keep running)") + `

` + b("SESSIONS") + `
  start <name> <path>        Create and start a new session in <path>
  start <name>               Resume an existing session: registry first, then
                             provider discovery by name (never creates one)
  start all                  Start every enabled session
  start <file.json>          Register and start everything in an ecosystem file
  stop <name> | stop all     Stop gracefully (SIGTERM, then SIGKILL)
  restart <name> | all       Stop then start
  remove <name>              Unregister from rcmanager ` + g("(never deletes the conversation)") + `
  enable <name>              Mark autostart on, and start it now
  disable <name>             Stop it and keep it stopped across restarts

` + b("INSPECTION") + `
  list, ls                   Table of every registered session
  status <name>              One-line status
  info <name>                Everything rcmanager knows, plus provider limitations
  logs [name] [-n N] [-f]    Per-session logs ` + g("(-f follows)") + `
  discover [name]            Sessions the provider knows about locally
  providers                  Installed provider CLIs and their limitations

` + b("CONFIGURATION") + `
  config list                Show all settings
  config get <key>           e.g. rcmanager config get defaultProvider
  config set <key> <value>   e.g. rcmanager config set defaultProvider claude
  config unset <key>         Restore a default
  reload                     Re-read config + registry ` + g("(running sessions keep running)") + `
  trust <path>               Let the provider act in a directory ` + g("(a security decision)") + `
                             ` + g("rcmanager start also offers this when a session needs it") + `
  untrust <path>             Withdraw it

` + b("SERVICE") + `  ` + g("(start rcmanager at boot)") + `
  service install            Detect this machine's service manager and register
  service status             Where rcmanager is registered, and whether it is running
  service detect             What managers are available here, and which wins
  service print              Show the generated unit without installing it
  service start|stop|restart Control the installed service
  service uninstall          Deregister ` + g("(sessions are not deleted)") + `
  ` + g("--manager <id>  pick one explicitly: systemd-user, launchd,") + `
  ` + g("                systemd-system, openrc, cron") + `

` + b("DAEMON") + `  ` + g("(stopping the supervisor does not stop your sessions)") + `
  daemon                     Run the supervisor in the foreground ` + g("(what the service runs)") + `
  daemon --detached          Start it in the background
  daemon --status            Is it running?
  daemon --stop              Stop it; sessions keep running and are re-adopted
  daemon --stop --with-sessions   Stop it and every session it manages

` + b("GLOBAL FLAGS") + `
  --provider <id>            Override the configured default provider
  --json                     Machine-readable output
  --force                    Restart a session that is already running
  --no-autostart             Never auto-spawn the daemon
  --trust                    Grant workspace trust without asking ` + g("(for scripts)") + `

` + b("ENVIRONMENT") + `
  RCMANAGER_HOME                 Runtime state and logs ` + g("(default ~/.rcmanager)") + `
  RCMANAGER_CLAUDE_BIN           Path to the claude CLI  ` + g(`(default "claude")`) + `
  RCMANAGER_CODEX_BIN            Path to the codex CLI   ` + g(`(default "codex")`) + `
`
}

// Main runs one rcmanager invocation and returns its exit code.
func Main(argv []string) int {
	// The pty wrapper carries another command's argv, flags and all, so it is
	// dispatched before anything tries to interpret those flags as rcmanager's.
	if len(argv) > 0 && argv[0] == ptywrap.Subcommand {
		return ptywrap.Run(argv[1:])
	}

	args, flags := Parse(argv)
	name := ""
	if len(args) > 0 {
		name, args = args[0], args[1:]
	}

	if flags.Bool("version") || flags.Bool("v") || name == "version" {
		fmt.Println(version.Version)
		return 0
	}
	if name == "" || flags.Bool("help") || flags.Bool("h") || name == "help" {
		fmt.Println(helpText())
		if name == "" {
			return 1
		}
		return 0
	}

	run, ok := commands()[name]
	if !ok {
		fmt.Fprintf(os.Stderr, "%s Unknown command %q.\n", ansi.Red("✖"), name)
		fmt.Fprintf(os.Stderr, "  Run %s for usage.\n", ansi.Bold("rcmanager help"))
		return 1
	}

	if err := run(args, flags); err != nil {
		report(err)
		return 1
	}
	return 0
}

func report(err error) {
	msg := err.Error()
	if strings.Contains(msg, paths.Get().Sock) && strings.Contains(msg, "connect") {
		fmt.Fprintf(os.Stderr, "%s The rcmanager daemon is not reachable.\n", ansi.Red("✖"))
		fmt.Fprintf(os.Stderr, "  %s\n", ansi.Grey("Start it with `rcmanager daemon --detached`, or start the installed service."))
		return
	}
	fmt.Fprintf(os.Stderr, "%s %s\n", ansi.Red("✖"), msg)
	if hint := rcerr.HintOf(err); hint != "" {
		fmt.Fprintf(os.Stderr, "  %s\n", ansi.Grey(hint))
	}
}
