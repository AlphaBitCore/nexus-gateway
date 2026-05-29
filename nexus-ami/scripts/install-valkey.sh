#!/bin/bash
# install-valkey.sh — build Valkey 8 + valkey-search from source on AL2023.
# AL2023 dnf has no valkey package (checked 2026-05); the official Valkey
# project ships valkey/valkey-bundle Docker images but no rpm. Source compile
# is the cleanest path for a baked AMI (no docker-in-AMI smell).
#
# Architecture: docs/developers/architecture/cross-cutting/deployment/ami-appliance-architecture.md
# Wire-compatible with Redis 7 — every go-redis/v9 client in Nexus works
# unchanged against it.

set -euo pipefail

# valkey-search 1.2.0 hard-requires Valkey >= 9.0.1 at module-init time. Hit
# on 2026-05-28 first-launch test: a Valkey 8.1.2 server with libsearch.so
# loadmodule logged
#   <search> Minimum required server version is 9.0.1, Current version is 8.1.2
#   Module /usr/lib/valkey/libsearch.so initialization failed.
# and aborted boot. Bumping to 9.0.4 (the latest in the 9.0.x line) restores
# compatibility while staying on the 9.0 series (avoiding 9.1.x feature drift).
VALKEY_VERSION=9.0.4
# valkey-search GitHub tags do NOT use a `v` prefix (e.g. `1.2.0`, not
# `v1.2.0`). Verified via GitHub API 2026-05-28; 1.2.0 is the latest stable
# release. Bumping requires re-checking the API:
#   curl -fsSL https://api.github.com/repos/valkey-io/valkey-search/tags
VALKEY_SEARCH_VERSION=1.2.0
BUILD_DIR=/tmp/valkey-build
INSTALL_PREFIX=/usr/local

echo "==> [install-valkey] installing build dependencies..."
# valkey-search 1.x switched to ninja + cmake + submodules (gRPC, Protobuf,
# Abseil bundled as submodules); cmake hard-checks the C++ compiler is
# either GCC ≥ 12 or Clang ≥ 16. AL2023's default gcc is 11.5.0 and default
# clang is 15.0.7 — both too old. AL2023 dnf ships versioned clang packages
# (confirmed 2026-05-28 dnf list available 'clang*'): clang18 / clang19 /
# clang20 are present; clang17 / clang16 are NOT in the repo. We pick the
# newest available.

dnf install -y \
  gcc \
  gcc-c++ \
  make \
  cmake \
  ninja-build \
  git \
  openssl-devel \
  systemd-devel \
  pkgconf

echo "==> [install-valkey] selecting a Clang ≥ 16 ..."
# Each clang${ver} package installs /usr/bin/clang-${ver} + /usr/bin/clang++-${ver}
# following the standard LLVM versioned-binary convention. We try newest-first
# and stop at the first one whose binary lands on PATH. 17/16 stay in the list
# in case a future repo refresh adds them back; current AL2023 (2026-05) skips
# straight from 15 → 18.
CLANG_BIN=""
CLANGXX_BIN=""
# Also install lld${ver} alongside clang${ver}. valkey-search's CMake compiles
# with -flto; linking libsearch.so requires LTO bitcode handling. GNU ld
# delegates LTO to LLVMgold.so which AL2023's `clang${ver}` package does NOT
# ship — verified 2026-05-28 build: link of libsearch.so failed with
# "cannot open /usr/lib64/llvm20/lib64/LLVMgold.so". lld is LLVM's native
# linker; it handles LTO bitcode directly, no plugin needed.
for ver in 20 19 18 17 16; do
  if dnf install -y "clang${ver}" "lld${ver}" 2>/dev/null && command -v "clang-${ver}" >/dev/null 2>&1; then
    CLANG_BIN="clang-${ver}"
    CLANGXX_BIN="clang++-${ver}"
    # `-fuse-ld=lld` only finds `ld.lld` (unversioned) on PATH. AL2023's
    # lld${ver} package installs versioned binaries (ld.lld-${ver}); add a
    # symlink if the unversioned name is missing.
    if ! command -v ld.lld >/dev/null 2>&1; then
      for candidate in "/usr/bin/ld.lld-${ver}" "/usr/bin/lld-${ver}"; do
        if [ -x "$candidate" ]; then
          ln -sf "$candidate" /usr/bin/ld.lld
          break
        fi
      done
    fi
    command -v ld.lld >/dev/null 2>&1 || {
      echo "ERROR: ld.lld not in PATH after installing lld${ver}; cannot use lld for LTO link." >&2
      ls -la /usr/bin/ld.lld* /usr/bin/lld* 2>&1 | head -10 >&2
      exit 1
    }
    echo "==> [install-valkey] using clang-${ver} + ld.lld (LTO via lld, not LLVMgold plugin)"
    break
  fi
