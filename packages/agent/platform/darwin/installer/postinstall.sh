#!/usr/bin/env bash
# Postinstall script for the Nexus Agent .pkg installer.
# Invoked AS ROOT by macOS Installer; do NOT run manually.
#
# Under the SMAppService model the pkg does NOT bootstrap launchd. The daemon
# plist ships inside the .app (Contents/Library/LaunchDaemons/) and the menu app
# registers it via SMAppService on first launch; the menu app also registers
# itself as a login item (SMAppService.mainApp). So this script only:
#   state dirs → device CA generation + System Keychain install →
#   migrate off any classic /Library registration → CLI-trust env vars →
#   DNS flush → open the app once so registration + approval kick off.

set -euo pipefail

AGENT_BIN="/Applications/NexusAgent.app/Contents/MacOS/nexus-agent"
APP_BUNDLE="/Applications/NexusAgent.app"
STATE_DIR="/Library/Application Support/com.nexus-gateway.agent"
LOG_DIR="/Library/Logs/com.nexus-gateway.agent"

# State directory (matches platform.DefaultPaths().StateDir on macOS).
# Holds: agent.yaml (operator-stamped, copied by the .pkg), device cert
# + key (issued at enroll time), device-ca.{pem,key} (issued at install
# time by `nexus-agent install-ca` below), audit DB.
mkdir -p "$STATE_DIR"
chown root:wheel "$STATE_DIR"
chmod 755 "$STATE_DIR"
if [ -f "$STATE_DIR/agent.yaml" ]; then
    chown root:wheel "$STATE_DIR/agent.yaml"
    chmod 640 "$STATE_DIR/agent.yaml"
fi

# Audit-queue reset on every install. The agent's local sqlite-backed
# audit queue (audit.db + WAL/SHM) accumulates unsynced events between
# successful Hub uploads. On reinstall — typical when iterating a new
# binary during dev or recovering from a stuck upload pipeline — any
# leftover queue is cruft: the new binary may have a different schema,
# encryption key may have rotated, or the operator just wants a clean
# baseline so the Diagnostics "AUDIT QUEUE" depth starts at 0. Wiping
# here is safe because Hub-side traffic_event is the system of record
# (only never-uploaded events are lost) and install-ca below recreates
# the encrypted DB lazily on first write.
echo "postinstall: clearing audit queue (sqlite + WAL/SHM)"
rm -f "$STATE_DIR/audit.db" \
      "$STATE_DIR/audit.db-shm" \
      "$STATE_DIR/audit.db-wal" \
      "$STATE_DIR/audit.db.status"

# User-writable flags subdirectory — used by the Swift menu-bar app to
# signal the LaunchDaemon without needing sudo. Currently:
#   user-quit  — presence tells the daemon's main() to self-exit on
#                every launchd respawn until the file is removed by
#                the menu app on next launch. See [[agent-quit-flag-design]].
# Mode 0777 (world-writable, no sticky bit) so the menu app can both
# create AND delete files here. Daemon owns the parent state dir, so
# this is the only sub-path the menu app ever writes under /Library.
mkdir -p "$STATE_DIR/flags"
chmod 0777 "$STATE_DIR/flags"

# Log directory.
mkdir -p "$LOG_DIR"
chown root:wheel "$LOG_DIR"
chmod 755 "$LOG_DIR"
touch "$LOG_DIR/agent.log"
chown root:wheel "$LOG_DIR/agent.log"
chmod 644 "$LOG_DIR/agent.log"

# Device CA: one-shot generation + persistence + System Keychain
# trust-anchor install. The agent's MITM TLS bumping path mints per-host
# leaf certs from this CA and presents them to local HTTPS clients —
# those clients only accept the leaves because the issuing CA sits in
# System Keychain as a trusted root. Idempotent on upgrade.
if [ -x "$AGENT_BIN" ]; then
    echo "postinstall: generating + installing device CA"
    if ! "$AGENT_BIN" install-ca; then
        echo "postinstall: WARNING — install-ca failed; intercepted TLS will not be trusted by host clients until you re-run \"sudo $AGENT_BIN install-ca\""
        # NOT fatal: agent can still start in passthrough mode without
        # MITM; the operator can re-run install-ca later.
    fi
else
    echo "postinstall: WARNING — $AGENT_BIN not present; skipping install-ca"
fi

