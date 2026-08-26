#!/bin/bash
# build-binaries.sh — compile the four Nexus Go services ON the Packer build
# instance with the Vectorscan (Hyperscan) content-scanning engine, then strip
# the build toolchain + source back off so neither ships in the AMI.
#
# Why on-instance: the services link libhs (Vectorscan) via cgo. The old
# CGO_ENABLED=0 cross-compile silently selected the pure-Go RE2 fallback matcher
# — but the gateway's content-scanning engine (and the perf profile built around
# it: the cgo scan limiter, hook prefilter, parallel hooks) is the Vectorscan
# path. libhs is C++ and cannot be cross-compiled from a macOS arm64 control
# host, so the build runs here on the linux/amd64 build instance — the same
# on-instance-compile model install-valkey.sh already uses.
#
# libhs MUST be FAT_RUNTIME=OFF: a FAT_RUNTIME=ON static archive linked into a Go
# cgo binary resolves its CPU-dispatch IFUNCs to a no-op stub, so hs_scan
# silently returns zero matches — content scanning disabled with no error (the
# ~11.9 MB FAT=OFF archive vs ~19 MB FAT=ON is the tell). Step 6's self-test is
# the gate that proves the linked engine actually fires.
#
# Architecture: docs/developers/architecture/cross-cutting/deployment/ami-appliance-architecture.md

set -euo pipefail

STAGING_DIR=/tmp/nexus
SRC_TARBALL="$STAGING_DIR/nexus-src.tar.gz"
SRC_DIR=/tmp/nexus-src
BUILD_ROOT=/tmp/nexus-toolchain
GOROOT=/usr/local/go

# Pinned toolchain versions + sha256 (supply-chain: matches install-valkey.sh).
GO_VERSION=1.26.4
GO_SHA256=1153d3d50e0ac764b447adfe05c2bcf08e889d42a02e0fe0259bd47f6733ad7f
RAGEL_VERSION=6.10
RAGEL_SHA256=5f156edb65d20b856d638dd9ee2dfb43285914d9aa2b6ec779dac0270cd56c3f
# Vectorscan is git-cloned at a pinned tag; the commit is verified post-clone.
VECTORSCAN_TAG=vectorscan/5.4.12
VECTORSCAN_COMMIT=b585ad466658624bb31fb1d194cdb168df34833c

export PATH="$GOROOT/bin:$PATH"

fetch_verify() {  # $1=url $2=out $3=sha256
  curl -fsSL "$1" -o "$2"
  echo "$3  $2" | sha256sum -c - || { echo "ERROR: sha256 mismatch for $2" >&2; exit 1; }
}

[ -f "$SRC_TARBALL" ] || { echo "ERROR: source tarball missing at $SRC_TARBALL" >&2; exit 1; }

echo "==> [build-binaries] installing transient build toolchain..."
# C/C++ toolchain + libhs build deps. libstdc++ (runtime) is installed by
# install.sh as a top-level package, so the autoremove in step 7 keeps it.
dnf install -y gcc gcc-c++ make cmake boost-devel pcre2-devel git

# --- Go ---
fetch_verify "https://go.dev/dl/go${GO_VERSION}.linux-amd64.tar.gz" /tmp/go.tgz "$GO_SHA256"
rm -rf "$GOROOT"; tar -C /usr/local -xzf /tmp/go.tgz; rm -f /tmp/go.tgz
go version

# --- ragel (not in the AL2023 repos) from source ---
mkdir -p "$BUILD_ROOT"; cd "$BUILD_ROOT"
fetch_verify "https://www.colm.net/files/ragel/ragel-${RAGEL_VERSION}.tar.gz" ragel.tgz "$RAGEL_SHA256"
tar xzf ragel.tgz; cd "ragel-${RAGEL_VERSION}"
./configure --prefix=/usr/local >/dev/null
make -j"$(nproc)" >/dev/null
make install >/dev/null
ragel --version | head -1

