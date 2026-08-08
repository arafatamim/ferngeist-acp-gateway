# Installation

Ferngeist Gateway is a self-hosted backend service for ACP-compatible agents.
Pick the channel for your OS; all channels install the daemon as a per-user
service that starts at login and stays running.

> **Channel availability:** the package channels below (winget, Homebrew tap,
> apt/dnf, AUR) are published by the goreleaser release pipeline on the next
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

### Homebrew (recommended)

```bash
brew tap arafatamim/homebrew-ferngeist
brew install ferngeist-gateway
ferngeist-gateway daemon install
```

Homebrew installs never trigger Gatekeeper, so no warnings appear. The daemon
runs as a per-user LaunchAgent (starts at login, keeps running).

### Manual (tarball)

1. Download `ferngeist-gateway_<ver>_darwin_arm64.tar.gz` (or `_darwin_amd64`).
2. Extract, then run `./ferngeist-gateway daemon install`.
3. Because the build is unsigned, Gatekeeper shows "unknown developer" on first
   launch: right-click the binary → Open → Open, then run `daemon install`.

## Linux

### Ubuntu / Debian

```bash
sudo dpkg -i ferngeist-gateway_<ver>_amd64.deb
# or, when the apt repo is live:
sudo apt install ferngeist-gateway
```

The package postinstall runs `daemon install` for the invoking user.

### Fedora / RHEL

```bash
sudo dnf install ferngeist-gateway_<ver>_x86_64.rpm
```

### Arch (AUR)

```bash
paru -S ferngeist-gateway-bin
```

### Manual (tarball)

```bash
tar -xzf ferngeist-gateway_<ver>_linux_amd64.tar.gz
./ferngeist-gateway daemon install
```

## Updating

- **winget:** `winget upgrade Ferngeist.Gateway`
- **Homebrew:** `brew upgrade ferngeist-gateway`
- **apt/dnf/AUR:** your package manager's normal upgrade
- **Manual installs:** `ferngeist-gateway update` (verifies against the release
  SHA256SUMS before swapping the binary). Package-manager installs refuse to
  self-update; use the package manager instead.

The daemon checks GitHub Releases periodically and pushes an update-available
notification to paired devices. Applying is always manual.
