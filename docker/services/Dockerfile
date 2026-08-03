# Nexus Gateway service images.
#
# One builder stage compiles all four Go services: they share a single go.work
# module graph and a single libhs install, so compiling them together costs one
# libhs link per architecture instead of four.
#
# The engine-firing self-test runs HERE, in the builder, because the distroless
# runtime has neither the Go toolchain nor the source tree. A FAT_RUNTIME=ON
# archive links cleanly and then silently returns zero matches, so linking
# successfully is not evidence that scanning works — running the matcher is.
#
# Architecture: docs/developers/architecture/cross-cutting/deployment/container-image-architecture.md
ARG BUILDBASE=nexus-buildbase:local
FROM ${BUILDBASE} AS builder

ARG BUILD_VERSION=dev

WORKDIR /src
COPY go.work go.work.sum ./
COPY packages/ packages/
# go.work's `use` list includes tests/integration-go alongside every
# packages/* module; `go build` validates the whole workspace before it will
# resolve anything, so this one non-packages/ module must be present too or
# workspace resolution fails even though nothing here imports it.
COPY tests/integration-go/ tests/integration-go/

RUN set -eux; \
    for svc in \
      nexus-hub:packages/nexus-hub/cmd/nexus-hub \
      control-plane:packages/control-plane/cmd/control-plane \
      ai-gateway:packages/ai-gateway/cmd/ai-gateway \
      compliance-proxy:packages/compliance-proxy/cmd/compliance-proxy; \
    do \
      name="${svc%%:*}"; path="${svc#*:}"; \
      echo "building $name"; \
      go build -tags vectorscan -trimpath \
        -ldflags "-s -w -X main.buildVersion=${BUILD_VERSION}" \
        -o "/out/$name" "./$path/"; \
    done

# An empty directory to graft into the two runtime images that need writable
# state. The runtime base is distroless — no shell, so no `RUN mkdir` there —
# and the ownership matters as much as the path: Docker initialises a fresh
# named volume from the image's directory INCLUDING its owner, so a mount point
# that does not exist in the image is created root-owned and a `:nonroot` image
# (uid 65532) cannot write inside it. Creating it here, then copying it in with
# --chown, is what makes the mounted volume writable.
RUN mkdir -p /out/state

# Firing self-test: proves the linked libhs actually scans. If this fails the
# binaries would pass PII through unredacted, so the build must not continue.
#
# The pattern is anchored, names the tests exactly, and the number that passed
# is asserted. All three properties are load-bearing: `-run` is an unanchored
# regexp and `go test` exits 0 on "no tests to run", so a loose pattern can
# select nothing and still be green, or select the wrong thing — this package
# also holds TestVectorscan_ScanDuringClose, which asserts that a scan after
# Close returns NO hits and therefore passes on a completely dead engine.
# TestHSSelfTest is the one that fails unless /secret/ matches once. Keep this
# list in step with scripts/release/build-tarball.sh, which gates the tarball
# binaries the same way.
RUN set -eu; \
    out="$(go test -tags vectorscan -count=1 -v \
      -run '^(TestHSSelfTest|TestVectorscan_ScanUnderCgoLimit|TestVectorscan_ScanComplete_ReportsCompletion)$' \
      ./packages/shared/policy/hooks/matcher/ 2>&1)" \
      || { printf '%s\n' "$out" >&2; echo "ERROR: Vectorscan self-test failed — the linked libhs does not scan (FAT_RUNTIME=ON?); these binaries would pass PII through UNREDACTED" >&2; exit 1; }; \
    printf '%s\n' "$out"; \
    passed="$(printf '%s\n' "$out" | grep -c '^--- PASS: ' || true)"; \
    if [ "$passed" != 3 ]; then \
      echo "ERROR: expected 3 Vectorscan firing self-tests to pass, got ${passed} — the gate selected the wrong tests (renamed? removed?), so nothing proves the linked libhs scans" >&2; \
      exit 1; \
    fi

# ── Runtime images ────────────────────────────────────────────────────────
# distroless/cc carries glibc and libstdc++, which the cgo link (-lstdc++)
# requires, runs as non-root, and ships no shell.

