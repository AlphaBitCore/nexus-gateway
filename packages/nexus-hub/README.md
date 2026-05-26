# Nexus Hub

The control-plane "kernel" — every other service registers with the Hub
as a **Thing** and synchronises configuration through the
**Device Shadow** mechanism. Hub owns: Thing Registry, Device Shadow,
scheduled jobs (rollups, retention, audit), the metrics pipeline, the
agent CA (mTLS issuance), and the SIEM bridge.

## Where it sits

| | |
|---|---|
| **Port** | `3060` (HTTP); WebSocket upgrade on `/ws/thing` for Things to subscribe to shadow change events |
| **DB** | PostgreSQL (`thing`, `thing_config_template`, `metric_rollup_5m`, audit/traffic tables, …) |
| **Cache** | Redis (desired-state, rate-limit, IAM) |
| **MQ** | NATS JetStream via `shared/mq` (audit event streams) |
| **Upstream of** | Control Plane (admin write path), AI Gateway, Compliance Proxy, Agent (all four register as Things) |

## Build

```bash
make nexus-hub-build       # outputs to dist/bin/nexus-hub/nexus-hub
# or
cd packages/nexus-hub && go run ./cmd/nexus-hub/ -config nexus-hub.dev.yaml
```

## Test

```bash
make nexus-hub-test        # go test -race -count=1 ./...
```

## Key directories

| Path | Purpose |
|---|---|
| `cmd/nexus-hub/` | Process entry point. Reads YAML config + env, wires every subsystem. |
| `internal/handler/` | HTTP routes (Thing registration, shadow get/set, admin internal APIs). |
| `internal/thingmgr/` | Thing Registry + Device Shadow state machine + break-glass reconcile. |
| `internal/jobs/` | Scheduler + job catalogue (rollups, retention, audit drain, alerteval). |
| `internal/metrics/` | Prometheus registry, rollup writer. |
| `internal/agentca/` | Agent enrollment + mTLS cert issuance (X.509). |
| `internal/siem/` | Optional one-way SIEM event bridge (Splunk HEC / generic webhook). |

## Configuration

- `nexus-hub.dev.yaml` — local boot defaults (ports, log level, feature flags).
- `nexus-hub.prod.yaml.example` — production-shape template.
- Secrets read from env per the "Secrets are env-only" binding
  (`.env.example`): `INTERNAL_SERVICE_TOKEN`, DB / Redis / NATS URLs,
  agent-CA passphrase, etc.

## Architecture references

- `docs/developers/architecture/cross-cutting/foundation/thing-model.md` — the Thing + Shadow contract every Hub
  consumer must respect.
- `docs/developers/architecture/cross-cutting/foundation/service-call-framework.md` — how Things discover Hub and
  what "desired vs reported" actually means at the wire level.
- `docs/developers/architecture/services/hub/nexus-hub-internals-architecture.md` — module-level breakdown
  of the Hub itself.
