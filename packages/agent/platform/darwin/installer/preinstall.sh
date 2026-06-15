#!/usr/bin/env bash
# Preinstall script for the Nexus Agent .pkg installer.
# Stops any running agent before the new bundle is written so the in-place
# upgrade does not race with an active process. Works for both the classic
# (/Library plist) and SMAppService (bundle-embedded) registrations — the
# bootout is by label, which targets the running job regardless of how it was
# registered. postinstall opens the app, which re-registers via SMAppService
# (RunAtLoad starts the new daemon binary); there is no launchd bootstrap here.
#
# This script is invoked automatically by macOS installer; do NOT run manually.

set -euo pipefail

BUNDLE_ID="com.nexus-gateway.agent"
LAUNCHAGENT_LABEL="com.nexus-gateway.agent.menubar"

# Stop the running daemon by label (classic or SMAppService) so the bundle can
# be replaced cleanly and the new binary isn't shadowed by the old mapped one.
echo "preinstall: stopping existing Nexus Agent daemon (if running)"
launchctl bootout "system/${BUNDLE_ID}" 2>/dev/null || true

# Stop any classic per-user LaunchAgent (pre-SMAppService builds). The
# SMAppService menu-bar app uses SMAppService.mainApp instead; this is
# legacy cleanup so the upgrade leaves no residue.
CONSOLE_UID=$(stat -f "%u" /dev/console 2>/dev/null || true)
if [ -n "$CONSOLE_UID" ] && [ "$CONSOLE_UID" != "0" ]; then
    echo "preinstall: stopping classic LaunchAgent for uid $CONSOLE_UID (if present)"
    launchctl bootout "gui/$CONSOLE_UID/${LAUNCHAGENT_LABEL}" 2>/dev/null || true
fi

# Always exit 0 — preinstall failures should not block the install.
exit 0
