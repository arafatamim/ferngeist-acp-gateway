#!/bin/sh
# Universal installer for the Ferngeist Gateway (macOS + Linux).
# Tries the native package managers in order — apt, pacman, rpm (dnf/yum),
# then Homebrew/Linuxbrew — and falls back to a user-level binary install.
# Every channel prints what it is about to do and asks for confirmation
# (unless --yes). Safe to re-run; all channels are idempotent.
#
# Usage:
#   curl -fsSL https://arafatamim.github.io/ferngeist-acp-gateway/install.sh | sh
#   curl -fsSL https://arafatamim.github.io/ferngeist-acp-gateway/install.sh | sh -s -- --yes
#   curl -fsSL https://arafatamim.github.io/ferngeist-acp-gateway/install.sh | sh -s -- --channel brew
#   curl -fsSL https://arafatamim.github.io/ferngeist-acp-gateway/install.sh | sh -s -- --binary
#   curl -fsSL https://arafatamim.github.io/ferngeist-acp-gateway/install.sh | sh -s -- --localhost
#
# Flags:
#   --yes, -y           skip all prompts (non-interactive / CI)
#   --channel <name>    force a channel: apt|pacman|rpm|brew|binary
#   --binary            force the user-level binary install
#   --localhost         daemon listens on 127.0.0.1 only (default: --lan)
#   --help, -h          show this help
set -eu

# ---------------------------------------------------------------------------
# Configuration
# ---------------------------------------------------------------------------
# FERNGEIST_REPO_BASE lets CI point the package channels at a local copy of
# the gh-pages repos before they are published.
REPO_BASE="${FERNGEIST_REPO_BASE:-https://arafatamim.github.io/ferngeist-acp-gateway}"
GIT_REPO="https://github.com/arafatamim/ferngeist-acp-gateway"
RELEASE_BASE="$GIT_REPO/releases"

YES=0
FORCE_CHANNEL=""
DAEMON_FLAGS="--lan"

# ---------------------------------------------------------------------------
# Helpers
# ---------------------------------------------------------------------------
say() { printf '%s\n' "$*"; }
step() { printf '==> %s\n' "$*"; }
warn() { printf 'WARNING: %s\n' "$*" >&2; }
die() { printf 'ERROR: %s\n' "$*" >&2; exit 1; }

have() { command -v "$1" >/dev/null 2>&1; }

confirm() {
    # $1 = prompt. Returns 0 (yes) / 1 (no). --yes always answers yes.
    if [ "$YES" = 1 ]; then
        printf '==> %s [y/N] y (--yes)\n' "$1"
        return 0
    fi
    printf '==> %s [y/N] ' "$1"
    # Under `curl ... | sh` stdin is the pipe, not the terminal; read from
    # /dev/tty so the user can actually answer. /dev/tty can exist yet fail
    # to open (no controlling terminal, e.g. CI), so any read failure or EOF
    # counts as "no" rather than tripping `set -u` below.
    if [ -t 0 ]; then
        read -r _ans || _ans=
    else
        read -r _ans 2>/dev/null </dev/tty || _ans=
    fi
    case "$_ans" in
        y|Y|yes|YES|Yes) return 0 ;;
        *) return 1 ;;
    esac
}

# ---------------------------------------------------------------------------
# Args
# ---------------------------------------------------------------------------
usage() {
    # `$0` is the script path when run from a file, or "sh" when piped —
    # only read the header from a real file.
    if [ -f "$0" ]; then
        sed -n '2,20p' "$0" | sed 's/^# \{0,1\}//'
    else
        say "Ferngeist Gateway installer — see the script source for usage and flags."
    fi
    exit 0
}

while [ $# -gt 0 ]; do
    case "$1" in
        --yes|-y) YES=1 ;;
        --channel) [ $# -ge 2 ] || die "--channel needs a value"; FORCE_CHANNEL="$2"; shift ;;
        --binary) FORCE_CHANNEL=binary ;;
        --localhost) DAEMON_FLAGS="" ;;
        --help|-h) usage ;;
        *) die "unknown argument: $1 (see --help)" ;;
    esac
    shift
