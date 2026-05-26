#!/usr/bin/env bash
# E23-S4: Submit the signed .pkg to Apple notarization and staple the result.
# Skips silently if Apple credentials are not configured (local dev mode).
#
# Usage: bash packages/agent/platform/darwin/Scripts/notarize.sh
# Env:   APPLE_ID
#        APPLE_TEAM_ID
#        APPLE_APP_SPECIFIC_PASSWORD

set -euo pipefail

REPO_ROOT="$(git rev-parse --show-toplevel)"
DIST_DIR="$REPO_ROOT/dist/macos"

if [ -z "${APPLE_ID:-}" ] || [ -z "${APPLE_TEAM_ID:-}" ] || [ -z "${APPLE_APP_SPECIFIC_PASSWORD:-}" ]; then
    echo "==> Notarization SKIPPED (Apple credentials not configured)"
    echo "    Required env vars: APPLE_ID, APPLE_TEAM_ID, APPLE_APP_SPECIFIC_PASSWORD"
    exit 0
fi

if ! command -v xcrun >/dev/null 2>&1; then
    echo "ERROR: xcrun not found (must run on macOS host)." >&2
    exit 1
fi

# Locate the latest .pkg in dist/macos
PKG="$(ls -t "$DIST_DIR"/NexusAgent-*.pkg 2>/dev/null | head -n1 || true)"
if [ -z "$PKG" ] || [ ! -f "$PKG" ]; then
    echo "ERROR: No .pkg found in $DIST_DIR. Run package.sh first." >&2
    exit 1
fi

echo "==> Submitting to Apple notarization service"
echo "    Package: $PKG"

# Submit and wait for the result. Suppress password from logs.
xcrun notarytool submit "$PKG" \
    --apple-id "$APPLE_ID" \
    --team-id "$APPLE_TEAM_ID" \
    --password "$APPLE_APP_SPECIFIC_PASSWORD" \
    --wait

echo "==> Notarization accepted; stapling ticket"
xcrun stapler staple "$PKG"

echo "==> Validating stapled ticket"
xcrun stapler validate "$PKG"

if command -v spctl >/dev/null 2>&1; then
    echo "==> Running Gatekeeper assessment"
    spctl --assess --type install --verbose=2 "$PKG" || {
        echo "WARNING: spctl assessment did not pass; check signing chain" >&2
    }
fi

echo "==> Notarization complete: $PKG"
