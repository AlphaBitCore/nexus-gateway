# Nexus Hub — container-shape config template.
# Rendered by the image entrypoint via envsubst; service addresses default to
# the docker-compose / Helm service names. Secrets stay blank and load from
# env (DATABASE_URL, REDIS_PASSWORD, INTERNAL_SERVICE_TOKEN).

server:
  port: 3060
  readTimeout: 30s
  writeTimeout: 30s
  shutdownTimeout: 15s

database:
  url: ""                      # env DATABASE_URL
  maxConns: 20
  minConns: 5

redis:
  mode: standalone
  addrs: ["${NEXUS_REDIS_ADDR}"]
  username: ""
  password: ""                 # env REDIS_PASSWORD
  db: 0
  sentinel:
    masterName: ""
    username: ""
    password: ""
  cluster:
    maxRedirects: 8
    routeRandomly: false
    readOnly: false
  tls:
    enabled: false
    insecureSkipVerify: false
    caFile: ""
    certFile: ""
    keyFile: ""
    serverName: ""
  poolSize: 200
  minIdleConns: 50
  maxRetries: 3
  dialTimeout: 5s
  readTimeout: 3s
  writeTimeout: 3s
  poolTimeout: 4s

mq:
  driver: "nats"
  nats:
    url: "${NEXUS_NATS_URL}"

consumers:
  enabled: true
  batchSize: 100
  flushInterval: 5s
  siem:
    enabled: false
    url: ""
    headers: {}
    format: "json"
    batchSize: 200
    flushInterval: 5s
    eventTypes: []

scheduler:
  enabled: true
  driftCheckInterval: 60s
  identityEnrichInterval: 5m
  enableAgentRollup: false

auth:
  internalServiceToken: ""     # env INTERNAL_SERVICE_TOKEN

# The auth server lives inside the console image's control-plane (:3001).
authServer:
  url: "${NEXUS_CONSOLE_INTERNAL_URL}"
  jwksURL: "${NEXUS_CONSOLE_INTERNAL_URL}/.well-known/jwks.json"
  issuer: "${NEXUS_AUTH_ISSUER}"

agentCA:
  certFile: ""
  keyFile: ""
  dir: "/var/lib/nexus/agentca"

otel:
  enabled: false
  endpoint: ""

log:
  level: "${NEXUS_LOG_LEVEL}"
  format: "json"
  file: ""                     # empty = stdout (container-native logging)

hub:
  id: "${NEXUS_HUB_ID}"
  advertiseAddr: "${NEXUS_HUB_ADVERTISE_ADDR}"
  allowedOrigins: []
