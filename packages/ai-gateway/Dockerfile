# syntax=docker/dockerfile:1
# Nexus AI Gateway — production container image.
#
# Three-stage cgo build linking Vectorscan (libhs) statically:
#   Stage `libhs`   compiles libhs from source for THIS target arch.
#   Stage `builder` builds the Go binary with -tags vectorscan and hard-gates
#                   on the engine actually working before the image can exist.
#   Final stage     debian-slim runtime, nonroot.
#
# A default no-tag build silently falls back to the pure-Go RE2 matcher —
# functionally correct but far slower under load. This image must never ship
# that fallback, so the build fails loudly instead (Gate 1 + Gate 2 below).
# Build context: repository root.
#   docker build -f packages/ai-gateway/Dockerfile -t alphabitcore/nexus-ai-gateway:test .

ARG VECTORSCAN_VERSION=5.4.11

FROM debian:bookworm-slim AS libhs
ARG VECTORSCAN_VERSION
RUN apt-get update && apt-get install -y --no-install-recommends \
      ca-certificates git cmake make g++ ragel libboost-dev libsqlite3-dev pkg-config \
    && rm -rf /var/lib/apt/lists/*
RUN git clone --depth 1 --branch vectorscan/${VECTORSCAN_VERSION} \
      https://github.com/VectorCamp/vectorscan.git /src/vectorscan
# FAT_RUNTIME must stay OFF: with ON the library compiles and links fine but
# hs_alloc_scratch fails inside a cgo binary and content scanning never fires
# — PII would pass through unredacted with no error surfaced anywhere.
# BUILD_AVX512 stays OFF so the binary does not SIGILL on hosts without
# avx512vbmi.
RUN cmake -S /src/vectorscan -B /build \
      -DCMAKE_BUILD_TYPE=Release \
      -DBUILD_SHARED_LIBS=OFF \
      -DFAT_RUNTIME=OFF \
      -DBUILD_AVX512=OFF \
      -DBUILD_EXAMPLES=OFF \
      -DBUILD_BENCHMARKS=OFF \
      -DBUILD_UNIT=OFF \
    && cmake --build /build -j"$(nproc)" \
    && cmake --install /build --prefix /usr/local

FROM golang:1.25-bookworm AS builder
RUN apt-get update && apt-get install -y --no-install-recommends g++ pkg-config \
    && rm -rf /var/lib/apt/lists/*
COPY --from=libhs /usr/local/include/hs /usr/local/include/hs
COPY --from=libhs /usr/local/lib /usr/local/lib
WORKDIR /app
# GOWORK=off: the image carries only this module + its sibling `replace`
# target (../shared), not the full go.work module set.
COPY packages/shared/ packages/shared/
COPY packages/ai-gateway/ packages/ai-gateway/
# Static-link variant (the `vsstatic` build tag): the archive is named
# explicitly and the C++ runtime is linked AFTER it, so libhs's C++ exception
# symbols (__cxa_*) resolve deterministically regardless of dead-code
# elimination. (pkg-config ordering left -lstdc++ unresolved for the
# hs_compile/rose path.) Same model as the agent's static universal build.
ENV GOWORK=off \
    CGO_ENABLED=1 \
    CGO_CFLAGS="-I/usr/local/include -I/usr/local/include/hs" \
    CGO_LDFLAGS="/usr/local/lib/libhs.a -lstdc++ -lm"
# Gate 1: the engine must work at build time. HSSelfTest exercises
# hs_compile + hs_alloc_scratch + hs_scan — the only check that catches a
# FAT_RUNTIME / link mistake ("build succeeded" does not).
RUN mkdir -p packages/shared/policy/hooks/matcher/hsselfcheck \
    && printf '%s\n' \
      'package main' \
      '' \
      'import (' \
      '	"fmt"' \
      '	"os"' \
      '' \
      '	"github.com/AlphaBitCore/nexus-gateway/packages/shared/policy/hooks/matcher"' \
      ')' \
      '' \
      'func main() {' \
      '	ver, rc, matches := matcher.HSSelfTest()' \
      '	fmt.Printf("vectorscan selftest: version=%s scanRC=%d matches=%d\n", ver, rc, matches)' \
      '	if rc != 0 || matches < 1 {' \
      '		fmt.Fprintln(os.Stderr, "FATAL: vectorscan linked but scratch/scan failed (FAT_RUNTIME=ON? bad libhs?)")' \
      '		os.Exit(1)' \
      '	}' \
      '}' \
      > packages/shared/policy/hooks/matcher/hsselfcheck/main.go \
    && cd packages/shared \
    && go build -tags "vectorscan vsstatic" -trimpath -ldflags="-s -w" -o /hs-selfcheck \
         ./policy/hooks/matcher/hsselfcheck \
    && /hs-selfcheck \
    && rm -r policy/hooks/matcher/hsselfcheck
WORKDIR /app/packages/ai-gateway
RUN go build -tags "vectorscan vsstatic" -trimpath -ldflags="-s -w" -o /ai-gateway ./cmd/ai-gateway/
# Gate 2: the shipped binary itself must carry the vectorscan tag — never
# ship the silent RE2 fallback.
RUN go version -m /ai-gateway | grep -q -- '-tags=vectorscan' \
    || { echo 'FATAL: ai-gateway built without -tags vectorscan (RE2 fallback image)'; exit 1; }

FROM debian:bookworm-slim
RUN apt-get update && apt-get install -y --no-install-recommends \
      ca-certificates gettext-base curl libstdc++6 \
    && rm -rf /var/lib/apt/lists/* \
    && useradd --system --uid 65532 --create-home nexus \
    && mkdir -p /etc/nexus && chown nexus:nexus /etc/nexus
COPY --from=builder /ai-gateway /usr/local/bin/ai-gateway
# Shipped so CI (and operators) can verify the engine inside the exact runtime
# environment: docker run --entrypoint hs-selfcheck <image>
COPY --from=builder /hs-selfcheck /usr/local/bin/hs-selfcheck
COPY deploy/docker/ai-gateway/config.yaml.tpl /opt/nexus/ai-gateway.config.yaml.tpl
COPY deploy/docker/ai-gateway/entrypoint.sh /usr/local/bin/nexus-entrypoint.sh
RUN chmod 0755 /usr/local/bin/nexus-entrypoint.sh
USER nexus
EXPOSE 3050
ENTRYPOINT ["nexus-entrypoint.sh"]
