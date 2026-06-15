#!/usr/bin/env bash
# E23-S2: Build the macOS .app bundle from Swift Package + Go binary.
# Produces: dist/macos/NexusAgent.app
#
# Requires (on the build host):
#   - Xcode Command Line Tools (swift, lipo, codesign)
#   - Go toolchain capable of cross-compiling to darwin
#
# Usage: bash packages/agent/platform/darwin/Scripts/build.sh

set -euo pipefail

REPO_ROOT="$(git rev-parse --show-toplevel)"
DIST_DIR="$REPO_ROOT/dist/macos"
BUILD_DIR="$DIST_DIR/build"
APP_DIR="$DIST_DIR/NexusAgent.app"
DARWIN_DIR="$REPO_ROOT/packages/agent/platform/darwin"

VERSION="${VERSION:-1.0.0}"
COMMIT="${COMMIT:-$(git -C "$REPO_ROOT" rev-parse --short HEAD 2>/dev/null || echo unknown)}"
BUILT_AT="${BUILT_AT:-$(date -u +%Y-%m-%dT%H:%M:%SZ)}"
# CFBundleVersion stamped into every copied Info.plist below. macOS uses
# this for OSSystemExtensionRequest replacement decisions: when the new
# bundle's CFBundleVersion is NOT strictly greater than the existing
# install's, macOS short-circuits the install with "already installed"
# WITHOUT calling actionForReplacingExtension and WITHOUT re-loading the
# extension binary into /Library/SystemExtensions/<UUID>/. The user
# installs a new .pkg, the .app on disk is fresh, but the running
# provider process is still the old binary — symptom is "Network filter
# not connected" with PID unchanged across installs. Using epoch
# seconds guarantees monotonic increase per build. See incident
# 2026-05-15.
BUILD_NUMBER="${BUILD_NUMBER:-$(date +%s)}"
LDFLAGS_VERSION="-X main.version=$VERSION -X main.commit=$COMMIT -X main.builtAt=$BUILT_AT"
# LDFLAGS_EXTRA is a generic injection seam for callers that need to stamp
# additional ldflag values (e.g. one-off feature flags during a release).
# Empty in the standard prod path.
LDFLAGS_EXTRA="${LDFLAGS_EXTRA:-}"

echo "==> E23 macOS build starting (version=$VERSION commit=$COMMIT built=$BUILT_AT build_number=$BUILD_NUMBER)"

# Stamp dynamic CFBundleVersion (and a stable CFBundleShortVersionString)
# into a copied Info.plist so macOS treats this build as strictly newer
# than any previous install. Use PlistBuddy with Add-as-fallback to
# tolerate plists that may not have the keys yet. Caller passes the
# absolute path of the destination plist (already copied into the .app).
stamp_plist_version() {
    local plist="$1"
    /usr/libexec/PlistBuddy -c "Set :CFBundleVersion $BUILD_NUMBER" "$plist" 2>/dev/null \
        || /usr/libexec/PlistBuddy -c "Add :CFBundleVersion string $BUILD_NUMBER" "$plist"
    /usr/libexec/PlistBuddy -c "Set :CFBundleShortVersionString 1.0.0" "$plist" 2>/dev/null \
        || /usr/libexec/PlistBuddy -c "Add :CFBundleShortVersionString string 1.0.0" "$plist"
    echo "    stamped CFBundleVersion=$BUILD_NUMBER into $plist"
}

# 1. Clean and prepare workspace
rm -rf "$DIST_DIR"
mkdir -p "$BUILD_DIR" "$APP_DIR/Contents/MacOS" "$APP_DIR/Contents/Resources"

# 2. Build Go binary — universal (arm64+amd64) when the host can cross-compile CGO,
#    otherwise fall back to the current host architecture.
echo "==> Building Go binary"
cd "$REPO_ROOT/packages/agent"

HOST_ARCH="$(uname -m)"  # arm64 or x86_64

can_cross_cgo() {
    # Probe amd64 cross-compile; suppress output; treat any error as no-CGO-cross.
    CGO_ENABLED=1 GOOS=darwin GOARCH=amd64 \
        go build -o /dev/null ./cmd/agent >/dev/null 2>&1
}

if [ "$HOST_ARCH" = "arm64" ] && can_cross_cgo; then
    echo "==> Cross-compiling universal binary (arm64 + amd64)"
    CGO_ENABLED=1 GOOS=darwin GOARCH=arm64 go build \
        -trimpath \
        -ldflags="-s -w $LDFLAGS_VERSION $LDFLAGS_EXTRA" \
        -o "$BUILD_DIR/nexus-agent-arm64" \
        ./cmd/agent
    CGO_ENABLED=1 GOOS=darwin GOARCH=amd64 go build \
        -trimpath \
        -ldflags="-s -w $LDFLAGS_VERSION $LDFLAGS_EXTRA" \
        -o "$BUILD_DIR/nexus-agent-amd64" \
        ./cmd/agent
    lipo -create -output "$BUILD_DIR/nexus-agent" \
        "$BUILD_DIR/nexus-agent-arm64" \
        "$BUILD_DIR/nexus-agent-amd64"
    rm -f "$BUILD_DIR/nexus-agent-arm64" "$BUILD_DIR/nexus-agent-amd64"
