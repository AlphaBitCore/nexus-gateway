# Nexus Compliance Proxy — container-shape config template.
# Rendered by the image entrypoint via envsubst; service addresses default to
# the docker-compose / Helm service names. Secrets stay blank and load from
# env (DATABASE_URL, REDIS_PASSWORD, INTERNAL_SERVICE_TOKEN,
# COMPLIANCE_PROXY_API_TOKEN).

# Externally-reachable base URL for this proxy (reported to the Thing Registry
# as staticInfo). Required.
publicURL: "${NEXUS_COMPLIANCE_PROXY_PUBLIC_URL}"

listener:
  address: ":3128"

ca:
  certPath: "/etc/compliance-proxy/ca.crt"
  keyPath: "/etc/compliance-proxy/ca.key"

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

accessControl:
  sourceIpAllowlist:
    - "10.0.0.0/8"
    - "172.16.0.0/12"
    - "192.168.0.0/16"
  domainAllowlist:
    - "api.openai.com:443"
    - "*.openai.com:443"
    - "api.anthropic.com:443"
    - "*.anthropic.com:443"
    - "generativelanguage.googleapis.com:443"
    - "aistudio.google.com:443"
    - "api.deepseek.com:443"
    - "api.x.ai:443"
    - "api.moonshot.cn:443"
    - "open.bigmodel.cn:443"
    - "api.minimax.chat:443"
    - "copilot-proxy.githubusercontent.com:443"
  internalNetworkExceptions: []

connections:
  maxConcurrentTunnels: 10000
  maxStreamsPerConnection: 100
  idleTimeout: "300s"
  shutdownGracePeriod: "30s"

upstream:
  maxConnsPerHost: 100
  idleConnTimeout: "90s"
  dialTimeout: "10s"

limits:
  requestBodyLimit: "10MB"
  responseBodyLimit: "10MB"
  sseBufferLimit: "8MB"

log:
  level: "${NEXUS_LOG_LEVEL}"
  format: "json"
  file: ""                     # empty = stdout (container-native logging)

metrics:
  address: ":9090"

# Bound on all interfaces (unlike the AMI's loopback bind): the console's
# control-plane calls this runtime API cross-container. The port is only
# published on the internal compose/K8s network, never the host.
runtimeApi:
  listenAddress: ":3040"

mq:
  driver: "nats"
  nats:
    url: "${NEXUS_NATS_URL}"

registry:
  nexusHubUrl: "${NEXUS_HUB_URL}"

auth:
  internalServiceToken: ""     # env INTERNAL_SERVICE_TOKEN ([MUST MATCH] across services)

compliance:
  enabled: true
  perHookTimeoutMs: 5000
  totalTimeoutMs: 15000
  parallelHooks: false
  checkpointChars: 500
  redactionRulesPath: ""
  rejectResponse:
    defaultLevel: 1
    contactInfo: "Contact administrator"
  hooks:
    - implementationId: "keyword-filter"
      name: "Default Keyword Filter"
      priority: 10
      enabled: false
      stage: "request"
      failBehavior: "fail-open"
      timeoutMs: 5000
      applicableIngress: "ALL"
      config:
        patterns: []
        caseSensitive: false
    - implementationId: "pii-detector"
      name: "Default PII Detector"
      priority: 20
      enabled: false
      stage: "request"
      failBehavior: "fail-open"
      timeoutMs: 5000
      applicableIngress: "ALL"
      config:
        types: ["email", "phone", "ssn", "credit_card"]
        action: "reject_hard"

alerting:
  enabled: true
  evalIntervalSec: 30
  webhook:
    url: ""
    headers: {}
    timeoutSec: 10
  cooldown:
    fireMinutes: 5
    resolveMinutes: 5
  persistenceDir: "/var/lib/nexus/alerting"

audit:
  enabled: true
  batch:
    size: 10
    flushIntervalMs: 500
    channelBufferSize: 1000
    adaptiveFlush: false
    flushIntervalMinMs: 500
    flushIntervalMaxMs: 10000
    adaptiveBatchSize: false
    batchSizeMin: 10
    batchSizeMax: 500
  ndjson:
    enabled: true
    dir: "/var/lib/nexus/audit-spool"
    maxFileSizeMB: 100
    maxTotalSizeMB: 1000
  pinning:
    exemptions: []
    autoExempt:
      enabled: true
      failureThreshold: 3
      windowSeconds: 3600
      exemptionDurationSeconds: 86400
