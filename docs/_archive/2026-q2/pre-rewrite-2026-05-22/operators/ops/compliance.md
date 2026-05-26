# Compliance Operations Guide

## Overview

Nexus Gateway provides enterprise compliance for AI traffic through a hook-based pipeline that inspects, classifies, and enforces policies on requests and responses flowing to AI providers. This document covers the operational aspects of the compliance system.

---

## Compliance Hook Pipeline

### Architecture

The compliance pipeline runs inside the compliance proxy (transparent TLS proxy) and the AI gateway (explicit proxy). It executes a chain of hooks in priority order against each intercepted transaction.

```
Client Request
     |
     v
+--------------------+
| Policy Resolver    |  Determines which hooks apply based on:
| (policy.go)        |  - Hook enabled status
|                    |  - Stage (request / response / connection)
|                    |  - Applicable ingress type
|                    |  - Priority ordering
+--------------------+
     |
     v
+--------------------+
| Pipeline Executor  |  Runs hooks with:
| (pipeline.go)      |  - Per-hook timeout (default 5s)
|                    |  - Total pipeline timeout (default 15s)
|                    |  - Parallel or sequential execution
+--------------------+
     |
     v
+--------------------+
| Decision Merger    |  Aggregates results:
|                    |  - REJECT_HARD: immediate block (first wins)
|                    |  - REJECT_SOFT: block unless overridden
|                    |  - APPROVE: allow traffic
|                    |  - ABSTAIN: no opinion
|                    |  - MODIFY: content transformation (AI gateway only)
+--------------------+
     |
     v
  Audit Records (TrafficEvent + TrafficEventNormalized; admin actions land in AdminAuditLog)
```

### Hook Execution Modes

The shared `Pipeline` (`packages/shared/policy/pipeline/pipeline.go`) supports both modes via the `parallel` flag (`pipeline.go:122-135`). All three services default to **sequential** — `compliance.parallelHooks: false` per `packages/compliance-proxy/compliance-proxy.config.yaml:120` (comment: "aligned with ai-gateway and agent").

| Mode | When it runs | Where the flag lives | Behavior |
|------|-------------|----------------------|----------|
| Sequential (default — all 3 services) | always for AI Gateway + Agent; default for Compliance Proxy | `pipeline.go:178 executeSequential` | Hooks run in priority order; short-circuit on REJECT_HARD. |
| Parallel (opt-in, Compliance Proxy only) | when admin sets `compliance.parallelHooks: true` in the compliance-proxy yaml | `compliance-proxy/cmd/compliance-proxy/wiring/compliance.go:150` reads `cfg.Compliance.ParallelHooks` into `pipeline.Pipeline.Parallel` | All hooks run concurrently via `pipeline.go:147 executeParallel`; the goroutines observe the shared context but the executor does NOT cancel siblings on REJECT_HARD (`pipeline.go:208` comment — parallel pipelines intentionally let all hooks complete so the audit record is complete; the REJECT_HARD verdict still wins at aggregation time). |

### Fail Behavior

Each hook declares its fail behavior:

| Fail Behavior | On Hook Error/Timeout | Effect |
|--------------|----------------------|--------|
| `fail-open` | Hook returns APPROVE | Traffic continues (default) |
| `fail-closed` | Hook returns REJECT_HARD | Traffic blocked |

### Pipeline Configuration

The pipeline (timeouts, parallel-vs-sequential execution, streaming mode,
reject-response template, redaction rules) is configured via the
`HookConfig` table in Postgres and pushed through the Hub-shadow
WebSocket flow. There is no local `compliance-proxy.config.yaml`
section for pipeline knobs.

Flow:

1. Admin edits a hook configuration in the Control Plane UI (`/hooks`).
2. Control Plane writes the row to `HookConfig` and asks Nexus Hub to
   bump the relevant Thing's desired-config shadow.
3. Hub publishes the updated shadow via WebSocket to every connected
   compliance-proxy + AI Gateway Thing.
4. Each service's `thingclient.OnConfigChanged` callback applies the
   new hook config in-process without restart.

See `docs/developers/architecture/cross-cutting/foundation/configuration-architecture.md`
for the canonical 4-layer config model.

### Built-in Hook Implementations

All 11 hook implementations registered in
`packages/shared/policy/hooks/builtins/builtins.go:24-34`. Every
implementation is platform-agnostic Go and shared verbatim across
agent, compliance-proxy, and AI gateway (三端一致 policy).

