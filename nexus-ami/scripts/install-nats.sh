#!/bin/bash
# install-nats.sh — install NATS Server 2.x (JetStream enabled) from the
# official release binary on AL2023.
#
# Architecture: docs/developers/architecture/cross-cutting/deployment/ami-appliance-architecture.md

set -euo pipefail

# shellcheck source=lib-verify.sh
source "$(dirname "$0")/lib-verify.sh"

NATS_VERSION=2.10.20
ARCH=$(uname -m)
# per-arch sha256 from the NATS-published SHA256SUMS for this release
# (the script previously ignored those sums). verify_sha256 fails the build on
# any mismatch. Re-record both on every version bump:
#   curl -fsSL https://github.com/nats-io/nats-server/releases/download/v<V>/SHA256SUMS
case "$ARCH" in
  x86_64)  NATS_ARCH=amd64; NATS_SHA256=979c6e633fb03987771b8f7baf99041b574638486ead35acdb868f6a7187a164 ;;
  aarch64) NATS_ARCH=arm64; NATS_SHA256=f12b65d736dcd6c58d1cd39a2378a1c72041b3b2ba158ab9665cac137c4c7be4 ;;
  *) echo "ERROR: unsupported arch $ARCH" >&2; exit 1 ;;
esac

TARBALL="nats-server-v$NATS_VERSION-linux-$NATS_ARCH.tar.gz"
URL="https://github.com/nats-io/nats-server/releases/download/v$NATS_VERSION/$TARBALL"

echo "==> [install-nats] downloading $URL..."
cd /tmp
curl -fsSL "$URL" -o "$TARBALL"
verify_sha256 "$NATS_SHA256" "$TARBALL"
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

# Per-message size ceiling. The default 1MB is too small for the audit
# pipeline: a traffic_event carrying a large prompt/response body can
# exceed 1MB, and a payload over max_payload makes JetStream Publish fail
# — under concurrency that surfaces as dropped audit/traffic rows. 16MB
# matches the Hub's ConnectRPC 16MB frame cap so both transports agree on
# the largest event they will carry.
max_payload: 16MB

# Connection ceiling. The co-located Nexus services are the only clients,
# but each maintains its own producer/consumer connections and reconnects
# under load — set explicitly above the implicit default so a reconnect
# burst is never refused.
max_connections: 4096

jetstream {
  store_dir: "/var/lib/nats"
  max_memory_store: 1GB
  # 8GB, not 32GB: the appliance root volume is 30GB (nexus.pkr.hcl) shared
  # with Postgres + Valkey AOF + spill + logs. A 32GB JetStream ceiling on a
  # 30GB disk is unenforceable — sustained streaming could fill the disk and
  # take down every co-located service before JetStream hit its own cap.
  max_file_store: 8GB
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
