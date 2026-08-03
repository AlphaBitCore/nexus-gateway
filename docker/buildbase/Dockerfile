# Nexus Gateway build base — Go toolchain + Vectorscan (libhs).
#
# The four Go services link libhs via cgo for content scanning. libhs is C++
# built from source, so it cannot be cross-compiled; each architecture builds
# this image natively.
#
# FAT_RUNTIME MUST stay OFF. A FAT_RUNTIME=ON static archive linked into a Go
# cgo binary resolves its CPU-dispatch IFUNCs to a no-op stub: hs_scan returns
# zero matches with no error, so content scanning is silently disabled and PII
# passes through unredacted. Archive size is the tell — 11,331,324 bytes with
# FAT=OFF, measured on arm64 at this pin, against roughly 19 MB with it on — but
# size is only a hint, not a gate. The firing self-test in docker/services/Dockerfile is the gate that
# proves the linked engine actually scans; scripts/release/verify-image.sh is the
# separate gate that measures this archive's instruction-set baseline.
#
# With FAT_RUNTIME=OFF the SIMD instruction set is fixed at compile time, so
# whatever cmake targets is what these images can run on. USE_CPU_NATIVE is the
# knob that decides: with it off, cmake/archdetect.cmake takes GNUCC_ARCH from
# the configuration's own target — X86_ARCH, which cflags-x86.cmake sets to
# x86-64-v2 for the baseline build and core-avx2 when BUILD_AVX2 is on, and
# ARMV8_ARCH = armv8-a on arm64. With it on, cmake probes the build host's own
# CPU instead, which on a modern release runner means an archive that SIGILLs on
# older machines. It is undefined in 5.4.12 and therefore falsy, but a default
# is not a pin, so -DUSE_CPU_NATIVE=OFF is passed explicitly. That wins over a
# future `option(USE_CPU_NATIVE … ON)`, since option() never overwrites an
# existing cache entry; it would NOT win over a plain `set(... FORCE)`, so the
# ISA gate remains the thing that actually decides.
#
# The floor itself — x86-64-v2 / armv8-a — comes from Vectorscan's own
# cflags-x86.cmake / cflags-arm.cmake and is not something a caller can pass,
# so it is measured rather than trusted: scripts/release/verify-image.sh
# disassembles the archive this stage produces.
#
# Architecture: docs/developers/architecture/cross-cutting/deployment/container-image-architecture.md
FROM debian:bookworm

ARG VS_TAG=vectorscan/5.4.12
ARG VS_COMMIT=b585ad466658624bb31fb1d194cdb168df34833c
ARG GO_VERSION=1.26.5
# From https://go.dev/dl/?mode=json — update together with GO_VERSION. Pinned
# because this toolchain compiles and links every published service binary and
# every tarball binary. libhs next to it is commit-pinned; the compiler that
# consumes it must not be the weaker link, and "the extracted binary runs" is
# not an integrity check.
ARG GO_SHA256_AMD64=5c2c3b16caefa1d968a94c1daca04a7ca301a496d9b086e17ad77bb81393f053
ARG GO_SHA256_ARM64=fe4789e92b1f33358680864bbe8704289e7bb5fc207d80623c308935bd696d49
# Empty for the baseline build; "-DBUILD_AVX2=ON" for the avx2 variant.
ARG EXTRA_CMAKE_FLAGS=""

ENV DEBIAN_FRONTEND=noninteractive

# ragel is packaged by Debian, so unlike the Amazon Linux AMI build we do not
# need a from-source ragel. binutils supplies the objdump the ISA gate uses.
RUN apt-get update -qq \
 && apt-get install -y -qq --no-install-recommends \
      gcc g++ make cmake libboost-dev pkg-config git curl ca-certificates \
      ragel binutils \
 && rm -rf /var/lib/apt/lists/*

# EXTRA_CMAKE_FLAGS is expanded LAST on the cmake line below, so anything it
# carries overrides the flags set before it — cmake applies -D cache
# assignments in order, and "-DFAT_RUNTIME=ON" there would silently win over
# the FAT_RUNTIME=OFF this file's header calls mandatory. The two callers
# (scripts/release/build-images.sh, .github/workflows/buildbase.yml) only ever
# pass one of these two literals; this makes that a property of the image
# rather than of its callers, so a hand-run `docker build --build-arg` cannot
# produce an image that looks like a buildbase and scans nothing.
RUN set -eux; \
    case "$EXTRA_CMAKE_FLAGS" in \
      ""|"-DBUILD_AVX2=ON") ;; \
      *) echo "ERROR: EXTRA_CMAKE_FLAGS must be empty or exactly -DBUILD_AVX2=ON, got: $EXTRA_CMAKE_FLAGS" >&2; exit 1 ;; \
    esac; \
    cd /tmp; \
    git clone -q --depth 1 --branch "$VS_TAG" https://github.com/VectorCamp/vectorscan.git; \
    cd vectorscan; \
    test "$(git rev-parse HEAD)" = "$VS_COMMIT"; \
    mkdir build && cd build; \
    cmake -DFAT_RUNTIME=OFF \
          -DBUILD_AVX512=OFF \
          -DUSE_CPU_NATIVE=OFF \
          -DCMAKE_BUILD_TYPE=Release \
          -DCMAKE_INSTALL_PREFIX=/usr/local \
          $EXTRA_CMAKE_FLAGS ..; \
    make -j"$(nproc)" hs; \
    make install; \
    pkg-config --exists libhs; \
    cd /; rm -rf /tmp/vectorscan

RUN set -eux; \
    arch="$(dpkg --print-architecture)"; \
    case "$arch" in \
      amd64) goarch=amd64; gosha="$GO_SHA256_AMD64" ;; \
      arm64) goarch=arm64; gosha="$GO_SHA256_ARM64" ;; \
      *) echo "unsupported architecture: $arch" >&2; exit 1 ;; \
    esac; \
    curl -fsSL "https://go.dev/dl/go${GO_VERSION}.linux-${goarch}.tar.gz" -o /tmp/go.tgz; \
    echo "${gosha}  /tmp/go.tgz" | sha256sum -c -; \
    tar -C /usr/local -xzf /tmp/go.tgz; \
    rm -f /tmp/go.tgz; \
    /usr/local/go/bin/go version

# CGO_LDFLAGS: libhs.a is C++ (boost) and needs libstdc++, and its fdr_compile
# unit needs libm's pow, but libhs.pc declares neither — its Libs is only
# "-lhs". cgo therefore places -lstdc++ and -lm ahead of the pkg-config -lhs on
# the link line. Debian's gcc defaults to --as-needed, which drops a library
# that satisfies no *already-seen* undefined symbol; because -lhs comes last,
# libstdc++ and libm are dropped before libhs introduces its references to them,
# and the link fails with thousands of undefined std:: symbols plus pow. The AMI
# build gets away with a bare "-lstdc++ -lm" only because Amazon Linux's ld does
# not default to --as-needed. -Wl,--no-as-needed forces both libraries in
# unconditionally, making the link order-independent across distributions.
ENV PATH=/usr/local/go/bin:$PATH \
    PKG_CONFIG_PATH=/usr/local/lib64/pkgconfig:/usr/local/lib/pkgconfig \
    CGO_ENABLED=1 \
    CGO_LDFLAGS="-Wl,--no-as-needed -lstdc++ -lm" \
    GOCACHE=/tmp/gocache \
    GOMODCACHE=/tmp/gomod
WORKDIR /src
