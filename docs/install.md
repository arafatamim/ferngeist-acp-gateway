# Installation

Ferngeist Gateway is a self-hosted backend service for ACP-compatible agents.
Pick the channel for your OS; all channels install the daemon as a per-user
service that starts at login and stays running.

> **Channel availability:** the package channels below (winget, Homebrew tap,
> apt/dnf, pacman) are published by the goreleaser release pipeline on the next
> tagged release. Until then, use the manual channel for your OS.

## One-command install (macOS + Linux)

```bash
curl -fsSL https://arafatamim.github.io/ferngeist-acp-gateway/install.sh | sh
```

The script detects your platform and tries the package managers in order —
**apt** (Debian/Ubuntu), **pacman** (Arch), **rpm/dnf** (Fedora/RHEL), then
**Homebrew/Linuxbrew** — falling back to a user-level binary install if none
apply. It prints what it is about to do and asks for confirmation at each step.

Options (pass after `sh -s --`):

```bash
# Non-interactive (skip prompts)
curl -fsSL https://arafatamim.github.io/ferngeist-acp-gateway/install.sh | sh -s -- --yes
# Force a specific channel
curl -fsSL https://arafatamim.github.io/ferngeist-acp-gateway/install.sh | sh -s -- --channel brew
# Force the user-level binary install
curl -fsSL https://arafatamim.github.io/ferngeist-acp-gateway/install.sh | sh -s -- --binary
# Listen on localhost only (default is LAN-enabled)
curl -fsSL https://arafatamim.github.io/ferngeist-acp-gateway/install.sh | sh -s -- --localhost
```

The detailed per-OS instructions below remain valid; the one-liner is a
convenience wrapper that picks the right one for you.

## Windows

### PowerShell installer (recommended)

One command, no admin needed, installs per-user:

```powershell
irm https://arafatamim.github.io/ferngeist-acp-gateway/install.ps1 | iex
```

Or download and run it directly (needed to pass flags):

```powershell
curl.exe -fsSL https://arafatamim.github.io/ferngeist-acp-gateway/install.ps1 -o install.ps1
powershell -ExecutionPolicy Bypass -File .\install.ps1
```

It fetches the latest release zip, verifies its checksum, installs the daemon as
a per-user scheduled task (one UAC prompt to create the task), and adds the CLI
to your user PATH. Pass `-Lan` to expose the gateway on your network (default is
localhost-only), `-Yes` to skip the install prompt, `-KeepDownloads` to keep the
downloaded zip. Updates are manual: run `ferngeist-gateway update` when a new
release is announced.

### winget

```powershell
winget install Ferngeist.Gateway
```

The first run shows a SmartScreen prompt because the build is unsigned; choose
"More info" → "Run anyway". The daemon installs and starts automatically.

### Manual (portable zip)

