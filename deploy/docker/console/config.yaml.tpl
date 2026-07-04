# Nexus Control Plane — container-shape config template (console image).
# Rendered by the console entrypoint via envsubst; service addresses default
# to the docker-compose / Helm service names. Secrets stay blank and load
# from env (DATABASE_URL, REDIS_PASSWORD, INTERNAL_SERVICE_TOKEN,
# COMPLIANCE_PROXY_API_TOKEN, CREDENTIAL_ENCRYPTION_KEY, AUTH_SERVER_ISSUER).

server:
  port: 3001
  shutdownTimeout: "10s"

database:
  url: ""                      # env DATABASE_URL
  maxConns: 25
  minConns: 5
  maxConnLifetime: "300s"

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

log:
  level: "${NEXUS_LOG_LEVEL}"
  format: "json"
  file: ""                     # empty = stdout (container-native logging)

bff:
  complianceProxyUrl: "${NEXUS_COMPLIANCE_PROXY_URL}"
  aiGatewayUrl: "${NEXUS_AI_GATEWAY_URL}"
  complianceProxyRuntimeUrl: "${NEXUS_COMPLIANCE_PROXY_URL}"
  complianceProxyApiToken: ""  # env COMPLIANCE_PROXY_API_TOKEN

registry:
  nexusHubUrl: "${NEXUS_HUB_URL}"

auth:
  internalServiceToken: ""     # env INTERNAL_SERVICE_TOKEN

crypto:
  encryptionKey: ""            # env CREDENTIAL_ENCRYPTION_KEY (64 hex chars)
  encryptionPassphrase: ""
  encryptionSalt: ""
  credentialKeyMap: ""
  production: true

retention:
  auditLogDays: 90
  adminAuditLogDays: 365
  metricRollupDays: 365
  agentAuditDays: 90

agent:
  caDir: "/var/lib/nexus/agentca"

otel:
  endpoint: ""
  serviceName: "nexus-control-plane"

scheduler:
  enabled: true

mq:
  driver: "nats"
  nats:
    url: "${NEXUS_NATS_URL}"

# OIDC issuer for tokens minted by this Control Plane. Must match the URL
# clients reach the console on; blank here so the AUTH_SERVER_ISSUER env
# override hook fires (L3 > L2 per configuration-architecture.md).
authServer:
  issuer: ""                   # env AUTH_SERVER_ISSUER
  keystoreDir: "/var/lib/nexus/authkeys"
