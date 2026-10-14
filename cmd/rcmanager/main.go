// Command rcmanager supervises AI coding-agent remote-control sessions.
//
// The OS service manager keeps rcmanager running, and rcmanager keeps each
// agent session running. It never touches repositories, branches or agent
// conversations.
package main

import (
	"os"

	"github.com/dumanhaydar/rcmanager/internal/cli"

	// Providers register themselves; importing them here is what makes them
	// available to the registry.
	_ "github.com/dumanhaydar/rcmanager/internal/provider/claude"
	_ "github.com/dumanhaydar/rcmanager/internal/provider/codex"
)

func main() {
	os.Exit(cli.Main(os.Args[1:]))
}
