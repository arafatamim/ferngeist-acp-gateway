#!/bin/sh
# The Homebrew flow on macOS or Linuxbrew: install the live formula from
# arafatamim/tap, re-run the installer, `brew upgrade` to the formula built
# from this checkout (pointed at its local archives), a later upgrade
# restarting the daemon by itself, then the documented uninstall. Linux needs a running user manager.
# Usage: test-brew.sh dir-with-snapshot-artifacts (has homebrew/Formula/ and *_brew.tar.gz)
set -eu
CI="$(cd "$(dirname "$0")" && pwd)"
INSTALL="$CI/../install.sh"
PKGS="$(cd "$1" && pwd)"
GW="$(brew --prefix)/bin/ferngeist-gateway"
export HOMEBREW_NO_AUTO_UPDATE=1
brewver() { "$GW" --version | awk '$1 == "VERSION" {print $2}'; }

echo "== install from the live tap: daemon starts with LAN on"
sh "$INSTALL" --yes --channel brew
OLD="$(brewver)"
sh "$CI/wait-daemon.sh" "$GW" "VERSION +$OLD" 'LAN ENABLED +true'

echo "== re-running the installer keeps it running"
sh "$INSTALL" --yes --channel brew
sh "$CI/wait-daemon.sh" "$GW" "VERSION +$OLD" 'LAN ENABLED +true'

echo "== a release pushes this checkout's formula; brew upgrade picks it up"
TAP="$(brew --repo arafatamim/tap)"
sed "s#https://github.com/arafatamim/ferngeist-acp-gateway/releases/download/[^/]*/#file://$PKGS/#" \
    "$PKGS/homebrew/Formula/ferngeist-gateway.rb" > "$TAP/Formula/ferngeist-gateway.rb"
git -C "$TAP" -c user.name=ci -c user.email=ci@localhost commit -qam "test formula"
brew upgrade ferngeist-gateway
NEW="$(brewver)"
[ "$NEW" != "$OLD" ] || { echo "FAIL: brew upgrade left $OLD" >&2; exit 1; }
# Once, per the caveat: 1.0.0 registered a copy of its binary.
"$GW" daemon install --keep-settings --lan
sh "$CI/wait-daemon.sh" "$GW" "VERSION +$NEW" 'LAN ENABLED +true'

echo "== later upgrades restart the daemon by itself"
# Swapping the keg's binary stands in for brew moving opt/ to a new keg.
KEG_BIN="$(cd "$(brew --prefix)/opt/ferngeist-gateway/bin" && pwd -P)/ferngeist-gateway"
sh "$CI/swap-binary.sh" "$KEG_BIN"
sh "$CI/wait-daemon.sh" "$GW" "VERSION +$NEW" 'LAN ENABLED +true'

echo "== documented uninstall: daemon uninstall, then brew uninstall"
"$GW" daemon uninstall
sh "$CI/wait-gone.sh" "$GW" "daemon uninstall"
brew uninstall ferngeist-gateway
[ ! -e "$GW" ] || { echo "FAIL: $GW still present after brew uninstall" >&2; exit 1; }
echo "PASS"
