#!/bin/sh
# Runs after dpkg/rpm installs the binary. `daemon install` registers a
# PER-USER systemd user service, which cannot be done as root (root has no
# user session bus; the unit would land under /root anyway). When the package
# was installed via sudo, re-run the install as the invoking user. When no
# invoking user is identifiable (e.g. su -c apt, container root), skip with a
# clear message instead of failing with a cryptic DBus error.
set -u

# The package installs the binary to /usr/bin; prefer it. Fall back to the
# self-managed path so an in-place upgrade of a manual install still registers
# the service.
if [ -x /usr/bin/ferngeist-gateway ]; then
    GATEWAY_BIN=/usr/bin/ferngeist-gateway
elif [ -n "${HOME:-}" ] && [ -x "$HOME/.local/share/ferngeist-gateway/bin/ferngeist-gateway" ]; then
    GATEWAY_BIN="$HOME/.local/share/ferngeist-gateway/bin/ferngeist-gateway"
else
    GATEWAY_BIN=
fi

# --keep-settings: on upgrade, reapply the options of the existing install
# (--remote/--port/...) instead of resetting to the LAN defaults below, which
# only apply to a first install.

# Resolve the non-root user who invoked the install (sudo sets SUDO_USER;
# pkexec sets PKEXEC_UID). Empty when we are root from a non-sudo context.
invoking_user=""
if [ "$(id -u)" = 0 ]; then
    if [ -n "${SUDO_USER:-}" ] && [ "$SUDO_USER" != "root" ]; then
        invoking_user="$SUDO_USER"
    elif [ -n "${PKEXEC_UID:-}" ]; then
        invoking_user="$(id -un "$PKEXEC_UID" 2>/dev/null || true)"
    fi
fi

capture_user_path() {
    account="$(getent passwd "$invoking_user")" || return 1
    user_home="$(printf '%s\n' "$account" | cut -d: -f6)"
    user_shell="$(printf '%s\n' "$account" | cut -d: -f7)"
    [ -n "$user_home" ] && [ -x "$user_shell" ] || return 1
    path_file="$(mktemp)" || return 1
    shell_command='printf %s "$PATH" >&3'
    case "$user_shell" in
        */fish) shell_command='string join : $PATH >&3' ;;
    esac
    captured_path=
    # env -i: start from a minimal PATH so root's sudo/pkexec PATH does not leak in.
    if (cd "$user_home" && timeout -k 2s 10s "$runuser_bin" -u "$invoking_user" -- \
        env -i HOME="$user_home" USER="$invoking_user" LOGNAME="$invoking_user" SHELL="$user_shell" \
        PATH=/usr/local/bin:/usr/bin:/bin \
        "$user_shell" -ilc "$shell_command" </dev/null 3>"$path_file" >/dev/null 2>&1); then
        captured_path="$(head -c 65536 "$path_file")"
    fi
    rm -f "$path_file"
    [ -n "$captured_path" ]
}

if [ -n "$GATEWAY_BIN" ]; then
    # Package installs target non-technical users who want the gateway
    # reachable from their phone/other devices: listen on 0.0.0.0 with LAN
    # enabled. Manual `daemon install` keeps the localhost-only default.
    if [ "$(id -u)" = 0 ] && [ -n "$invoking_user" ]; then
        # runuser/su do not carry the user's session environment; systemctl
        # --user needs XDG_RUNTIME_DIR (the user bus socket lives there).
        # Set it explicitly so the re-run reaches the user's systemd instance.
        uid="$(id -u "$invoking_user" 2>/dev/null || true)"
        if [ -n "$uid" ] && [ -d "/run/user/$uid" ]; then
            export XDG_RUNTIME_DIR="/run/user/$uid"
            export DBUS_SESSION_BUS_ADDRESS="unix:path=/run/user/$uid/bus"
        fi
        if command -v runuser >/dev/null 2>&1; then
            runuser_bin="$(command -v runuser)"
            env_bin="$(command -v env)"
            install_path="$PATH"
            preserve_path=0
            if capture_user_path; then
                # daemon install looks up systemctl/loginctl on this PATH; keep them reachable.
                install_path="$captured_path:/usr/local/bin:/usr/bin:/bin"
            else
                printf 'WARNING: could not capture %s shell PATH; keeping the saved PATH if available. Run daemon install from your normal shell to refresh it.\n' "$invoking_user" >&2
                preserve_path=1
            fi
            if ! "$runuser_bin" -u "$invoking_user" -- "$env_bin" "PATH=$install_path" \
                "FERNGEIST_GATEWAY_PRESERVE_INSTALL_PATH=$preserve_path" \
                "$GATEWAY_BIN" daemon install --keep-settings --lan >/dev/null 2>&1; then
                printf 'WARNING: ferngeist-gateway daemon could not be (re)installed automatically.\n' >&2
                printf '  After logging in as %s, run: %s daemon install --keep-settings --lan\n' "$invoking_user" "$GATEWAY_BIN" >&2
            fi
        elif command -v su >/dev/null 2>&1; then
            printf 'WARNING: cannot capture %s shell PATH without runuser; keeping the saved PATH if available. Run daemon install from your normal shell to refresh it.\n' "$invoking_user" >&2
            export FERNGEIST_GATEWAY_PRESERVE_INSTALL_PATH=1
            if ! su -s /bin/sh "$invoking_user" -c "\"$GATEWAY_BIN\" daemon install --keep-settings --lan" >/dev/null 2>&1; then
                printf 'WARNING: ferngeist-gateway daemon could not be (re)installed automatically.\n' >&2
                printf '  After logging in as %s, run: %s daemon install --keep-settings --lan\n' "$invoking_user" "$GATEWAY_BIN" >&2
            fi
        else
            printf 'WARNING: ferngeist-gateway daemon not (re)installed; run as %s:\n  %s daemon install --keep-settings --lan\n' "$invoking_user" "$GATEWAY_BIN" >&2
        fi
    elif [ "$(id -u)" = 0 ]; then
        printf 'WARNING: ferngeist-gateway daemon service not registered (root install has no user session).\n' >&2
        printf '  After logging in as your user, run: %s daemon install --keep-settings --lan\n' "$GATEWAY_BIN" >&2
    else
        "$GATEWAY_BIN" daemon install --keep-settings --lan || true
    fi
fi

exit 0