# Console user lookup (used by the migration cleanup, the CLI-trust env
# var block, and the post-install app launch below).
CONSOLE_USER=$(stat -f "%Su" /dev/console 2>/dev/null || true)
CONSOLE_UID=$(stat -f "%u" /dev/console 2>/dev/null || true)
echo "postinstall: console user lookup → CONSOLE_USER='$CONSOLE_USER' CONSOLE_UID='$CONSOLE_UID'"

# Migration: remove any classic registration left by a pre-SMAppService build.
# Earlier builds staged a free-standing LaunchDaemon to /Library/LaunchDaemons
# and a LaunchAgent to /Library/LaunchAgents, bootstrapped by this script. The
# SMAppService model ties registration to the app bundle, so those /Library
# plists must be booted out and removed — otherwise the old daemon keeps
# running alongside the new SMAppService one. Dev-phase: no parallel legacy
# path; this deletes the old mechanism outright.
CLASSIC_DAEMON_PLIST="/Library/LaunchDaemons/com.nexus-gateway.agent.plist"
CLASSIC_AGENT_PLIST="/Library/LaunchAgents/com.nexus-gateway.agent.menubar.plist"
if [ -f "$CLASSIC_DAEMON_PLIST" ]; then
    echo "postinstall: migrating off classic LaunchDaemon — booting out + removing $CLASSIC_DAEMON_PLIST"
    launchctl bootout "system/com.nexus-gateway.agent" 2>/dev/null || true
    rm -f "$CLASSIC_DAEMON_PLIST"
fi
if [ -f "$CLASSIC_AGENT_PLIST" ]; then
    echo "postinstall: migrating off classic LaunchAgent — removing $CLASSIC_AGENT_PLIST"
    if [ -n "$CONSOLE_UID" ] && [ "$CONSOLE_UID" != "0" ]; then
        launchctl bootout "gui/$CONSOLE_UID/com.nexus-gateway.agent.menubar" 2>/dev/null || true
    fi
    rm -f "$CLASSIC_AGENT_PLIST"
fi

# NEXUS_DEVICE_CA_PEM env var for callers that don't read the macOS
# System Keychain (Node.js / Python / Go std/x509 — anything bundling
# its own CA trust store). Most notably Claude Code CLI (Node-based)
# verifies upstream certs against bundled NSS roots and rejects our
# agent-minted leaf with "SSL certificate verification failed" unless
# the device CA is explicitly added via NODE_EXTRA_CA_CERTS.
#
# We expose ONE canonical env var pointing at the on-disk PEM, plus
# the three downstream aliases (NODE_EXTRA_CA_CERTS / REQUESTS_CA_BUNDLE
# / SSL_CERT_FILE) the popular language stacks read. Wrote into the
# console user's ~/.zshenv (zsh default since Catalina) AND
# ~/.bash_profile (covers bash). System-level /etc/zshenv too as
# fallback for Spotlight / Dock-launched apps that don't source rc
# files. All blocks are bracketed with managed tags so reinstalls
# replace them idempotently.
DEVICE_CA_PATH="$STATE_DIR/device-ca.pem"
ENV_SNIPPET_TAG="# nexus-agent device CA (managed — do not edit)"
ENV_SNIPPET_END="# end nexus-agent"

if [ -n "$CONSOLE_USER" ] && [ "$CONSOLE_USER" != "root" ]; then
    USER_HOME=$(eval echo "~$CONSOLE_USER")
    if [ ! -d "$USER_HOME" ]; then
        echo "postinstall: WARNING — console user $CONSOLE_USER home dir $USER_HOME does not exist; skipping rc-file env var write"
    else
        for RC in "$USER_HOME/.zshenv" "$USER_HOME/.bash_profile"; do
            # C4 fix (post-T33 review): root postinstall touching a
            # user-controlled path follows symlinks. A console user
            # could plant ~/.zshenv → /etc/sudoers and gain root via
            # the chown + cat>> sequence below. Defenses: refuse to
            # operate on symlinks (root level), and make the actual
            # file creation run AS the user via su -m -c so the kernel
            # enforces normal user-level path resolution.
            if [ -L "$RC" ]; then
                echo "postinstall: SKIP — $RC is a symlink (refusing root-write to user-controlled path)"
                continue
            fi
            if [ ! -e "$RC" ]; then
                # Create as the target user (no symlink-follow risk
                # because the user can only write inside their own
                # homedir). install(1) atomically creates with
                # explicit mode + ownership; safer than touch+chown.
                /usr/bin/su -m "$CONSOLE_USER" -c "/usr/bin/install -m 644 /dev/null '$RC'" || {
                    echo "postinstall: WARNING — could not create $RC as $CONSOLE_USER; skipping"
                    continue
                }
            fi
            # Re-check after creation: if somehow it's now a symlink,
            # bail. Closes the (already-tiny) TOCTOU window.
            if [ -L "$RC" ]; then
                echo "postinstall: SKIP — $RC became a symlink mid-write; aborting"
                continue
            fi
            # Strip any prior managed block (idempotent on reinstall).
            if grep -q "$ENV_SNIPPET_TAG" "$RC" 2>/dev/null; then
                /usr/bin/su -m "$CONSOLE_USER" -c "/usr/bin/sed -i.bak '/$ENV_SNIPPET_TAG/,/$ENV_SNIPPET_END/d' '$RC' && /bin/rm -f '$RC.bak'"
            fi
            # Append snippet AS the user — never as root — so the
            # kernel honors the user's effective UID and a planted
            # symlink can't redirect to a root-owned file.
            /usr/bin/su -m "$CONSOLE_USER" -c "/bin/cat >> '$RC'" <<EOF

