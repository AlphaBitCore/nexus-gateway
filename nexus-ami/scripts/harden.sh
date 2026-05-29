#!/bin/bash
# harden.sh — final cleanup before AMI snapshot. MUST run as the LAST Packer
# provisioner. AWS Marketplace rejects the AMI if any of this is left in.
#
# Architecture: docs/developers/architecture/cross-cutting/deployment/ami-appliance-architecture.md §7

set -euo pipefail

echo "==> [harden] removing SSH authorized_keys (recursive)..."
find / -name 'authorized_keys' -type f -delete 2>/dev/null || true

echo "==> [harden] removing SSH host keys (regenerated on first boot)..."
find /etc/ssh -name 'ssh_host_*' -type f -delete 2>/dev/null || true

echo "==> [harden] enforcing strict sshd config..."
sed -i 's/^#*PermitRootLogin.*/PermitRootLogin no/'               /etc/ssh/sshd_config
sed -i 's/^#*PasswordAuthentication.*/PasswordAuthentication no/' /etc/ssh/sshd_config
sed -i 's/^#*PermitEmptyPasswords.*/PermitEmptyPasswords no/'     /etc/ssh/sshd_config

echo "==> [harden] locking the root password..."
passwd -l root || true

echo "==> [harden] clearing shell history..."
find /root /home -name '.bash_history' -type f -delete 2>/dev/null || true
find /root /home -name '.zsh_history'  -type f -delete 2>/dev/null || true
unset HISTFILE || true
history -c 2>/dev/null || true

echo "==> [harden] truncating logs..."
find /var/log -type f -exec truncate -s 0 {} \; 2>/dev/null || true
journalctl --rotate 2>/dev/null || true
journalctl --vacuum-time=1s 2>/dev/null || true

echo "==> [harden] resetting machine-id (regenerated on first boot)..."
truncate -s 0 /etc/machine-id
# /var/lib/dbus/machine-id is the legacy compatibility symlink for systems
# that ship dbus (Fedora desktop, RHEL with dbus). AL2023 minimal AMI does
# NOT install dbus by default — /var/lib/dbus/ does not exist (verified
# 2026-05-28 build, `ln -sf` failed). Skip the symlink when dbus isn't
# around; systemd alone reads /etc/machine-id directly and regenerates it
# on first boot.
if [ -d /var/lib/dbus ]; then
  rm -f /var/lib/dbus/machine-id
  ln -sf /etc/machine-id /var/lib/dbus/machine-id
fi

echo "==> [harden] cleaning cloud-init state..."
cloud-init clean --logs 2>/dev/null || true

echo "==> [harden] clearing DHCP leases and MAC-bound network rules..."
rm -rf /var/lib/dhclient/* /var/lib/dhcp/* 2>/dev/null || true
rm -f  /etc/udev/rules.d/70-persistent-net.rules

echo "==> [harden] clearing sudo password caches..."
rm -rf /var/db/sudo/* 2>/dev/null || true

echo "==> [harden] clearing package manager caches..."
dnf clean all
rm -rf /var/cache/dnf/* /var/cache/yum/* 2>/dev/null || true

echo "==> [harden] clearing /tmp, /var/tmp, and any leftover Nexus staging..."
rm -rf /tmp/nexus 2>/dev/null || true
find /tmp     -mindepth 1 -delete 2>/dev/null || true
find /var/tmp -mindepth 1 -delete 2>/dev/null || true

echo "==> [harden] clearing per-stateful service data accumulated during install..."
# Each of these is regenerated on first-boot or by the service itself; leaving
# install-time content baked into the AMI is a leak / non-determinism source.
rm -rf /var/lib/pgsql/data/* /var/lib/valkey/* /var/lib/nats/* 2>/dev/null || true
rm -f  /etc/nexus/.initialized 2>/dev/null || true
# Per-instance admin credentials are generated on first boot, never baked into
# the AMI. Wipe both the current path and the legacy /var/log location so no
# build-time test artifact can leak into the published image (AWS Marketplace
# AMI policy: no hardcoded/shared credentials in the AMI).
rm -f  /root/nexus-admin-credentials.txt 2>/dev/null || true
rm -f  /var/log/nexus/admin-credentials.txt 2>/dev/null || true

echo "==> [harden] zeroing free space (shrinks EBS snapshot)..."
dd if=/dev/zero of=/zerofile bs=1M 2>/dev/null || true
rm -f /zerofile
sync

echo "==> [harden] Nexus AMI hardening complete."
