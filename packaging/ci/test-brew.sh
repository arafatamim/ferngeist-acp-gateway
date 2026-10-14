#!/bin/sh
# The Homebrew flow on macOS or Linuxbrew: install the live formula from
# arafatamim/tap, re-run the installer, `brew upgrade` to the formula built
# from this checkout (pointed at its local archives), then the documented
# uninstall. Linux needs a running user manager.
# Usage: test-brew.sh dir-with-snapshot-artifacts (has homebrew/Formula/ and *_brew.tar.gz)
set -eu
CI="$(cd "$(dirname "$0")" && pwd)"
INSTALL="$CI/../install.sh"
PKGS="$(cd "$1" && pwd)"
GW="$(brew --prefix)/bin/ferngeist-gateway"
case "$(uname -s)" in
    Darwin) SVC="$HOME/Library/Application Support/Ferngeist Gateway/bin/ferngeist-gateway" ;;
    *) SVC="$HOME/.local/share/ferngeist-gateway/bin/ferngeist-gateway" ;;
esac
export HOMEBREW_NO_AUTO_UPDATE=1
brewver() { "$GW" --version | awk '$1 == "VERSION" {print $2}'; }

echo "== install from the live tap: daemon starts with LAN on"
sh "$INSTALL" --yes --channel brew
OLD="$(brewver)"
sh "$CI/wait-daemon.sh" "$SVC" "VERSION +$OLD" 'LAN ENABLED +true'

echo "== re-running the installer keeps it running"
sh "$INSTALL" --yes --channel brew
sh "$CI/wait-daemon.sh" "$SVC" "VERSION +$OLD" 'LAN ENABLED +true'

echo "== a release pushes this checkout's formula; brew upgrade picks it up"
TAP="$(brew --repo arafatamim/tap)"
sed "s#https://github.com/arafatamim/ferngeist-acp-gateway/releases/download/[^/]*/#file://$PKGS/#" \
    "$PKGS/homebrew/Formula/ferngeist-gateway.rb" > "$TAP/Formula/ferngeist-gateway.rb"
git -C "$TAP" -c user.name=ci -c user.email=ci@localhost commit -qam "test formula"
brew upgrade ferngeist-gateway
NEW="$(brewver)"
[ "$NEW" != "$OLD" ] || { echo "FAIL: brew upgrade left $OLD" >&2; exit 1; }
# Per the formula caveat: brew sandboxes post_install, so this is manual.
"$GW" daemon install --keep-settings --lan
sh "$CI/wait-daemon.sh" "$SVC" "VERSION +$NEW" 'LAN ENABLED +true'

echo "== documented uninstall: daemon uninstall, then brew uninstall"
"$GW" daemon uninstall
sh "$CI/wait-gone.sh" "$SVC" "daemon uninstall"
brew uninstall ferngeist-gateway
[ ! -e "$GW" ] || { echo "FAIL: $GW still present after brew uninstall" >&2; exit 1; }
echo "PASS"
