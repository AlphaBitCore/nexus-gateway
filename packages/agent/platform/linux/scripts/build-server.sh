#!/usr/bin/env bash
# Build the headless / server Nexus Agent for ALL mainstream Linux distros.
#
# Unlike build.sh (native-Linux GUI build: tray + Wails dashboard), this
# produces a single portable daemon binary plus deb / rpm / archlinux
# packages — no GUI, no WebKit. It is driven from macOS or Linux because the
# heavy lifting runs in containers; Docker is the only host requirement.
#
# Portability strategy: the daemon needs CGO (the audit queue uses
# go-sqlcipher, which has no non-cgo driver). A CGO binary is glibc-version
# floor-locked, so we compile inside manylinux2014 (glibc 2.17). The result
# runs on every target below (glibc is forward-compatible):
#
#   deb       → Ubuntu, Linux Mint, Debian      (apt install ./nexus-agent_*.deb)
#   rpm       → Fedora, CentOS, RHEL, Rocky/Alma (dnf install ./nexus-agent-*.rpm)
#   archlinux → Arch, Manjaro                    (pacman -U nexus-agent-*.pkg.tar.zst)
#
# Verified: RHEL/CentOS 7+ (glibc 2.17+), Ubuntu 18.04+, Fedora, Arch.
#
# Usage:
#   bash packages/agent/platform/linux/scripts/build-server.sh            # VERSION=1.0.0
#   VERSION=1.2.3 bash packages/agent/platform/linux/scripts/build-server.sh
#
# Output: dist/linux/{nexus-agent_<v>_amd64.deb, nexus-agent-<v>-1.x86_64.rpm,
#                     nexus-agent-<v>-1-x86_64.pkg.tar.zst}

set -euo pipefail

VERSION="${VERSION:-1.0.0}"
REPO_ROOT="$(git rev-parse --show-toplevel)"
COMMIT="$(git -C "$REPO_ROOT" rev-parse --short HEAD 2>/dev/null || echo unknown)"
BUILT="$(date -u +%Y-%m-%dT%H:%M:%SZ)"
BUILD_BASE="quay.io/pypa/manylinux2014_x86_64" # glibc 2.17 + gcc 10
NFPM_IMAGE="goreleaser/nfpm:latest"
NFPM_CFG="packages/agent/platform/linux/installer/nfpm.server.yaml"
STAGING="$REPO_ROOT/dist/linux/staging"

command -v docker >/dev/null || { echo "ERROR: docker is required" >&2; exit 1; }

echo "==> Server build  version=$VERSION  commit=$COMMIT  built=$BUILT"
mkdir -p "$STAGING"

# ─── 1. Portable daemon binary (CGO, glibc 2.17) ──────────────────────
# --platform linux/amd64 so this also works from Apple Silicon (emulated).
echo "==> Compiling daemon in $BUILD_BASE (CGO, glibc 2.17)"
docker run --rm --platform linux/amd64 \
  -v "$REPO_ROOT":/src -w /src/packages/agent \
  -e GOWORK=off -e CGO_ENABLED=1 -e GOOS=linux -e GOARCH=amd64 -e GOTOOLCHAIN=local \
  "$BUILD_BASE" bash -c '
    set -e
    if [ ! -x /usr/local/go/bin/go ]; then
      curl -sSL https://go.dev/dl/go1.25.0.linux-amd64.tar.gz -o /tmp/go.tgz
      tar -C /usr/local -xzf /tmp/go.tgz
    fi
    export PATH=/usr/local/go/bin:$PATH
    go build -trimpath \
      -ldflags="-s -w -X main.version='"$VERSION"' -X main.commit='"$COMMIT"' -X main.builtAt='"$BUILT"'" \
      -o /src/dist/linux/staging/nexus-agent ./cmd/agent
    echo "    max glibc required: $(objdump -T /src/dist/linux/staging/nexus-agent | grep -oE "GLIBC_[0-9.]+" | sort -V | uniq | tail -1)"
  '

# ─── 2. deb + rpm + archlinux packages ────────────────────────────────
for fmt in deb rpm archlinux; do
  echo "==> Packaging $fmt"
  docker run --rm -v "$REPO_ROOT":/work -w /work -e VERSION="$VERSION" \
    "$NFPM_IMAGE" package -f "$NFPM_CFG" -p "$fmt" -t dist/linux/
done

echo "==> Done:"
ls -1 "$REPO_ROOT"/dist/linux/nexus-agent*1* 2>/dev/null | grep -E '\.(deb|rpm|pkg\.tar\.zst)$' || true