else
    # CGO cross-compilation unavailable (common on arm64-only CI or dev machines);
    # build a native binary for the current host architecture.
    if [ "$HOST_ARCH" = "arm64" ]; then GOARCH=arm64; else GOARCH=amd64; fi
    echo "==> Building native binary (GOARCH=$GOARCH)"
    CGO_ENABLED=1 GOOS=darwin GOARCH="$GOARCH" go build \
        -trimpath \
        -ldflags="-s -w $LDFLAGS_VERSION $LDFLAGS_EXTRA" \
        -o "$BUILD_DIR/nexus-agent" \
        ./cmd/agent
fi

# 4. Build Swift targets: NexusAgentUI (menu bar app) + NexusAgentExtension (NE extension).
# Package.swift lives at the darwin/ level so both targets can share NexusAgent/Shared.
SWIFT_BUILT=0
SYSEXT_BUILT=0
if command -v swift >/dev/null 2>&1; then
    echo "==> Building Swift UI executable"
    cd "$DARWIN_DIR"
    if swift build -c release --arch arm64 --arch x86_64 2>/dev/null; then
        BIN_PATH="$(swift build -c release --arch arm64 --arch x86_64 --show-bin-path 2>/dev/null || true)"

        SWIFT_BIN="$BIN_PATH/NexusAgentUI"
        if [ -f "$SWIFT_BIN" ]; then
            cp "$SWIFT_BIN" "$APP_DIR/Contents/MacOS/NexusAgent"
            SWIFT_BUILT=1
        fi

        # Bundle the System Extension inside the app.
        EXT_BIN="$BIN_PATH/NexusAgentExtension"
        if [ -f "$EXT_BIN" ]; then
            echo "==> Bundling System Extension"
            SYSEXT_ID="com.nexus-gateway.agent.extension"
            SYSEXT_BUNDLE="$APP_DIR/Contents/Library/SystemExtensions/$SYSEXT_ID.systemextension"
            mkdir -p "$SYSEXT_BUNDLE/Contents/MacOS"
            cp "$EXT_BIN" "$SYSEXT_BUNDLE/Contents/MacOS/$SYSEXT_ID"
            cp "$DARWIN_DIR/NexusAgent/NexusAgentExtension/Info.plist" "$SYSEXT_BUNDLE/Contents/Info.plist"
            stamp_plist_version "$SYSEXT_BUNDLE/Contents/Info.plist"
            chmod +x "$SYSEXT_BUNDLE/Contents/MacOS/$SYSEXT_ID"
            SYSEXT_BUILT=1
            echo "==> System Extension bundled: $SYSEXT_BUNDLE"
        else
            echo "==> WARNING: NexusAgentExtension binary not found at $EXT_BIN"
        fi
    fi
fi

if [ "$SWIFT_BUILT" -eq 0 ]; then
    echo "==> WARNING: Swift toolchain unavailable or build failed"
    echo "    Creating placeholder NexusAgent executable"
    printf '#!/bin/sh\nexec "$(dirname "$0")/nexus-agent" "$@"\n' > "$APP_DIR/Contents/MacOS/NexusAgent"
fi

if [ "$SYSEXT_BUILT" -eq 0 ]; then
    echo "==> WARNING: System Extension not built — transparent proxy will not function"
fi

# 5. Copy Go binary into the bundle
cp "$BUILD_DIR/nexus-agent" "$APP_DIR/Contents/MacOS/nexus-agent"

# 5b. Embed the SMAppService daemon plist at Contents/Library/LaunchDaemons/.
# The menu app registers this with SMAppService.daemon(plistName:) on first
# launch — the registration is bundle-tied, so the daemon is no longer staged
# to /Library/LaunchDaemons by the pkg (package.sh) and deleting the app
# deregisters it. The plist is data, covered by the app's code signature.
mkdir -p "$APP_DIR/Contents/Library/LaunchDaemons"
cp "$DARWIN_DIR/installer/LaunchDaemon.plist" \
    "$APP_DIR/Contents/Library/LaunchDaemons/com.nexus-gateway.agent.plist"

# 6. Copy Info.plist + AppIcon.icns into the .app bundle.
# Info.plist declares CFBundleIconFile=AppIcon, so the icon file MUST
# be present at Contents/Resources/AppIcon.icns. Without it the
# Finder + Dock fall back to the generic gear-on-grid icon, which is
# what users saw on every fresh install before the brand asset was
# wired into the build.
cp "$DARWIN_DIR/NexusAgent/Info.plist" "$APP_DIR/Contents/Info.plist"
stamp_plist_version "$APP_DIR/Contents/Info.plist"
if [ -f "$DARWIN_DIR/NexusAgent/Resources/AppIcon.icns" ]; then
    cp "$DARWIN_DIR/NexusAgent/Resources/AppIcon.icns" "$APP_DIR/Contents/Resources/AppIcon.icns"
