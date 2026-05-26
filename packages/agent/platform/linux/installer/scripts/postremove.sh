#!/bin/sh
# postremove: reload systemd after units are gone. State directories
# (/var/lib/nexus-agent, /var/log/nexus-agent, /etc/nexus-agent) are
# deliberately preserved so an `apt remove`/`yum erase` followed by a
# reinstall keeps the audit DB + enrollment cert. Use `apt purge` or
# `rpm -e --noscripts` + manual rm to wipe.
set -eu

if command -v systemctl >/dev/null 2>&1; then
    systemctl daemon-reload || true
fi