$ENV_SNIPPET_TAG
export NEXUS_DEVICE_CA_PEM="$DEVICE_CA_PATH"
export NODE_EXTRA_CA_CERTS="\${NODE_EXTRA_CA_CERTS:-$DEVICE_CA_PATH}"
export REQUESTS_CA_BUNDLE="\${REQUESTS_CA_BUNDLE:-$DEVICE_CA_PATH}"
export SSL_CERT_FILE="\${SSL_CERT_FILE:-$DEVICE_CA_PATH}"
$ENV_SNIPPET_END
EOF
            echo "postinstall: wrote NEXUS_DEVICE_CA_PEM block to $RC"
        done
    fi
else
    echo "postinstall: no console user logged in (CONSOLE_USER='$CONSOLE_USER'); user-level env vars will only land on next reinstall when a user IS logged in"
fi
SYSTEM_PROFILE="/etc/zshenv"
if [ -f "$SYSTEM_PROFILE" ] && grep -q "$ENV_SNIPPET_TAG" "$SYSTEM_PROFILE" 2>/dev/null; then
    sed -i.bak "/$ENV_SNIPPET_TAG/,/$ENV_SNIPPET_END/d" "$SYSTEM_PROFILE"
    rm -f "$SYSTEM_PROFILE.bak"
fi
cat >> "$SYSTEM_PROFILE" <<EOF

$ENV_SNIPPET_TAG
export NEXUS_DEVICE_CA_PEM="$DEVICE_CA_PATH"
$ENV_SNIPPET_END
EOF

# Flush macOS DNS cache + restart mDNSResponder. Whenever the NE filter
# config goes from absent → present (this install), getaddrinfo can
# stall ~5 s with a stale cache while `dig` direct-to-UDP/53 still
# works — looks exactly like "I just installed and now Google times
# out." Two commands fix it instantly. See memory:
# feedback_macos_mdns_flush_after_ne_state_change.
echo "postinstall: flushing DNS cache + restarting mDNSResponder"
dscacheutil -flushcache 2>/dev/null || true
killall -HUP mDNSResponder 2>/dev/null || true

# Open the app once into the console user's GUI session so SMAppService
# daemon + login-item registration and NE activation kick off immediately
# (replacing the old LaunchAgent's job of opening the app post-install).
# On a managed device the registrations are pre-approved by the
# com.apple.servicemanagement profile, so this lands silently; on an
# unmanaged device it surfaces the one-time approval prompts. Best-effort.
if [ -n "$CONSOLE_UID" ] && [ "$CONSOLE_UID" != "0" ] && [ -d "$APP_BUNDLE" ]; then
    echo "postinstall: opening $APP_BUNDLE in console user session (uid=$CONSOLE_UID) to trigger SMAppService registration"
    launchctl asuser "$CONSOLE_UID" /usr/bin/open -g "$APP_BUNDLE" 2>/dev/null || \
        echo "postinstall: WARNING — could not auto-open the app; the user must open NexusAgent.app once to finish setup"
else
    # No console user (e.g. an auto-update applied at the loginwindow). The
    # SMAppService registration is app-driven, so the daemon — booted out by
    # preinstall on an upgrade — is not re-registered until a user logs in and
    # the login item launches the app. This is a known pre-login-enforcement gap
    # for the headless-update case; documented in the lifecycle design's
    # device-validation punch list. Most agent deployments have a console user.
    echo "postinstall: no console user; NexusAgent.app will register on first manual launch / next login"
fi

exit 0