else
    echo "==> WARNING: AppIcon.icns missing under $DARWIN_DIR/NexusAgent/Resources/ — Finder will show the generic icon"
fi

# 7. Copy SPM-generated resource bundle into Contents/Resources/.
# Required for `Bundle.module` (used by String(localized:bundle:.module)) — without
# this bundle the app crashes on first localized string lookup with an assertion
# failure in resource_bundle_accessor.swift.
if [ "$SWIFT_BUILT" -eq 1 ]; then
    SPM_BUNDLE="$BIN_PATH/NexusAgentUI_NexusAgentUI.bundle"
    if [ -d "$SPM_BUNDLE" ]; then
        cp -R "$SPM_BUNDLE" "$APP_DIR/Contents/Resources/"
    else
        echo "==> WARNING: SPM resource bundle not found at $SPM_BUNDLE — UI will crash on localized strings"
    fi
fi
# Also keep the raw xcstrings file alongside (harmless backup for inspection).
if [ -f "$DARWIN_DIR/NexusAgentUI/Sources/Resources/Localizable.xcstrings" ]; then
    cp "$DARWIN_DIR/NexusAgentUI/Sources/Resources/Localizable.xcstrings" "$APP_DIR/Contents/Resources/"
fi

# 8. Build the Wails dashboard and embed it as a nested .app inside the
#    main bundle at Contents/Resources/Nexus Agent Dashboard.app — the
#    exact path AgentViewModel.launchDashboardApp() searches.
DASH_BUILT=0
if command -v wails >/dev/null 2>&1; then
    echo "==> Building Wails dashboard (darwin universal)"
    cd "$REPO_ROOT/packages/agent/ui"
    if wails build -platform darwin/universal -clean -trimpath \
        -ldflags="-s -w -X main.version=$VERSION"; then
        SRC_APP="$REPO_ROOT/packages/agent/ui/build/bin/Nexus Agent Dashboard.app"
        if [ -d "$SRC_APP" ]; then
            DEST="$APP_DIR/Contents/Resources/Nexus Agent Dashboard.app"
            mkdir -p "$APP_DIR/Contents/Resources"
            cp -R "$SRC_APP" "$DEST"
            DASH_BUILT=1
            echo "==> Dashboard embedded: $DEST"
        else
            echo "==> WARNING: wails build produced no $SRC_APP"
        fi
    else
        echo "==> WARNING: wails build failed; menu bar 'Open Dashboard' will be broken"
    fi
    cd "$REPO_ROOT"
else
    echo "==> NOTE: wails CLI unavailable — will fall back to the committed prebuilt Dashboard.app if present"
    echo "    To rebuild against current Wails sources, install: go install github.com/wailsapp/wails/v2/cmd/wails@latest"
fi

# Fallback: when wails is missing or the build above failed, reuse the
# Dashboard bundle committed at packages/agent/ui/build/bin/. Treating
# the committed copy as the canonical fallback means a clean checkout
# (or a CI machine without the wails CLI) still produces a .pkg that
# embeds the dashboard — the previous behaviour silently shipped a
# .app where the menu bar "Open Dashboard" / "Sign in" handlers popped
# a "Dashboard not installed" alert on every click.
if [ "$DASH_BUILT" -eq 0 ]; then
    PREBUILT_APP="$REPO_ROOT/packages/agent/ui/build/bin/Nexus Agent Dashboard.app"
    if [ -d "$PREBUILT_APP" ]; then
        DEST="$APP_DIR/Contents/Resources/Nexus Agent Dashboard.app"
        mkdir -p "$APP_DIR/Contents/Resources"
        rm -rf "$DEST"
        cp -R "$PREBUILT_APP" "$DEST"
        DASH_BUILT=1
        echo "==> Dashboard embedded from committed prebuilt: $DEST"
        echo "    NOTE: this is the checked-in copy; install wails CLI to rebuild against current sources."
    else
        echo "==> WARNING: Dashboard NOT bundled (no wails CLI and no prebuilt at $PREBUILT_APP) — menu bar 'Open Dashboard' will be broken."
    fi
fi

# 9. Set executable bits
chmod +x "$APP_DIR/Contents/MacOS/NexusAgent"
chmod +x "$APP_DIR/Contents/MacOS/nexus-agent"

echo "==> Build complete: $APP_DIR"
ls -la "$APP_DIR/Contents/MacOS/"
if [ "$DASH_BUILT" -eq 1 ]; then
    echo "Embedded dashboard:"
    ls -la "$APP_DIR/Contents/Resources/Nexus Agent Dashboard.app/Contents/MacOS/" || true
fi
