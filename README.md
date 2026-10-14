<h1 align="center">rcmanager</h1>

<p align="center">
  <b>Keep your AI coding-agent sessions alive — across crashes, upgrades and reboots.</b><br>
  A supervisor for Claude Code and Codex remote-control sessions, in one static Go binary.
</p>

<p align="center">
  <a href="https://github.com/dumanhaydar/rcmanager/actions/workflows/ci.yml"><img alt="CI" src="https://github.com/dumanhaydar/rcmanager/actions/workflows/ci.yml/badge.svg"></a>
  <a href="https://github.com/dumanhaydar/rcmanager/releases/latest"><img alt="Release" src="https://img.shields.io/github/v/release/dumanhaydar/rcmanager?color=00ADD8"></a>
  <img alt="Platforms" src="https://img.shields.io/badge/platform-macOS%20%C2%B7%20Linux-lightgrey">
  <img alt="Go" src="https://img.shields.io/badge/go-1.24%2B-00ADD8?logo=go&logoColor=white">
  <img alt="Dependencies" src="https://img.shields.io/badge/runtime%20deps-none-success">
  <a href="LICENSE"><img alt="License" src="https://img.shields.io/badge/license-MIT-blue"></a>
</p>

---

You start an agent session with remote control, close your laptop lid, and come
back to a dead process and a link that goes nowhere. The agent did not crash —
your terminal did, or the SSH connection dropped, or the machine rebooted at
3 a.m.

`rcmanager` is the supervisor in between. Your OS service manager keeps
`rcmanager` running; `rcmanager` keeps each agent session running, resumes the
*same* conversation after a reboot, answers the startup modals that would
otherwise stall it forever, and never touches your repositories, branches or
transcripts.

```console
$ rcmanager ls
┌──────────────┬──────────┬────────┬───────┬────────────┬─────────────────┬────────┐
│ Name         │ Provider │ Status │ PID   │ Session ID │ Directory       │ Uptime │
├──────────────┼──────────┼────────┼───────┼────────────┼─────────────────┼────────┤
│ api-refactor │ claude   │ online │ 41827 │ 6108a473   │ ~/projects/api  │ 3h12m  │
│ docs-rewrite │ claude   │ online │ 41903 │ c8c6a93b   │ ~/projects/docs │ 3h12m  │
└──────────────┴──────────┴────────┴───────┴────────────┴─────────────────┴────────┘
  daemon pid 41820 · state ~/.rcmanager
```

Those two sessions kept that uptime through a `kill -9` of the supervisor that
was watching them. They never noticed.

## Install

```sh
curl -fsSL https://raw.githubusercontent.com/dumanhaydar/rcmanager/main/install.sh | sh
```

One static binary into `~/.local/bin`, verified against the release's
`SHA256SUMS`. Nothing is started and nothing is registered until you ask.

<details>
<summary>Other ways</summary>

```sh
# A specific version, or somewhere else
RCMANAGER_VERSION=v1.1.0 PREFIX=/usr/local \
  curl -fsSL https://raw.githubusercontent.com/dumanhaydar/rcmanager/main/install.sh | sh

# From source
go install github.com/dumanhaydar/rcmanager/cmd/rcmanager@latest

# From a clone, with go-task
git clone https://github.com/dumanhaydar/rcmanager && cd rcmanager && task install
```

