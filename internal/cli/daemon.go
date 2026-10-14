package cli

import (
	"fmt"
	"os"
	"os/signal"
	"syscall"

	"github.com/dumanhaydar/rcmanager/internal/ansi"
	"github.com/dumanhaydar/rcmanager/internal/client"
	"github.com/dumanhaydar/rcmanager/internal/daemon"
	"github.com/dumanhaydar/rcmanager/internal/ipc"
	"github.com/dumanhaydar/rcmanager/internal/paths"
	"github.com/dumanhaydar/rcmanager/internal/ui"
)

// daemonCmd runs, inspects or stops the supervisor.
func daemonCmd(_ []string, flags Flags) error {
	if flags.Bool("stop") {
		d := client.DaemonInfo()
		if !d.Alive {
			ui.Info("daemon is not running.")
			return nil
		}
		// Stopping the supervisor is not the same as stopping the work. Managed
		// sessions keep running and the next daemon adopts them, unless asked
		// otherwise.
		stopSessions := flags.Bool("with-sessions")
		_ = client.Call("shutdown", ipc.Args{StopSessions: stopSessions}, nil, client.Options{Autostart: false})
		ui.OK(fmt.Sprintf("daemon (pid %d) stopping", d.Pid))
		if stopSessions {
			ui.Info("managed sessions are being stopped too")
		} else {
			ui.Info("managed sessions keep running — `rcmanager stop all` ends them, `--with-sessions` does both")
		}
		return nil
	}

	if flags.Bool("status") {
		d := client.DaemonInfo()
		state := ansi.Grey("stopped")
		if d.Alive {
			state = ansi.Green("running")
		}
		pid := ""
		if d.Pid != 0 {
			pid = fmt.Sprintf(" · pid %d", d.Pid)
		}
		fmt.Println(state + pid)
		fmt.Println(ansi.Grey("socket " + paths.Get().Sock))
		fmt.Println(ansi.Grey("log    " + paths.Get().DaemonLog))
		fmt.Println(ansi.Grey("state  " + paths.Get().Home))
		return nil
	}

	if flags.Bool("detached") {
		d, err := client.EnsureDaemon(true)
		if err != nil {
			return err
		}
		ui.OK(fmt.Sprintf("daemon running (pid %d)", d.Pid))
		return nil
	}

	// Bare `rcmanager daemon` in a terminal: run in the foreground, which is what
	// the service runs too.
	d := daemon.New()
	if err := d.Start(); err != nil {
		return err
	}
	fmt.Printf("rcmanager daemon listening on %s (pid %d)\n", paths.Get().Sock, os.Getpid())

	// SIGTERM here means "the supervisor is going away", not "stop the work":
	// sessions are detached and the next daemon adopts them.
	signals := make(chan os.Signal, 1)
	signal.Notify(signals, syscall.SIGTERM, syscall.SIGINT, syscall.SIGHUP)
	go func() {
		sig := <-signals
		d.Shutdown(sig.String(), false, true)
	}()

	d.Wait()
	return nil
}
