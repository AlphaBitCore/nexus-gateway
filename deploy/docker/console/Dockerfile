# syntax=docker/dockerfile:1
# Nexus Console — the admin console in one image: nginx (TLS, SPA, reverse
# proxy) + the control-plane binary (admin API + auth server on loopback
# :3001... exposed on :3001 for the Hub's authServer/JWKS calls) + the
# Vite-built control-plane-UI dist.
#
# Replaces the separate control-plane / control-plane-ui / nginx containers:
# one light image, one thing to version. Pure Go + static assets — no cgo.
# Build context: repository root.
#   docker build -f deploy/docker/console/Dockerfile -t alphabitcore/nexus-console:test .

FROM golang:1.25-alpine AS cp-builder
WORKDIR /app
# GOWORK=off: the image carries only this module + its sibling `replace`
# targets (../shared, ../nexus-agent-core), not the full go.work module set.
COPY packages/shared/ packages/shared/
COPY packages/nexus-agent-core/ packages/nexus-agent-core/
COPY packages/control-plane/ packages/control-plane/
WORKDIR /app/packages/control-plane
RUN GOWORK=off CGO_ENABLED=0 go build -trimpath -ldflags="-s -w" -o /control-plane ./cmd/control-plane/

FROM node:20-alpine AS ui-builder
WORKDIR /app
COPY package.json package-lock.json ./
# ui-shared is a workspace dependency of control-plane-ui — both manifests
# must exist before npm ci or the workspace resolve fails.
COPY packages/ui-shared/package.json packages/ui-shared/
COPY packages/control-plane-ui/package.json packages/control-plane-ui/
# --ignore-scripts: skip the root `prepare` (git-hooks setup) lifecycle — it is
# a dev convenience that needs scripts/ + a git checkout, neither present here.
RUN npm ci --ignore-scripts --workspace=packages/control-plane-ui --workspace=packages/ui-shared
COPY packages/ui-shared packages/ui-shared
COPY packages/control-plane-ui packages/control-plane-ui
# The UI build's `sync-locales` prestep runs ../../scripts/sync-locales.mjs.
COPY scripts/ scripts/
RUN cd packages/control-plane-ui && npm run build

FROM nginx:1.27-alpine
RUN apk add --no-cache ca-certificates openssl && \
    mkdir -p /etc/nexus /opt/nexus /var/lib/nexus/agentca /var/lib/nexus/authkeys && \
    rm -f /etc/nginx/conf.d/default.conf
COPY --from=cp-builder /control-plane /usr/local/bin/control-plane
COPY --from=ui-builder /app/packages/control-plane-ui/dist /opt/nexus/ui
COPY deploy/docker/console/nginx.conf.tpl /opt/nexus/nginx.conf.tpl
COPY deploy/docker/console/config.yaml.tpl /opt/nexus/control-plane.config.yaml.tpl
COPY deploy/docker/console/entrypoint.sh /usr/local/bin/console-entrypoint.sh
RUN chmod 0755 /usr/local/bin/console-entrypoint.sh

# 80 http→https redirect · 443 console UI/API · 3001 control-plane direct
# (internal network only — the Hub validates tokens against its JWKS).
EXPOSE 80 443 3001
ENTRYPOINT ["console-entrypoint.sh"]
