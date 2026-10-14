#!/bin/sh
# The manual/binary install update path: registers the daemon from a build of
# this checkout stamped as an old version, runs `ferngeist-gateway update`
# against the live latest release, then purges. Linux, macOS and Windows
# (Git Bash). Linux needs a running user manager.
# Usage: test-self-update.sh path-to-old-ferngeist-gateway
set -eu
CI="$(cd "$(dirname "$0")" && pwd)"
OLD_BIN="$1"
case "$(uname -s)" in
    Darwin) SVC="$HOME/Library/Application Support/Ferngeist Gateway/bin/ferngeist-gateway" ;;
    Linux) SVC="$HOME/.local/share/ferngeist-gateway/bin/ferngeist-gateway" ;;
    *) SVC="$(cygpath -u "$LOCALAPPDATA")/FerngeistGateway/service/bin/ferngeist-gateway.exe" ;;
esac
LATEST="$(curl -fsSLI -o /dev/null -w '%{url_effective}' https://github.com/arafatamim/ferngeist-acp-gateway/releases/latest)"
LATEST="${LATEST##*/v}"
OLD="$("$OLD_BIN" --version | awk '$1 == "VERSION" {print $2}')"

echo "== register the old build ($OLD)"
"$OLD_BIN" daemon install --lan
sh "$CI/wait-daemon.sh" "$SVC" "VERSION +$OLD" 'LAN ENABLED +true'

echo "== ferngeist-gateway update moves the running daemon to $LATEST"
"$SVC" update
sh "$CI/wait-daemon.sh" "$SVC" "VERSION +$LATEST" 'LAN ENABLED +true'

echo "== daemon uninstall --purge stops it and removes the install"
"$SVC" daemon uninstall --purge
sh "$CI/wait-gone.sh" "$SVC" "daemon uninstall --purge"
# Windows deletes the running binary from a detached helper a few seconds later.
for _ in $(seq 15); do [ -e "$SVC" ] || break; sleep 2; done
[ ! -e "$SVC" ] || { echo "FAIL: $SVC survived --purge" >&2; exit 1; }
echo "PASS"
