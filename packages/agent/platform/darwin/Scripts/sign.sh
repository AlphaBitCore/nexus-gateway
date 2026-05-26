#!/usr/bin/env bash
# E23-S2: Code-sign the .app bundle with a Developer ID Application certificate.
# Skips silently if DEVELOPER_ID_APPLICATION env var is not set (local dev mode).
#
# Usage: bash packages/agent/platform/darwin/Scripts/sign.sh
# Env:   DEVELOPER_ID_APPLICATION="Developer ID Application: Acme Corp (TEAM_ID)"

set -euo pipefail

REPO_ROOT="$(git rev-parse --show-toplevel)"
APP_DIR="$REPO_ROOT/dist/macos/NexusAgent.app"
ENTITLEMENTS="$REPO_ROOT/packages/agent/platform/darwin/NexusAgent/NexusAgent.entitlements"

if [ -z "${DEVELOPER_ID_APPLICATION:-}" ]; then
    echo "==> Code signing SKIPPED (DEVELOPER_ID_APPLICATION env var not set)"
    echo "    The .app will not be signed and will not pass Gatekeeper."
    exit 0
fi

if [ ! -d "$APP_DIR" ]; then
    echo "ERROR: $APP_DIR not found. Run build.sh first." >&2
    exit 1
fi

if ! command -v codesign >/dev/null 2>&1; then
    echo "ERROR: codesign command not found (must run on macOS host)." >&2
    exit 1
fi

echo "==> Signing inner Go binary"
codesign --force --options runtime --timestamp \
    --sign "$DEVELOPER_ID_APPLICATION" \
    "$APP_DIR/Contents/MacOS/nexus-agent"

echo "==> Signing Swift app with entitlements"
codesign --force --options runtime --timestamp \
    --entitlements "$ENTITLEMENTS" \
    --sign "$DEVELOPER_ID_APPLICATION" \
    "$APP_DIR"

echo "==> Verifying signature"
codesign --verify --deep --strict --verbose=2 "$APP_DIR"

echo "==> Code signing complete"