done
if [ -z "$CLANG_BIN" ]; then
  echo "ERROR: AL2023 dnf has no clang ≥ 16 (tried clang20 / 19 / 18 / 17 / 16); valkey-search 1.x requires ≥ 16." >&2
  echo "Available clang/lld packages:" >&2
  dnf list available 'clang*' 'lld*' 2>&1 | head -30 >&2
  exit 1
fi

echo "==> [install-valkey] verifying compiler versions..."
gcc --version | head -1
"$CLANG_BIN" --version | head -1

# ─── Valkey core ────────────────────────────────────────────────────────────

echo "==> [install-valkey] downloading Valkey $VALKEY_VERSION source..."
mkdir -p "$BUILD_DIR" && cd "$BUILD_DIR"
curl -fsSL "https://github.com/valkey-io/valkey/archive/refs/tags/$VALKEY_VERSION.tar.gz" \
  -o "valkey-$VALKEY_VERSION.tar.gz"
tar xzf "valkey-$VALKEY_VERSION.tar.gz"
cd "valkey-$VALKEY_VERSION"

echo "==> [install-valkey] building Valkey ($(nproc) parallel jobs)..."
make -j"$(nproc)" USE_SYSTEMD=yes BUILD_TLS=yes
make install PREFIX="$INSTALL_PREFIX"

# ─── valkey-search module ───────────────────────────────────────────────────
# semantic cache module — packages/ai-gateway/internal/cache/semantic/ requires
# the FT.CREATE / FT.SEARCH commands this module provides.
#
# valkey-search 1.x uses ninja + cmake + git submodules (gRPC, Protobuf,
# Abseil all vendored). We use the project's canonical `build.sh` rather
# than calling cmake directly — the build script knows where the submodules
# live and how to wire them. --recurse-submodules at clone time pulls all
# vendored deps in one shot.

echo "==> [install-valkey] cloning valkey-search $VALKEY_SEARCH_VERSION (with submodules)..."
cd "$BUILD_DIR"
git clone --recurse-submodules --depth 1 --shallow-submodules \
  --branch "$VALKEY_SEARCH_VERSION" \
  https://github.com/valkey-io/valkey-search.git
cd valkey-search

# ─── Patch: Linux x86_64 + clang duplicate-overload in type_conversions.h ───
# vmsdk/src/type_conversions.h has:
#   template <> inline absl::StatusOr<uint64_t>      To(absl::string_view);
#   #if defined(__clang__) && !defined(RunningClangd)
#   template <> inline absl::StatusOr<unsigned long> To(absl::string_view);
#   #endif
# On Linux x86_64 (LP64) `uint64_t === unsigned long`, so the guarded overload
# collides with the unguarded one and clang ≥ 18 emits a hard redefinition
# error in vmsdklib — verified 2026-05-28 against tags 1.2.0 AND main. The
# overload only matters on platforms where the two types differ (macOS arm64/
# x86_64: uint64_t = unsigned long long ≠ unsigned long). Fix: tighten the
# guard to also require !defined(__linux__). No-op on macOS; eliminates the
# redefinition on Linux. Idempotent: re-running the sed is harmless.
echo "==> [install-valkey] patching type_conversions.h for Linux+clang duplicate template..."
sed -i 's|^#if defined(__clang__) && !defined(RunningClangd)$|#if defined(__clang__) \&\& !defined(RunningClangd) \&\& !defined(__linux__)|' vmsdk/src/type_conversions.h
grep -q '!defined(__linux__)' vmsdk/src/type_conversions.h || {
  echo "ERROR: type_conversions.h patch did not apply — upstream source layout changed" >&2
  echo "Expected line to patch: #if defined(__clang__) && !defined(RunningClangd)" >&2
  echo "Current grep for the guard line:" >&2
  grep -n 'defined(__clang__)' vmsdk/src/type_conversions.h >&2 || true
  exit 1
}

