#!/bin/sh
# Replaces a binary the way package managers do (a new file renamed over the
# old path, no daemon install afterwards), then waits for the daemon to
# restart on its own: its service PID must change.
# Usage: swap-binary.sh path-to-real-binary [sudo]
set -eu
CI="$(cd "$(dirname "$0")" && pwd)"
BIN="$1"
AS="${2:-}"
OLD="$(sh "$CI/service-pid.sh")"
# env -u SUDO_USER: like unattended-upgrades/PackageKit, nothing re-registers.
$AS env -u SUDO_USER sh -c 'cp "$1" "$1.new" && mv "$1.new" "$1"' _ "$BIN"
for _ in $(seq 30); do
    NOW="$(sh "$CI/service-pid.sh")"
    if [ -n "$NOW" ] && [ "$NOW" != 0 ] && [ "$NOW" != "$OLD" ]; then
        echo "daemon restarted: pid $OLD -> $NOW"
        exit 0
    fi
    sleep 2
done
echo "FAIL: daemon (pid $OLD) did not restart after $BIN was replaced" >&2
exit 1
