#!/usr/bin/env bash
# E23-S3: Package the .app bundle into a .pkg installer with LaunchDaemon.
# Optionally signs the .pkg if DEVELOPER_ID_INSTALLER env var is set.
#
# Usage: bash packages/agent/platform/darwin/Scripts/package.sh
# Env:   VERSION="1.0.0"
#        PKG_BASENAME="NexusAgent-1.0.0"    (optional; overrides the default filename)
#        DEVELOPER_ID_INSTALLER="Developer ID Installer: Acme Corp (TEAM_ID)"  (optional)

set -euo pipefail

REPO_ROOT="$(git rev-parse --show-toplevel)"
DIST_DIR="$REPO_ROOT/dist/macos"
APP_DIR="$DIST_DIR/NexusAgent.app"
DARWIN_DIR="$REPO_ROOT/packages/agent/platform/darwin"

VERSION="${VERSION:-1.0.0}"
PKG_BASENAME="${PKG_BASENAME:-NexusAgent-${VERSION}}"

if [ ! -d "$APP_DIR" ]; then
    echo "ERROR: $APP_DIR not found. Run build.sh first." >&2
    exit 1
fi

if ! command -v pkgbuild >/dev/null 2>&1; then
    echo "ERROR: pkgbuild not found (must run on macOS host)." >&2
    exit 1
fi

echo "==> Preparing pkg staging directory"
PKG_ROOT="$DIST_DIR/pkg-root"
PKG_SCRIPTS="$DIST_DIR/pkg-scripts"
APP_SUPPORT_DIR="Library/Application Support/com.nexus-gateway.agent"
LOGS_DIR="Library/Logs/com.nexus-gateway.agent"
rm -rf "$PKG_ROOT" "$PKG_SCRIPTS"
mkdir -p \
    "$PKG_ROOT/Applications" \
    "$PKG_ROOT/$APP_SUPPORT_DIR" \
    "$PKG_ROOT/$LOGS_DIR" \
    "$PKG_SCRIPTS"

# Stage the .app
cp -R "$APP_DIR" "$PKG_ROOT/Applications/"

# NOTE: the LaunchDaemon plist is no longer staged to /Library/LaunchDaemons
# and the LaunchAgent plist is gone entirely. Registration is now bundle-tied:
# the daemon plist ships INSIDE the .app at Contents/Library/LaunchDaemons/
# (build.sh) and the menu app registers both the daemon (SMAppService.daemon)
# and itself as a login item (SMAppService.mainApp) on first launch. Deleting
# the app deregisters the daemon — no /Library residue. The pkg postinstall no
# longer bootstraps launchd; it only stages config + CA + dirs (and, on an
# upgrade from a classic build, removes the stale /Library plists).

# Stage the production config alongside future state files inside
# /Library/Application Support/<bundle-id>/. Apple's File System Programming
# Guide places all third-party app state under this directory; co-locating
# the config + cert/key + audit DB keeps everything under one umbrella that
# admin tools (and our uninstall.sh) can wipe atomically.
PROD_CONFIG="$REPO_ROOT/packages/agent/agent.prod.yaml"
if [ ! -f "$PROD_CONFIG" ]; then
    echo "ERROR: $PROD_CONFIG not found." >&2
    echo "  The repo ships only the template at agent.prod.yaml.example." >&2
    echo "  Copy it and fill in your deployment values before building:" >&2
    echo "    cp $PROD_CONFIG.example $PROD_CONFIG" >&2
    echo "  Then fill in the real Hub URLs (hubURL/hubHTTPURL) and cpURL for your deployment." >&2
    exit 1
fi
cp "$PROD_CONFIG" "$PKG_ROOT/$APP_SUPPORT_DIR/agent.yaml"

# Stage installer scripts
cp "$DARWIN_DIR/installer/preinstall.sh" "$PKG_SCRIPTS/preinstall"
cp "$DARWIN_DIR/installer/postinstall.sh" "$PKG_SCRIPTS/postinstall"
chmod +x "$PKG_SCRIPTS/preinstall" "$PKG_SCRIPTS/postinstall"

COMPONENT_PKG="$DIST_DIR/component.pkg"
COMPONENT_PLIST="$DIST_DIR/component.plist"
DIST_PKG="$DIST_DIR/${PKG_BASENAME}.pkg"

# Generate a component plist that pins the .app to /Applications and
# disables relocation. Without this, macOS Installer silently skips the
# .app when no prior installation is found at an alternate path.
echo "==> Generating component plist"
pkgbuild --root "$PKG_ROOT" \
    --analyze \
    "$COMPONENT_PLIST"

# Patch: set BundleIsRelocatable=false so the installer never tries to
# find an existing copy and always writes to the declared install-location.
/usr/libexec/PlistBuddy \
    -c "Set :0:BundleIsRelocatable false" \
    "$COMPONENT_PLIST"

echo "==> Building component package"
pkgbuild --root "$PKG_ROOT" \
    --component-plist "$COMPONENT_PLIST" \
    --scripts "$PKG_SCRIPTS" \
    --identifier com.nexus-gateway.agent.pkg \
    --version "$VERSION" \
    --install-location / \
    "$COMPONENT_PKG"

echo "==> Building distribution package"
# Use Distribution.xml so the macOS Installer dialog shows a proper
# "Nexus Agent" title + welcome / conclusion text. Without --distribution
# productbuild generates a default distribution that derives the title
# from the embedded payload's CFBundleName, which is empty for our
# daemon, hence the "" Installer bug.
DIST_XML="$DARWIN_DIR/installer/Distribution.xml"
DIST_RESOURCES="$DARWIN_DIR/installer/Resources"
DIST_BUILD_DIR="$DIST_DIR/dist-build"
mkdir -p "$DIST_BUILD_DIR"
# productbuild --distribution expects component pkgs in the current
# directory (or referenced via relative paths). Stage the component pkg
# next to Distribution.xml so productbuild's <pkg-ref> resolves.
cp "$COMPONENT_PKG" "$DIST_BUILD_DIR/component.pkg"
cp "$DIST_XML" "$DIST_BUILD_DIR/Distribution.xml"
( cd "$DIST_BUILD_DIR" && productbuild \
    --distribution Distribution.xml \
    --resources "$DIST_RESOURCES" \
    --package-path . \
    --version "$VERSION" \
    "$DIST_PKG" )
rm -rf "$DIST_BUILD_DIR"

# Optional: sign the .pkg with Developer ID Installer
if [ -n "${DEVELOPER_ID_INSTALLER:-}" ]; then
    echo "==> Signing .pkg with Developer ID Installer"
    SIGNED_PKG="$DIST_DIR/${PKG_BASENAME}-signed.pkg"
    productsign --sign "$DEVELOPER_ID_INSTALLER" "$DIST_PKG" "$SIGNED_PKG"
    mv "$SIGNED_PKG" "$DIST_PKG"
    echo "==> Verifying .pkg signature"
    pkgutil --check-signature "$DIST_PKG"
else
    echo "==> .pkg signing SKIPPED (DEVELOPER_ID_INSTALLER not set)"
fi

# Cleanup intermediate
rm -f "$COMPONENT_PKG" "$COMPONENT_PLIST"
rm -rf "$PKG_ROOT" "$PKG_SCRIPTS"

echo "==> Package complete: $DIST_PKG"
ls -la "$DIST_PKG"
