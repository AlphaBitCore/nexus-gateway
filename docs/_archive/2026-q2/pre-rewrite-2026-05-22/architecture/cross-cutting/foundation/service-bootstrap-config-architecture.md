---
doc: service-bootstrap-config-architecture
area: cross-cutting
service: foundation
tier: 1
updated: 2026-05-20
---

# Service Bootstrap Config Architecture

> **Tier 2 architecture doc.** Read when touching `packages/<service>/<service>.dev.yaml` files, the config-loading pipeline, or any env-var override path. **Bootstrap config** is what each service needs to START — DB connections, listen ports, Hub URL, logging. **Runtime config** is everything that flows through Thing shadow afterwards. Don't conflate them.

---

## 1. Bootstrap vs runtime

| | Bootstrap | Runtime |
|---|---|---|
| Where | `<service>.dev.yaml` + env-var overrides | Hub shadow (Cat A/B/C keys) |
| When | Read at process start | Pulled on boot + on change-signal |
| Examples | DB URL, Redis URL, Hub URL, listen port, log level, mTLS cert paths | Hook config, routing rules, kill switch, agent settings |
| Reload? | Not without restart | Hot-swap via `atomic.Pointer` |
| Editable in admin UI? | No (file / env only) | Yes |

The two layers are deliberate. Bootstrap is the **minimum the process needs to talk to Hub**. Runtime is **everything else**, owned by Hub, governed by the Thing model.

## 2. The `*.dev.yaml` files

Each service has one:

- `packages/nexus-hub/nexus-hub.dev.yaml`
- `packages/control-plane/control-plane.dev.yaml`
- `packages/ai-gateway/ai-gateway.dev.yaml`
- `packages/compliance-proxy/compliance-proxy.dev.yaml`
- `packages/agent/agent.dev.yaml`

In production, these are replaced with prod-tuned equivalents (deploy time substitution). The dev versions are checked in; the prod ones live in the deployment system.

## 3. Common bootstrap shape (representative — actual files vary per service)

Field names follow `configuration-architecture.md` §6: yaml uses camelCase; `log:` (not `logging:`); `database.maxConns/minConns/maxConnLifetime` (pgxpool naming, not `database/sql`).

```yaml
server:
  port: 3001
  readTimeout: 30s
  writeTimeout: 30s
  shutdownTimeout: 10s

log:
  level: "info"     # debug | info | warn | error
  format: "json"    # json | text
  file: "logs/control-plane.log"
  stackOnError: false

database:
  url: ""                     # env: DATABASE_URL (shared infra, unprefixed)
  maxConns: 50                # pgxpool
  minConns: 10
  maxConnLifetime: 30m
  maxConnIdleTime: 5m
  healthCheckPeriod: 1m

redis:
  mode: standalone            # standalone | sentinel | cluster (see config-arch §9)
  addrs: ["localhost:6379"]
  db: 0
  poolSize: 10

mq:
  driver: nats
  url: "nats://localhost:4222"   # env: NATS_URL

registry:                       # service registers as a Thing on boot
  nexusHubUrl: "http://localhost:3060"

telemetry:
  otelEndpoint: ""               # OTLP collector URL; empty = disabled
  otelSampleRatio: 0.1
```

## 4. Env-var overrides

Every bootstrap field has an env-var override. Convention per `configuration-architecture.md` §6 — service-private knobs carry the service prefix (`NEXUS_HUB_PORT`); shared-infrastructure URLs and `[MUST MATCH]` cross-service tokens are bare (no service prefix).

```bash
# Service-private (prefixed)
NEXUS_HUB_PORT=3060
NEXUS_HUB_ID=hub-prod-01
NEXUS_HUB_ADVERTISE_ADDR=hub.internal:3060

# Shared infrastructure (unprefixed — same env name across every service)
DATABASE_URL="postgres://..."
REDIS_ADDRS="localhost:6379"           # universal Redis schema, see config-arch §9
NATS_URL="nats://localhost:4222"
NEXUS_HUB_URL="ws://localhost:3060"    # entity-shaped: "the Hub URL", same across CP / AI-GW / Proxy

# [MUST MATCH] cross-service secrets / IDs (unprefixed)
INTERNAL_SERVICE_TOKEN=...
AUTH_SERVER_ISSUER=https://auth.example.com
CREDENTIAL_ENCRYPTION_KEY=...

# Per-service common ones
LOG_LEVEL=info
LOG_FORMAT=json
```

Env vars > YAML. The config loader merges in that order.

## 5. Why bootstrap is file-based (not in Hub)

Chicken-and-egg: the service needs to **find** Hub to receive runtime config. Hub's URL has to come from somewhere prior. We chose: file + env. Always available, simple to override, no network dependency.

Once the service is up, it registers with Hub as a Thing, then pulls runtime config. From that point forward, configuration flows through shadow.

## 6. Bootstrap-secret hygiene

Server services (Hub, CP, AI Gateway, Compliance Proxy) authenticate to each other via the `INTERNAL_SERVICE_TOKEN` env var — a shared `[MUST MATCH]` secret per `.env.example`. There is no per-process bootstrap-token-to-mTLS-cert flow today; the four server services are deployment-trusted peers and self-register as Things on boot.

The agent is the only enrollment-token-driven peer: a one-shot token is passed via the `nexus-agent enroll --token <…>` CLI (or SSO flow), which exchanges it with Hub for a device certificate. After enrollment the device cert (under `platform.DefaultPaths().CertDir`) replaces the token — `nexus-agent run` ignores any subsequently-passed enrollment token. See `agent-enrollment-architecture.md` for the full lifecycle.

## 7. What's NOT in bootstrap config

Anything reactive to admin policy:

- Hook configs.
- Routing rules.
- Provider credentials.
- Kill switch state.
- Agent settings.
- Alert rules.

These live in Hub shadow; the service pulls them after registration. Putting them in `*.dev.yaml` would mean admins can't edit them without redeploying — a major regression.

## 8. Reloading bootstrap

By design: not supported without restart.

If an operator changes a `*.dev.yaml` value (e.g., raises `log.level` to debug), the service must be restarted to pick it up. There are two reasons:

1. Some bootstrap fields (listen port, DB connection pool) can't change at runtime safely.
2. Treating bootstrap as immutable-per-process simplifies the mental model — "if it's in the yaml, it's frozen at boot".

The diag mode subsystem (`diag-event-triage-architecture.md` §2) offers runtime DEBUG-level logging as a Hub-managed shadow key, so operators don't usually need to edit yaml to enable debug logs.

## 9. Validation

Each service validates its bootstrap config at startup:

- Required fields present.
- URLs parseable.
- File paths readable (TLS certs).
- Ports in-range.

A failed validation exits with a non-zero code and a clear error message. The service does **not** retry — assume the config is broken and surface to the operator.

## 10. Sources

- `packages/<service>/<service>.dev.yaml` — per-service bootstrap.
- `packages/<service>/internal/config/` — loader + validator.
- `packages/shared/core/logging/` — log config integration.
- `tests/.env.local` — env-var examples for local dev.

## 11. Cross-references

- `thing-config-sync-architecture.md` — runtime config flow (what bootstrap **isn't**).
- `agent-enrollment-architecture.md` — service-side bootstrap-token usage.
- `shared-package-architecture.md` — where common config primitives live.
- CLAUDE.md "Service log files" — log path conventions match bootstrap defaults.
