package cli

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/dumanhaydar/rcmanager/internal/ansi"
	"github.com/dumanhaydar/rcmanager/internal/config"
	"github.com/dumanhaydar/rcmanager/internal/daemon"
	"github.com/dumanhaydar/rcmanager/internal/format"
	"github.com/dumanhaydar/rcmanager/internal/provider"
	"github.com/dumanhaydar/rcmanager/internal/rcerr"
	"github.com/dumanhaydar/rcmanager/internal/registry"
)

// target is a concrete registry entry, plus where it came from.
type target struct {
	entry      *registry.Entry
	origin     string // new | registry | discovered
	discovered *provider.Discovered
	duplicates []provider.Discovered
}

// resolveTarget turns `rcmanager start <name> [path]` into a concrete registry
// entry.
//
// With a path  -> create and start a new session there.
// Without one  -> the registry first, then provider discovery. rcmanager never
//
//	silently creates a new session in this branch.
func resolveTarget(name, dir, providerID string, quiet bool) (*target, error) {
	if err := registry.AssertValidName(name); err != nil {
		return nil, err
	}
	cfg := config.Load()

	// 1. New session: an explicit path is the only way to create one.
	if dir != "" {
		cwd, err := filepath.Abs(dir)
		if err != nil {
			return nil, err
		}
		st, err := os.Stat(cwd)
		if err != nil {
			return nil, rcerr.New("Directory does not exist: "+cwd, "")
		}
		if !st.IsDir() {
			return nil, rcerr.New("Not a directory: "+cwd, "")
		}

		id := providerID
		if id == "" {
			id = cfg.DefaultProvider
		}
		p, err := provider.Get(id)
		if err != nil {
			return nil, err
		}
		if !p.Capabilities().CreateNew {
			hint := ""
			if l := p.Limitations(); len(l) > 0 {
				hint = l[0]
			}
			return nil, rcerr.Newf(hint, "Provider %q cannot create new sessions.", p.ID())
		}

		existing := registry.Get(name)
		if existing != nil && existing.Cwd != cwd && !quiet {
			fmt.Fprintln(os.Stderr, ansi.Yellow(fmt.Sprintf(
				"! %q was registered at %s; re-registering at %s.", name, existing.Cwd, cwd)))
		}

		// Running the same `start <name> <path>` twice is the same session, not
		// a request for a new one. Minting a fresh id here used to rewrite the
		// registry under a session that was already running — it kept its
		// conversation, the registry pointed at one that did not exist, and the
		// next restart quietly began an empty conversation instead of resuming.
		if existing != nil && existing.Cwd == cwd && existing.Provider == p.ID() {
			return &target{entry: existing, origin: "registry"}, nil
		}

		// Fix the session id now rather than letting the provider invent one
		// per launch. Without it every restart would be a different
		// conversation — which is how "create a session" quietly became
		// "create an environment and hope".
		sessionID := ""
		if p.Capabilities().ResumeBySessionID {
			sessionID = daemon.NewUUID()
		}
		return &target{
			entry: &registry.Entry{
				Name:      name,
				Provider:  p.ID(),
				SessionID: sessionID,
				Cwd:       cwd,
				Enabled:   true,
			},
			origin: "new",
		}, nil
	}

	// 2. Already registered: resume that exact session.
	registered := registry.Get(name)
	if registered != nil && (providerID == "" || registered.Provider == providerID) {
		return &target{entry: registered, origin: "registry"}, nil
	}

	// 3. Discovery, against the provider's own local session metadata only.
	id := providerID
	if id == "" && registered != nil {
		id = registered.Provider
	}
	if id == "" {
		id = cfg.DefaultProvider
	}
	p, err := provider.Get(id)
	if err != nil {
		return nil, err
	}
	matches := p.Discover(name)

	if len(matches) == 0 {
		return nil, rcerr.Newf(
			fmt.Sprintf("To create one, give it a directory:  rcmanager start %s /path/to/project --provider %s", name, p.ID()),
			"No %s session named %q is registered with rcmanager or found locally.", p.DisplayName(), name)
	}

	// Most recently active wins.
	best, rest := matches[0], matches[1:]
	if len(rest) > 0 && !quiet {
		var others []string
		for _, m := range rest {
			short := m.SessionID
			if len(short) > 8 {
				short = short[:8]
			}
			others = append(others, short+" "+format.Ago(m.LastActive))
		}
		fmt.Fprintln(os.Stderr, ansi.Grey(fmt.Sprintf(
			"  %d sessions named %q; using the most recently active (%s). Others: %s",
			len(matches), name, format.Ago(best.LastActive), strings.Join(others, ", "))))
	}

	if best.Cwd == "" {
		return nil, rcerr.Newf("Register it explicitly with a path instead.",
			"Session %s has no recorded working directory.", best.SessionID)
	}

	return &target{
		entry: &registry.Entry{
			Name:      name,
			Provider:  p.ID(),
			SessionID: best.SessionID,
			Cwd:       best.Cwd,
			Enabled:   true,
		},
		origin:     "discovered",
		discovered: &best,
		duplicates: rest,
	}, nil
}
