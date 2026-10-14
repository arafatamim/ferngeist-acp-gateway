#!/bin/sh
# Waits up to ~30s for the daemon to stop answering `daemon status`.
# Usage: wait-gone.sh path-to-ferngeist-gateway what-should-have-stopped-it
for _ in $(seq 15); do
    "$1" daemon status >/dev/null 2>&1 || exit 0
    sleep 2
done
"$1" daemon status
echo "FAIL: daemon still running after $2" >&2
exit 1