done

# ---------------------------------------------------------------------------
# Platform detection
# ---------------------------------------------------------------------------
OS="$(uname -s)"
case "$OS" in
    Linux) PLATFORM=linux ;;
    Darwin) PLATFORM=darwin ;;
    *) die "unsupported operating system: $OS (expected Linux or Darwin)" ;;
esac

# Termux (Android userspace) is a Debian-like userland but has no systemd,
# no sudo by default, and installs per-user into $PREFIX. It has apt-get and
# dpkg, so plain channel detection would pick apt and fail. Force the
# user-level binary install instead.
IS_TERMUX=0
if [ "$PLATFORM" = linux ] && [ -n "${PREFIX:-}" ] && [ "$(uname -o 2>/dev/null)" = "Android" ]; then
    IS_TERMUX=1
fi

ARCH="$(uname -m)"
case "$ARCH" in
    x86_64|amd64) GOARCH=amd64 ;;
    arm64|aarch64) GOARCH=arm64 ;;
    *) die "unsupported architecture: $ARCH (expected x86_64 or arm64)" ;;
esac

# The service manager copies the running binary into its own bin dir on
# `daemon install`; the CLI symlink below points at that canonical copy.
case "$PLATFORM" in
    linux) SERVICE_BIN="$HOME/.local/share/ferngeist-gateway/bin/ferngeist-gateway" ;;
    darwin) SERVICE_BIN="$HOME/Library/Application Support/Ferngeist Gateway/bin/ferngeist-gateway" ;;
esac

# ---------------------------------------------------------------------------
# Channel availability
# ---------------------------------------------------------------------------
# Order of preference (user-specified): apt -> pacman -> rpm -> brew -> binary.
# On macOS none of apt/pacman/rpm exist natively, so brew is first there.
channel_available() {
    case "$1" in
        apt) [ "$PLATFORM" = linux ] && have apt-get && have dpkg ;;
        pacman) [ "$PLATFORM" = linux ] && have pacman ;;
        rpm) [ "$PLATFORM" = linux ] && { have dnf || have yum; } ;;
        brew) have brew ;;
        binary) return 0 ;;
        *) return 1 ;;
    esac
}

# Pick the channel to use. --channel overrides; otherwise first available.
pick_channel() {
    if [ -n "$FORCE_CHANNEL" ]; then
        if channel_available "$FORCE_CHANNEL"; then
            if [ "$IS_TERMUX" = 1 ] && [ "$FORCE_CHANNEL" != binary ]; then
                warn "Termux has no systemd/sudo; forcing the binary install (--channel $FORCE_CHANNEL ignored)"
                CHANNEL=binary
                return 0
            fi
            CHANNEL="$FORCE_CHANNEL"
            return 0
        fi
        die "requested channel '$FORCE_CHANNEL' is not available on $OS/$ARCH"
    fi
    if [ "$IS_TERMUX" = 1 ]; then
        step "Termux detected (no systemd/sudo); using the user-level binary install"
        CHANNEL=binary
        return 0
    fi
    for c in apt pacman rpm brew binary; do
        if channel_available "$c"; then
            CHANNEL="$c"
            return 0
        fi
    done
    die "no install method available on $OS/$ARCH"
}

# ---------------------------------------------------------------------------
# Package-manager channels (all require root/sudo; all idempotent)
# ---------------------------------------------------------------------------
run_as_root() {
    if [ "$(id -u)" = 0 ]; then
        "$@"
    else
        sudo "$@"
    fi
}

