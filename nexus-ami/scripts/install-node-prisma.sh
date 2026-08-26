#!/bin/bash
# install-node-prisma.sh — install a self-contained Node.js 20 runtime under
# /opt/nexus/node and run `npm install` inside /opt/nexus/prisma so the
# first-boot Prisma client / seed / tsx commands work offline.
#
# Why self-contained?
#   - AL2023's dnf node is older + slower-moving; pinning a specific Node 20
#     binary keeps the AMI reproducible across Marketplace rebuilds.
#   - Only the first-boot path uses Node; nothing else on the appliance needs
#     it, so installing into /opt/nexus/node keeps it out of the system PATH.
#
# Architecture: docs/developers/architecture/cross-cutting/deployment/ami-appliance-architecture.md

set -euo pipefail

# NODE_VERSION must satisfy Prisma's engines.node constraint. Prisma 7.8.0
# requires "^20.19 || ^22.12 || >=24.0"; chokidar@5 + readdirp@5 (transitive
# deps) also require ">=20.19.0". Hard-pinned 20.18.1 produced an npm
# EBADENGINE fatal at AMI build time — verified 2026-05-28. Stay within
# 20.x LTS line ("Iron") to keep the runtime delta minimal across rebuilds.
# shellcheck source=lib-verify.sh
source "$(dirname "$0")/lib-verify.sh"

NODE_VERSION=20.19.0
ARCH=$(uname -m)
# per-arch sha256 from the Node-published SHASUMS256.txt for this
# release (the script previously ignored those sums). verify_sha256 fails the
# build on any mismatch. Re-record both on every version bump:
#   curl -fsSL https://nodejs.org/dist/v<V>/SHASUMS256.txt
case "$ARCH" in
  x86_64)  NODE_ARCH=x64;   NODE_SHA256=b4e336584d62abefad31baecff7af167268be9bb7dd11f1297112e6eed3ca0d5 ;;
  aarch64) NODE_ARCH=arm64; NODE_SHA256=dbe339e55eb393955a213e6b872066880bb9feceaa494f4d44c7aac205ec2ab9 ;;
  *) echo "ERROR: unsupported arch $ARCH" >&2; exit 1 ;;
esac

NODE_DIR=/opt/nexus/node
PRISMA_DIR=/opt/nexus/prisma

TARBALL="node-v$NODE_VERSION-linux-$NODE_ARCH.tar.xz"
URL="https://nodejs.org/dist/v$NODE_VERSION/$TARBALL"

echo "==> [install-node-prisma] downloading Node.js $NODE_VERSION..."
cd /tmp
curl -fsSL "$URL" -o "$TARBALL"
verify_sha256 "$NODE_SHA256" "$TARBALL"
mkdir -p "$NODE_DIR"
tar xJf "$TARBALL" -C "$NODE_DIR" --strip-components=1
rm -f "$TARBALL"

export PATH="$NODE_DIR/bin:$PATH"

echo "==> [install-node-prisma] node $(node --version) | npm $(npm --version) installed at $NODE_DIR"

echo "==> [install-node-prisma] running npm install in $PRISMA_DIR..."
cd "$PRISMA_DIR"
"$NODE_DIR/bin/npm" install --omit=dev --no-audit --no-fund

# Install tsx + typescript globally so first-boot-db.sh can call them
# regardless of devDependencies.
"$NODE_DIR/bin/npm" install -g --no-audit --no-fund tsx typescript

echo "==> [install-node-prisma] complete."
