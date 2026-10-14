#!/bin/sh
# CI check for the Linux package channels. Runs packaging/install.sh --yes as
# root inside a bare ubuntu/fedora/archlinux container, so it exercises the
# repo, its signatures and the package itself. The per-user service is not
# covered here (no systemd in containers; root skips it); the ubuntu-systemd
# job in test-install.yml covers that.
#
# Usage: test-linux.sh [dir-with-packages-to-upgrade-to]
# FERNGEIST_REPO_BASE (optional) points install.sh at another repo copy.
set -eu

SRC="$(cd "$(dirname "$0")/../.." && pwd)"
# Absolute: apt-get treats a relative path without ./ as a package name.
UPGRADE_DIR="${1:+$(cd "$1" && pwd)}"
BIN=/usr/bin/ferngeist-gateway

die() { printf 'FAIL: %s\n' "$*" >&2; exit 1; }
have() { command -v "$1" >/dev/null 2>&1; }
version() { "$BIN" --version | awk '$1 == "VERSION" {print $2}'; }

# Bare images lack what install.sh relies on.
if have apt-get; then
    apt-get update -qq
    apt-get install -y -qq curl gnupg ca-certificates >/dev/null
elif have pacman; then
    # The archlinux image ships without a local master key, which
    # `pacman-key --lsign-key` needs.
    pacman-key --init >/dev/null 2>&1
fi
have curl || { have dnf && dnf install -y -q curl; }

LOG="$(mktemp)"
sh "$SRC/packaging/install.sh" --yes >"$LOG" 2>&1 || { cat "$LOG"; die "install.sh failed"; }
cat "$LOG"
# pacman still installs when it can't parse a db-embedded (armored) signature.
# Checked only for a freshly published repo (verify-repos); the live one is
# fixed by the next release.
if [ -n "${FERNGEIST_REPO_BASE:-}" ] && grep -q 'signature format error' "$LOG"; then
    die "pacman could not parse a repo signature"
fi
[ -x "$BIN" ] || die "package channel did not install $BIN"
BEFORE="$(version)"
[ -n "$BEFORE" ] || die "$BIN --version printed no VERSION"
echo "installed $BEFORE"

[ -n "$UPGRADE_DIR" ] || exit 0

if have apt-get; then
    apt-get install -y "$UPGRADE_DIR"/*amd64*.deb
elif have dnf; then
    dnf install -y "$UPGRADE_DIR"/*amd64*.rpm
else
    pacman -U --noconfirm "$UPGRADE_DIR"/*amd64*.pkg.tar.zst
    # Without post_upgrade the daemon keeps its old binary copy after -Syu.
    grep -q 'post_upgrade' /var/lib/pacman/local/ferngeist-gateway-*/install ||
        die "arch package has no post_upgrade scriptlet"
fi
AFTER="$(version)"
[ "$AFTER" != "$BEFORE" ] || die "upgrade left the version at $BEFORE"
echo "upgraded $BEFORE -> $AFTER"
