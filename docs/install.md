# Installation

Ferngeist Gateway is a self-hosted backend service for ACP-compatible agents.
Pick the channel for your OS; all channels install the daemon as a background
service that starts without you starting it by hand and stays running (a
per-user service on systemd and launchd, a per-user scheduled task on Windows, a
system service running as your user on OpenRC). Whether it survives a *reboot*
with nobody logged in depends on the platform: always on OpenRC, never on a
Windows logon task, and on systemd and macOS it needs the user's session to
outlive logins — systemd's linger, which the installer enables.

> **Channel availability:** the package channels below (Homebrew tap, apt/dnf,
> pacman) are published by the goreleaser release pipeline on the next tagged
> release. Until the channels you want exist, use the one-command installer or
> the manual channel for your OS.

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
a per-user scheduled task (no UAC prompt), and adds the CLI
to your user PATH. The gateway listens on your local network by default (same as
the macOS/Linux installer); pass `-Localhost` to keep it on localhost only,
`-Yes` to skip the install prompt, `-KeepDownloads` to keep the
downloaded zip. The task starts when you log on, so a machine that reboots and
nobody logs into does not start the gateway — arrange an automatic logon, or
keep the box logged in, if it is meant to run headless. Updates are manual: run `ferngeist-gateway update` when a new
release is announced.

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

Or install manually:

```bash
brew install arafatamim/tap/ferngeist-gateway
```

The installer registers the daemon as a per-user service (a LaunchAgent on
macOS, a systemd user unit on Linuxbrew) with LAN enabled. After a manual
`brew install`, start it with `ferngeist-gateway daemon install --keep-settings --lan`.
After `brew upgrade ferngeist-gateway`, the daemon restarts on the new version
within seconds. Check `ferngeist-gateway daemon status` for the running version.
If you upgraded from 1.0.0 or earlier, run the `daemon install` command once.

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

The package postinstall runs `daemon install --keep-settings --lan` for the
invoking user: a first install listens on `0.0.0.0` (LAN), and upgrades reapply
the options of your existing install (`--remote`, `--port`, `--host`,
`--public-url`) instead of resetting them. Re-run
`ferngeist-gateway daemon install` with the flags you want to change them.

To keep the gateway running after logout and at boot, enable lingering once:
`sudo loginctl enable-linger $USER`. Put custom environment variables (for
example `FERNGEIST_GATEWAY_TAILSCALE_AUTH_KEY`) in
`~/.local/share/ferngeist-gateway/config/daemon.override.env`; unlike
`daemon.env` it is never rewritten by `daemon install`.

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

### Alpine / Gentoo / Artix (OpenRC)

On a host without systemd, `daemon install` detects OpenRC and registers a
system service instead of a user unit. OpenRC 0.60+ also has user services
(`rc-service --user`, runscripts in `~/.config/rc/init.d`), but they are bound
to a login session, and OpenRC starts a user session at boot only through a
system `user.$USER` service — which needs root too. So this install is a system
service that needs sudo: run it from the account the gateway should run as.

```bash
sudo ./ferngeist-gateway daemon install
```

The generated `/etc/init.d/ferngeist-gateway` runscript supervises the daemon
with `supervise-daemon` (crash restarts, like the systemd unit's
`Restart=on-failure`) and runs it as the sudoing user, with state in that
user's `~/.local/share/ferngeist-gateway` — the same layout as the systemd
install, and no linger step is needed because the service is enabled in the
`default` runlevel. `daemon uninstall`, `start`, `stop`, and `restart` also
require sudo on these hosts, and so does `update` once the service is
installed — it stops the service to replace its binary, and it replaces the
*sudoing* user's copy, not root's.

When upgrading an OpenRC installation from `v2.2.0`, download and extract the
fixed release, then run `sudo ./ferngeist-gateway daemon install --keep-settings`
from the gateway user's account. This replaces the installed runscript. The
`v2.2.0` updater replaces only the binary and would restart the old runscript.

### Manual (tarball)

```bash
tar -xzf ferngeist-gateway_<ver>_linux_amd64.tar.gz
./ferngeist-gateway daemon install
```

## packslip (any platform)

Each release carries a signed [packslip](https://packslip.dev) that lists the
archives above with their digests. With the `packslip` CLI installed:

```sh
packslip install github.com/arafatamim/ferngeist-acp-gateway
ferngeist-gateway daemon install
```

To check a tarball you downloaded yourself against it:

```sh
packslip verify packslip.sigstore.json --artifact ferngeist-gateway_<ver>_linux_amd64.tar.gz
gh attestation verify ferngeist-gateway_<ver>_linux_amd64.tar.gz --repo arafatamim/ferngeist-acp-gateway
```

## Uninstalling

`ferngeist-gateway daemon uninstall` stops the daemon and removes the
background-service registration. Run it from the same binary you installed
(or any copy). On Windows, run it from an elevated terminal; on systemd
Linux and macOS run it as your own user (no `sudo`); on OpenRC Linux run it
with `sudo`, like the install:

```powershell
ferngeist-gateway daemon uninstall
```

Erase user data too (pairings, settings, logs, cached agent runtimes):

```powershell
ferngeist-gateway daemon uninstall --purge
```

All platforms share the same contract:

| | default `uninstall` | `--purge` |
|---|---|---|
| Windows (scheduled task) | task + daemon stopped — binary, wrapper, config, data stay | full install dir (binary, config, data) |
| macOS (LaunchAgent) | LaunchAgent plist + daemon stopped — binary, config, data stay | full install dir |
| Linux (systemd user unit) | unit file + daemon stopped — binary, config, data stay | full install dir |
| Linux (OpenRC runscript) | init script + runlevel entry + daemon stopped — binary, config, data stay | full install dir |

Notes:

- Removing the apt/dnf/pacman package (`sudo apt remove ferngeist-gateway`,
  etc.) also runs `daemon uninstall` for the user who ran `sudo`.
- Uninstall semantics are consistent across platforms: plain `uninstall`
  unregisters the background service and stops the daemon, but leaves every
  installed file in place — a later `daemon install` re-registers the same
  install and resumes from the same state, no re-download needed. `--purge`
  is the only way to reclaim the binary, config, and data.
- Windows extra detail: since installing on Windows copies the binary into
  the service dir, running `daemon uninstall --purge` from that installed
  copy works — the manager renames the running binary aside and a detached
  helper deletes it a few seconds after the command exits.

## Updating

- **Homebrew:** `brew upgrade ferngeist-gateway`
- **apt:** `sudo apt update && sudo apt upgrade ferngeist-gateway`
- **dnf/yum:** `sudo dnf upgrade ferngeist-gateway` (repo installs only)
- **pacman:** `sudo pacman -Syu` (repo installs only)
- **Manual installs:** `ferngeist-gateway update` (verifies against the release
  SHA256SUMS before swapping the binary). Package-manager installs refuse to
  self-update; use the package manager instead.

Package-manager installs run the packaged binary directly, so the daemon
restarts on the new version within seconds of any upgrade, including
unattended ones.

The daemon checks GitHub Releases periodically and pushes an update-available
notification to paired devices. Applying is always manual.
