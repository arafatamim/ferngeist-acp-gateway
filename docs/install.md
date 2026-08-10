# Installation

Ferngeist Gateway is a self-hosted backend service for ACP-compatible agents.
Pick the channel for your OS; all channels install the daemon as a per-user
service that starts at login and stays running.

> **Channel availability:** the package channels below (winget, Homebrew tap,
> apt/dnf, pacman) are published by the goreleaser release pipeline on the next
> tagged release. Until then, use the manual channel for your OS.

## Windows

### winget (recommended)

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

Install the package directly (manual):

```bash
sudo dpkg -i ferngeist-gateway_<ver>_amd64.deb
```

Or add the signed apt repository and install via apt (updates included):

```bash
# One-time setup: trusts the repo signing key, adds the sources line,
# and installs the package. Requires sudo.
curl -fsSL https://arafatamim.github.io/ferngeist-acp-gateway/deb/setup.sh | sudo sh
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

Add the signed dnf/yum repository and install (updates included):

```bash
# One-time setup: imports the signing key, adds the repo file, installs.
curl -fsSL https://arafatamim.github.io/ferngeist-acp-gateway/rpm/setup.sh | sudo sh
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

Add the signed pacman repository and install (updates included):

```bash
# One-time setup: trusts the repo signing key, adds the repo section,
# installs. Requires root.
curl -fsSL https://arafatamim.github.io/ferngeist-acp-gateway/arch/setup.sh | sudo sh
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
