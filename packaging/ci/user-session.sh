#!/bin/sh
# GitHub Ubuntu runners have no login session: start the runner user's
# systemd manager via linger and export its runtime dir to later steps.
set -eu
sudo loginctl enable-linger "$USER"
echo "XDG_RUNTIME_DIR=/run/user/$(id -u)" >> "$GITHUB_ENV"
for _ in $(seq 30); do
    [ -S "/run/user/$(id -u)/bus" ] && exit 0
    sleep 1
done
echo "user bus never came up" >&2
exit 1
