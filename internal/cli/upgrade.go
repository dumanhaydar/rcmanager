package cli

import (
	"fmt"
	"time"

	"github.com/dumanhaydar/rcmanager/internal/ansi"
	"github.com/dumanhaydar/rcmanager/internal/client"
	"github.com/dumanhaydar/rcmanager/internal/ipc"
	"github.com/dumanhaydar/rcmanager/internal/service"
	"github.com/dumanhaydar/rcmanager/internal/session"
	"github.com/dumanhaydar/rcmanager/internal/ui"
	"github.com/dumanhaydar/rcmanager/internal/version"
)

// upgrade is what to run after replacing the binary.
//
// A running daemon keeps executing the code it started with, so a new binary
// changes nothing until the supervisor is restarted. That restart is safe —
// sessions detach and are re-adopted — but it has to actually happen, and the
// installed unit also needs refreshing in case the binary moved.
func upgrade(_ []string, flags Flags) error {
	fmt.Printf("%s %s\n", ansi.Bold("rcmanager"), ansi.Grey("v"+version.Version))

	before := client.Handshake(1500 * time.Millisecond)
	if before != nil && before.Version != version.Version {
		ui.Info(fmt.Sprintf("daemon is running %s; this CLI is %s", before.Version, version.Version))
	}

	var running []session.View
	if before != nil {
		_ = client.Call("list", ipc.Args{}, &running, client.Options{Autostart: false})
		live := 0
		for _, s := range running {
			if s.Status == session.Online || s.Status == session.Launching {
				live++
			}
		}
		ui.Info(fmt.Sprintf("%d session(s) registered, %d running — they keep running through this", len(running), live))
	}

	// Refresh the unit first: it records the absolute path of the binary, which
	// a reinstall elsewhere would have changed.
	for _, m := range service.List() {
		if !m.Status().Installed {
			continue
		}
		ui.Info("refreshing " + m.DisplayName())
		if installed, err := m.Install(service.NewContext()); err != nil {
			ui.Warn(err.Error())
		} else {
			ui.OK("refreshed " + installed.Path)
			for _, n := range installed.Notes {
				ui.Info(n)
			}
		}
		break
	}

	if before == nil {
		ui.Info("no daemon was running; starting one")
		d, err := client.EnsureDaemon(true)
		if err != nil {
			return err
		}
		ui.OK(fmt.Sprintf("daemon running (pid %d)", d.Pid))
		return nil
	}

	// Detach rather than stop: the sessions outlive the supervisor and the
	// successor adopts them.
	_ = client.Call("shutdown", ipc.Args{StopSessions: false}, nil, client.Options{Autostart: false})
	ui.Info("old daemon stopping; sessions left running")

	deadline := time.Now().Add(20 * time.Second)
	for time.Now().Before(deadline) {
		time.Sleep(200 * time.Millisecond)
		if client.Handshake(500*time.Millisecond) == nil {
			break
		}
	}

	d, err := client.EnsureDaemon(true)
	if err != nil {
		return err
	}
	ui.OK(fmt.Sprintf("daemon running (pid %d) on %s", d.Pid, version.Version))

	// Wait for the handover to finish before reporting, so the table does not
	// catch a session mid-adoption.
	deadline = time.Now().Add(60 * time.Second)
	for time.Now().Before(deadline) {
		if pong := client.Handshake(1500 * time.Millisecond); pong != nil && pong.Ready {
			break
		}
		time.Sleep(250 * time.Millisecond)
	}
	fmt.Println()
	return list(nil, flags)
}