# --- libhs (Vectorscan) FAT_RUNTIME=OFF ---
cd "$BUILD_ROOT"
git clone -q --depth 1 --branch "$VECTORSCAN_TAG" https://github.com/VectorCamp/vectorscan.git
cd vectorscan
[ "$(git rev-parse HEAD)" = "$VECTORSCAN_COMMIT" ] || { echo "ERROR: vectorscan commit mismatch (got $(git rev-parse HEAD), want $VECTORSCAN_COMMIT)" >&2; exit 1; }
mkdir build && cd build
cmake -DFAT_RUNTIME=OFF -DBUILD_AVX512=OFF -DCMAKE_BUILD_TYPE=Release -DCMAKE_INSTALL_PREFIX=/usr/local .. >/dev/null
make -j"$(nproc)" hs >/dev/null
make install >/dev/null
pkg-config --exists libhs && echo "==> [build-binaries] libhs $(pkg-config --modversion libhs) (FAT_RUNTIME=OFF)"

# --- Compile the four services with the Vectorscan engine ---
echo "==> [build-binaries] compiling Nexus services (-tags vectorscan)..."
rm -rf "$SRC_DIR"; mkdir -p "$SRC_DIR" "$STAGING_DIR/bin"
tar -C "$SRC_DIR" -xzf "$SRC_TARBALL"
cd "$SRC_DIR"
VER="ami@$(cat "$STAGING_DIR/NEXUS_VERSION" 2>/dev/null || echo unknown)+vs"
export PKG_CONFIG_PATH=/usr/local/lib64/pkgconfig
export CGO_ENABLED=1
export CGO_LDFLAGS="-lstdc++ -lm"
for svc in \
  nexus-hub:packages/nexus-hub/cmd/nexus-hub \
  control-plane:packages/control-plane/cmd/control-plane \
  ai-gateway:packages/ai-gateway/cmd/ai-gateway \
  compliance-proxy:packages/compliance-proxy/cmd/compliance-proxy; do
  name=${svc%%:*}; path=${svc#*:}
  echo "==> [build-binaries]   building $name ..."
  go build -tags vectorscan -ldflags "-X main.buildVersion=$VER" -o "$STAGING_DIR/bin/$name" "./$path/"
done

# --- Firing self-test: prove the linked libhs actually scans (FAT_RUNTIME gate) ---
echo "==> [build-binaries] verifying Vectorscan fires..."
go test -tags vectorscan -count=1 -run 'TestVectorscan_Scan' ./packages/shared/policy/hooks/matcher/ >/dev/null \
  || { echo "ERROR: Vectorscan self-test failed — the linked libhs does not scan (FAT_RUNTIME=ON?); binaries would pass PII through UNREDACTED" >&2; exit 1; }
echo "==> [build-binaries] Vectorscan firing verified."

# --- Strip the Nexus source + cgo build footprint so they never ship ---
# Deletes the proprietary Nexus source tree, the Go toolchain + module cache, and
# the ragel/libhs/vectorscan build trees + installs. We deliberately do NOT
# dnf-remove the shared C/C++ toolchain (gcc/gcc-c++/make/cmake/boost/pcre2):
# install-valkey.sh runs AFTER this and compiles valkey-search from source, so it
# needs them — and they are part of the existing image already. libstdc++
# (runtime) stays for the vectorscan-linked binaries.
echo "==> [build-binaries] removing Nexus source + cgo build footprint from the image..."
rm -rf "$GOROOT" "$SRC_DIR" "$BUILD_ROOT" /root/go /root/.cache/go-build "$SRC_TARBALL"
rm -rf /usr/local/lib64/libhs.a /usr/local/lib64/pkgconfig/libhs.pc /usr/local/include/hs
rm -f  /usr/local/bin/ragel /usr/local/bin/ragelh 2>/dev/null || true
echo "==> [build-binaries] done (4 binaries in $STAGING_DIR/bin/)."