# Force the build to use Clang (≥ 16 on AL2023) instead of the default
# gcc-11.5.0 (which valkey-search 1.x cmake rejects: "Minimum GCC required
# is 12 and later").
#
# Cap parallelism at 4 regardless of available cores. Each gRPC/Protobuf/ICU
# compile worker can hold 1.5–2 GB resident; running all 8 cores parallel
# on t3.2xlarge would push 16+ GB and risk OOM-killer even on 32 GB hosts.
# --jobs=4 is the sweet spot: comfortable 16–24 GB working set, no OOM.
echo "==> [install-valkey] building valkey-search with ${CLANG_BIN} (jobs=4, linker=lld)..."
export CC="$CLANG_BIN"
export CXX="$CLANGXX_BIN"
# Force lld as the linker for every link step. CMake propagates LDFLAGS into
# CMAKE_{EXE,SHARED,MODULE}_LINKER_FLAGS at configure time, so this reaches
# the libsearch.so shared-library link where the LTO bitcode lives. Required
# because AL2023's clang20 ships without LLVMgold.so — see comment above the
# clang/lld install loop.
export LDFLAGS="${LDFLAGS:-} -fuse-ld=lld"
./build.sh --jobs=4

echo "==> [install-valkey] locating built libsearch.so..."
SO_PATH=$(find . -name 'libsearch.so' -type f 2>/dev/null | head -1)
if [ -z "$SO_PATH" ]; then
  echo "ERROR: libsearch.so not found after build" >&2
  find . -name '*.so' -type f 2>/dev/null | head -20 >&2
  exit 1
fi
echo "==> [install-valkey] found: $SO_PATH"

install -d -m 0755 /usr/lib/valkey
# 0755 (not 0644) — Valkey's module loader explicitly checks the execute bit
# before dlopen as a safety guard, and refuses to load with "It does not have
# execute permissions." Hit on 2026-05-28 — the AMI booted with valkey in a
# restart loop. Standard convention for shared libs on Linux is 0755.
install -m 0755 "$SO_PATH" /usr/lib/valkey/libsearch.so

# ─── User + directories + config ────────────────────────────────────────────

echo "==> [install-valkey] creating valkey user + dirs..."
if ! id -u valkey >/dev/null 2>&1; then
  useradd --system --no-create-home --shell /sbin/nologin --user-group valkey
fi
install -d -o valkey -g valkey -m 0750 /var/lib/valkey /var/log/valkey /var/run/valkey
install -d -o root   -g root   -m 0755 /etc/valkey

cat > /etc/valkey/valkey.conf <<'EOF'
# Nexus appliance — Valkey config (localhost-only).
bind 127.0.0.1
port 6379
protected-mode yes
supervised systemd
loglevel notice
logfile /var/log/valkey/valkey.log
dir /var/lib/valkey
appendonly yes
appendfsync everysec
maxmemory-policy allkeys-lru
loadmodule /usr/lib/valkey/libsearch.so
EOF
chmod 0644 /etc/valkey/valkey.conf

# ─── Cleanup ────────────────────────────────────────────────────────────────

rm -rf "$BUILD_DIR"

echo "==> [install-valkey] complete (Valkey $VALKEY_VERSION + valkey-search $VALKEY_SEARCH_VERSION)."
