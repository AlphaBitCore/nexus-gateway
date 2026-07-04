# syntax=docker/dockerfile:1
# Nexus Compliance Proxy — production container image.
#
# Same vectorscan build discipline as the AI Gateway (see
# packages/ai-gateway/Dockerfile for the full rationale): static per-arch
# libhs with FAT_RUNTIME=OFF + BUILD_AVX512=OFF, cgo build under
# -tags vectorscan, and two hard gates so a silent RE2-fallback image can
# never ship.
# Build context: repository root (the module depends on ../shared via a
# sibling replace directive, so a package-dir context cannot build).
#   docker build -f packages/compliance-proxy/Dockerfile -t alphabitcore/nexus-compliance-proxy:test .

ARG VECTORSCAN_VERSION=5.4.11

FROM debian:bookworm-slim AS libhs
ARG VECTORSCAN_VERSION
RUN apt-get update && apt-get install -y --no-install-recommends \
      ca-certificates git cmake make g++ ragel libboost-dev libsqlite3-dev pkg-config \
    && rm -rf /var/lib/apt/lists/*
RUN git clone --depth 1 --branch vectorscan/${VECTORSCAN_VERSION} \
      https://github.com/VectorCamp/vectorscan.git /src/vectorscan
# FAT_RUNTIME=OFF is mandatory (silent hs_alloc_scratch failure in cgo
# otherwise); BUILD_AVX512=OFF for CPU portability.
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
COPY packages/compliance-proxy/ packages/compliance-proxy/
# Static-link variant (`vsstatic` tag): archive named explicitly, C++ runtime
# linked AFTER it so libhs's __cxa_* exception symbols resolve deterministically
# (pkg-config ordering left them unresolved on the hs_compile/rose path).
ENV GOWORK=off \
    CGO_ENABLED=1 \
    CGO_CFLAGS="-I/usr/local/include -I/usr/local/include/hs" \
    CGO_LDFLAGS="/usr/local/lib/libhs.a -lstdc++ -lm"
# Gate 1: engine self-test at build time (catches FAT_RUNTIME/link mistakes).
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
WORKDIR /app/packages/compliance-proxy
RUN go build -tags "vectorscan vsstatic" -trimpath -ldflags="-s -w" -o /nexus-compliance-proxy ./cmd/compliance-proxy
# Gate 2: the shipped binary must carry the vectorscan tag.
RUN go version -m /nexus-compliance-proxy | grep -q -- '-tags=vectorscan' \
    || { echo 'FATAL: compliance-proxy built without -tags vectorscan (RE2 fallback image)'; exit 1; }

FROM debian:bookworm-slim
RUN apt-get update && apt-get install -y --no-install-recommends \
      ca-certificates gettext-base curl libstdc++6 openssl \
    && rm -rf /var/lib/apt/lists/* \
    && useradd --system --uid 65532 --create-home nexus \
    && mkdir -p /etc/nexus /etc/compliance-proxy /var/lib/nexus/alerting /var/lib/nexus/audit-spool \
    && chown -R nexus:nexus /etc/nexus /etc/compliance-proxy /var/lib/nexus
COPY --from=builder /nexus-compliance-proxy /usr/local/bin/nexus-compliance-proxy
# Shipped so CI (and operators) can verify the engine inside the exact runtime
# environment: docker run --entrypoint hs-selfcheck <image>
COPY --from=builder /hs-selfcheck /usr/local/bin/hs-selfcheck
COPY deploy/docker/compliance-proxy/config.yaml.tpl /opt/nexus/compliance-proxy.config.yaml.tpl
COPY deploy/docker/compliance-proxy/entrypoint.sh /usr/local/bin/nexus-entrypoint.sh
RUN chmod 0755 /usr/local/bin/nexus-entrypoint.sh
USER nexus
# 3128 CONNECT proxy · 3040 runtime API (console control-plane calls it) · 9090 metrics
EXPOSE 3128 3040 9090
ENTRYPOINT ["nexus-entrypoint.sh"]
