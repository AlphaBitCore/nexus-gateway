#!/bin/sh
# preremove: stop the system daemon before binaries are removed.
# Idempotent — `|| true` because the unit may already be inactive.
set -eu

if command -v systemctl >/dev/null 2>&1; then
    systemctl stop nexus-agent.service || true
    systemctl disable nexus-agent.service || true
fi
