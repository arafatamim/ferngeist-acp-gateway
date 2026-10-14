#!/bin/sh
# Runs before dpkg/rpm/pacman removes the package. The per-user service runs
# a copy of the binary under the user's home, so without this it would keep
# running (and restart at login) after the package is gone. Upgrades run this
# hook too (deb "upgrade", rpm "1"); those keep the service.
set -u
case "${1:-}" in upgrade|failed-upgrade|deconfigure|1) exit 0 ;; esac

GATEWAY_BIN=/usr/bin/ferngeist-gateway
[ -x "$GATEWAY_BIN" ] || exit 0

if [ "$(id -u)" != 0 ]; then
    "$GATEWAY_BIN" daemon uninstall || true
    exit 0
fi

# Same invoking-user resolution as postinstall.sh.
invoking_user=""
if [ -n "${SUDO_USER:-}" ] && [ "$SUDO_USER" != "root" ]; then
    invoking_user="$SUDO_USER"
elif [ -n "${PKEXEC_UID:-}" ]; then
    invoking_user="$(id -un "$PKEXEC_UID" 2>/dev/null || true)"
fi
if [ -z "$invoking_user" ]; then
    printf 'WARNING: if a user registered the ferngeist-gateway daemon, stop it as that user with:\n  ~/.local/share/ferngeist-gateway/bin/ferngeist-gateway daemon uninstall\n' >&2
    exit 0
fi

uid="$(id -u "$invoking_user" 2>/dev/null || true)"
if [ -n "$uid" ] && [ -d "/run/user/$uid" ]; then
    export XDG_RUNTIME_DIR="/run/user/$uid"
    export DBUS_SESSION_BUS_ADDRESS="unix:path=/run/user/$uid/bus"
fi
if command -v runuser >/dev/null 2>&1; then
    runuser -u "$invoking_user" -- "$GATEWAY_BIN" daemon uninstall >/dev/null 2>&1
else
    su -s /bin/sh "$invoking_user" -c "\"$GATEWAY_BIN\" daemon uninstall" >/dev/null 2>&1
fi || printf 'WARNING: could not stop the ferngeist-gateway daemon; as %s run:\n  ~/.local/share/ferngeist-gateway/bin/ferngeist-gateway daemon uninstall\n' "$invoking_user" >&2
exit 0
