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

# App-side teardown FIRST (while the .app still exists). The menu app is the only
# thing that can deactivate the NE system extension (SIP blocks the shell) and
# unregister the SMAppService daemon + login item. Launch it headless in the
# console user's GUI session and wait (`open -W`) for it to finish + quit; the
# file removal below then cleans up the residue. Best-effort — if it can't run
# (no console user, app already gone) we fall through to label bootout + rm.
CONSOLE_UID=$(stat -f "%u" /dev/console 2>/dev/null || true)
if [ -d "$APP_PATH" ] && [ -n "$CONSOLE_UID" ] && [ "$CONSOLE_UID" != "0" ]; then
    echo "==> Running app-side uninstall (NE deactivate + SMAppService unregister)"
    # -n is REQUIRED: the menu app is normally already running (login item), and
    # `open` without -n just activates the existing instance and DROPS --args, so
    # the --uninstall branch would never run. -n forces a fresh instance that
    # enters runUninstall(); -W waits for that instance to terminate. The fresh
    # instance operates on shared system state and quits itself, so the transient
    # second instance is harmless.
    launchctl asuser "$CONSOLE_UID" /usr/bin/open -n -W -a "$APP_PATH" --args --uninstall 2>/dev/null || \
        echo "    (app-side teardown could not run; continuing with file removal)"
fi

echo "==> Stopping LaunchDaemon (if running)"
# Boot out by label rather than by plist path: this stops the daemon whether it
# was registered the classic way (free-standing /Library plist) or via
# SMAppService (bundle-embedded plist) — both land on the same system-domain
# label. The label form unloads it regardless of any plist file's presence;
# harmless no-op when nothing is loaded. Under SMAppService, removing the .app
# (below) is what deregisters the daemon; this bootout just stops the running
# process first so the removal is clean.
launchctl bootout "system/${BUNDLE_ID}" 2>/dev/null || true

# Classic LaunchAgent cleanup. Pre-SMAppService builds staged a per-user
# LaunchAgent; the SMAppService menu-bar app uses SMAppService.mainApp instead.
# Boot out + remove any classic agent so an upgrade-then-uninstall leaves no
# residue. Harmless no-op on a fresh SMAppService install (file absent).
LAUNCHAGENT_PLIST="/Library/LaunchAgents/com.nexus-gateway.agent.menubar.plist"
echo "==> Stopping classic LaunchAgent (if present)"
if [ -f "$LAUNCHAGENT_PLIST" ]; then
    CONSOLE_UID=$(stat -f "%u" /dev/console 2>/dev/null || true)
    if [ -n "$CONSOLE_UID" ] && [ "$CONSOLE_UID" != "0" ]; then
        launchctl bootout "gui/$CONSOLE_UID/com.nexus-gateway.agent.menubar" 2>/dev/null || true
    fi
fi

echo "==> Removing classic LaunchDaemon plist (if present)"
# Only the classic free-standing plist lives at this /Library path; the
# SMAppService daemon plist ships inside the .app and is removed with it below.
rm -f "$DAEMON_PLIST"

echo "==> Removing classic LaunchAgent plist (if present)"
rm -f "$LAUNCHAGENT_PLIST"

echo "==> Removing application"
rm -rf "$APP_PATH"

echo "==> Removing app state (certs, audit DB, config) at $APP_SUPPORT"
rm -rf "$APP_SUPPORT"

echo "==> Removing logs at $LOGS_DIR"
rm -rf "$LOGS_DIR"

echo "==> Removing managed device-CA env-var blocks from shell rc files"
# postinstall.sh exports NEXUS_DEVICE_CA_PEM / NODE_EXTRA_CA_CERTS /
# REQUESTS_CA_BUNDLE / SSL_CERT_FILE into the console user's ~/.zshenv +
# ~/.bash_profile and into /etc/zshenv, all pointing at
# "$APP_SUPPORT/device-ca.pem" — which we just deleted above. If the block
# is left behind, every Node/OpenSSL process that starts logs
#   Ignoring extra certs from <path>, load failed:
#   error:10000002:SSL routines:OPENSSL_internal:system library
# (ENOENT on the missing file) for the rest of the machine's life. Strip
# the managed blocks symmetrically so uninstall fully reverses install.
ENV_SNIPPET_TAG="# nexus-agent device CA (managed — do not edit)"
ENV_SNIPPET_END="# end nexus-agent"
RC_CONSOLE_USER=$(stat -f "%Su" /dev/console 2>/dev/null || true)
if [ -n "$RC_CONSOLE_USER" ] && [ "$RC_CONSOLE_USER" != "root" ]; then
    RC_USER_HOME=$(eval echo "~$RC_CONSOLE_USER")
    for RC in "$RC_USER_HOME/.zshenv" "$RC_USER_HOME/.bash_profile"; do
        # Refuse to follow a symlink — same root-touching-user-path defense
        # postinstall.sh applies when writing these files.
        if [ -L "$RC" ]; then
            echo "    SKIP $RC (symlink)"
            continue
        fi
        if [ -f "$RC" ] && grep -q "$ENV_SNIPPET_TAG" "$RC" 2>/dev/null; then
            # Edit AS the user so the kernel enforces user-level path
            # resolution (mirrors the su -m write in postinstall.sh).
            if /usr/bin/su -m "$RC_CONSOLE_USER" -c "/usr/bin/sed -i.bak '/$ENV_SNIPPET_TAG/,/$ENV_SNIPPET_END/d' '$RC' && /bin/rm -f '$RC.bak'"; then
                echo "    Stripped device-CA block from $RC"
            else
                echo "    WARNING — could not strip block from $RC"
            fi
        fi
    done
fi
SYSTEM_PROFILE="/etc/zshenv"
if [ -f "$SYSTEM_PROFILE" ] && grep -q "$ENV_SNIPPET_TAG" "$SYSTEM_PROFILE" 2>/dev/null; then
    sed -i.bak "/$ENV_SNIPPET_TAG/,/$ENV_SNIPPET_END/d" "$SYSTEM_PROFILE"
    rm -f "$SYSTEM_PROFILE.bak"
    echo "    Stripped device-CA block from $SYSTEM_PROFILE"
fi

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
echo "    The app-side step above requested deactivation of the NE system"
echo "    extension (the only SIP-legal path) and unregistered the SMAppService"
echo "    daemon + login item. macOS may defer the extension's actual removal to"
echo "    the next reboot — if 'systemextensionsctl list' still shows a Nexus"
echo "    entry, reboot to finish removing it."
