#!/bin/sh
# The per-user Linux package flow on a real systemd host, run as a non-root
# user with passwordless sudo and a running user manager: install, re-run,
# uninstall + reinstall, --localhost, upgrade, an unattended-style binary
# swap, package removal.
# Usage: test-user.sh dir-with-packages-built-from-this-checkout
set -eu
CI="$(cd "$(dirname "$0")" && pwd)"
INSTALL="$CI/../install.sh"
PKGS="$(cd "$1" && pwd)"
GW=/usr/bin/ferngeist-gateway

pkgver() { "$GW" --version | awk '$1 == "VERSION" {print $2}'; }
running() { sh "$CI/wait-daemon.sh" "$GW" "$@"; }

if command -v apt-get >/dev/null 2>&1; then
    upgrade() { sudo apt-get install -y "$PKGS"/*amd64*.deb; }
    remove() { sudo apt-get remove -y ferngeist-gateway; }
elif command -v dnf >/dev/null 2>&1; then
    upgrade() { sudo dnf install -y "$PKGS"/*amd64*.rpm; }
    remove() { sudo dnf remove -y ferngeist-gateway; }
else
    upgrade() { sudo pacman -U --noconfirm "$PKGS"/*amd64*.pkg.tar.zst; }
    remove() { sudo pacman -R --noconfirm ferngeist-gateway; }
fi

echo "== install from the live repo: daemon starts with LAN on"
sh "$INSTALL" --yes
OLD="$(pkgver)"
running "VERSION +$OLD" 'LAN ENABLED +true'

echo "== re-running the installer keeps it running"
sh "$INSTALL" --yes
running "VERSION +$OLD" 'LAN ENABLED +true'

echo "== daemon uninstall stops it; re-running the installer brings it back"
"$GW" daemon uninstall
sh "$CI/wait-gone.sh" "$GW" "daemon uninstall"
sh "$INSTALL" --yes
running "VERSION +$OLD" 'LAN ENABLED +true'

echo "== --localhost re-registers localhost only"
sh "$INSTALL" --yes --localhost
running "VERSION +$OLD" 'LAN ENABLED +false'

# Last package change before removal: re-running install.sh against the repo
# would put the repo version back (pacman -S downgrades).
echo "== upgrading the package restarts the daemon on the new version, keeping settings"
upgrade
NEW="$(pkgver)"
[ "$NEW" != "$OLD" ] || { echo "FAIL: upgrade left $GW at $OLD" >&2; exit 1; }
running "VERSION +$NEW" 'LAN ENABLED +false'
# The service now runs /usr/bin directly instead of a copy.
grep -q "^ExecStart=\"$GW\"" "$HOME/.config/systemd/user/ferngeist-gateway.service"
[ ! -e "$HOME/.local/share/ferngeist-gateway/bin/ferngeist-gateway" ]

echo "== a root-only upgrade (unattended-upgrades) restarts the daemon by itself"
sh "$CI/swap-binary.sh" "$GW" sudo
running "VERSION +$NEW" 'LAN ENABLED +false'

echo "== removing the package stops the daemon"
remove
[ ! -e "$GW" ] || { echo "FAIL: $GW still present after removal" >&2; exit 1; }
for _ in $(seq 15); do
    systemctl --user is-active --quiet ferngeist-gateway.service || break
    sleep 2
done
! systemctl --user is-active --quiet ferngeist-gateway.service ||
    { echo "FAIL: daemon still running after package removal" >&2; exit 1; }
echo "PASS"
