#!/usr/bin/env bash
# Postinstall script for the Nexus Agent .pkg installer.
# Invoked AS ROOT by macOS Installer; do NOT run manually.
#
# Order matters: state dirs → device CA generation + System Keychain
# install → LaunchDaemon bootstrap. The daemon's runtime path then
# loads the persisted CA from disk instead of regenerating per
# restart (which previously polluted Keychain with duplicate entries
# every time launchd respawned the agent).

set -euo pipefail

DAEMON_PLIST="/Library/LaunchDaemons/com.nexus-gateway.agent.plist"
LAUNCHAGENT_PLIST="/Library/LaunchAgents/com.nexus-gateway.agent.menubar.plist"
AGENT_BIN="/Applications/NexusAgent.app/Contents/MacOS/nexus-agent"
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
# here is safe because:
#   - Hub-side traffic_event is the system of record; only events that
#     never made it off the box are lost
#   - install-ca below recreates the encrypted DB lazily on first
#     write, so no schema migration concerns
#   - The wipe runs BEFORE launchctl kickstart so the daemon comes up
#     against an empty file rather than racing with a half-deleted one
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
# trust-anchor install. The agent's MITM TLS bumping path (when wired
# up by the NETransparentProxyProvider in a future epic) mints
# per-host leaf certs from this CA and presents them to local HTTPS
# clients — those clients only accept the leaves because the issuing
# CA sits in System Keychain as a trusted root.
#
# Idempotent on upgrade: install-ca's LoadOrGenerateCA reuses the
# existing device-ca.{pem,key} when present; `security
# add-trusted-cert` is a no-op when the cert is already a trusted
# anchor (matched by SHA-256 fingerprint).
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

if [ ! -f "$DAEMON_PLIST" ]; then
    echo "postinstall: $DAEMON_PLIST not found; skipping launchd bootstrap"
    exit 0
fi

# launchd requires LaunchDaemon plists to be owned by root:wheel with 0644 perms.
chown root:wheel "$DAEMON_PLIST"
chmod 644 "$DAEMON_PLIST"

# On a fresh install the daemon is not yet loaded — bootstrap it.
# On upgrade the daemon is already loaded (and running) with the OLD
# binary mapped in memory; macOS Installer atomically replaces the
# .app bundle on disk but launchd does not notice. `kickstart -k`
# kills the running process so KeepAlive=true respawns it from the
# freshly-installed binary. Without this, a .pkg upgrade silently
# fails to take effect until the next reboot.
if launchctl print "system/com.nexus-gateway.agent" >/dev/null 2>&1; then
    echo "postinstall: daemon already loaded; forcing kickstart -k to pick up new binary"
    launchctl kickstart -k "system/com.nexus-gateway.agent"
else
    echo "postinstall: bootstrapping LaunchDaemon (fresh install)"
    launchctl bootstrap system "$DAEMON_PLIST"
fi

# LaunchAgent: per-user agent that auto-opens NexusAgent.app at every
# Aqua login. Required so the menu-bar app gets a chance to call
# NETransparentProxyManager.startVPNTunnel after a reboot — the Go
# daemon comes up via launchd at boot, but only a per-user GUI process
# can wire macOS NetworkExtension framework to our provider. Without
# this the user has to manually open the .app after every reboot
# before traffic interception starts.
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

# C3 fix (post-T33 review): CONSOLE_USER + CONSOLE_UID were defined
# only in the LaunchAgent block farther down. The user-RC env-var
# block referenced them via ${CONSOLE_USER:-} and silently no-op'd on
# every install — so Claude Code CLI never saw NEXUS_DEVICE_CA_PEM
# and kept failing with "SSL certificate verification failed".
# Hoist the lookup up here and ECHO the result so the postinstall
# log shows whether we found a console user (no more guessing why
# .zshenv didn't get written).
CONSOLE_USER=$(stat -f "%Su" /dev/console 2>/dev/null || true)
CONSOLE_UID=$(stat -f "%u" /dev/console 2>/dev/null || true)
echo "postinstall: console user lookup → CONSOLE_USER='$CONSOLE_USER' CONSOLE_UID='$CONSOLE_UID'"

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

if [ -f "$LAUNCHAGENT_PLIST" ]; then
    chown root:wheel "$LAUNCHAGENT_PLIST"
    chmod 644 "$LAUNCHAGENT_PLIST"

    # Bootstrap into the currently-logged-in console user's domain
    # right now so we don't make them log out / back in just to get
    # the auto-start wired. CONSOLE_USER + CONSOLE_UID were defined
    # at the env-var block above (post-T33-fix C3); we re-use them
    # here instead of re-running stat.
    if [ -n "$CONSOLE_UID" ] && [ "$CONSOLE_UID" != "0" ] && [ -n "$CONSOLE_USER" ]; then
        echo "postinstall: bootstrapping LaunchAgent for current console user $CONSOLE_USER (uid=$CONSOLE_UID)"
        launchctl bootout "gui/$CONSOLE_UID/com.nexus-gateway.agent.menubar" 2>/dev/null || true
        if ! launchctl bootstrap "gui/$CONSOLE_UID" "$LAUNCHAGENT_PLIST"; then
            echo "postinstall: WARNING — LaunchAgent bootstrap failed; user must log out + back in (or open NexusAgent.app once manually) to enable auto-start"
        fi
    else
        echo "postinstall: no console user logged in; LaunchAgent will activate on next login"
    fi
fi

# Flush macOS DNS cache + restart mDNSResponder. Whenever the NE filter
# config goes from absent → present (this install), getaddrinfo can
# stall ~5 s with a stale cache while `dig` direct-to-UDP/53 still
# works — looks exactly like "I just installed and now Google times
# out." Two commands fix it instantly. See memory:
# feedback_macos_mdns_flush_after_ne_state_change.
echo "postinstall: flushing DNS cache + restarting mDNSResponder"
dscacheutil -flushcache 2>/dev/null || true
killall -HUP mDNSResponder 2>/dev/null || true

exit 0
