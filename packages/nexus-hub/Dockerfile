# syntax=docker/dockerfile:1
# Nexus Hub — production container image. Pure Go (no cgo, no vectorscan).
# Build context: repository root.
#   docker build -f packages/nexus-hub/Dockerfile -t alphabitcore/nexus-hub:test .

FROM golang:1.25-alpine AS builder
WORKDIR /app

# GOWORK=off: the image carries only this module + its sibling `replace`
# target (../shared), not the full go.work module set.
COPY packages/shared/ packages/shared/
COPY packages/nexus-hub/ packages/nexus-hub/

WORKDIR /app/packages/nexus-hub
ARG BUILD_VERSION=dev
RUN GOWORK=off CGO_ENABLED=0 go build -trimpath \
    -ldflags="-s -w -X main.buildVersion=${BUILD_VERSION}" \
    -o /nexus-hub ./cmd/nexus-hub/

FROM alpine:3.21
RUN apk add --no-cache ca-certificates gettext curl && \
    adduser -D -u 65532 nexus && \
    mkdir -p /etc/nexus /var/lib/nexus/agentca && \
    chown -R nexus:nexus /etc/nexus /var/lib/nexus
COPY --from=builder /nexus-hub /usr/local/bin/nexus-hub
COPY deploy/docker/hub/config.yaml.tpl /opt/nexus/nexus-hub.config.yaml.tpl
COPY deploy/docker/hub/entrypoint.sh /usr/local/bin/nexus-entrypoint.sh
RUN chmod 0755 /usr/local/bin/nexus-entrypoint.sh
USER nexus
EXPOSE 3060
ENTRYPOINT ["nexus-entrypoint.sh"]