| Hook ID | Purpose |
|---------|---------|
| `keyword-filter` | Pattern-based keyword blocking against a configured term list. |
| `pii-detector` | Built-in PII detection (email, phone, SSN, credit card). |
| `content-safety` | Content-safety classifier (toxic / unsafe / disallowed categories). |
| `rate-limiter` | Per-key / per-org / per-IP rate limiting via the shared limiter package. |
| `request-size-validator` | Reject requests whose body exceeds the configured byte budget. |
| `ip-access-filter` | Allow / deny by client IP CIDR list. |
| `data-residency` | Block requests that would cross a configured residency boundary. |
| `rulepack-engine` | Evaluate a versioned rule pack (the engine `RulePack` table feeds). |
| `noop` | No-op hook used for staging / dry-run wiring. |
| `webhook-forward` | Forward the decision context to an external webhook (audit / SIEM fan-out). |
| `quality-checker` | Output-quality / response-shape sanity checks. |

### Ingress Filtering

Hooks can be scoped to specific ingress types via `applicableIngress`:

| Value | Meaning |
|-------|---------|
| `ALL` | Applies to all ingress paths |
| `COMPLIANCE_PROXY` | Only compliance proxy traffic |
| `VK` | Only AI gateway (virtual key) traffic |
| `AGENT` | Only desktop agent traffic |

---

## Data Collection and Retention

### What is Collected

Each AI traffic request produces a `TrafficEvent` record (with optional `TrafficEventNormalized` companion for canonicalized request/response shape) containing:

| Field | Description | PII |
|-------|-------------|-----|
| `transactionId` | Per-request correlation ID | No |
| `connectionId` | Per-tunnel correlation ID | No |
| `trafficSource` | Ingress type (COMPLIANCE_PROXY, AGENT, DNS_TERMINATED) | No |
| `sourceIp` | Client IP address | Yes |
| `targetHost` | Destination AI provider hostname | No |
| `method` | HTTP method | No |
| `path` | Request path (e.g. `/v1/chat/completions`) | No |
| `statusCode` | HTTP response status | No |
| `latencyMs` | Total request latency | No |
| `hookDecision` | Final compliance decision (allow/reject/warn) | No |
| `hookReason` | Human-readable reason for the decision | No |
| `hookReasonCode` | Machine-readable reason code | No |
| `hooksPipeline` | Full hook execution trace (JSONB) | No |
| `dataClassification` | Content sensitivity level (PUBLIC/INTERNAL/CONFIDENTIAL/RESTRICTED) | No |
| `bumpStatus` | TLS interception outcome | No |
| `timestamp` | Event time | No |
| `subjectId` | Enterprise identity (nullable, Phase 2) | Yes |

### What is NOT Collected

- **Request/response body content** is NOT stored for allowed traffic (default OFF).
- **Authentication credentials** (API keys, Bearer tokens) are never persisted.
- Body content is stored only on hook reject, and only after redaction.

### Data Retention

Retention is enforced by the Nexus Hub `data-retention` scheduled job (runs every 24 hours by default; tunable via `NEXUS_HUB_SCHEDULER_DATA_RETENTION_INTERVAL`). Per-tier day counts are read from the Hub's `Scheduler.Retention` config block with the following env overrides:

| Data Type | Default Retention | Env Override |
|-----------|------------------|--------------|
| `traffic_event` rows | configured | `NEXUS_HUB_RETENTION_TRAFFIC_EVENT_DAYS` |
| `traffic_event` payload spill | configured | `NEXUS_HUB_RETENTION_TRAFFIC_EVENT_PAYLOAD_DAYS` |
| Admin audit logs (`AdminAuditLog`) | configured | `NEXUS_HUB_RETENTION_ADMIN_AUDIT_DAYS` |
| Metric rollups (legacy aggregate) | configured | `NEXUS_HUB_RETENTION_METRIC_ROLLUP_DAYS` |
| Agent audit events | configured | `NEXUS_HUB_RETENTION_AGENT_AUDIT_DAYS` |
| Rollup 5m / 1h / 1d / 1mo tiers | configured | `NEXUS_HUB_RETENTION_ROLLUP_{5M,1H,1D,1MO}_DAYS` |

The defaults ship in `packages/nexus-hub/internal/config/config.go`; production overrides live in the Hub service's `EnvironmentFile=` (systemd) or K8s Secret per the env-only contract.

---

## Redaction

### How Redaction Works

The `Redactor` (`packages/shared/compliance/redact.go`) applies regex-based pattern replacement to text content before persistence. Redaction runs on any body content that is stored (rejected request bodies).

### Built-in Redaction Rules

