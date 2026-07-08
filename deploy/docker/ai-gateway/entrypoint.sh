#!/bin/sh
# Nexus AI Gateway container entrypoint — renders the config template with the
# service addresses for this deployment, then execs the gateway.
set -eu

: "${NEXUS_REDIS_ADDR:=valkey:6379}"
: "${NEXUS_NATS_URL:=nats://nats:4222}"
: "${NEXUS_HUB_URL:=http://nexus-hub:3060}"
: "${NEXUS_AI_GATEWAY_PUBLIC_URL:=http://localhost:3050}"
: "${NEXUS_LOG_LEVEL:=info}"
export NEXUS_REDIS_ADDR NEXUS_NATS_URL NEXUS_HUB_URL NEXUS_AI_GATEWAY_PUBLIC_URL NEXUS_LOG_LEVEL

envsubst '${NEXUS_REDIS_ADDR} ${NEXUS_NATS_URL} ${NEXUS_HUB_URL} ${NEXUS_AI_GATEWAY_PUBLIC_URL} ${NEXUS_LOG_LEVEL}' \
  < /opt/nexus/ai-gateway.config.yaml.tpl > /etc/nexus/ai-gateway.config.yaml

exec /usr/local/bin/ai-gateway -config /etc/nexus/ai-gateway.config.yaml
