#!/usr/bin/env bash
# E23-S3: Preinstall script for the Nexus Agent .pkg installer.
# Stops any running LaunchDaemon before the new files are written so the
# in-place upgrade does not race with an active process.
#
# This script is invoked automatically by macOS installer; do NOT run manually.

set -euo pipefail

DAEMON_PLIST="/Library/LaunchDaemons/com.nexus-gateway.agent.plist"
LAUNCHAGENT_PLIST="/Library/LaunchAgents/com.nexus-gateway.agent.menubar.plist"

if [ -f "$DAEMON_PLIST" ]; then
    echo "preinstall: Stopping existing Nexus Agent LaunchDaemon"
    launchctl bootout system "$DAEMON_PLIST" 2>/dev/null || true
fi

# Boot the LaunchAgent out of the current console user's domain too,
# so the upgrade can replace the plist file in place. postinstall
# re-bootstraps it after the new files land.
if [ -f "$LAUNCHAGENT_PLIST" ]; then
    CONSOLE_UID=$(stat -f "%u" /dev/console 2>/dev/null || true)
    if [ -n "$CONSOLE_UID" ] && [ "$CONSOLE_UID" != "0" ]; then
        echo "preinstall: Stopping existing LaunchAgent for uid $CONSOLE_UID"
        launchctl bootout "gui/$CONSOLE_UID/com.nexus-gateway.agent.menubar" 2>/dev/null || true
    fi
fi

# Always exit 0 — preinstall failures should not block the install.
exit 0
