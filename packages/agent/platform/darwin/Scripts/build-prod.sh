#!/usr/bin/env bash
# Build a production-ready NexusAgent macOS release: .app bundle + .pkg installer.
#
# Outputs:
#   dist/macos/NexusAgent.app              — drag-to-Applications bundle
#   dist/macos/NexusAgent-<VERSION>.pkg    — notarized + stapled installer
#                                            (installs app + LaunchDaemon
#                                            + /Library/Application Support/.../agent.yaml)
#
# Usage:
#   bash packages/agent/platform/darwin/Scripts/build-prod.sh
#
# Optional env vars:
#   VERSION                  — semver override (default: derived from latest prod-* git tag)
#   DEVELOPER_ID_APPLICATION — "Developer ID Application: Acme (TEAMID)" for .app signing
#   DEVELOPER_ID_INSTALLER   — "Developer ID Installer: Acme (TEAMID)" for .pkg signing
#   PROVISION_PROFILE        — path to provisioning profile for the host app (com.nexus-gateway.agent)
#   PROVISION_PROFILE_EXT    — path to provisioning profile for the NE extension
#                              (com.nexus-gateway.agent.extension); required for app-proxy-provider
#   NOTARYTOOL_PROFILE       — keychain profile name created with:
#                                xcrun notarytool store-credentials "<profile>" \
#                                  --apple-id "<email>" --team-id "<TEAMID>" --password "<app-pw>"
#                              When set, submits the signed .pkg for notarization and staples.
#                              Takes precedence over SKIP_NOTARIZE.
#   SKIP_NOTARIZE            — set to "1" to skip notarization + staple entirely. The
#                              codesign + pkgbuild + productsign steps still run, producing
#                              a structurally valid but unstapled .pkg. Intended for CI
#                              machines and developer machines without notarytool credentials.
#                              MUST NOT be used for production or beta distribution.
#                              Ignored when NOTARYTOOL_PROFILE is also set.
#
# Prerequisites: Xcode CLT (swift, codesign, lipo), Go toolchain, pkgbuild/productbuild.

set -euo pipefail

REPO_ROOT="$(git rev-parse --show-toplevel)"
DARWIN_DIR="$REPO_ROOT/packages/agent/platform/darwin"

EXT_ENTITLEMENTS="$DARWIN_DIR/NexusAgent/NexusAgentExtension/NexusAgentExtension.entitlements"

# Derive version from the latest prod-* tag + commit sha (matches ec2-single-node deploy convention).
if [ -z "${VERSION:-}" ]; then
    TAG="$(git describe --tags --match 'prod-*' --abbrev=0 2>/dev/null || echo "0.0.0")"
    SHA="$(git rev-parse --short HEAD)"
    VERSION="${TAG#prod-}+${SHA}"
fi

PKG_BASENAME="NexusAgent-${VERSION}"

echo "==> Building prod release (version=$VERSION)"

# Step 1: Build the .app bundle (Go daemon + Swift UI + System Extension).
VERSION="$VERSION" \
    bash "$DARWIN_DIR/Scripts/build.sh"

APP_DIR="$REPO_ROOT/dist/macos/NexusAgent.app"
SYSEXT_PATH="$APP_DIR/Contents/Library/SystemExtensions/com.nexus-gateway.agent.extension.systemextension"

# Step 2 (optional): Sign everything with Developer ID Application.
# Signing order: innermost bundles first (extension → daemon → app).
if [ -n "${DEVELOPER_ID_APPLICATION:-}" ]; then
    echo "==> Signing .app with Developer ID Application"

    # Sign the NE System Extension first (must be signed before the enclosing app).
    if [ -d "$SYSEXT_PATH" ]; then
        echo "==> Signing System Extension (entitlements: $(basename "$EXT_ENTITLEMENTS"))"
        if [ -n "${PROVISION_PROFILE_EXT:-}" ]; then
            echo "    Embedding extension provisioning profile"
            cp "$PROVISION_PROFILE_EXT" "$SYSEXT_PATH/Contents/embedded.provisionprofile"
        fi
        codesign --force --options runtime --timestamp \
            --sign "$DEVELOPER_ID_APPLICATION" \
            --entitlements "$EXT_ENTITLEMENTS" \
            "$SYSEXT_PATH"
        echo "==> System Extension signed"
    else
        echo "==> System Extension not present — skipping extension signing"
    fi

    # Go daemon binary: minimal entitlements (no NE — avoids SIGKILL without matching profile).
    codesign --force --options runtime --timestamp \
        --sign "$DEVELOPER_ID_APPLICATION" \
        --entitlements "$DARWIN_DIR/NexusAgent/NexusAgentDaemon.entitlements" \
        "$APP_DIR/Contents/MacOS/nexus-agent"

    # Embedded Wails Dashboard (E40 C7): a nested .app bundle at
    # Contents/Resources/Nexus Agent Dashboard.app launched on
    # demand by the menu-bar app's "Open Dashboard" handler. Must be
    # signed with --options runtime + --timestamp (Apple notary rejects
    # unsigned/un-hardened/un-timestamped binaries — issue id seen in
    # CI logs: "The binary is not signed", "secure timestamp",
    # "hardened runtime"). No entitlements file: the dashboard is a
    # plain WebKit Wails app — no NE, no sandbox features. Sign the
    # nested .app directly; codesign --deep walks into the contained
    # nexus-dashboard executable.
    DASHBOARD_APP="$APP_DIR/Contents/Resources/Nexus Agent Dashboard.app"
    if [ -d "$DASHBOARD_APP" ]; then
        echo "==> Signing embedded Wails Dashboard"
        codesign --force --options runtime --timestamp --deep \
            --sign "$DEVELOPER_ID_APPLICATION" \
            "$DASHBOARD_APP"
    fi

    # Host .app bundle: NE install entitlement + provisioning profile.
    if [ -n "${PROVISION_PROFILE:-}" ]; then
        echo "==> Embedding host app provisioning profile"
        cp "$PROVISION_PROFILE" "$APP_DIR/Contents/embedded.provisionprofile"
    fi
    codesign --force --options runtime --timestamp \
        --sign "$DEVELOPER_ID_APPLICATION" \
        --entitlements "$DARWIN_DIR/NexusAgent/NexusAgent.entitlements" \
        "$APP_DIR"

    codesign --verify --deep --strict "$APP_DIR"
    echo "==> .app signature verified"
