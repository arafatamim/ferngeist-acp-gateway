#!/bin/sh
# Runs test-user.sh on a distro GitHub has no VM for: boots systemd in a
# privileged container, lingers a sudo-capable user so it has a user
# manager, and runs the test as that user. Run from the repo root.
# Usage: systemd-container.sh fedora|archlinux packages-dir
set -eu
DISTRO="$1"
PKGS="$2"
case "$DISTRO" in
    fedora) PREP='dnf -y install systemd dbus-broker sudo util-linux curl gnupg2 procps-ng findutils' ;;
    archlinux) PREP='pacman -Syu --noconfirm sudo && pacman-key --init' ;;
    *) echo "unsupported distro: $DISTRO" >&2; exit 1 ;;
esac

docker build -t "sd-$DISTRO" - <<EOF
FROM $DISTRO:latest
RUN $PREP
RUN useradd -m tester && echo 'tester ALL=(ALL) NOPASSWD: ALL' > /etc/sudoers.d/tester
ENV container=docker
CMD ["/usr/lib/systemd/systemd"]
EOF

docker run -d --name sd --privileged --cgroupns=host \
    -v /sys/fs/cgroup:/sys/fs/cgroup:rw --tmpfs /run --tmpfs /tmp \
    -v "$PWD:/src:ro" "sd-$DISTRO"
docker exec sd sh -c 'systemctl is-system-running --wait || true'
docker exec sd loginctl enable-linger tester
UID_T="$(docker exec sd id -u tester)"
for _ in $(seq 30); do
    docker exec sd test -S "/run/user/$UID_T/bus" && break
    sleep 1
done
docker exec -u tester -w /home/tester -e HOME=/home/tester \
    -e XDG_RUNTIME_DIR="/run/user/$UID_T" \
    -e DBUS_SESSION_BUS_ADDRESS="unix:path=/run/user/$UID_T/bus" \
    sd sh "/src/packaging/ci/test-user.sh" "/src/$PKGS"
