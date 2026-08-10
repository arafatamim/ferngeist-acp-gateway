#!/bin/sh
# One-time pacman repo setup for the Ferngeist Gateway.
# Trusts the repo signing key, adds the repo section, and installs the
# package. Safe to re-run (idempotent). Requires root.
#
# Usage:  curl -fsSL https://arafatamim.github.io/ferngeist-acp-gateway/arch/setup.sh | sudo sh
set -eu

REPO_BASE="https://arafatamim.github.io/ferngeist-acp-gateway/arch"
PACMAN_CONF="/etc/pacman.conf"
TMPKEY="$(mktemp)"
trap 'rm -f "$TMPKEY"' EXIT

echo "==> Adding Ferngeist Gateway signing key to pacman keyring"
curl -fsSL "$REPO_BASE/key.asc" -o "$TMPKEY"
pacman-key --add "$TMPKEY"
FP="$(gpg --with-colons "$TMPKEY" 2>/dev/null | awk -F: '$1=="fpr" {print $10; exit}')"
if [ -z "$FP" ]; then
  echo "Could not read signing key fingerprint from $REPO_BASE/key.asc" >&2
  exit 1
fi
pacman-key --lsign-key "$FP"

echo "==> Adding repo: $PACMAN_CONF"
if ! grep -q '^\[ferngeist-gateway\]' "$PACMAN_CONF"; then
  printf '\n[ferngeist-gateway]\nSigLevel = Required\nServer = %s/$arch\n' "$REPO_BASE" >> "$PACMAN_CONF"
fi

echo "==> Updating package lists and installing ferngeist-gateway"
pacman -Syu --noconfirm
pacman -S --noconfirm ferngeist-gateway

echo "==> Done. The gateway is installed; manage it with: ferngeist-gateway --help"
