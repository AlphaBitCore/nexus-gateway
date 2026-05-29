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
NODE_VERSION=20.19.0
ARCH=$(uname -m)
case "$ARCH" in
  x86_64)  NODE_ARCH=x64 ;;
  aarch64) NODE_ARCH=arm64 ;;
  *) echo "ERROR: unsupported arch $ARCH" >&2; exit 1 ;;
esac

NODE_DIR=/opt/nexus/node
PRISMA_DIR=/opt/nexus/prisma

TARBALL="node-v$NODE_VERSION-linux-$NODE_ARCH.tar.xz"
URL="https://nodejs.org/dist/v$NODE_VERSION/$TARBALL"

echo "==> [install-node-prisma] downloading Node.js $NODE_VERSION..."
cd /tmp
curl -fsSL "$URL" -o "$TARBALL"
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