install_via_apt() {
    step "Installing via apt (Debian/Ubuntu)"
    if ! confirm "Add the Ferngeist Gateway apt repository and install the package? (requires sudo)"; then
        DECLINED=1; return 0
    fi
    KEYRING="/usr/share/keyrings/ferngeist-gateway.gpg"
    SOURCES="/etc/apt/sources.list.d/ferngeist-gateway.list"
    step "Trusting the repo signing key at $KEYRING"
    # --batch --yes: a re-run must overwrite the keyring, not prompt for it.
    curl -fsSL "$REPO_BASE/deb/key.asc" | run_as_root gpg --batch --yes --dearmor -o "$KEYRING"
    step "Adding apt source: $SOURCES"
    run_as_root sh -c "echo 'deb [signed-by=$KEYRING] $REPO_BASE/deb/ stable main' > '$SOURCES'"
    step "Updating package lists"
    run_as_root apt-get update
    step "Installing ferngeist-gateway"
    run_as_root apt-get install -y ferngeist-gateway
    return 0
}

install_via_pacman() {
    step "Installing via pacman (Arch Linux)"
    if ! confirm "Add the Ferngeist Gateway pacman repository and install the package? (requires root)"; then
        DECLINED=1; return 0
    fi
    PACMAN_CONF="/etc/pacman.conf"
    TMPKEY="$(mktemp)"
    trap 'rm -f "$TMPKEY"' EXIT
    step "Adding Ferngeist Gateway signing key to the pacman keyring"
    curl -fsSL "$REPO_BASE/arch/key.asc" -o "$TMPKEY"
    run_as_root pacman-key --add "$TMPKEY"
    FP="$(gpg --with-colons --show-keys "$TMPKEY" 2>/dev/null | awk -F: '$1=="fpr" {print $10; exit}')"
    [ -n "$FP" ] || die "could not read signing key fingerprint from $REPO_BASE/arch/key.asc"
    run_as_root pacman-key --lsign-key "$FP"
    if ! grep -q '^\[ferngeist-gateway\]' "$PACMAN_CONF" 2>/dev/null; then
        step "Adding repo section to $PACMAN_CONF"
        run_as_root sh -c "printf '\n[ferngeist-gateway]\nSigLevel = Required\nServer = %s/\$arch\n' '$REPO_BASE/arch' >> '$PACMAN_CONF'"
    fi
    step "Updating package lists"
    run_as_root pacman -Syu --noconfirm
    step "Installing ferngeist-gateway"
    run_as_root pacman -S --noconfirm ferngeist-gateway
    return 0
}

install_via_rpm() {
    step "Installing via rpm (Fedora/RHEL)"
    if ! confirm "Add the Ferngeist Gateway dnf/yum repository and install the package? (requires sudo)"; then
        DECLINED=1; return 0
    fi
    REPO_FILE="/etc/yum.repos.d/ferngeist-gateway.repo"
    step "Importing the Ferngeist Gateway signing key"
    run_as_root rpm --import "$REPO_BASE/rpm/key.asc"
    step "Adding repo: $REPO_FILE"
    run_as_root sh -c "cat > '$REPO_FILE' <<'EOF'
[ferngeist-gateway]
name=Ferngeist Gateway
baseurl=$REPO_BASE/rpm/\$basearch
enabled=1
gpgcheck=1
repo_gpgcheck=1
gpgkey=$REPO_BASE/rpm/key.asc
EOF"
    step "Installing ferngeist-gateway"
    if have dnf; then
        run_as_root dnf install -y ferngeist-gateway
    else
        run_as_root yum install -y ferngeist-gateway
    fi
    return 0
}

install_via_brew() {
    step "Installing via Homebrew/Linuxbrew"
    if ! confirm "Tap the Ferngeist Gateway formula and install via brew? (no sudo needed)"; then
        DECLINED=1; return 0
    fi
    # The qualified name taps arafatamim/homebrew-tap on the way; newer brew
    # also refuses a bare name from an untrusted tap.
    step "Installing ferngeist-gateway from the arafatamim/tap tap"
    brew install arafatamim/tap/ferngeist-gateway
    return 0
}

