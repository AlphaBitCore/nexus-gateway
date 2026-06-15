#!/bin/sh
# postinstall: create the nexus-agent system user, fix permissions,
# generate + install the device CA into the OS trust store, then
# enable + (re-)start the daemon. Idempotent — safe to re-run on
# upgrade. Runs as root (apt/yum invokes maintainer scripts with
# euid=0).
set -eu

# ─── 1. nexus-agent system user + group ────────────────────────
# The daemon runs as `nexus-agent` (not root) per E42 FR-L5/L6,
# with CAP_NET_ADMIN granted via the systemd unit's
# AmbientCapabilities. Skip both creates on upgrade (the entries
# already exist).
if ! getent group nexus-agent >/dev/null; then
    groupadd --system nexus-agent
fi
if ! id -u nexus-agent >/dev/null 2>&1; then
    useradd --system \
        --gid nexus-agent \
        --no-create-home \
        --home /var/lib/nexus-agent \
        --shell /usr/sbin/nologin \
        --comment "Nexus Agent daemon" \
        nexus-agent
fi

# ─── 2. State directories ───────────────────────────────────────
# Permissions per FR-L5:
#   /etc/nexus-agent/        0750  nexus-agent:nexus-agent
#   /var/lib/nexus-agent/    0750  nexus-agent:nexus-agent
#   /var/log/nexus-agent/    0750  nexus-agent:nexus-agent
install -d -m 0750 -o nexus-agent -g nexus-agent /etc/nexus-agent
install -d -m 0750 -o nexus-agent -g nexus-agent /var/lib/nexus-agent
install -d -m 0750 -o nexus-agent -g nexus-agent /var/log/nexus-agent
# agent.yaml is owned by nexus-agent but world-unreadable since it
# can contain bearer tokens after enrollment.
if [ -f /etc/nexus-agent/agent.yaml ]; then
    chown nexus-agent:nexus-agent /etc/nexus-agent/agent.yaml
    chmod 0640 /etc/nexus-agent/agent.yaml
fi

# ─── 3. Device CA + OS trust store ──────────────────────────────
# Generates /var/lib/nexus-agent/device-ca.{pem,key} (0644 / 0600)
# and installs the cert into the OS trust store, then runs the distro's
# refresh tool so host HTTPS clients trust intercepted TLS. install-ca
# auto-detects the trust-store layout — Debian/Ubuntu
# (update-ca-certificates), RHEL/Fedora/Amazon Linux (update-ca-trust),
# Arch, Alpine — so no per-distro path is passed here. Idempotent:
# re-running on upgrade loads the existing CA rather than regenerating.
# Failure here is fatal because the agent's MITM relay can't function
# without OS trust.
/usr/lib/nexus-agent/nexus-agent install-ca \
    --device-ca-out=/var/lib/nexus-agent/device-ca

# Re-chown after install-ca writes (it runs as root and chmod
# already sets the right modes, but the owner needs to be
# nexus-agent so the runtime daemon can read them).
chown nexus-agent:nexus-agent \
    /var/lib/nexus-agent/device-ca.pem \
    /var/lib/nexus-agent/device-ca.key

# ─── 4. systemd unit registration ───────────────────────────────
if command -v systemctl >/dev/null 2>&1; then
    systemctl daemon-reload || true

    # Enable to start at boot. We do NOT start on a fresh install
    # — the operator must first enroll the agent against their
    # Hub (via Dashboard or `nexus-agent enroll-sso`). Upgrades
    # of an already-active service get a restart so the new
    # binary loads.
    systemctl enable nexus-agent.service || true
    if systemctl is-active --quiet nexus-agent.service; then
        systemctl restart nexus-agent.service || true
    fi

    # User-level tray unit lives under /usr/lib/systemd/user/ —
    # each logged-in user enables it via
    #   systemctl --user enable --now nexus-agent-tray.service
    # We can't blanket-enable per-user units from a system-level
    # postinstall. /etc/xdg/autostart/ covers DE-level autostart.
fi

# NEXUS_DEVICE_CA_PEM env var for callers that don't read the system
# CA store (Node.js / Python / Go std/x509 — anything bundling its own
# trust store). Most notably Claude Code CLI (Node-based) verifies
# upstream certs against bundled NSS roots and rejects our agent-
# minted leaf with "SSL certificate verification failed" unless the
# device CA is added via NODE_EXTRA_CA_CERTS. Linux convention: drop
# a profile.d snippet that login shells (bash/dash/ksh/zsh in login
# mode) source via /etc/profile. Idempotent — overwrites each install.
DEVICE_CA_PATH="/var/lib/nexus-agent/device-ca.pem"
PROFILE_D_FILE="/etc/profile.d/nexus-agent-ca.sh"
cat > "$PROFILE_D_FILE" <<EOF
# nexus-agent device CA (managed by postinstall — reinstall overwrites)
# Path of the device CA cert the agent uses to mint per-host leaves.
# Exposed under one canonical name + the three downstream aliases the
# popular language stacks read.
export NEXUS_DEVICE_CA_PEM="$DEVICE_CA_PATH"
export NODE_EXTRA_CA_CERTS="\${NODE_EXTRA_CA_CERTS:-$DEVICE_CA_PATH}"
export REQUESTS_CA_BUNDLE="\${REQUESTS_CA_BUNDLE:-$DEVICE_CA_PATH}"
export SSL_CERT_FILE="\${SSL_CERT_FILE:-$DEVICE_CA_PATH}"
EOF
chmod 644 "$PROFILE_D_FILE"

exit 0
