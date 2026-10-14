package service

import (
	"os/user"
	"regexp"
	"runtime"
	"strings"

	"github.com/dumanhaydar/rcmanager/internal/rcerr"
)

// cron @reboot — the universal fallback.
//
// Containers, WSL without systemd, and minimal images often have no service
// manager at all but do have cron. @reboot gets rcmanager up after a restart;
// rcmanager's own daemon then supervises the sessions. There is no
// restart-on-crash here, so this is the last resort, not a peer of systemd.

const cronMarker = "# rcmanager (managed by `rcmanager service`) — do not edit this line"

func cronEntry(ctx Context) string {
	env := strings.Join(ctx.EnvLines(func(k, v string) string { return k + "=" + v }), " ")
	if env != "" {
		env += " "
	}
	return cronMarker + "\n@reboot RCMANAGER_HOME=" + ctx.RcmanHome + " PATH=" + ctx.Path + " " +
		env + ctx.Entrypoint + " daemon --detached\n"
}

var blankRuns = regexp.MustCompile(`\n{3,}`)

// currentCrontab is the user's crontab minus any block rcmanager previously wrote.
func currentCrontab() string {
	r := Exec("crontab", []string{"-l"}, "")
	text := ""
	if r.OK {
		text = r.Out
	}
	lines := strings.Split(text, "\n")
	var kept []string
	for i, line := range lines {
		if line == cronMarker {
			continue
		}
		if i > 0 && lines[i-1] == cronMarker {
			continue
		}
		kept = append(kept, line)
	}
	return strings.TrimSpace(blankRuns.ReplaceAllString(strings.Join(kept, "\n"), "\n\n"))
}

type cron struct{}

func init() { register(cron{}) }

func (cron) ID() string          { return "cron" }
func (cron) DisplayName() string { return "cron (@reboot)" }
func (cron) Priority() int       { return 10 }

func (cron) Detect() Availability {
	if runtime.GOOS == "windows" {
		return Availability{false, "not supported on Windows"}
	}
	if Which("crontab") == "" {
		return Availability{false, "crontab not found"}
	}
	return Availability{true, "crontab is available (no restart-on-crash)"}
}

func (cron) UnitPath() string {
	name := "this user"
	if u, err := user.Current(); err == nil {
		name = u.Username
	}
	return "crontab of " + name
}

func (cron) Render(ctx Context) string { return cronEntry(ctx) }

func (m cron) Install(ctx Context) (Installed, error) {
	parts := []string{}
	if before := currentCrontab(); before != "" {
		parts = append(parts, before)
	}
	parts = append(parts, strings.TrimSpace(cronEntry(ctx)))
	next := strings.Join(parts, "\n\n") + "\n"

	if w := Exec("crontab", []string{"-"}, next); !w.OK {
		return Installed{}, rcerr.New("could not write the crontab: "+w.Err, "")
	}
	return Installed{
		Path:  m.UnitPath(),
		Notes: []string{"cron only starts rcmanager at boot — it will not restart it if it crashes"},
	}, nil
}

func (cron) Uninstall() Removed {
	before := Exec("crontab", []string{"-l"}, "").Out
	if !strings.Contains(before, cronMarker) {
		return Removed{}
	}
	if next := currentCrontab(); next != "" {
		Exec("crontab", []string{"-"}, next+"\n")
	} else {
		Exec("crontab", []string{"-r"}, "")
	}
	return Removed{Removed: true}
}

func (cron) Status() Status {
	installed := strings.Contains(Exec("crontab", []string{"-l"}, "").Out, cronMarker)
	detail := "no @reboot entry"
	if installed {
		detail = "@reboot entry present"
	}
	return Status{
		Installed: installed,
		// cron cannot report this; the daemon status covers it.
		Running: false,
		Enabled: installed,
		Detail:  detail,
	}
}

func (cron) Start() Run       { return Run{OK: true} }
func (cron) Stop() Run        { return Run{OK: true} }
func (cron) LogsHint() string { return "rcmanager daemon --status" }
