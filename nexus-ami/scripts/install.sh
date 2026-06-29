#!/bin/bash
# install.sh — orchestrator. Runs ONCE during Packer build (NOT per-instance).
# Assumes: Amazon Linux 2023 base, artifacts/ staged at /tmp/nexus/ by Packer
# file provisioner.
#
# Architecture: docs/developers/architecture/cross-cutting/deployment/ami-appliance-architecture.md

set -euo pipefail

NEXUS_USER=nexus
NEXUS_GROUP=nexus
INSTALL_DIR=/opt/nexus
BIN_DIR=$INSTALL_DIR/bin
UI_DIR=$INSTALL_DIR/ui
PRISMA_DIR=$INSTALL_DIR/prisma
NODE_DIR=$INSTALL_DIR/node
CONFIG_DIR=/etc/nexus
LOG_DIR=/var/log/nexus
DATA_DIR=/var/lib/nexus
STAGING_DIR=/tmp/nexus
SCRIPT_DIR=/usr/local/sbin
TARBALL=/tmp/nexus-artifacts.tar.gz

# ─── 0. Extract artifacts tarball uploaded by Packer file provisioner ──────
# Packer uploads a single artifacts.tar.gz to /tmp/nexus-artifacts.tar.gz
# (atomic transfer — avoids the recursive-SCP partial-upload bug we hit
# when source was directory-shape on slow links). We extract it under
# /tmp/nexus/ so the rest of this script can reference $STAGING_DIR/bin/
# etc. exactly as if Packer had uploaded the directory directly.

echo "==> [install] extracting $TARBALL -> $STAGING_DIR ..."
if [ ! -f "$TARBALL" ]; then
  echo "ERROR: tarball not found at $TARBALL — Packer file provisioner did not deliver it" >&2
  exit 1
fi
mkdir -p "$STAGING_DIR"
tar -C "$STAGING_DIR" -xzf "$TARBALL"
rm -f "$TARBALL"
echo "==> [install] extracted artifacts ($(du -sh "$STAGING_DIR" | awk '{print $1}'))"

# ─── 1. Update base OS + install base packages ──────────────────────────────

echo "==> [install] dnf update -y (required for Marketplace scan-clean)..."
dnf update -y
# Only firewalld + nginx need installing — openssl, ca-certificates, jq, tar,
# gzip, rsync, procps-ng, curl-minimal all ship preinstalled in AL2023. We
# explicitly do NOT install the full `curl` package because it conflicts with
# the pre-installed curl-minimal (and curl-minimal already provides the curl
# CLI features the install/first-boot scripts need: -f / -s / -S / -L /
# --connect-timeout / etc.).
dnf install -y \
  firewalld \
  nginx

# ─── 2. Create system user ──────────────────────────────────────────────────

echo "==> [install] creating nexus system user..."
if ! id -u "$NEXUS_USER" >/dev/null 2>&1; then
  useradd --system --no-create-home --shell /sbin/nologin --user-group "$NEXUS_USER"
fi

# ─── 3. Create directory structure ──────────────────────────────────────────

echo "==> [install] creating directory structure..."
install -d -o root          -g root          -m 0755 "$INSTALL_DIR" "$BIN_DIR" "$UI_DIR" "$PRISMA_DIR" "$NODE_DIR"
install -d -o root          -g "$NEXUS_GROUP" -m 0750 "$CONFIG_DIR" /etc/compliance-proxy
install -d -o "$NEXUS_USER" -g "$NEXUS_GROUP" -m 0750 "$LOG_DIR" "$DATA_DIR" \
                                                       "$DATA_DIR/agentca" \
                                                       "$DATA_DIR/audit-spool" \
                                                       "$DATA_DIR/alerting"

# ─── 4. Install Nexus Go binaries ───────────────────────────────────────────

echo "==> [install] installing Nexus Go binaries..."
for binary in nexus-hub control-plane ai-gateway compliance-proxy; do
  if [ ! -f "$STAGING_DIR/bin/$binary" ]; then
    echo "ERROR: missing binary $STAGING_DIR/bin/$binary" >&2
    exit 1
  fi
  install -o root -g root -m 0755 "$STAGING_DIR/bin/$binary" "$BIN_DIR/$binary"
done

# ─── 5. Install UI static assets ────────────────────────────────────────────

echo "==> [install] installing UI static assets..."
if [ ! -d "$STAGING_DIR/ui-dist" ]; then
  echo "ERROR: missing UI dist at $STAGING_DIR/ui-dist" >&2
  exit 1
fi
rsync -a --delete "$STAGING_DIR/ui-dist/" "$UI_DIR/"
chown -R root:root "$UI_DIR"

# ─── 6. Install Prisma schema + seed + admin-password helper ────────────────