# ---------------------------------------------------------------------------
# Binary fallback (user-level, no root; both platforms)
# ---------------------------------------------------------------------------
install_via_binary() {
    step "Installing the binary at user level"
    if ! confirm "Download the latest release and install the daemon for this user only? (no sudo needed)"; then
        return 1
    fi

    # Resolve the latest release: tag + the matching asset + SHA256SUMS.
    step "Resolving the latest release"
    # Follow the releases/latest redirect to .../tag/<tag> rather than asking
    # the API, whose 60 requests/hour per IP shared NATs and CI exhaust.
    TAG="$(curl -fsSLI -o /dev/null -w '%{url_effective}' "$RELEASE_BASE/latest")"
    TAG="${TAG##*/}"
    case "$TAG" in v[0-9]*) ;; *) die "could not resolve the latest release tag" ;; esac
    # Termux runs Android's bionic libc, whose linker64 rejects the static
    # ET_EXEC linux build ("unexpected e_type"); the android build is a PIE
    # that bionic accepts. Fetch the android asset on Termux, linux elsewhere.
    if [ "$IS_TERMUX" = 1 ]; then
        ASSET_OS=android
    else
        ASSET_OS="$PLATFORM"
    fi
    ASSET="ferngeist-gateway_${TAG#v}_${ASSET_OS}_${GOARCH}.tar.gz"
    step "Latest release: $TAG (asset $ASSET)"

    TMPDIR_BIN="$(mktemp -d)"
    trap 'rm -rf "$TMPDIR_BIN"' EXIT
    ARCHIVE="$TMPDIR_BIN/ferngeist-gateway.tar.gz"

    step "Downloading $RELEASE_BASE/download/$TAG/$ASSET"
    curl -fL --progress-bar "$RELEASE_BASE/download/$TAG/$ASSET" -o "$ARCHIVE"

    step "Verifying sha256 checksum"
    SUMS="$TMPDIR_BIN/SHA256SUMS"
    curl -fsSL "$RELEASE_BASE/download/$TAG/SHA256SUMS" -o "$SUMS"
    EXPECTED="$(awk -v a="$ASSET" '$2 == a {print $1}' "$SUMS")"
    [ -n "$EXPECTED" ] || die "no checksum for $ASSET in SHA256SUMS"
    # macOS ships shasum, not (always) sha256sum.
    if have sha256sum; then
        ACTUAL="$(sha256sum "$ARCHIVE" | awk '{print $1}')"
    else
        ACTUAL="$(shasum -a 256 "$ARCHIVE" | awk '{print $1}')"
    fi
    [ "$EXPECTED" = "$ACTUAL" ] || die "checksum mismatch for $ASSET (expected $EXPECTED, got $ACTUAL)"

    step "Extracting binary"
    tar -xzf "$ARCHIVE" -C "$TMPDIR_BIN"
    BIN="$TMPDIR_BIN/ferngeist-gateway"
    [ -x "$BIN" ] || die "binary not found in $ASSET"

    if [ "$IS_TERMUX" = 1 ]; then
        # Termux has no systemd, so `daemon install` cannot register a
        # service. Persist the binary ourselves into ~/.local/bin (the temp
        # dir is deleted on exit) and hand the user the runit recipe.
        BIN_DIR="$HOME/.local/bin"
        mkdir -p "$BIN_DIR"
        cp "$BIN" "$BIN_DIR/ferngeist-gateway"
        chmod +x "$BIN_DIR/ferngeist-gateway"
        step "Installed the binary at $BIN_DIR/ferngeist-gateway"
        warn "add $BIN_DIR to your PATH to use 'ferngeist-gateway' from a terminal:"
        warn "  echo 'export PATH=\"\$HOME/.local/bin:\$PATH\"' >> ~/.bashrc"
        step "Termux has no systemd; run the gateway as a foreground daemon or a runit service:"
        step "  $BIN_DIR/ferngeist-gateway daemon run $DAEMON_FLAGS"
        step "  # or, for a supervised service: pkg install termux-services && sv-enable ferngeist-gateway (with a run script)"
        return 0
    fi

    # `daemon install` copies the running binary into the service bin dir
    # and registers + starts the per-user service. It can fail on headless
    # boxes (no systemd user session / no launchd) — the binary is still
    # usable, so warn instead of aborting the install.
    step "Installing + starting the daemon (per-user service, $([ -z "$DAEMON_FLAGS" ] && echo 'localhost only' || echo 'LAN enabled'))"
    if ! "$BIN" daemon install $DAEMON_FLAGS; then
        # $BIN lives in the temp dir deleted on exit; keep a copy where the
        # CLI symlink below points so the retry command actually exists.
        mkdir -p "$(dirname "$SERVICE_BIN")"
        cp "$BIN" "$SERVICE_BIN"
        warn "daemon install failed; run 'ferngeist-gateway daemon install' once you have a logged-in user session"
    fi

    # Symlink the CLI into a user bin dir so `ferngeist-gateway` is on PATH.
    BIN_DIR="$HOME/.local/bin"
    if [ -d "$BIN_DIR" ] || [ ! -e "$BIN_DIR" ]; then
        mkdir -p "$BIN_DIR"
        if ln -sf "$SERVICE_BIN" "$BIN_DIR/ferngeist-gateway" 2>/dev/null; then
            step "CLI linked at $BIN_DIR/ferngeist-gateway"
        else
            warn "could not symlink the CLI into $BIN_DIR; use the daemon's copy at $SERVICE_BIN"
        fi
    fi

    # macOS does not add ~/.local/bin to PATH by default; tell the user.
    if [ "$PLATFORM" = darwin ]; then
        warn "macOS: add $BIN_DIR to your PATH to use 'ferngeist-gateway' from a terminal:"
        warn "  echo 'export PATH=\"\$HOME/.local/bin:\$PATH\"' >> ~/.zshrc"
    fi

    step "Done."
    return 0
}

