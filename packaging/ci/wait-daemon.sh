#!/bin/sh
# Waits up to ~60s for the daemon to answer `daemon status` with every given
# line regex matching (e.g. 'VERSION +1\.0\.0' 'LAN ENABLED +true'), then
# prints the status. Exits non-zero if it never does.
# Usage: wait-daemon.sh path-to-ferngeist-gateway [line-regex...]
BIN="$1"; shift
ok() {
    out="$("$BIN" daemon status 2>&1)" || return 1
    for want; do
        printf '%s\n' "$out" | grep -Eq "^$want\$" || return 1
    done
}
for _ in $(seq 30); do
    ok "$@" && break
    sleep 2
done
printf '%s\n' "$out"
ok "$@" || { echo "FAIL: daemon status never matched: $*" >&2; exit 1; }
