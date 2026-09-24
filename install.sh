#!/bin/sh
# Install agentory from a GitHub release.
#
#   curl -fsSL https://raw.githubusercontent.com/hao-ji-xing/agentory/main/install.sh | sh
#
# Downloads the archive for this OS and CPU, verifies it against the
# release's checksums.txt, puts the binary in ~/.local/bin and installs the
# bundled agent skill for Claude Code and Codex when they are present.
#
# Options (or the matching environment variables):
#   --version <tag>   release to install, e.g. v0.2.0    AGENTORY_VERSION (default: latest)
#   --dir <path>      where to put the binary              AGENTORY_INSTALL_DIR (default: ~/.local/bin)
#   --no-skill        do not install the agent skill       AGENTORY_SKILL=no
#
# AGENTORY_DOWNLOAD_BASE overrides https://github.com/<repo>/releases/download
# (a mirror, or a local directory served over HTTP for testing).
set -eu

REPO="hao-ji-xing/agentory"
VERSION="${AGENTORY_VERSION:-latest}"
INSTALL_DIR="${AGENTORY_INSTALL_DIR:-$HOME/.local/bin}"
SKILL="${AGENTORY_SKILL:-auto}"
BASE="${AGENTORY_DOWNLOAD_BASE:-https://github.com/$REPO/releases/download}"

say() { printf '%s\n' "$*"; }
die() { printf 'agentory install: %s\n' "$*" >&2; exit 1; }

while [ $# -gt 0 ]; do
	case "$1" in
	--version) [ $# -ge 2 ] || die "--version needs a value"; VERSION="$2"; shift 2 ;;
	--dir) [ $# -ge 2 ] || die "--dir needs a value"; INSTALL_DIR="$2"; shift 2 ;;
	--no-skill) SKILL=no; shift ;;
	-h | --help) sed -n '2,17p' "$0" 2>/dev/null | sed 's/^# \{0,1\}//'; exit 0 ;;
	*) die "unknown option: $1" ;;
	esac
done

if command -v curl >/dev/null 2>&1; then
	fetch() { curl -fsSL --retry 2 -o "$2" "$1"; }
elif command -v wget >/dev/null 2>&1; then
	fetch() { wget -q -O "$2" "$1"; }
else
	die "need curl or wget"
fi

case "$(uname -s)" in
Darwin) os=darwin ;;
Linux) os=linux ;;
MINGW* | MSYS* | CYGWIN*) die "on Windows use install.ps1 (see the README)" ;;
*) die "unsupported OS: $(uname -s)" ;;
esac
case "$(uname -m)" in
x86_64 | amd64) arch=amd64 ;;
arm64 | aarch64) arch=arm64 ;;
*) die "unsupported CPU: $(uname -m)" ;;
esac

# The latest release is found through the redirect of /releases/latest,
# which needs no API token and has no rate limit.
if [ "$VERSION" = latest ]; then
	if command -v curl >/dev/null 2>&1; then
		url=$(curl -fsSLI -o /dev/null -w '%{url_effective}' "https://github.com/$REPO/releases/latest") ||
			die "cannot reach GitHub to find the latest release"
		VERSION=${url##*/}
	else
		VERSION=$(wget -qO- "https://api.github.com/repos/$REPO/releases/latest" |
			sed -n 's/.*"tag_name": *"\([^"]*\)".*/\1/p' | head -n 1)
	fi
	case "$VERSION" in
	v*) ;;
	*) die "no release found for $REPO" ;;
	esac
fi
case "$VERSION" in v*) ;; *) VERSION="v$VERSION" ;; esac

asset="agentory_${VERSION#v}_${os}_${arch}.tar.gz"
tmp=$(mktemp -d 2>/dev/null || mktemp -d -t agentory)
trap 'rm -rf "$tmp"' EXIT INT TERM

say "Downloading agentory $VERSION for $os/$arch"
fetch "$BASE/$VERSION/$asset" "$tmp/$asset" || die "download failed: $BASE/$VERSION/$asset"
fetch "$BASE/$VERSION/checksums.txt" "$tmp/checksums.txt" || die "download failed: checksums.txt"

want=$(awk -v f="$asset" '$2 == f { print $1 }' "$tmp/checksums.txt")
[ -n "$want" ] || die "$asset is not listed in checksums.txt"
if command -v sha256sum >/dev/null 2>&1; then
	got=$(sha256sum "$tmp/$asset" | awk '{ print $1 }')
elif command -v shasum >/dev/null 2>&1; then
	got=$(shasum -a 256 "$tmp/$asset" | awk '{ print $1 }')
else
	die "need sha256sum or shasum to verify the download"
fi
[ "$got" = "$want" ] || die "checksum mismatch for $asset (got $got, want $want)"

mkdir -p "$tmp/x"
tar -xzf "$tmp/$asset" -C "$tmp/x"
[ -f "$tmp/x/agentory" ] || die "the archive has no agentory binary"

mkdir -p "$INSTALL_DIR"
target="$INSTALL_DIR/agentory"
[ -L "$target" ] && say "Replacing the symlink $target"
cp "$tmp/x/agentory" "$target.new"
chmod 755 "$target.new"
mv -f "$target.new" "$target"
say "Installed $target ($("$target" version 2>/dev/null || echo "$VERSION"))"

# The skill tells an agent when and how to use agentory. It is installed
# where an agent is present, and never over a symlink (a developer's clone).
install_skill() { # $1 = agent home, $2 = agent name
	[ -d "$1" ] || return 0
	dest="$1/skills/agentory"
	if [ -L "$dest" ]; then
		say "Skill for $2 is a symlink ($dest), left as is"
		return 0
	fi
	mkdir -p "$dest"
	cp -R "$tmp/x/skills/agentory/." "$dest/"
	say "Installed the $2 skill in $dest"
}
# A skill added with `npx skills add` lives in ~/.agents/skills and is kept
# up to date by `npx skills update`; a second copy would show up twice.
if [ "$SKILL" != no ] && [ -e "$HOME/.agents/skills/agentory" ]; then
	say "Skill managed by 'npx skills' ($HOME/.agents/skills/agentory), left as is"
	SKILL=no
fi
if [ "$SKILL" != no ] && [ -d "$tmp/x/skills/agentory" ]; then
	install_skill "${CLAUDE_CONFIG_DIR:-$HOME/.claude}" "Claude Code"
	install_skill "${CODEX_HOME:-$HOME/.codex}" "Codex"
fi

case ":$PATH:" in
*":$INSTALL_DIR:"*) ;;
*)
	say ""
	say "$INSTALL_DIR is not on your PATH. Add this line to your shell profile:"
	say "  export PATH=\"$INSTALL_DIR:\$PATH\""
	;;
esac
say ""
say "Next: run 'agentory doctor', then 'agentory index' to build the index."
