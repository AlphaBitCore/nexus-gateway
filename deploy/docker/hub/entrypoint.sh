#!/bin/sh
# Nexus Hub container entrypoint — renders the config template with the
# service addresses for this deployment, then execs the hub.
set -eu

: "${NEXUS_REDIS_ADDR:=valkey:6379}"
: "${NEXUS_NATS_URL:=nats://nats:4222}"
: "${NEXUS_CONSOLE_INTERNAL_URL:=http://nexus-console:3001}"
: "${NEXUS_AUTH_ISSUER:=${NEXUS_CONSOLE_INTERNAL_URL}}"
: "${NEXUS_HUB_ID:=hub-docker-1}"
: "${NEXUS_HUB_ADVERTISE_ADDR:=http://nexus-hub:3060}"
: "${NEXUS_LOG_LEVEL:=info}"
export NEXUS_REDIS_ADDR NEXUS_NATS_URL NEXUS_CONSOLE_INTERNAL_URL \
  NEXUS_AUTH_ISSUER NEXUS_HUB_ID NEXUS_HUB_ADVERTISE_ADDR NEXUS_LOG_LEVEL

envsubst '${NEXUS_REDIS_ADDR} ${NEXUS_NATS_URL} ${NEXUS_CONSOLE_INTERNAL_URL} ${NEXUS_AUTH_ISSUER} ${NEXUS_HUB_ID} ${NEXUS_HUB_ADVERTISE_ADDR} ${NEXUS_LOG_LEVEL}' \
  < /opt/nexus/nexus-hub.config.yaml.tpl > /etc/nexus/nexus-hub.config.yaml

exec /usr/local/bin/nexus-hub --config /etc/nexus/nexus-hub.config.yaml