else
    echo "==> .app signing SKIPPED (DEVELOPER_ID_APPLICATION not set)"
    echo "    To open without Gatekeeper warning: xattr -dr com.apple.quarantine dist/macos/NexusAgent.app"
fi

# Step 3: Package into a .pkg installer.
PKG_BASENAME="$PKG_BASENAME" VERSION="$VERSION" \
    bash "$DARWIN_DIR/Scripts/package.sh"

# Step 4 (optional): Sign the .pkg with Developer ID Installer.
PKG="$REPO_ROOT/dist/macos/${PKG_BASENAME}.pkg"
if [ -n "${DEVELOPER_ID_INSTALLER:-}" ]; then
    echo "==> Signing .pkg with Developer ID Installer"
    SIGNED_PKG="${PKG%.pkg}-signed.pkg"
    productsign --timestamp \
        --sign "$DEVELOPER_ID_INSTALLER" \
        "$PKG" "$SIGNED_PKG"
    mv "$SIGNED_PKG" "$PKG"
    pkgutil --check-signature "$PKG"
else
    echo "==> .pkg signing SKIPPED (DEVELOPER_ID_INSTALLER not set)"
fi

# Step 5 (optional): Notarize + staple the .pkg via Apple's notary service.
# SKIP_NOTARIZE=1 bypasses this step for CI / developer machines that lack
# notarytool credentials. The produced .pkg is structurally valid (codesigned +
# packaged) but unstapled — do NOT distribute unstapled builds to end users.
# NOTARYTOOL_PROFILE takes precedence when both are set.
if [ -n "${NOTARYTOOL_PROFILE:-}" ] && [ "${SKIP_NOTARIZE:-0}" != "1" ]; then
    echo "==> Submitting .pkg to Apple notary service (profile: $NOTARYTOOL_PROFILE)"
    xcrun notarytool submit "$PKG" \
        --keychain-profile "$NOTARYTOOL_PROFILE" \
        --wait
    echo "==> Stapling notarization ticket"
    xcrun stapler staple "$PKG"
    xcrun stapler validate "$PKG"
    NOTARIZED="yes"
    echo "==> Notarization complete"
elif [ "${SKIP_NOTARIZE:-0}" = "1" ]; then
    NOTARIZED="skipped (SKIP_NOTARIZE=1)"
    echo "==> Notarization SKIPPED (SKIP_NOTARIZE=1)"
    echo "    The .pkg is structurally valid but unstapled — for local testing only."
else
    NOTARIZED="skipped (NOTARYTOOL_PROFILE not set)"
    echo "==> Notarization SKIPPED (NOTARYTOOL_PROFILE not set)"
    echo "    Without notarization macOS 13+ will block installation to /Applications."
    echo "    Store credentials first:"
    echo "      xcrun notarytool store-credentials \"nexus-notarytool\" \\"
    echo "        --apple-id \"<email>\" --team-id \"<TEAMID>\" --password \"<app-specific-pw>\""
    echo "    Then rerun with: NOTARYTOOL_PROFILE=nexus-notarytool ..."
fi

# ── Summary report ────────────────────────────────────────────────────────────
echo ""
echo "=== build-agent report ==="
echo "Version:      $VERSION"
echo "Output:       dist/macos/${PKG_BASENAME}.pkg"
echo "Notarized:    $NOTARIZED"
echo "Entitlements: $(basename "$EXT_ENTITLEMENTS")"
echo "========================="
echo ""
echo "Next step: enroll the device before running the agent."
echo "  sudo /Applications/NexusAgent.app/Contents/MacOS/nexus-agent enroll \\"
echo "    --hub-url https://hub.example.com \\"
echo "    --token <enrollment-token>"
