package daemon

import (
	"os"
	"os/signal"
	"path/filepath"
	"runtime/pprof"
	"syscall"
	"time"

	"github.com/dumanhaydar/rcmanager/internal/paths"
)

// profileWindow is how long a profile runs for once asked.
const profileWindow = 30 * time.Second

// watchProfileSignal writes a CPU profile when the daemon is sent SIGUSR1.
//
// A supervisor's performance questions are about a process that is already
// running, on a machine that may not be yours, in a state you cannot
// reproduce on demand — so the profiler has to be reachable from the outside
// without a port, a flag or a restart:
//
//	kill -USR1 $(rcmanager daemon --status | sed -n 's/.*pid //p')
//
// The profile lands in $RCMANAGER_HOME and is read with
// `go tool pprof -top <file>`. Nothing is collected until asked.
func (d *Daemon) watchProfileSignal() {
	signals := make(chan os.Signal, 1)
	signal.Notify(signals, syscall.SIGUSR1)

	go func() {
		for range signals {
			file := filepath.Join(paths.Get().Home,
				"profile-"+time.Now().UTC().Format("20060102T150405Z")+".cpu")
			fd, err := os.OpenFile(file, os.O_CREATE|os.O_TRUNC|os.O_WRONLY, 0o600)
			if err != nil {
				d.line("profile: " + err.Error())
				continue
			}
			if err := pprof.StartCPUProfile(fd); err != nil {
				d.line("profile: " + err.Error())
				_ = fd.Close()
				continue
			}
			d.line("profiling for " + profileWindow.String() + " into " + file)
			time.Sleep(profileWindow)
			pprof.StopCPUProfile()
			_ = fd.Close()
			d.line("profile written to " + file)
		}
	}()
}