Download the archive for your platform straight from
[Releases](https://github.com/dumanhaydar/rcmanager/releases/latest) if you
would rather read the checksum yourself.

</details>

Requires one agent CLI at run time — [`claude`](https://claude.com/claude-code)
or `codex` — and nothing else. macOS and Linux, amd64 and arm64.

## 60 seconds

```console
$ rcmanager start api-refactor ~/projects/api    # create and supervise a new session
! Claude has never been run in ~/projects/api, so it will refuse to start there.
? Trust ~/projects/api for Claude Code? [y/N] y
✔ trusted ~/projects/api
✔ registered api-refactor
  ✔ starting api-refactor
✔ api-refactor started (pid 41827)
· the remote-control URL appears in the log shortly: rcmanager logs api-refactor

$ rcmanager info api-refactor
name        api-refactor
provider    Claude Code (claude) · 2.1.296 (Claude Code)
status      online
enabled     true
pid         41827
session id  6108a473-0053-4fb0-986e-4647486adf5d
directory   ~/projects/api
started     2026-10-14T09:02:07Z (4m)
restarts    0
remote url  https://claude.ai/code/session_01RDLippg3f1VTL2o8xpydE8

$ rcmanager service install                      # and keep it that way across reboots
· detected launchd (LaunchAgent) (launchd)
✔ installed ~/Library/LaunchAgents/io.github.dumanhaydar.rcmanager.plist
✔ launchd (LaunchAgent): installed=true enabled=true running=true
```

Then `rcmanager logs api-refactor -f` to watch it, `rcmanager restart
api-refactor` when you want a clean slate, `rcmanager ls` to see everything.

Already have a session running in a terminal somewhere? `rcmanager start
<its-name>` finds it from the provider's own metadata and adopts it — it never
silently creates a new conversation when you asked to resume one.

## What survives what

Every row below is tested, most of them in the test suite and all of them on a
real machine:

| What happens | The supervisor | Your session | Your conversation |
| --- | --- | --- | --- |
| `rcmanager restart <name>` | keeps running | restarted | resumed by id |
| Daemon upgraded (`rcmanager upgrade`) | replaced | **keeps running**, re-adopted | untouched |
| Daemon crashes or is `kill -9`'d | relaunched by the service | **keeps running**, re-adopted | untouched |
| `systemctl stop rcmanager` / `launchctl bootout` | stopped | **keeps running** | untouched |
| Agent process crashes | keeps running | restarted, with backoff | resumed by id |
| Agent crash-loops | keeps running | given up on, with the reason | intact, waiting |
| Machine reboots | started at boot | started fresh | **resumed by id** |
| `rcmanager stop <name>` | keeps running | stopped | intact, waiting |
| `rcmanager remove <name>` | keeps running | stopped | **never deleted** |

"Re-adopted" is literal: the new daemon takes over the *same process*, at the
same pid, and resumes reading its output at the exact byte it left off.

## Declare your sessions

Keep them in a file next to your code:

```json
{
  "sessions": [
    {
      "name": "api-refactor",
      "provider": "claude",
      "cwd": "~/projects/api",
      "providerOptions": { "resumeMode": "summary", "model": "opus" }
    },
    {
      "name": "docs-rewrite",
      "provider": "claude",
      "cwd": "~/projects/docs",
      "autostart": false,
      "note": "started by hand when needed",
      "restartDelayMs": 10000
    }
  ]
}
```

```console
$ rcmanager start ./rcmanager.json
· 2 session(s) from ./rcmanager.json
  ✔ api-refactor  started · pid 41827
  -  docs-rewrite  disabled — not started
```

Edit `registry.json` or `config.json` by hand and rcmanager picks it up on its
own. Saving a file is not permission to interrupt an agent mid-thought, though,
so a change to *how* a session launches is reported and waits for you:

```console
$ rcmanager ls
  api-refactor: registry changed args — restart to apply: rcmanager restart api-refactor
```

Supervision limits, notes and log settings apply to the live session
immediately. Set `restartOnChange true` if you would rather it just act.

## How it works

```
service manager  ──supervises──▶  rcmanager daemon  ──supervises──▶  agent sessions
systemd/launchd                   unix socket                        one process each
                                       │                                   │
                                  registry.json                       stdio in files
                                  state.json                          + a fifo
```

Three decisions carry most of the weight.

**Session stdio outlives the daemon.** Piping an agent's stdio to the supervisor
kills it with SIGPIPE the moment the supervisor exits. So stdout and stderr are
plain append-only files that the daemon *tails* — a successor resumes at the
recorded offset instead of replaying old output and re-answering prompts — and
stdin is a fifo opened read-write, so it never reaches end-of-file and a later
daemon can reopen the write end to answer a modal.

**The pty lives in its own process.** Claude's `--resume … --remote-control` is
the interactive UI and refuses to run on a pipe, but a pty owned by the daemon
would hang up the agent at shutdown. rcmanager re-executes itself as a tiny
`pty-wrapper`, which allocates the pty with plain ioctls and pumps bytes to the
files and fifo above. Unlike `script(1)`, this behaves identically on macOS and
Linux — BSD `script` refuses to start at all when its own stdin is not a
terminal.

**Nothing is signalled on faith.** Every kill is gated on the live argv of the
pid still matching what rcmanager recorded, so a recycled pid, or an agent you
started yourself, is never touched. Under systemd each session additionally runs
in its own transient scope, so `systemctl stop rcmanager` cannot sweep up the
work.

And one thing rcmanager deliberately will not do: answer a security prompt for
you. It clicks through the resume-summary and "Enable Remote Control?" modals,
which are the things you already asked for by starting the session. A
workspace-trust dialog is reported and the session waits, because that one is
your decision — `rcmanager trust <path>` is a separate command, typed on
purpose.

## Providers

| Provider | Per-session remote control | New sessions | Notes |
| --- | --- | --- | --- |
| **Claude Code** (`claude`) | yes, bound to a session id | yes | needs a pty; workspace trust required |
| **OpenAI Codex CLI** (`codex`) | no — one machine-wide daemon | no | singleton: one managed session per machine |

`rcmanager providers` prints each adapter's capabilities and its honest
limitations — including the ones that are the provider's, not rcmanager's.
Adding a provider is one file in [`internal/provider/`](internal/provider) plus
a blank import.

## Commands

| | |
| --- | --- |
| `rcmanager setup` | Guided first run: provider, boot service, first sessions |
| `rcmanager start <name> [path]` | Create, resume or adopt a session |
| `rcmanager start all` · `stop all` · `restart all` | Act on everything, with live progress |
| `rcmanager start <file.json>` | Register and start an ecosystem file |
| `rcmanager stop` · `restart` · `remove <name>` | `remove` never deletes the conversation |
| `rcmanager rename <name> <new-name>` | Renames the handle; the session keeps running |
| `rcmanager enable` · `disable <name>` | Autostart on and start now · stop and stay stopped |
| `rcmanager ls` · `status` · `info <name>` | `--json` everywhere |
| `rcmanager logs [name] [-n N] [-f]` | Per-session logs, ANSI-stripped and rotated |
| `rcmanager discover [name]` | Sessions the provider knows about locally |
| `rcmanager config list\|get\|set\|unset` | Supervision policy |
| `rcmanager reload` | Re-read config and registry without interrupting work |
| `rcmanager trust` · `untrust <path>` | Workspace trust, explicitly |
| `rcmanager service install\|status\|print\|detect\|…` | Boot registration |
| `rcmanager daemon [--status\|--stop\|--detached]` | The supervisor itself |
| `rcmanager upgrade` | After a new binary: refresh the unit, restart the daemon |

`rcmanager service install` picks what the machine actually has: systemd (user,
then system), launchd, OpenRC, or a cron `@reboot` as a last resort. Every
setting, every `providerOptions` key and the full `$RCMANAGER_HOME` layout are
documented in **[docs/CONFIGURATION.md](docs/CONFIGURATION.md)**.

## Development

```sh
task                      # list every task
task build                # ./bin/rcmanager, version stamped from git
task run -- list          # build, then run a command against it
task test                 # go test ./...
task test:race            # where a supervisor's bugs actually show up
task check                # gofmt + vet + race suite — what CI runs
task dist                 # darwin and linux, amd64 and arm64
task deploy HOST=user@box # build for that host and install it there over SSH
```

The test suite drives real supervision — fifos, ptys, adoption, crash-loop
budgets and the daemon's IPC — against a fake provider whose "agent" is a shell
script, so it needs neither provider CLI installed and runs on every platform in
CI. That paid for itself immediately: the first Linux run found a deadlock macOS
had been hiding, in the one path where a session's output arrived before the
supervisor's first read of it.

`task deploy` detects the remote platform itself, stages the binary in a
temporary file before moving it into place, and touches nothing else: the remote
`$RCMANAGER_HOME`, its registry and any running session are left alone.

## Credits

A Go port of [**ybouane/rcman**](https://github.com/ybouane/rcman), whose design
— the registry, the adoption protocol, the provider contract, the honesty about
what each agent CLI cannot do — is the reason this was worth porting rather than
inventing. MIT, like the original.
