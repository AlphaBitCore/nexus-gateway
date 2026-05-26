#!/usr/bin/env bash
# E40 Phase 3 D2: Build the Linux agent + tray + dashboard artifacts.
#
# Produces in $REPO_ROOT/dist/linux/staging/:
#   nexus-agent         — Go daemon (cmd/agent)
#   nexus-agent-tray    — fyne.io/systray-based tray (cmd/agent-tray)
#   nexus-dashboard     — Wails dashboard (packages/agent/ui/build/bin/...)
#
# The output layout is what packages/agent/platform/linux/installer/
# specs (D3a) consume to assemble deb / rpm packages.
#
# Requires (on the build host — must be Linux, see below):
#   - Go toolchain capable of building for linux/amd64
#   - Wails v2.10+ CLI:   go install github.com/wailsapp/wails/v2/cmd/wails@latest
#   - npm + node (resolves the @nexus-gateway/* workspace)
#   - WebKitGTK + GTK3 dev headers (libgtk-3-dev libwebkit2gtk-4.0-dev or
#     libwebkit2gtk-4.1-dev on newer distros); without them Wails fails.
#   - libayatana-appindicator3-dev (CGO dep for fyne.io/systray on Linux).
#
# This script is intentionally *native Linux only* — cross-compiling
# CGO + WebKit from macOS is impractical, so the CI matrix (Phase 3
# D4) runs this on an ubuntu-latest runner.
#
# Usage:  bash packages/agent/platform/linux/scripts/build.sh
# Env:    VERSION = "1.0.0" (default)

set -euo pipefail

VERSION="${VERSION:-1.0.0}"
REPO_ROOT="$(git rev-parse --show-toplevel)"
DIST_DIR="$REPO_ROOT/dist/linux"
STAGING="$DIST_DIR/staging"

if [ "$(uname -s)" != "Linux" ]; then
    echo "ERROR: this script must run on a Linux host (got $(uname -s))." >&2
    echo "       Wails + CGO + WebKitGTK cannot be cross-compiled from macOS/Windows in practice." >&2
    exit 1
fi

echo "==> E40 Linux build starting (version=$VERSION)"

rm -rf "$DIST_DIR"
mkdir -p "$STAGING"

# ─── 1. nexus-agent (daemon) ──────────────────────────────────────────
echo "==> Building nexus-agent"
cd "$REPO_ROOT/packages/agent"
CGO_ENABLED=1 GOOS=linux GOARCH=amd64 go build \
    -trimpath \
    -ldflags="-s -w -X main.version=$VERSION" \
    -o "$STAGING/nexus-agent" \
    ./cmd/agent

# ─── 2. nexus-agent-tray (Wails-independent system tray) ─────────────
echo "==> Building nexus-agent-tray"
CGO_ENABLED=1 GOOS=linux GOARCH=amd64 go build \
    -trimpath \
    -ldflags="-s -w -X main.version=$VERSION" \
    -o "$STAGING/nexus-agent-tray" \
    ./cmd/agent-tray

# ─── 3. nexus-dashboard (Wails) ───────────────────────────────────────
echo "==> Building nexus-dashboard (wails)"
if ! command -v wails >/dev/null 2>&1; then
    echo "ERROR: wails CLI not on PATH. Install with:" >&2
    echo "       go install github.com/wailsapp/wails/v2/cmd/wails@latest" >&2
    exit 1
fi
cd "$REPO_ROOT/packages/agent/ui"
# -clean removes stale builds; -trimpath + -s -w mirror the Go binary
# flags so the binary is reproducible and stripped.
wails build \
    -platform linux/amd64 \
    -clean \
    -trimpath \
    -ldflags="-s -w -X main.version=$VERSION" \
    -tags webkit2_41

DASH_BIN="$REPO_ROOT/packages/agent/ui/build/bin/nexus-dashboard"
if [ ! -f "$DASH_BIN" ]; then
    echo "ERROR: wails build produced no $DASH_BIN" >&2
    exit 1
fi
cp "$DASH_BIN" "$STAGING/nexus-dashboard"
chmod +x "$STAGING/nexus-dashboard"

# ─── 4. Done ──────────────────────────────────────────────────────────
echo "==> Build complete: $STAGING"
ls -la "$STAGING"
