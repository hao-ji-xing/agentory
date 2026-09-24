#!/bin/sh
# Check a GoReleaser build end to end: serve dist/ like a GitHub release,
# run install.sh against it, and verify the binary and the bundled skill.
# Run after `goreleaser release --snapshot --clean` (CI does both).
set -eu

cd "$(dirname "$0")/.."
ver=$(sed -n 's/.*"version":"\([^"]*\)".*/\1/p' dist/metadata.json)
[ -n "$ver" ] || { echo "dist/metadata.json has no version; run goreleaser first" >&2; exit 1; }

work=$(mktemp -d)
trap 'kill "$server" 2>/dev/null || true; rm -rf "$work"' EXIT INT TERM
mkdir -p "$work/release/v$ver" "$work/home/.claude" "$work/home/.codex"
cp dist/*.tar.gz dist/*.zip dist/checksums.txt "$work/release/v$ver/"

port=18766
(cd "$work/release" && exec python3 -m http.server "$port" --bind 127.0.0.1 >/dev/null 2>&1) &
server=$!
i=0
until curl -fsS --noproxy '*' -o /dev/null "http://127.0.0.1:$port/v$ver/checksums.txt"; do
	i=$((i + 1)); [ $i -lt 50 ] || { echo "local server did not start" >&2; exit 1; }
	sleep 0.1
done

NO_PROXY='*' no_proxy='*' HOME="$work/home" AGENTORY_DOWNLOAD_BASE="http://127.0.0.1:$port" \
	sh install.sh --version "v$ver" --dir "$work/bin"

fail() { echo "FAIL: $*" >&2; exit 1; }
got=$("$work/bin/agentory" version)
[ "$got" = "agentory $ver" ] || fail "version printed '$got', want 'agentory $ver'"
for agent in .claude .codex; do
	cmp -s skills/agentory/SKILL.md "$work/home/$agent/skills/agentory/SKILL.md" ||
		fail "skill missing or different under $agent"
done
# A skill managed by `npx skills` (in ~/.agents/skills) is left alone.
mkdir -p "$work/home2/.claude" "$work/home2/.agents/skills/agentory"
NO_PROXY='*' no_proxy='*' HOME="$work/home2" AGENTORY_DOWNLOAD_BASE="http://127.0.0.1:$port" \
	sh install.sh --version "v$ver" --dir "$work/bin2" >/dev/null
[ ! -e "$work/home2/.claude/skills/agentory" ] || fail "installer copied a skill next to the npx-managed one"

for f in dist/*.zip; do
	unzip -l "$f" | grep -q 'skills/agentory/SKILL.md' || fail "$f has no skill"
done
echo "install.sh installed agentory $ver and its skill from the release archives"