1. Download `ferngeist-gateway_<ver>_windows_amd64.zip` from the latest
   [release](https://github.com/arafatamim/ferngeist-acp-gateway/releases).
2. Extract, then run:
   ```powershell
   .\ferngeist-gateway.exe daemon install
   ```
3. The daemon runs as a per-user scheduled task; check it with
   `.\ferngeist-gateway.exe daemon status`.

## macOS

### Homebrew / Linuxbrew (recommended)

Install via the one-command installer (tries brew):

```bash
curl -fsSL https://arafatamim.github.io/ferngeist-acp-gateway/install.sh | sh
```

Or tap + install manually:

```bash
brew tap arafatamim/ferngeist-acp-gateway https://github.com/arafatamim/ferngeist-acp-gateway
brew install ferngeist-gateway
```

The formula installs the binary and runs the daemon install (`daemon install --lan`)
as a per-user service — same as the Linux packages. On macOS it is a LaunchAgent;
on Linux (Linuxbrew) a systemd user unit. Updates: `brew upgrade ferngeist-gateway`.

Homebrew installs never trigger Gatekeeper, so no warnings appear. To make the
gateway reachable only on localhost, re-run `ferngeist-gateway daemon install`.

### Manual (tarball)

1. Download `ferngeist-gateway_<ver>_darwin_arm64.tar.gz` (or `_darwin_amd64`).
2. Extract, then run `./ferngeist-gateway daemon install`.
3. Because the build is unsigned, Gatekeeper shows "unknown developer" on first
   launch: right-click the binary → Open → Open, then run `daemon install`.

## Linux

### Ubuntu / Debian

Install via the one-command installer (tries apt first):

```bash
curl -fsSL https://arafatamim.github.io/ferngeist-acp-gateway/install.sh | sh
```

Prefer to add the repo by hand (e.g. to pin the keyring or audit the steps)?

```bash
# One-time: add the repo signing key, then the sources line.
curl -fsSL https://arafatamim.github.io/ferngeist-acp-gateway/deb/key.asc | sudo gpg --dearmor -o /usr/share/keyrings/ferngeist-gateway.gpg
echo "deb [signed-by=/usr/share/keyrings/ferngeist-gateway.gpg] https://arafatamim.github.io/ferngeist-acp-gateway/deb/ stable main" | sudo tee /etc/apt/sources.list.d/ferngeist-gateway.list
sudo apt update
sudo apt install ferngeist-gateway
```

The package postinstall runs `daemon install` for the invoking user. By default
the service listens on `127.0.0.1` (localhost only). To make the gateway
reachable from other devices on your network, run
`ferngeist-gateway daemon install --lan` (equivalent to `--host 0.0.0.0`).

### Fedora / RHEL

Install via the one-command installer (tries rpm/dnf):

```bash
curl -fsSL https://arafatamim.github.io/ferngeist-acp-gateway/install.sh | sh
```

Manual equivalent (e.g. to pin the key):

```bash
sudo rpm --import https://arafatamim.github.io/ferngeist-acp-gateway/rpm/key.asc
sudo tee /etc/yum.repos.d/ferngeist-gateway.repo >/dev/null <<'EOF'
[ferngeist-gateway]
name=Ferngeist Gateway
baseurl=https://arafatamim.github.io/ferngeist-acp-gateway/rpm/$basearch
enabled=1
gpgcheck=1
repo_gpgcheck=1
gpgkey=https://arafatamim.github.io/ferngeist-acp-gateway/rpm/key.asc
EOF
sudo dnf install ferngeist-gateway
```

### Arch Linux

Install via the one-command installer (tries pacman):

```bash
curl -fsSL https://arafatamim.github.io/ferngeist-acp-gateway/install.sh | sh
```

Manual equivalent (e.g. to audit the key):

```bash
curl -fsSL https://arafatamim.github.io/ferngeist-acp-gateway/arch/key.asc | sudo pacman-key --add -
sudo pacman-key --lsign-key <fingerprint-from-key.asc>
sudo tee -a /etc/pacman.conf >/dev/null <<'EOF'

[ferngeist-gateway]
SigLevel = Required
Server = https://arafatamim.github.io/ferngeist-acp-gateway/arch/$arch
EOF
sudo pacman -Syu
sudo pacman -S ferngeist-gateway
```

### Manual (tarball)

```bash
tar -xzf ferngeist-gateway_<ver>_linux_amd64.tar.gz
./ferngeist-gateway daemon install
```

## Updating

- **winget:** `winget upgrade Ferngeist.Gateway`
- **Homebrew:** `brew upgrade ferngeist-gateway`
- **apt:** `sudo apt update && sudo apt upgrade ferngeist-gateway`
- **dnf/yum:** `sudo dnf upgrade ferngeist-gateway` (repo installs only)
- **pacman:** `sudo pacman -Syu` (repo installs only)
- **Manual installs:** `ferngeist-gateway update` (verifies against the release
  SHA256SUMS before swapping the binary). Package-manager installs refuse to
  self-update; use the package manager instead.

The daemon checks GitHub Releases periodically and pushes an update-available
notification to paired devices. Applying is always manual.
