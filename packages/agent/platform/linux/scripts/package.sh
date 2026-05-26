#!/usr/bin/env bash
# E40 Phase 3 D3a: Package the Linux artifacts (built by build.sh) into
# .deb (Debian/Ubuntu) and .rpm (RHEL/Fedora/SUSE) via nfpm.
#
# Produces in $REPO_ROOT/dist/linux/:
#   nexus-agent_${VERSION}_amd64.deb
#   nexus-agent-${VERSION}.x86_64.rpm
#
# Requires:
#   - nfpm v2.30+   (https://nfpm.goreleaser.com/install/)
#                   Install: go install github.com/goreleaser/nfpm/v2/cmd/nfpm@latest
#   - The binaries from build.sh already present in dist/linux/staging/
#
# Usage:  bash packages/agent/platform/linux/scripts/package.sh
# Env:    VERSION = "1.0.0" (default; passed through to nfpm.yaml's ${VERSION})

set -euo pipefail

VERSION="${VERSION:-1.0.0}"
REPO_ROOT="$(git rev-parse --show-toplevel)"
DIST_DIR="$REPO_ROOT/dist/linux"
STAGING="$DIST_DIR/staging"
NFPM_CFG="$REPO_ROOT/packages/agent/platform/linux/installer/nfpm.yaml"

if [ ! -d "$STAGING" ] || [ ! -x "$STAGING/nexus-agent" ]; then
    echo "ERROR: $STAGING/nexus-agent missing. Run build.sh first." >&2
    exit 1
fi

if ! command -v nfpm >/dev/null 2>&1; then
    echo "ERROR: nfpm CLI not found." >&2
    echo "       Install: go install github.com/goreleaser/nfpm/v2/cmd/nfpm@latest" >&2
    exit 1
fi

cd "$REPO_ROOT"
export VERSION

echo "==> Packaging .deb"
nfpm pkg --packager deb --config "$NFPM_CFG" --target "$DIST_DIR/"

echo "==> Packaging .rpm"
nfpm pkg --packager rpm --config "$NFPM_CFG" --target "$DIST_DIR/"

echo "==> Done."
ls -la "$DIST_DIR"/*.{deb,rpm} 2>/dev/null || true
