// Package ptywrap gives a managed session a real terminal, without a native
// dependency and without tying its lifetime to the daemon.
//
// Some agent CLIs only expose a feature from their interactive UI (Claude's
// `--resume <id> --remote-control` is one) and refuse to run when stdin is not
// a terminal. The obvious answer — have the daemon allocate the pty — is wrong
// here: the daemon would hold the master, and closing it on shutdown hangs up
// the terminal and kills the agent, which is the one thing rcmanager promises not
// to do.
//
// So the pty belongs to a separate, tiny process: rcmanager re-executes itself as
// `rcmanager pty-wrapper -- <command> …`, that process allocates the pty and
// pumps bytes between it and its own stdio — which are the fifo and the raw
// files from package sio, and therefore survive the daemon. The wrapper also
// keeps the signature interlock intact, because the real command's argv is
// still right there in its own.
//
// script(1) would do the same job, but BSD and util-linux disagree on its
// flags and macOS's version refuses to start at all when its stdin is not a
// terminal. Owning the twenty lines of ioctl is less trouble.
package ptywrap

import (
	"os"
	"strings"
)

// Launch is a command line, possibly wrapped in something else.
type Launch struct {
	Command string
	Args    []string
}

// Subcommand is the hidden command the wrapper process runs as.
const Subcommand = "pty-wrapper"

// ShellQuote escapes parts for a POSIX shell, so a path with spaces survives.
// Kept for diagnostics: rcmanager logs the command it launched.
func ShellQuote(parts []string) string {
	quoted := make([]string, 0, len(parts))
	for _, p := range parts {
		quoted = append(quoted, "'"+strings.ReplaceAll(p, "'", `'\''`)+"'")
	}
	return strings.Join(quoted, " ")
}

// Wrap returns the launch that runs command under a pty.
func Wrap(command string, args []string) Launch {
	self, err := os.Executable()
	if err != nil {
		self = "rcmanager"
	}
	full := append([]string{Subcommand, "--", command}, args...)
	return Launch{Command: self, Args: full}
}

// Env is the terminal environment a pty session needs. A CLI that sees no
// TERM (or CI=1) degrades its UI, and rcmanager wants the interactive one.
func Env() map[string]string {
	term := os.Getenv("TERM")
	if term == "" || term == "dumb" {
		term = "xterm-256color"
	}
	return map[string]string{
		"TERM":        term,
		"COLUMNS":     "120",
		"LINES":       "40",
		"CI":          "",
		"FORCE_COLOR": "0", // logs are stripped anyway; less noise on the wire
	}
}
