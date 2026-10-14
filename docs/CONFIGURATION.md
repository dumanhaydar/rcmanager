# Configuration

Everything rcmanager keeps lives under `$RCMANAGER_HOME` (default `~/.rcmanager`), outside
the binary. Nothing is ever written next to the executable.

```
~/.rcmanager/
  config.json     global settings            (you may edit this)
  registry.json   registered sessions        (you may edit this)
  state.json      last known runtime state   (rcmanager owns this)
  daemon.pid      the supervisor's pid
  daemon.log      the supervisor's log
  logs/           per-session logs, ANSI-stripped and rotated
  run/            per-session stdin fifo and raw capture files
  daemon.sock     the control socket
```

The daemon watches `config.json` and `registry.json` and reconciles when they
change. `rcmanager reload` does the same on demand.

## Global settings

`rcmanager config list` prints the effective values; `rcmanager config set <key>
<value>` stores one; `rcmanager config unset <key>` restores its default.

| Key | Default | Meaning |
| --- | --- | --- |
| `defaultProvider` | `claude` | Provider used when `--provider` is omitted |
| `autorestart` | `true` | Restart managed sessions that exit unexpectedly |
| `maxRestarts` | `15` | Restarts allowed inside `restartWindowMs` before a session is marked errored |
| `restartWindowMs` | `60000` | Sliding window used to detect crash loops |
| `restartDelayMs` | `2000` | Delay before a crashed session is restarted |
| `minUptimeMs` | `5000` | An exit sooner than this counts toward the crash-loop budget |
| `stopTimeoutMs` | `15000` | Grace period between SIGTERM and SIGKILL |
| `maxLogBytes` | `10485760` | Rotate a log file once it exceeds this size |
| `logRetain` | `3` | Rotated log files kept per stream |
| `stripAnsiInLogs` | `true` | Strip ANSI escapes before writing session logs |
| `watchConfig` | `true` | Reload `config.json` and `registry.json` when they change on disk |
| `restartOnChange` | `false` | Restart a session when an edit changes how it launches. Off: rcmanager flags it and waits |
| `isolateSessions` | `auto` | `auto` runs each session in its own systemd scope where available; `off` disables that |

Only a short-lived exit counts against the crash-loop budget, so a session that
ran happily for hours always gets a fresh allowance.

## Sessions

A session entry is the same shape in `registry.json` and in an ecosystem file:

| Field | Required | Meaning |
| --- | --- | --- |
| `name` | yes | Letters, digits, `.`, `_`, `-`; must start with a letter or digit |
| `provider` | yes | `claude` or `codex` |
| `cwd` | yes | Working directory. `~` is expanded |
| `sessionId` | no | The conversation to resume. rcmanager mints one for providers that resume by id |
| `enabled` | no (default `true`) | Whether the session should be running. `autostart` is accepted as an alias |
| `args` | no | Extra arguments appended to the provider's command line |
| `env` | no | Extra environment for the managed process |
| `providerOptions` | no | Provider-specific options (below) |
| `note` | no | Free text, shown by `rcmanager info` |
| `autorestart`, `maxRestarts`, `restartDelayMs`, `restartWindowMs`, `minUptimeMs`, `stopTimeoutMs` | no | Per-session overrides of the global settings |

An ecosystem file may be a bare array or a `{"sessions": [...]}` document:

```json
{
  "sessions": [
    {
      "name": "api-worker",
      "provider": "claude",
      "cwd": "~/projects/api",
      "enabled": true,
      "note": "keeps the API refactor moving",
      "providerOptions": { "resumeMode": "summary", "model": "opus" },
      "restartDelayMs": 5000
    },
    {
      "name": "spare",
      "provider": "claude",
      "cwd": "~/projects/spare",
      "autostart": false
    }
  ]
}
```

`rcmanager start ./rcmanager.json` registers and starts everything in it.

### What a reload does, and does not do

Saving a file is not permission to restart an agent mid-thought.

- Supervision limits and notes are applied to the live session immediately.
- A change to `provider`, `sessionId`, `cwd`, `args`, `env` or
  `providerOptions` changes *how* the session launches, so it cannot be applied
  in place. rcmanager reports it (`needsRestart` in `rcmanager info`, and a line in
  `rcmanager list`) and waits for `rcmanager restart <name>`. Set
  `restartOnChange true` to have it act instead.
- Sessions added, removed, enabled or disabled do move: that is an explicit
  instruction either way.

## One supervisor per conversation

Two rcmanager daemons started with different `RCMANAGER_HOME` values each have
their own registry, and each is entitled to supervise a session as far as it
can tell. Both then restart it, and two agents end up appending to one
transcript. rcmanager refuses that:

```console
$ rcmanager start api-refactor
✖ refusing to start: session 3f5a37e1-… is already supervised by another
  rcmanager daemon (pid 2628 · state ~/.rcmanager-old · as "api")
  Two supervisors on one conversation fork its history. Stop that daemon
  (`rcmanager daemon --stop`), or manage this session from it.
```

The lock is keyed on the *conversation* — the session id — so the same
transcript registered under two different names is still caught. A provider
that allows only one managed session per machine (Codex) is keyed on the
provider instead, which makes that rule hold across daemons too.

It is `flock(2)`, held by the daemon for exactly as long as it supervises the
session, which has three consequences worth knowing:

- A daemon that is `kill -9`'d, or whose machine loses power, leaves nothing
  stale behind: the kernel drops the lock with the process.
- Handing over releases it. `rcmanager upgrade`, a service restart and a crash
  all let the successor claim what it is about to adopt.
