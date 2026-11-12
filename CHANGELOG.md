# Changelog

## 1.5.0 — 2026-11-12

A provider CLI that is not signed in where the daemon runs it is now reported
as such, instead of as an online session "stuck on a prompt".

- **New status `auth-required`.** Before every start (boot restore, autorestart,
  `start`, `restart`, `start all`), the daemon runs `claude auth status` from its
  own context. If it says signed out, the session is not launched: it is marked
  `auth-required`, `daemon.log` says so once per provider, and `rcmanager ls`
  puts the cause on one line instead of one per session. A status command that
  fails or times out does not block a start.
- **"Not logged in · Run /login" is recognised** in a Claude session's output at
  startup. The session is stopped and marked `auth-required` rather than left
  running, unreachable, and not restarted into the same wall. Only believed
  before the session has published a URL, so a conversation that merely
  mentions the message does not take its own session down.
- **New `rcmanager doctor`.** Asks each installed provider whether it is signed
  in from your shell and from the daemon, shows the method, the login expiry
  and the CLI binary each one runs, and explains a difference. On macOS it
  points at a stale `Claude Code-credentials` Keychain entry shadowing a valid
  `~/.claude/.credentials.json`, and prints the commands to inspect and remove
  it. rcmanager never runs them: it does not touch the Keychain or credentials.
- Only whether, how and until when a provider is signed in is ever logged —
  never the account, the organisation or a token.
- `rcmanager info` now shows the session's current message.
- A failed `claude auth status` / `codex login status` now reads as "unknown"
  rather than "not signed in", in `setup` too. Older Claude builds' prose
  "Not logged in" no longer reads as signed in.
- A session stopped by a blocking condition is reported as such even with
  `autorestart` off, and releases its conversation claim.

## 1.4.3 — 2026-10-16

- The remote-control URL is read from Claude's own session record
  (`~/.claude/sessions/<pid>.json`) when the screen gets it wrong, and the
  "no remote-control URL" warning clears once it is found.
- Declines Claude Code 2.1.30x's "Claude in Chrome" startup prompt, which
  stalled new sessions before remote control came up.
- Starting an already-registered session again keeps its conversation id.

## 1.4.2 — 2026-10-14

- Stops recompiling the Claude prompt patterns on every chunk of session
  output: 2.6% → 2.0% of a core under constant TUI repaint.

## 1.4.1 — 2026-10-14

- Checks an adopted session's command line about once a minute instead of
  every five seconds (on macOS, a `ps` fork each time): 0.70% → 0.30% of a core
  for eleven idle sessions.

## 1.4.0 — 2026-10-14

- Adds `rcmanager reset <name>`: same name, same directory, fresh
  conversation, nothing deleted.
- The supervisor costs about a third of what it did: 0.6% of a core for ten
  idle sessions, 2.6% under constant TUI repaint.

## 1.3.1 — 2026-10-14

- Reads a remote-control announcement the TUI damaged, and keeps looking for
  one after adopting a session that had none.

## 1.3.0 — 2026-10-14

- Refuses to supervise a conversation another rcmanager daemon already holds,
  and names the daemon in the way.

## 1.2.0 — 2026-10-14

- Adds `rcmanager rename <name> <new-name>`: the session keeps running and the
  conversation is untouched.

## 1.1.0 — 2026-10-14

- Everything is named rcmanager: the binary, the state directory, the
  environment variables and the service units. Static binaries for macOS and
  Linux on amd64 and arm64.
