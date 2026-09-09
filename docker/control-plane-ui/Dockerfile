# Control Plane UI — Vite build served by nginx.
#
# Build context is the repo root: the UI depends on the packages/ui-shared
# workspace, so a context scoped to packages/control-plane-ui would not resolve.
# --platform=$BUILDPLATFORM: 在构建机原生架构上跑 npm/vite(产物 dist 是架构无关的
# 静态 JS/HTML),避免为目标 amd64 跨构建时用 QEMU 模拟跑 Node 导致极慢。最终 nginx
# 阶段仍按 --platform linux/amd64 出镜像(见 build 命令),供 Fargate X86_64 拉取。
FROM --platform=$BUILDPLATFORM node:22-bookworm-slim AS builder
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
# nexus-ui.conf 用 ${NEXUS_*_UPSTREAM} 占位上游主机名。放进官方 envsubst 模板目录,
# nginx 入口(/docker-entrypoint.d/20-envsubst-on-templates.sh)在启动时按环境变量
# 渲染成 /etc/nginx/conf.d/default.conf。下面三个 ENV 是默认值 = compose 服务名,
# 与旧版行为一致;Prime ECS 的 task-def 覆盖为 CloudMap SD 的 FQDN(<svc>.prime.local)。
# NEXUS_RESOLVER:nexus-ui.conf 变量式 proxy_pass 的动态 DNS 解析器。默认 Docker 内嵌 DNS
#   127.0.0.11(compose 可用);Prime ECS 的 task-def 覆盖为 169.254.169.253(AmazonProvidedDNS)。
ENV NEXUS_CP_UPSTREAM=control-plane \
    NEXUS_GW_UPSTREAM=ai-gateway \
    NEXUS_HUB_UPSTREAM=nexus-hub \
    NEXUS_RESOLVER=127.0.0.11
COPY deploy/nginx/nexus-ui.conf /etc/nginx/templates/default.conf.template
EXPOSE 8080