| Pattern | Replacement | Description |
|---------|-------------|-------------|
| Bearer tokens | `[REDACTED_BEARER_TOKEN]` | OAuth/API bearer tokens |
| Email addresses | `[REDACTED_EMAIL]` | Standard email format |
| Credit card numbers | `[REDACTED_CREDIT_CARD]` | Visa, MC, Amex, Discover |
| US phone numbers | `[REDACTED_PHONE]` | With separators to avoid false positives |
| US SSN | `[REDACTED_SSN]` | Format: XXX-XX-XXXX |

### Custom Redaction Rules

Provide a YAML file via `compliance.redactionRulesPath` in the compliance proxy config:

```yaml
- pattern: "\\b(PROJ-\\d{4,})\\b"
  replacement: "[REDACTED_PROJECT_ID]"
  description: "Internal project identifiers"
- pattern: "(?i)secret[_-]?key[=:]\\s*\\S+"
  replacement: "[REDACTED_SECRET_KEY]"
  description: "Secret key assignments"
```

---

## Audit Trail

### Storage Tiers

1. **PostgreSQL** (primary): Batch-inserted via async writer with configurable batch size and flush interval.
2. **NDJSON fallback** (secondary): When PostgreSQL is unreachable, events are written to local NDJSON (newline-delimited JSON) files. These are replayed to PostgreSQL upon recovery.

### NDJSON Fallback Configuration

```yaml
audit:
  ndjson:
    enabled: true
    dir: "/var/lib/nexus-proxy/audit-spool"
    maxFileSizeMB: 100     # Max size per file before rotation
    maxTotalSizeMB: 1000   # Max total spool directory size
```

### Adaptive Batch Tuning

The audit writer supports adaptive flush intervals and batch sizes for optimal throughput:

```yaml
audit:
  batch:
    adaptiveFlush: true          # Scale flush interval by queue depth
    flushIntervalMinMs: 500      # Flush fast when queue is full
    flushIntervalMaxMs: 10000    # Batch aggressively when queue is empty
    adaptiveBatchSize: true      # Scale batch size by queue depth
    batchSizeMin: 10             # Small batches at low load
    batchSizeMax: 500            # Large COPY batches at high load
```

### SIEM Integration

The compliance proxy supports forwarding audit events to external SIEM systems:

```yaml
siem:
  enabled: true
  sink: "http"                  # "file", "http", or "command"
  httpUrl: "https://splunk-hec.example.com/services/collector"
  httpHeaders:
    Authorization: "Splunk your-hec-token"
  batchSize: 100
  flushIntervalMs: 5000
  spoolDir: "/var/lib/nexus-proxy/siem-spool"
```

Sink types:
- `file`: Local NDJSON file (one event per line)
- `http`: HTTP POST to Splunk HEC, Datadog, Elastic, or generic webhook
- `command`: External command (e.g. `aws s3api put-object`)

The SIEM forwarder has its own queue and spool, so SIEM outages do not affect primary PostgreSQL audit writes.

---

## Cross-Border Data Flow

The compliance proxy is a **visibility and enforcement tool**. It provides inspection, policy enforcement, and audit evidence for AI traffic that may cross jurisdictional boundaries.

### What the Proxy Provides

- Per-request audit records showing what data was sent, to which provider, when
- Compliance decision records (allow/reject and reason)
- Data classification labels for transferred content
- Inspection coverage metrics

### What the Proxy Does NOT Do

- Determine whether a cross-border transfer is lawful
- Execute or manage Standard Contractual Clauses (SCCs)
- Perform PIPL security assessments
- Make legal determinations about transfer adequacy

These responsibilities belong to enterprise legal counsel. The proxy provides the technical evidence supporting compliance demonstration.

### Audit Data Localization

`TrafficEvent` rows (and their `TrafficEventNormalized` / `traffic_event_payload` companions) constitute personal data. The database must be located in a jurisdiction consistent with the data subjects' location and applicable data localization requirements. NDJSON fallback files are subject to the same localization requirements.

---

## Alerting

The compliance proxy includes a built-in alerting system:

```yaml
alerting:
  enabled: true
  evalIntervalSec: 30
  webhook:
    url: "https://hooks.slack.com/services/..."
    headers: {}
    timeoutSec: 10
  cooldown:
    fireMinutes: 5
    resolveMinutes: 5
  persistenceDir: "/var/lib/nexus-proxy/alerting"
```

When `persistenceDir` is set, alerting channel and threshold configurations survive process restart (written as JSON files under the directory with mode 0700). Without persistence, runtime changes revert to YAML defaults on restart.

The `persistenceBackend` field supports `"file"` (default) or `"redis"` for multi-instance deployments where all instances need to share alerting state.
