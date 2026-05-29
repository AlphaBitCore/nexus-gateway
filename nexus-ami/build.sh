#!/usr/bin/env bash
# build.sh — staging wrapper. Compiles all Nexus binaries + UI dist + bundles
# the Prisma schema, then invokes `packer build`.
#
# Architecture: docs/developers/architecture/cross-cutting/deployment/ami-appliance-architecture.md
#
# Usage:
#   cd nexus-ami
#   ./build.sh                    # full pipeline (binaries + UI + packer)
#   ./build.sh --skip-packer      # stage artifacts only; don't run packer (for CI dry-run)
#   ./build.sh --stage-only       # alias for --skip-packer
#
# Prerequisites:
#   - Go 1.25+ (`make build-all` driver)
#   - Node 20+ (`make control-plane-ui-build`)
#   - Packer 1.10+ (https://www.packer.io/) unless --skip-packer
#   - AWS credentials in environment (AWS_ACCESS_KEY_ID + AWS_SECRET_ACCESS_KEY
#     or AWS_PROFILE) unless --skip-packer

set -euo pipefail

SKIP_PACKER=false
for arg in "$@"; do
  case "$arg" in
    --skip-packer|--stage-only) SKIP_PACKER=true ;;
    -h|--help)
      sed -n '2,18p' "$0"
      exit 0
      ;;
    *) echo "ERROR: unknown flag $arg" >&2; exit 1 ;;
  esac
done

SCRIPT_DIR="$(cd "$(dirname "$0")" && pwd)"
REPO_ROOT="$(cd "$SCRIPT_DIR/.." && pwd)"
ARTIFACTS_DIR="$SCRIPT_DIR/artifacts"

echo "==> [build] cleaning previous staging dirs..."
rm -rf "$ARTIFACTS_DIR/bin" "$ARTIFACTS_DIR/ui-dist" "$ARTIFACTS_DIR/prisma" "$ARTIFACTS_DIR/scripts"
rm -f  "$SCRIPT_DIR/artifacts.tar.gz"
mkdir -p "$ARTIFACTS_DIR/bin" "$ARTIFACTS_DIR/ui-dist" "$ARTIFACTS_DIR/prisma"

# ─── 1. Build Go binaries ──────────────────────────────────────────────────

echo "==> [build] compiling Nexus Go binaries (make build-all)..."
cd "$REPO_ROOT"
GOOS=linux GOARCH=amd64 CGO_ENABLED=0 make \
  nexus-hub-build control-plane-build ai-gateway-build compliance-proxy-build

for svc in nexus-hub control-plane ai-gateway compliance-proxy; do
  src="$REPO_ROOT/dist/bin/$svc/$svc"
  [ -x "$src" ] || { echo "ERROR: missing $src" >&2; exit 1; }
  cp "$src" "$ARTIFACTS_DIR/bin/$svc"
done

# ─── 2. Build Control Plane UI Vite dist ───────────────────────────────────

echo "==> [build] building Control Plane UI (Vite)..."
cd "$REPO_ROOT"
make control-plane-ui-build

ui_dist="$REPO_ROOT/packages/control-plane-ui/dist"
[ -d "$ui_dist" ] || { echo "ERROR: missing UI dist at $ui_dist" >&2; exit 1; }
cp -r "$ui_dist"/. "$ARTIFACTS_DIR/ui-dist/"

# ─── 3. Bundle Prisma schema + seed ────────────────────────────────────────

echo "==> [build] bundling Prisma schema + seed..."
cd "$REPO_ROOT/tools/db-migrate"
cp schema.prisma                  "$ARTIFACTS_DIR/prisma/"
cp package.json package-lock.json "$ARTIFACTS_DIR/prisma/"
cp -r seed                        "$ARTIFACTS_DIR/prisma/seed"
cp prisma.config.ts               "$ARTIFACTS_DIR/prisma/"
cp -r migrations                  "$ARTIFACTS_DIR/prisma/migrations" 2>/dev/null || true

# ─── 3b. Bundle scripts/ into artifacts/scripts/ ───────────────────────────
# Packer's file provisioner needs the destination dir to exist before scp can
# upload into it. Bundling scripts/ as a subdir of artifacts/ means one
# `file` provisioner uploads everything in one shot (see nexus.pkr.hcl).

echo "==> [build] bundling scripts/ into artifacts/scripts/..."
cp -r "$SCRIPT_DIR/scripts" "$ARTIFACTS_DIR/scripts"

# ─── 4. Show what we staged ────────────────────────────────────────────────

echo "==> [build] artifact tree:"
( cd "$ARTIFACTS_DIR" && find . -maxdepth 3 -type d -print ) | sed 's|^|     |'

# ─── 4b. Compress artifacts/ → artifacts.tar.gz ────────────────────────────
# Packer's file provisioner uses recursive SCP. For our 234 MB payload over
# slow links (e.g., China → us-east-1), SCP silently drops individual files
# on transient connection blips — leading to "missing binary" errors at
# install.sh time with no upload-side error message. Tarballing makes the
# transfer atomic (one file → succeed or fail as a whole) AND faster
# (gzipped Go binaries compress to ~40-50% of their uncompressed size).

TARBALL="$SCRIPT_DIR/artifacts.tar.gz"
echo "==> [build] compressing artifacts/ → artifacts.tar.gz ..."
rm -f "$TARBALL"
tar -C "$ARTIFACTS_DIR" -czf "$TARBALL" .
echo "==> [build] tarball: $(du -h "$TARBALL" | awk '{print $1}') (vs $(du -sh "$ARTIFACTS_DIR" | awk '{print $1}') uncompressed)"

# ─── 5. packer build ───────────────────────────────────────────────────────

if $SKIP_PACKER; then
  echo "==> [build] --skip-packer: stopping here. Run 'cd $SCRIPT_DIR && packer init . && packer build nexus.pkr.hcl' yourself."
  exit 0
fi

if ! command -v packer >/dev/null 2>&1; then
  echo "ERROR: packer is not installed (https://www.packer.io/downloads). Pass --skip-packer to stop after staging." >&2
  exit 1
fi

cd "$SCRIPT_DIR"
echo "==> [build] packer init ..."
packer init .
echo "==> [build] packer build ..."
packer build nexus.pkr.hcl
