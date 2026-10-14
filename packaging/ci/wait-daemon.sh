#!/bin/sh
# Waits up to ~60s for the freshly (re)installed daemon to answer
# `daemon status`, then prints the status. Exits non-zero if it never does.
# Usage: wait-daemon.sh [path-to-ferngeist-gateway]
BIN="${1:-ferngeist-gateway}"
for _ in $(seq 30); do
    "$BIN" daemon status >/dev/null 2>&1 && break
    sleep 2
done
"$BIN" daemon status
