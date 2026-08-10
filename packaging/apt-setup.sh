#!/bin/sh
# One-time apt repo setup for the Ferngeist Gateway.
# Trusts the repo signing key, adds the sources line, and installs the
# package. Safe to re-run (idempotent). Requires sudo.
#
# Usage:  curl -fsSL https://arafatamim.github.io/ferngeist-acp-gateway/deb/setup.sh | sudo sh
set -eu

REPO_BASE="https://arafatamim.github.io/ferngeist-acp-gateway/deb"
KEYRING="/usr/share/keyrings/ferngeist-gateway.gpg"
SOURCES="/etc/apt/sources.list.d/ferngeist-gateway.list"

echo "==> Adding Ferngeist Gateway signing key to $KEYRING"
curl -fsSL "$REPO_BASE/key.asc" | gpg --dearmor -o "$KEYRING"

echo "==> Adding apt source: $SOURCES"
echo "deb [signed-by=$KEYRING] $REPO_BASE/ stable main" > "$SOURCES"

echo "==> Updating package lists"
apt-get update

echo "==> Installing ferngeist-gateway"
apt-get install -y ferngeist-gateway

echo "==> Done. The gateway is installed; manage it with: ferngeist-gateway --help"
