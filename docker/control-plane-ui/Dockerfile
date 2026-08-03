# Control Plane UI — Vite build served by nginx.
#
# Build context is the repo root: the UI depends on the packages/ui-shared
# workspace, so a context scoped to packages/control-plane-ui would not resolve.
FROM node:22-bookworm-slim AS builder
WORKDIR /src
COPY package.json package-lock.json ./
COPY packages/ packages/
# --ignore-scripts: the root "prepare" script (scripts/setup-git-hooks.mjs)
# is not in this build context (the repo's scripts/ directory holds dev-only
# tooling, not a runtime dependency) — without this flag `npm ci` fails with
# MODULE_NOT_FOUND. Git hooks are a local-checkout convenience and have no
# bearing on the built image.
RUN npm ci --no-audit --no-fund --ignore-scripts
WORKDIR /src/packages/control-plane-ui
RUN npx tsc --noEmit && npx vite build

FROM nginx:1.27-alpine
LABEL org.opencontainers.image.title="control-plane-ui" \
      org.opencontainers.image.source="https://github.com/AlphaBitCore/nexus-gateway" \
      org.opencontainers.image.licenses="Apache-2.0"
COPY --from=builder /src/packages/control-plane-ui/dist /usr/share/nginx/html
COPY deploy/nginx/nexus-ui.conf /etc/nginx/conf.d/default.conf
EXPOSE 8080
