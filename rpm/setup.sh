#!/bin/sh
# One-time dnf/yum repo setup for the Ferngeist Gateway.
# Trusts the repo signing key, adds the repo file, and installs the package.
# Safe to re-run (idempotent). Requires sudo.
#
# Usage:  curl -fsSL https://arafatamim.github.io/ferngeist-acp-gateway/rpm/setup.sh | sudo sh
set -eu

REPO_BASE="https://arafatamim.github.io/ferngeist-acp-gateway/rpm"
REPO_FILE="/etc/yum.repos.d/ferngeist-gateway.repo"

echo "==> Importing Ferngeist Gateway signing key"
rpm --import "$REPO_BASE/key.asc"

echo "==> Adding repo: $REPO_FILE"
cat > "$REPO_FILE" <<EOF
[ferngeist-gateway]
name=Ferngeist Gateway
baseurl=$REPO_BASE/\$basearch
enabled=1
gpgcheck=1
repo_gpgcheck=1
gpgkey=$REPO_BASE/key.asc
EOF

echo "==> Installing ferngeist-gateway"
if command -v dnf >/dev/null 2>&1; then
  dnf install -y ferngeist-gateway
else
  yum install -y ferngeist-gateway
fi

echo "==> Done. The gateway is installed; manage it with: ferngeist-gateway --help"