- A start that fails releases it too. A daemon that could not run the session
  does not get to keep other daemons away from it.

This is the only thing rcmanager writes outside `$RCMANAGER_HOME`, because a
guard kept inside the home it is guarding against would not be one. The locks
are empty files under `~/.cache/rcmanager/claims` (or `$XDG_CACHE_HOME`, or
`$RCMANAGER_CLAIM_DIR` when you want to point them somewhere else), each
holding one line naming the daemon that holds it. If that directory cannot be
used at all, rcmanager says so on the session and supervises anyway: refusing
to work would be a worse failure than the one this prevents.

Note that this is a different check from the provider's. rcmanager also
refuses to start an agent on a conversation that an agent process outside
rcmanager is already running — a terminal, an IDE, or an orphan left by a
daemon that was killed. Both refusals name the process or daemon in the way,
so the fix is always visible.

## Renaming a session

```sh
rcmanager rename old-name new-name
```

The name is rcmanager's handle for a session, so renaming is bookkeeping: the
registry entry, the logs, the raw capture files and the stdin fifo all move to
the new name, and the agent keeps running throughout — the files are renamed,
not recreated, so the running process keeps writing to the same inodes and
capture resumes at the byte it had reached.

Two things deliberately do not change:

- **The conversation.** The session id is untouched, so this is the same
  conversation under a new label. `remove` and `rename` both leave transcripts
  alone.
- **The provider's own record.** Claude stores the display name a session was
  *created* with, and rcmanager does not rewrite other programs' metadata. So
  `rcmanager discover` still lists it under the old title, and resuming it by
  name on a fresh machine means the old name. `rcmanager info` always shows
  both the current name and the session id.

For a provider that puts the session name on its command line — a Claude
environment, or a Codex session — the process that is already running was
launched under the old name. rcmanager says so and flags the session for a
restart rather than pretending otherwise, and the signature interlock keeps
describing the live process so stopping it still works.

## Provider options

### Claude Code (`claude`)

| Key | Values | Meaning |
| --- | --- | --- |
| `attach` | `resume`, `create`, `bridge`, `environment` | Which mechanism binds remote control. Inferred from `sessionId` when unset |
| `resumeMode` | `summary` (default), `full` | The answer to the resume prompt an old or large session shows. `summary` is far cheaper on usage limits |
| `model` | e.g. `opus` | Passed as `--model` |
| `permissionMode` | e.g. `plan` | Passed as `--permission-mode` |
| `trustWorkspace` | `true` | Accept the workspace trust dialog for *this* session. A security decision, so it is never implied |
| `capacity`, `spawn` | numbers | `attach: "environment"` only |
| `bridgeSessionId` | id | `attach: "bridge"` only |

`attach` is worth understanding once:

- `claude --remote-control` (the flag) is **one session**, publishing
  `claude.ai/code/session_…`. This is what rcmanager supervises.
- `claude remote-control` (the subcommand) is an **environment**: one persistent
  server hosting up to `--capacity` conversations created on demand, publishing
  `claude.ai/code?environment=env_…`. rcmanager cannot supervise those
  individually, so it is opt-in via `attach: "environment"`.

**Workspace trust.** Claude refuses to start in an untrusted directory, and the
resume path shows a modal that stalls an unattended session forever. rcmanager reads
`~/.claude.json` up front and refuses to start with the fix rather than hanging:

```bash
rcmanager trust ~/projects/foo     # records it in Claude's own settings, with a backup
rcmanager untrust ~/projects/foo
```

`rcmanager start` offers this interactively when a session needs it. It is never
granted implicitly.

### OpenAI Codex CLI (`codex`)

Codex's remote control is a single machine-wide app-server daemon, not a
per-session process, so rcmanager supervises exactly **one** Codex session per
machine and refuses a second. The managed child is rcmanager itself, re-executed as
a supervisor that keeps that daemon alive with remote control enabled and stops
it again on `rcmanager stop`. There are no provider options. Pair the machine once,
interactively, with `codex remote-control pair`.

## The service

`rcmanager service install` picks the best manager this machine has:

| Manager | Priority | Notes |
| --- | --- | --- |
| `systemd-user` | 100 | Preferred: runs as you, with your credentials. Enables lingering so it starts at boot without a login |
| `launchd` | 90 | macOS LaunchAgent, `RunAtLoad` + `KeepAlive` |
| `systemd-system` | 60 | Needs root or passwordless sudo; runs as `User=` |
| `openrc` | 50 | Alpine and friends; needs root |
| `cron` | 10 | `@reboot` only — no restart on crash. The last resort |

`rcmanager service print` shows the generated unit without installing it, and
`--manager <id>` picks one explicitly.

The systemd units are written with `KillMode=process` on purpose: systemd stops
a *cgroup*, not a process, so the default would SIGKILL every managed agent
whenever the service is restarted. Where `systemd-run` is available, each
session additionally runs in its own transient scope, which makes the guarantee
unconditional.

## Upgrading

A running daemon keeps executing the code it started with, so replacing the
binary changes nothing until the supervisor restarts:

```bash
rcmanager upgrade      # refreshes the unit, restarts the daemon, keeps sessions running
```

The CLI also says so on its own if it notices it is talking to a daemon from a
different build.

## Stopping things

- `rcmanager stop <name>` ends one session.
- `rcmanager daemon --stop` stops the **supervisor** only; sessions keep running and
  are adopted by the next daemon.
- `rcmanager daemon --stop --with-sessions` stops both.
- `rcmanager remove <name>` unregisters a session. The provider's conversation is
  never touched — rcmanager does not delete transcripts.
