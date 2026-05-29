#!/bin/bash
# install-nats.sh — install NATS Server 2.x (JetStream enabled) from the
# official release binary on AL2023.
#
# Architecture: docs/developers/architecture/cross-cutting/deployment/ami-appliance-architecture.md

set -euo pipefail

NATS_VERSION=2.10.20
ARCH=$(uname -m)
case "$ARCH" in
  x86_64)  NATS_ARCH=amd64 ;;
  aarch64) NATS_ARCH=arm64 ;;
  *) echo "ERROR: unsupported arch $ARCH" >&2; exit 1 ;;
esac

TARBALL="nats-server-v$NATS_VERSION-linux-$NATS_ARCH.tar.gz"
URL="https://github.com/nats-io/nats-server/releases/download/v$NATS_VERSION/$TARBALL"

echo "==> [install-nats] downloading $URL..."
cd /tmp
curl -fsSL "$URL" -o "$TARBALL"
tar xzf "$TARBALL"
install -m 0755 "nats-server-v$NATS_VERSION-linux-$NATS_ARCH/nats-server" /usr/local/bin/nats-server
rm -rf "$TARBALL" "nats-server-v$NATS_VERSION-linux-$NATS_ARCH"

echo "==> [install-nats] creating nats user + dirs..."
if ! id -u nats >/dev/null 2>&1; then
  useradd --system --no-create-home --shell /sbin/nologin --user-group nats
fi
install -d -o nats -g nats -m 0750 /var/lib/nats /var/log/nats
install -d -o root -g root -m 0755 /etc/nats

cat > /etc/nats/nats-server.conf <<'EOF'
# Nexus appliance — NATS Server with JetStream (localhost-only).
listen: "127.0.0.1:4222"
http: "127.0.0.1:8222"

server_name: "nexus-appliance"

jetstream {
  store_dir: "/var/lib/nats"
  max_memory_store: 1GB
  max_file_store: 32GB
}

log_file: "/var/log/nats/nats-server.log"
logtime: true
debug: false
trace: false

# No external clustering for the appliance form factor; the Hub is the only
# JetStream client and runs on the same host.
EOF
chmod 0644 /etc/nats/nats-server.conf

echo "==> [install-nats] complete (NATS $NATS_VERSION)."