FROM gcr.io/distroless/cc-debian12:nonroot AS nexus-hub
ARG BUILD_VERSION=dev
LABEL org.opencontainers.image.title="nexus-hub" \
      org.opencontainers.image.source="https://github.com/AlphaBitCore/nexus-gateway" \
      org.opencontainers.image.licenses="Apache-2.0"
COPY --from=builder /out/nexus-hub /usr/local/bin/nexus-hub
COPY packages/nexus-hub/nexus-hub.config.yaml /etc/nexus/nexus-hub.yaml
EXPOSE 3060
ENTRYPOINT ["/usr/local/bin/nexus-hub"]
CMD ["--config", "/etc/nexus/nexus-hub.yaml"]

FROM gcr.io/distroless/cc-debian12:nonroot AS control-plane
ARG BUILD_VERSION=dev
LABEL org.opencontainers.image.title="control-plane" \
      org.opencontainers.image.source="https://github.com/AlphaBitCore/nexus-gateway" \
      org.opencontainers.image.licenses="Apache-2.0"
COPY --from=builder /out/control-plane /usr/local/bin/control-plane
COPY packages/control-plane/control-plane.config.yaml /etc/nexus/control-plane.yaml
EXPOSE 3001
ENTRYPOINT ["/usr/local/bin/control-plane"]
CMD ["--config", "/etc/nexus/control-plane.yaml"]

FROM gcr.io/distroless/cc-debian12:nonroot AS ai-gateway
ARG BUILD_VERSION=dev
LABEL org.opencontainers.image.title="ai-gateway" \
      org.opencontainers.image.source="https://github.com/AlphaBitCore/nexus-gateway" \
      org.opencontainers.image.licenses="Apache-2.0"
COPY --from=builder /out/ai-gateway /usr/local/bin/ai-gateway
COPY packages/ai-gateway/ai-gateway.config.yaml /etc/nexus/ai-gateway.yaml
# Audit spill root (ai-gateway.config.yaml audit.spoolDir). Present and owned by
# the runtime uid so a volume mounted here is writable — deploy/docker-compose.yml
# mounts one. Without it the spool cannot be created, and the default spillblock
# loss mode downgrades to `block` at startup: no audit is lost, but the request
# path back-pressures on the in-heap queue alone with no disk buffer to absorb a
# burst. The systemd unit grants the same path via ReadWritePaths=/var/lib/nexus.
COPY --from=builder --chown=65532:65532 /out/state /var/lib/nexus
EXPOSE 3050
ENTRYPOINT ["/usr/local/bin/ai-gateway"]
CMD ["--config", "/etc/nexus/ai-gateway.yaml"]

FROM gcr.io/distroless/cc-debian12:nonroot AS compliance-proxy
ARG BUILD_VERSION=dev
LABEL org.opencontainers.image.title="compliance-proxy" \
      org.opencontainers.image.source="https://github.com/AlphaBitCore/nexus-gateway" \
      org.opencontainers.image.licenses="Apache-2.0"
COPY --from=builder /out/compliance-proxy /usr/local/bin/compliance-proxy
COPY packages/compliance-proxy/compliance-proxy.config.yaml /etc/nexus/compliance-proxy.yaml
# NDJSON audit fallback root (compliance-proxy.config.yaml audit.ndjson.dir),
# same ownership reasoning as ai-gateway above. Since 1.5.0 this service resolves
# its overflow policy through shared/audit/lossmode exactly as the gateway does,
# and defaults to spillblock — so with no writable spool it does not drop, it
# degrades to `block` (Mode.WithoutDurableSink) and back-pressures. For a service
# that sits in an intercepted TLS path that is the more disruptive failure of the
# two: the stall lands on somebody's connection. The systemd unit grants the same
# path via ReadWritePaths=/var/lib/nexus-proxy.
COPY --from=builder --chown=65532:65532 /out/state /var/lib/nexus-proxy
EXPOSE 3128
ENTRYPOINT ["/usr/local/bin/compliance-proxy"]
CMD ["--config", "/etc/nexus/compliance-proxy.yaml"]
