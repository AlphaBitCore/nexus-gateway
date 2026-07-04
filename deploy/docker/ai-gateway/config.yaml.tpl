# Nexus AI Gateway — container-shape config template.
# Rendered by the image entrypoint via envsubst; service addresses default to
# the docker-compose / Helm service names. Secrets are NOT templated here —
# they stay blank and are loaded from the environment by the config loader
# (DATABASE_URL, REDIS_PASSWORD, ADMIN_KEY_HMAC_SECRET,
# CREDENTIAL_ENCRYPTION_KEY, INTERNAL_SERVICE_TOKEN).

# Externally-reachable base URL clients use to reach this gateway (reported to
# the Thing Registry as staticInfo; the admin UI renders it). Required.
publicURL: "${NEXUS_AI_GATEWAY_PUBLIC_URL}"

server:
  port: 3050
  readTimeout: "30s"
  writeTimeout: "360s"

database:
  url: ""                      # env DATABASE_URL

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

auth:
  hmacSecret: ""               # env ADMIN_KEY_HMAC_SECRET
  credentialMasterKey: ""      # env CREDENTIAL_ENCRYPTION_KEY (64 hex chars)
  credentialKeyMap: ""
  internalServiceToken: ""     # env INTERNAL_SERVICE_TOKEN

log:
  level: "${NEXUS_LOG_LEVEL}"
  format: "json"
  file: ""                     # empty = stdout (container-native logging)

registry:
  nexusHubUrl: "${NEXUS_HUB_URL}"

mq:
  driver: "nats"
  nats:
    url: "${NEXUS_NATS_URL}"

cors:
  enabled: false
  allowedOrigins: []
  allowedMethods: ["GET", "POST", "OPTIONS"]
  allowedHeaders: ["Content-Type", "Authorization", "x-nexus-virtual-key", "x-request-id"]
  maxAgeSec: 600

cache:
  enabled: true
  ttl: 5m
  prefix: "ai-gw:"
  broker: true

otel:
  endpoint: ""
  serviceName: "nexus-ai-gateway"

observability:
  latencyDetail: true

routing:
  defaultRetryPolicy:
    maxAttemptsPerTarget: 1
    retryOn: ["network", "timeout", "429", "5xx"]
    backoffInitial: 250ms
    backoffMax: 5s
    backoffJitter: 0.2
