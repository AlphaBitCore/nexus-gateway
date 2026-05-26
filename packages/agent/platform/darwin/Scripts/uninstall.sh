#!/usr/bin/env bash
# Cleanly remove the macOS Nexus Agent.
# Stops the LaunchDaemon, removes the .app, drops all on-disk state, and
# clears the audit-DB key from the System Keychain.
#
# Usage: sudo bash packages/agent/platform/darwin/Scripts/uninstall.sh

set -euo pipefail

if [ "$EUID" -ne 0 ]; then
    echo "This script must be run as root. Re-run with sudo." >&2
    exit 1
fi

BUNDLE_ID="com.nexus-gateway.agent"
DAEMON_PLIST="/Library/LaunchDaemons/${BUNDLE_ID}.plist"
APP_PATH="/Applications/NexusAgent.app"
APP_SUPPORT="/Library/Application Support/${BUNDLE_ID}"
LOGS_DIR="/Library/Logs/${BUNDLE_ID}"

# Paths from older installs (pre platform.Paths refactor). Kept so this
# script also cleans up after legacy deployments.
LEGACY_CONFIG_DIR="/etc/nexus-agent"
LEGACY_STATE_DIR="/var/lib/nexus-agent"
LEGACY_LOG_DIR="/var/log/nexus"

echo "==> Stopping LaunchDaemon (if running)"
if [ -f "$DAEMON_PLIST" ]; then
    launchctl bootout system "$DAEMON_PLIST" 2>/dev/null || true
fi

LAUNCHAGENT_PLIST="/Library/LaunchAgents/com.nexus-gateway.agent.menubar.plist"
echo "==> Stopping LaunchAgent (if running)"
if [ -f "$LAUNCHAGENT_PLIST" ]; then
    CONSOLE_UID=$(stat -f "%u" /dev/console 2>/dev/null || true)
    if [ -n "$CONSOLE_UID" ] && [ "$CONSOLE_UID" != "0" ]; then
        launchctl bootout "gui/$CONSOLE_UID/com.nexus-gateway.agent.menubar" 2>/dev/null || true
    fi
fi

echo "==> Removing LaunchDaemon plist"
rm -f "$DAEMON_PLIST"

echo "==> Removing LaunchAgent plist"
rm -f "$LAUNCHAGENT_PLIST"

echo "==> Removing application"
rm -rf "$APP_PATH"

echo "==> Removing app state (certs, audit DB, config) at $APP_SUPPORT"
rm -rf "$APP_SUPPORT"

echo "==> Removing logs at $LOGS_DIR"
rm -rf "$LOGS_DIR"

echo "==> Removing legacy paths (pre-platform.Paths layout)"
rm -rf "$LEGACY_CONFIG_DIR" "$LEGACY_STATE_DIR" "$LEGACY_LOG_DIR"
rm -f /var/log/nexus-agent.log /var/log/nexus-agent.err

echo "==> Removing audit-DB key from System Keychain"
# The agent persists the SQLCipher key as a generic password keyed on the
# bundle ID. Multiple entries can accumulate across reinstalls; delete in a
# loop until none remain.
for _ in 1 2 3 4 5; do
    security delete-generic-password -s "$BUNDLE_ID" /Library/Keychains/System.keychain 2>/dev/null || break
done

echo "==> Removing device-CA trust anchor from System Keychain"
# install-ca added the device CA as a trusted root with label
# "nexus-agent-device-ca". `security delete-certificate -c <CN>` finds
# by Common Name; LoadOrGenerateCA mints with CN starting with
# "Nexus Agent Device CA". Multiple entries can accumulate when older
# uninstalls left orphans; loop until find-certificate misses.
for _ in 1 2 3 4 5; do
    if security find-certificate -c "Nexus Agent Device CA" /Library/Keychains/System.keychain >/dev/null 2>&1; then
        security delete-certificate -c "Nexus Agent Device CA" /Library/Keychains/System.keychain 2>/dev/null || break
    else
        break
    fi
done

echo "==> Forgetting pkg receipts"
pkgutil --forget "${BUNDLE_ID}.pkg" 2>/dev/null || true
pkgutil --forget "${BUNDLE_ID}.distribution" 2>/dev/null || true

# Per-user IPC socket / user-level state.
HOME_DIR="${SUDO_USER:+/Users/$SUDO_USER}"
if [ -n "${HOME_DIR:-}" ] && [ -d "$HOME_DIR/.nexus" ]; then
    printf "==> Remove user data at %s/.nexus? [y/N] " "$HOME_DIR"
    read -r answer
    if [ "$answer" = "y" ] || [ "$answer" = "Y" ]; then
        rm -rf "$HOME_DIR/.nexus"
        echo "    Removed."
    else
        echo "    Kept."
    fi
fi

echo "==> Flushing macOS DNS cache + restarting mDNSResponder"
# After NE filter teardown, getaddrinfo (browsers, curl, every app that
# uses the system resolver) can hang ~5 s with the stale cache while
# `dig` direct-to-UDP/53 still works. This is a macOS-side cache
# invalidation race that happens reliably whenever a NETransparentProxy
# state changes from configured to not-configured. Two commands fix it
# instantly; without them the user sees "I uninstalled the agent and now
# I can't reach Google" and blames the agent. See memory:
# feedback_macos_mdns_flush_after_ne_state_change.
dscacheutil -flushcache 2>/dev/null || true
killall -HUP mDNSResponder 2>/dev/null || true

echo ""
echo "==> Uninstall complete"
echo "    Note: macOS does NOT allow uninstalling system extensions while SIP"
echo "    is enabled. The extension binary remains at /Library/SystemExtensions/"
echo "    but is harmless — it will be replaced on next install."
