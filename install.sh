#!/bin/sh
# Install rcmanager — a supervisor for AI coding-agent remote-control sessions.
#
#   curl -fsSL https://raw.githubusercontent.com/dumanhaydar/rcmanager/main/install.sh | sh
#
# It downloads the release archive for this machine, checks it against the
# release's SHA256SUMS, and puts one binary on your PATH. Nothing else: no
# daemon is started, no service is registered, nothing is written outside the
# install directory. `rcmanager setup` does those, when you ask it to.
#
# Knobs, all optional:
#   PREFIX=/usr/local          where to install (default ~/.local)
#   RCMANAGER_VERSION=v1.2.3       a specific release (default: the latest)
#   RCMANAGER_REPO=owner/name      a fork
set -eu

REPO="${RCMANAGER_REPO:-dumanhaydar/rcmanager}"
PREFIX="${PREFIX:-$HOME/.local}"
BIN_DIR="$PREFIX/bin"

say()  { printf '%s\n' "$*"; }
info() { printf '  %s\n' "$*"; }
die()  { printf '\033[31m✖\033[0m %s\n' "$*" >&2; exit 1; }
ok()   { printf '\033[32m✔\033[0m %s\n' "$*"; }

need() {
	command -v "$1" >/dev/null 2>&1 || die "$1 is required but not installed."
}

# ------------------------------------------------------------------ platform

os="$(uname -s | tr '[:upper:]' '[:lower:]')"
case "$os" in
	darwin|linux) ;;
	*) die "unsupported operating system: $(uname -s). rcmanager supports macOS and Linux." ;;
esac

case "$(uname -m)" in
	arm64|aarch64) arch="arm64" ;;
	x86_64|amd64)  arch="amd64" ;;
	*) die "unsupported architecture: $(uname -m). rcmanager ships amd64 and arm64." ;;
esac

need curl
need tar

# sha256 is spelled differently on macOS and Linux, and either is fine.
if command -v sha256sum >/dev/null 2>&1; then
	sha256() { sha256sum "$1" | cut -d' ' -f1; }
elif command -v shasum >/dev/null 2>&1; then
	sha256() { shasum -a 256 "$1" | cut -d' ' -f1; }
else
	die "neither sha256sum nor shasum is available, so the download cannot be verified."
fi

# ------------------------------------------------------------------- version

api="https://api.github.com/repos/$REPO/releases"

if [ -n "${RCMANAGER_VERSION:-}" ]; then
	tag="$RCMANAGER_VERSION"
else
	# Parsed with grep rather than a JSON tool, so the script needs nothing
	# beyond curl and tar on a bare machine.
	tag="$(curl -fsSL "$api/latest" 2>/dev/null \
		| grep -m1 '"tag_name"' \
		| sed 's/.*"tag_name"[[:space:]]*:[[:space:]]*"\([^"]*\)".*/\1/')" || true
	[ -n "$tag" ] || die "could not work out the latest release of $REPO. Pass one: RCMANAGER_VERSION=v1.0.0"
fi
version="${tag#v}"

archive="rcmanager_${version}_${os}_${arch}.tar.gz"
base="https://github.com/$REPO/releases/download/$tag"

say ""
say "rcmanager $tag · $os/$arch"
info "from   $base"
info "into   $BIN_DIR/rcmanager"
say ""

# ------------------------------------------------------------------ download

tmp="$(mktemp -d)"
# Leave nothing behind, including on a failed download or a Ctrl-C.
trap 'rm -rf "$tmp"' EXIT HUP INT TERM

curl -fsSL "$base/$archive" -o "$tmp/$archive" \
	|| die "could not download $archive — does release $tag include this platform?"

if curl -fsSL "$base/SHA256SUMS" -o "$tmp/SHA256SUMS" 2>/dev/null; then
	expected="$(grep " $archive\$" "$tmp/SHA256SUMS" | cut -d' ' -f1)"
	[ -n "$expected" ] || die "SHA256SUMS does not mention $archive."
	actual="$(sha256 "$tmp/$archive")"
	[ "$expected" = "$actual" ] || die "checksum mismatch for $archive.
  expected $expected
  got      $actual
This is worth looking into rather than working around."
	info "checksum verified"
else
	# Refusing outright would be unhelpful for a fork that publishes archives
	# without sums, but silence would be worse: say what was not checked.
	info "no SHA256SUMS in this release — the download could not be verified"
fi

tar -xzf "$tmp/$archive" -C "$tmp"
[ -f "$tmp/rcmanager" ] || die "the archive did not contain an rcmanager binary."

# -------------------------------------------------------------------- install

mkdir -p "$BIN_DIR" || die "could not create $BIN_DIR"
[ -w "$BIN_DIR" ] || die "$BIN_DIR is not writable. Install elsewhere with PREFIX=~/.local, or re-run with sudo."

previous=""
if [ -x "$BIN_DIR/rcmanager" ]; then
	previous="$("$BIN_DIR/rcmanager" --version 2>/dev/null || true)"
fi

chmod 755 "$tmp/rcmanager"
# Moved into place rather than written over: replacing the file atomically
# means a running daemon keeps its own open binary, and no command ever reads
# a half-written one.
mv "$tmp/rcmanager" "$BIN_DIR/rcmanager.incoming"
mv "$BIN_DIR/rcmanager.incoming" "$BIN_DIR/rcmanager"

installed="$("$BIN_DIR/rcmanager" --version 2>/dev/null || true)" \
	|| die "the installed binary does not run on this machine."

if [ -n "$previous" ] && [ "$previous" != "$installed" ]; then
	ok "rcmanager $previous → $installed"
	info "a running daemon keeps the old code until it is restarted: rcmanager upgrade"
else
	ok "rcmanager $installed installed in $BIN_DIR"
fi

# --------------------------------------------------------------------- PATH

case ":$PATH:" in
	*":$BIN_DIR:"*) ;;
	*)
		say ""
		say "$BIN_DIR is not on your PATH. Add it:"
		# ~/.zshenv rather than ~/.zshrc on purpose: a non-interactive shell
		# (ssh host 'rcmanager …', cron, a service) never reads ~/.zshrc.
		case "$(basename "${SHELL:-sh}")" in
			zsh)  info "echo 'export PATH=\"$BIN_DIR:\$PATH\"' >> ~/.zshenv" ;;
			bash) info "echo 'export PATH=\"$BIN_DIR:\$PATH\"' >> ~/.bash_profile" ;;
			fish) info "fish_add_path $BIN_DIR" ;;
			*)    info "export PATH=\"$BIN_DIR:\$PATH\"" ;;
		esac
		;;
esac

say ""
say "Next:"
info "rcmanager setup                     guided first run"
info "rcmanager start <name> <path>       supervise a new session"
info "rcmanager ls                        what is running"
say ""
