#!/bin/sh
# Runs after dpkg/rpm installs the binary. `daemon install` is idempotent and
# per-user; if HOME is unavailable (root package install), skip silently.
if [ -n "$HOME" ] && [ -x "$HOME/.local/share/ferngeist-gateway/bin/ferngeist-gateway" ]; then
    "$HOME/.local/share/ferngeist-gateway/bin/ferngeist-gateway" daemon install || true
fi
exit 0