# ---------------------------------------------------------------------------
# Main
# ---------------------------------------------------------------------------
step "Ferngeist Gateway installer — $OS ($ARCH)"
step "Detected platform: $PLATFORM"
pick_channel
step "Channel: $CHANNEL"

# Declines set DECLINED instead of returning non-zero: under `set -e` a
# failing function call would exit before the fallback below could run.
DECLINED=0
case "$CHANNEL" in
    apt) install_via_apt ;;
    pacman) install_via_pacman ;;
    rpm) install_via_rpm ;;
    brew) install_via_brew ;;
    binary) install_via_binary ;;
esac

if [ "$DECLINED" = 1 ]; then
    # A forced channel declined = abort, not a silent downgrade.
    [ -z "$FORCE_CHANNEL" ] || die "install cancelled"
    step "Declined; falling back to the user-level binary install"
    CHANNEL=binary
    install_via_binary
elif [ "$CHANNEL" != binary ]; then
    # Register here too: package scripts only run when the package changes
    # (a re-run after `daemon uninstall` would leave no daemon), brew has no
    # post_install (it is sandboxed), and --localhost must drop --keep-settings.
    if [ "$CHANNEL" = brew ]; then GW="$(brew --prefix)/bin/ferngeist-gateway"; else GW=/usr/bin/ferngeist-gateway; fi
    if [ -n "$DAEMON_FLAGS" ]; then FLAGS="--keep-settings $DAEMON_FLAGS"; else FLAGS=""; fi
    if [ "$(id -u)" = 0 ]; then
        # Under sudo the package postinstall already registered it with --lan.
        [ -n "${SUDO_USER:-}" ] && [ -n "$DAEMON_FLAGS" ] ||
            warn "running as root: as your own user, run '$GW daemon install $FLAGS'"
    else
        step "Registering the daemon for this user"
        "$GW" daemon install $FLAGS || warn "could not register the daemon; run '$GW daemon install $FLAGS'"
    fi
fi

say ""
say "Ferngeist Gateway installed. Manage it with: ferngeist-gateway --help"
case "$CHANNEL" in
    apt) say "Update: sudo apt update && sudo apt upgrade ferngeist-gateway" ;;
    pacman) say "Update: sudo pacman -Syu" ;;
    rpm) say "Update: sudo dnf upgrade ferngeist-gateway (or yum)" ;;
    brew) say "Update: brew upgrade ferngeist-gateway" ;;
    binary) say "Update: ferngeist-gateway update (package-manager builds refuse self-update)" ;;
esac
