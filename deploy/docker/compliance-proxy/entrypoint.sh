#!/bin/sh
# Nexus Compliance Proxy container entrypoint — renders the config template,
# ensures a MITM CA exists, then execs the proxy.
set -eu

: "${NEXUS_REDIS_ADDR:=valkey:6379}"
: "${NEXUS_NATS_URL:=nats://nats:4222}"
: "${NEXUS_HUB_URL:=http://nexus-hub:3060}"
: "${NEXUS_COMPLIANCE_PROXY_PUBLIC_URL:=http://localhost:3128}"
: "${NEXUS_LOG_LEVEL:=info}"
export NEXUS_REDIS_ADDR NEXUS_NATS_URL NEXUS_HUB_URL NEXUS_COMPLIANCE_PROXY_PUBLIC_URL NEXUS_LOG_LEVEL

envsubst '${NEXUS_REDIS_ADDR} ${NEXUS_NATS_URL} ${NEXUS_HUB_URL} ${NEXUS_COMPLIANCE_PROXY_PUBLIC_URL} ${NEXUS_LOG_LEVEL}' \
  < /opt/nexus/compliance-proxy.config.yaml.tpl > /etc/nexus/compliance-proxy.config.yaml

# The TLS-bump CA. Production deployments mount a real CA at
# /etc/compliance-proxy/ca.{crt,key}; for dev/demo we generate a self-signed
# one on first start so the container comes up without ceremony.
CA_CERT=/etc/compliance-proxy/ca.crt
CA_KEY=/etc/compliance-proxy/ca.key
if [ ! -f "$CA_CERT" ] || [ ! -f "$CA_KEY" ]; then
  echo "compliance-proxy: no CA found — generating a self-signed MITM CA (dev/demo only; mount a real CA for production)"
  # The cert issuer requires an EC private key (P-256) and a path-length-0
  # basic constraint so a leaked CA key cannot mint subordinate CAs.
  openssl ecparam -name prime256v1 -genkey -noout -out "$CA_KEY"
  openssl req -x509 -new -key "$CA_KEY" -days 825 -out "$CA_CERT" \
    -subj "/CN=Nexus Compliance Proxy CA/O=Nexus Gateway (dev)" \
    -addext "basicConstraints=critical,CA:TRUE,pathlen:0"
  chmod 0600 "$CA_KEY"
fi

exec /usr/local/bin/nexus-compliance-proxy -config /etc/nexus/compliance-proxy.config.yaml
