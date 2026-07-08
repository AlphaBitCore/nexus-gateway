#!/bin/sh
# Nexus Console container entrypoint — renders nginx + control-plane configs,
# ensures TLS material exists, then supervises both processes: if either
# exits, the container exits and the orchestrator restarts it.
set -eu

: "${NEXUS_AI_GATEWAY_UPSTREAM:=http://nexus-ai-gateway:3050}"
: "${NEXUS_HUB_UPSTREAM:=http://nexus-hub:3060}"
: "${NEXUS_AI_GATEWAY_URL:=http://nexus-ai-gateway:3050}"
: "${NEXUS_COMPLIANCE_PROXY_URL:=http://nexus-compliance-proxy:3040}"
: "${NEXUS_HUB_URL:=http://nexus-hub:3060}"
: "${NEXUS_REDIS_ADDR:=valkey:6379}"
: "${NEXUS_NATS_URL:=nats://nats:4222}"
: "${NEXUS_LOG_LEVEL:=info}"
# The control-plane reads CONTROL_PLANE_PUBLIC_URL from the environment
# directly (required config); default it so a bare `docker run` still boots.
: "${CONTROL_PLANE_PUBLIC_URL:=https://localhost}"
export NEXUS_AI_GATEWAY_UPSTREAM NEXUS_HUB_UPSTREAM NEXUS_AI_GATEWAY_URL \
  NEXUS_COMPLIANCE_PROXY_URL NEXUS_HUB_URL NEXUS_REDIS_ADDR NEXUS_NATS_URL \
  NEXUS_LOG_LEVEL CONTROL_PLANE_PUBLIC_URL

# Render configs. The nginx template only substitutes the two upstream vars —
# everything else ($host, $remote_addr, ...) is nginx syntax and must survive.
envsubst '${NEXUS_AI_GATEWAY_UPSTREAM} ${NEXUS_HUB_UPSTREAM}' \
  < /opt/nexus/nginx.conf.tpl > /etc/nginx/conf.d/nexus.conf
envsubst '${NEXUS_REDIS_ADDR} ${NEXUS_NATS_URL} ${NEXUS_HUB_URL} ${NEXUS_AI_GATEWAY_URL} ${NEXUS_COMPLIANCE_PROXY_URL} ${NEXUS_LOG_LEVEL}' \
  < /opt/nexus/control-plane.config.yaml.tpl > /etc/nexus/control-plane.config.yaml

# TLS: mount a real cert/key at /etc/nexus/tls.{crt,key} for production;
# otherwise generate a self-signed pair so the console comes up for dev/demo.
if [ ! -f /etc/nexus/tls.crt ] || [ ! -f /etc/nexus/tls.key ]; then
  echo "console: no TLS material found — generating self-signed cert (dev/demo only)"
  openssl req -x509 -newkey rsa:2048 -nodes -days 825 \
    -keyout /etc/nexus/tls.key -out /etc/nexus/tls.crt \
    -subj "/CN=nexus-console/O=Nexus Gateway (dev)"
  chmod 0600 /etc/nexus/tls.key
fi

/usr/local/bin/control-plane -config /etc/nexus/control-plane.config.yaml &
CP_PID=$!

# Wait (up to 60s) for the control-plane to serve /healthz before nginx
# starts proxying to it.
i=0
until wget -q -O /dev/null http://127.0.0.1:3001/healthz 2>/dev/null; do
  if ! kill -0 "$CP_PID" 2>/dev/null; then
    echo "console: control-plane exited during startup" >&2
    exit 1
  fi
  i=$((i + 1))
  if [ "$i" -gt 60 ]; then
    echo "console: control-plane not ready after 60s" >&2
    kill -TERM "$CP_PID" 2>/dev/null || true
    exit 1
  fi
  sleep 1
done
echo "console: control-plane ready"

nginx -g 'daemon off;' &
NGINX_PID=$!

shutdown() {
  kill -TERM "$CP_PID" "$NGINX_PID" 2>/dev/null || true
}
trap shutdown TERM INT

# Fail-fast supervision: exit when either process dies so the orchestrator
# restarts the whole console atomically.
while kill -0 "$CP_PID" 2>/dev/null && kill -0 "$NGINX_PID" 2>/dev/null; do
  sleep 2
done
shutdown
wait "$CP_PID" 2>/dev/null || true
wait "$NGINX_PID" 2>/dev/null || true
echo "console: a supervised process exited — stopping container" >&2
exit 1
