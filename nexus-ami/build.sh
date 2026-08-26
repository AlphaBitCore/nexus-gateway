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
rm -f  "$SCRIPT_DIR/artifacts.tar.gz" "$ARTIFACTS_DIR/nexus-src.tar.gz" "$ARTIFACTS_DIR/NEXUS_VERSION"
mkdir -p "$ARTIFACTS_DIR/ui-dist" "$ARTIFACTS_DIR/prisma"

# ─── 1. Stage Go source (compiled ON the build instance) ───────────────────
# The Go services link libhs (Vectorscan) via cgo, so they are built natively on
# the linux/amd64 Packer instance by scripts/build-binaries.sh — NOT
# cross-compiled here. A CGO_ENABLED=0 cross-compile silently selects the pure-Go
# RE2 fallback matcher; the gateway's content-scanning engine is the Vectorscan
# cgo path, and libhs is C++ that can't cross-compile from a macOS arm64 control
# host. We ship the committed source tree (git archive HEAD) so the build is
# reproducible from a tagged commit — commit your changes before building.

echo "==> [build] staging Go source (git archive HEAD)..."
cd "$REPO_ROOT"
if ! git diff --quiet HEAD 2>/dev/null; then
  echo "WARN: working tree has uncommitted changes — the AMI builds from HEAD, so they will NOT be included." >&2
fi
git archive --format=tar.gz -o "$ARTIFACTS_DIR/nexus-src.tar.gz" HEAD
git rev-parse HEAD > "$ARTIFACTS_DIR/NEXUS_VERSION"
echo "==> [build] source: $(du -h "$ARTIFACTS_DIR/nexus-src.tar.gz" | awk '{print $1}') @ $(cat "$ARTIFACTS_DIR/NEXUS_VERSION")"

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
# 1.0 uses a multi-file schema/ folder applied via `prisma db push` (no
# migration history) plus schema-extras.sql for the PostgreSQL-native objects
# db push can't express. prisma.config.ts points Prisma at the schema/ folder.
cp -r schema                      "$ARTIFACTS_DIR/prisma/schema"
cp schema-extras.sql              "$ARTIFACTS_DIR/prisma/"
cp prisma.config.ts               "$ARTIFACTS_DIR/prisma/"
cp package.json                   "$ARTIFACTS_DIR/prisma/"
# The lockfile is hoisted to the npm workspace root; copy it only if a local one
# exists. install-node-prisma.sh runs `npm install` (not `npm ci`) on the
# appliance, so the lockfile is not required for a successful install.
[ -f package-lock.json ] && cp package-lock.json "$ARTIFACTS_DIR/prisma/" || true
cp -r seed                        "$ARTIFACTS_DIR/prisma/seed"

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
