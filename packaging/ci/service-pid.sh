#!/bin/sh
# Prints the daemon's PID as its service manager sees it (empty or 0 when it
# is not running). Usage: service-pid.sh
if [ "$(uname -s)" = Darwin ]; then
    launchctl list com.ferngeist.gateway 2>/dev/null | sed -n 's/.*"PID" = \([0-9]*\);.*/\1/p'
else
    systemctl --user show -p MainPID --value ferngeist-gateway.service
fi
