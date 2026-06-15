# Linux Nexus Agent image for CI daemon-bound e2e tests.
#
# Depends only on the sibling packages/shared module (its single go.mod
# replace). Build context is the repo root so both modules are visible:
#   docker build -f packages/agent/Dockerfile .
#
# CGO_ENABLED=1 is REQUIRED: the audit queue uses go-sqlcipher, which has no
# non-cgo driver — a CGO_ENABLED=0 binary builds but errors at `run` when it
# opens the encrypted audit DB. (enroll alone would survive without cgo, but
# the daemon / interception E2E does not.) The golang build image ships gcc;
# the debian-slim runtime ships the matching glibc.
#
# The runtime image carries iptables/nftables because the Linux agent intercepts
# via netfilter redirect (see agent-linux-platform-architecture.md); `run`
# therefore needs NET_ADMIN. `enroll` only needs HTTP to the Hub.
FROM golang:1.25-bookworm AS build
WORKDIR /src
COPY packages/shared/ packages/shared/
COPY packages/agent/ packages/agent/
WORKDIR /src/packages/agent
# GOWORK=off so the build uses this module's `replace => ../shared`, not the
# repo go.work (which references modules we did not copy).
RUN GOWORK=off CGO_ENABLED=1 go build -trimpath -o /out/nexus-agent ./cmd/agent

FROM debian:bookworm-slim
RUN apt-get update \
 && apt-get install -y --no-install-recommends iptables ca-certificates curl \
 && rm -rf /var/lib/apt/lists/*
COPY --from=build /out/nexus-agent /usr/local/bin/nexus-agent
COPY packages/agent/agent.dev.yaml /etc/nexus-agent/agent.yaml
# Config + cert live under /etc/nexus-agent (mountable / overridable in CI).
ENV NEXUS_AGENT_CONFIG=/etc/nexus-agent/agent.yaml
WORKDIR /etc/nexus-agent
ENTRYPOINT ["/usr/local/bin/nexus-agent"]
CMD ["version"]
