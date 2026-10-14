#!/bin/sh
# Runs test-user.sh on a distro GitHub has no VM for: boots systemd in a
# privileged container, lingers a sudo-capable user so it has a user
# manager, and runs the test as that user. Run from the repo root.
# Usage: systemd-container.sh fedora|archlinux packages-dir
set -eu
DISTRO="$1"
PKGS="$2"
case "$DISTRO" in
    fedora) PREP='dnf -y install systemd dbus sudo util-linux curl gnupg2 procps-ng findutils' ;;
    archlinux) PREP='pacman -Syu --noconfirm sudo && pacman-key --init' ;;
    *) echo "unsupported distro: $DISTRO" >&2; exit 1 ;;
esac

docker build -t "sd-$DISTRO" - <<EOF
FROM $DISTRO:latest
RUN $PREP
# chmod: Fedora's /etc/shadow is mode 000, and the runner host's AppArmor
# unix-chkpwd profile (applied by path) denies the capability to read it
# anyway, so PAM couldn't start user@.service or sudo. Arch ships 600.
RUN useradd -m tester && echo 'tester ALL=(ALL) NOPASSWD: ALL' > /etc/sudoers.d/tester && chmod 600 /etc/shadow
ENV container=docker
CMD ["/usr/lib/systemd/systemd"]
EOF

docker run -d --name sd --privileged --cgroupns=host \
    -v /sys/fs/cgroup:/sys/fs/cgroup:rw --tmpfs /run --tmpfs /tmp \
    -v "$PWD:/src:ro" "sd-$DISTRO"
for _ in $(seq 60); do
    case "$(docker exec sd systemctl is-system-running 2>/dev/null)" in
        running|degraded) break ;;
    esac
    sleep 1
done
docker exec sd systemctl is-system-running || { docker logs sd; docker exec sd systemctl --failed || true; }
docker exec sd loginctl enable-linger tester
UID_T="$(docker exec sd id -u tester)"
for _ in $(seq 30); do
    docker exec sd test -S "/run/user/$UID_T/bus" && break
    sleep 1
done
docker exec sd test -S "/run/user/$UID_T/bus" || {
    docker exec sd systemctl status "user@$UID_T" --no-pager || true
    docker exec sd journalctl -b --no-pager -n 50 || true
    echo "tester's user bus never came up" >&2
    exit 1
}
docker exec -u tester -w /home/tester -e HOME=/home/tester \
    -e XDG_RUNTIME_DIR="/run/user/$UID_T" \
    -e DBUS_SESSION_BUS_ADDRESS="unix:path=/run/user/$UID_T/bus" \
    sd sh "/src/packaging/ci/test-user.sh" "/src/$PKGS"