echo "==> [install] installing Prisma schema + seed..."
if [ ! -d "$STAGING_DIR/prisma" ]; then
  echo "ERROR: missing prisma bundle at $STAGING_DIR/prisma" >&2
  exit 1
fi
rsync -a --delete "$STAGING_DIR/prisma/" "$PRISMA_DIR/"
install -o root -g root -m 0755 "$STAGING_DIR/scripts/set-admin-password.js" "$PRISMA_DIR/set-admin-password.js"
chown -R root:root "$PRISMA_DIR"

# ─── 7. Install service configs ─────────────────────────────────────────────

echo "==> [install] installing prod-shape config files..."
for svc in nexus-hub control-plane ai-gateway compliance-proxy; do
  install -o root -g "$NEXUS_GROUP" -m 0640 \
    "$STAGING_DIR/configs/$svc.config.yaml" "$CONFIG_DIR/$svc.config.yaml"
done
install -o root -g root -m 0644 "$STAGING_DIR/configs/nginx-nexus.conf" /etc/nginx/conf.d/nexus.conf
rm -f /etc/nginx/conf.d/default.conf

# ─── 8. Install systemd units ───────────────────────────────────────────────

echo "==> [install] installing systemd units..."
install -o root -g root -m 0644 "$STAGING_DIR/systemd/"*.service /etc/systemd/system/

# ─── 9. Install first-boot helpers under /usr/local/sbin ────────────────────

echo "==> [install] installing first-boot scripts..."
install -o root -g root -m 0755 "$STAGING_DIR/scripts/first-boot.sh"          "$SCRIPT_DIR/nexus-first-boot"
install -o root -g root -m 0755 "$STAGING_DIR/scripts/first-boot-secrets.sh" "$SCRIPT_DIR/nexus-first-boot-secrets"
install -o root -g root -m 0755 "$STAGING_DIR/scripts/first-boot-ca.sh"      "$SCRIPT_DIR/nexus-first-boot-ca"
install -o root -g root -m 0755 "$STAGING_DIR/scripts/first-boot-db.sh"      "$SCRIPT_DIR/nexus-first-boot-db"
# Every-boot reconcile + manual command (nexus-reconcile-url) — re-syncs all
# IP-derived config (publicURL / issuer / OAuth redirect / TLS cert SAN) to the
# instance's current public IP after a stop/start or Elastic IP change.
install -o root -g root -m 0755 "$STAGING_DIR/scripts/reconcile-public-url.sh" "$SCRIPT_DIR/nexus-reconcile-url"

# ─── 10. Install runtime dependencies (Postgres / Valkey / NATS / Node) ─────

bash "$STAGING_DIR/scripts/install-postgres.sh"
bash "$STAGING_DIR/scripts/install-valkey.sh"
bash "$STAGING_DIR/scripts/install-nats.sh"
bash "$STAGING_DIR/scripts/install-node-prisma.sh"

# ─── 11. Configure firewall ─────────────────────────────────────────────────

echo "==> [install] configuring firewalld..."
systemctl enable firewalld
systemctl start firewalld
firewall-cmd --permanent --add-service=ssh
firewall-cmd --permanent --add-port=443/tcp    # nginx (UI + /api/*)
firewall-cmd --permanent --add-port=80/tcp     # nginx (HTTP redirect to 443)
firewall-cmd --permanent --add-port=3050/tcp   # AI Gateway (SDK direct)
firewall-cmd --permanent --add-port=3128/tcp   # Compliance Proxy CONNECT
firewall-cmd --reload

# ─── 12. Enable services to start at boot ───────────────────────────────────

echo "==> [install] enabling services..."
systemctl daemon-reload
systemctl enable nginx
systemctl enable postgresql
systemctl enable valkey
systemctl enable nats
systemctl enable nexus-first-boot.service
systemctl enable nexus-reconcile-url.service
systemctl enable nexus-hub.service
systemctl enable nexus-control-plane.service
systemctl enable nexus-gateway.service
systemctl enable nexus-proxy.service

# ─── 13. Configure logrotate for Nexus log dir ──────────────────────────────

echo "==> [install] writing logrotate config..."
cat > /etc/logrotate.d/nexus <<'EOF'
/var/log/nexus/*.log {
    daily
    rotate 14
    compress
    delaycompress
    missingok
    notifempty
    create 0640 nexus nexus
    sharedscripts
    postrotate
        systemctl reload-or-restart nexus-hub.service nexus-control-plane.service \
                                    nexus-gateway.service nexus-proxy.service \
                                    > /dev/null 2>&1 || true
    endscript
}
EOF

echo "==> [install] cleaning staging directory..."
rm -rf "$STAGING_DIR"

echo "==> [install] install.sh complete."
