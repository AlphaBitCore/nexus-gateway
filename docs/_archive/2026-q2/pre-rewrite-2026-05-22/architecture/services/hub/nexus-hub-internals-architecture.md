---
doc: nexus-hub-internals-architecture
area: service
service: hub
tier: 1
---

# `packages/nexus-hub/internal/` — Internal Subpackages Reference

> **Tier 3 architecture doc.** Reference card for nexus-hub subpackages that don't have their own dedicated architecture doc. Each is a single-paragraph description plus consumer notes.

This doc does **not** cover the larger Tier-1 / Tier-2 subpackages — `jobs/` is in `jobs-architecture.md`, `alerts/eval/` + `alerts/engine/` + `alerts/client/` in `alerting-architecture.md`, `traffic/siem/` in `siem-bridge-architecture.md`, `identity/agentca/` + `identity/enrollment/` in `agent-enrollment-architecture.md`, `traffic/ingest/` + `jobs/consumer/` (traffic-side) in `audit-pipeline-architecture.md`, `fleet/manager/` + `fleet/shadow/` in `thing-model.md` + `thing-config-sync-architecture.md`, `jobs/scheduler/` + `jobs/store/` + `jobs/defs/` in `jobs-architecture.md`, `jwks/` in `jwt-verifier-architecture.md`, `quota/store/` + `quota/rollup/` in `quota-architecture.md`, `observability/opsmetrics/` in `metrics-rollup-architecture.md`, `compliance/catbagent/` in `agent-enrollment-architecture.md`.

The top-level layout under `packages/nexus-hub/internal/` is:

```
alerts/{client,engine,eval}      compliance/catbagent
config                            fleet/{handler,manager,overrides,shadow,smartgroup,store}
handler                           identity/{agentca,enrollment,handler,store}
jobs/{consumer,defs,scheduler,store}
jwks                              observability/{handler,opsmetrics}
quota/{rollup,store}              self/{reg,shadow}
storage/{hubstore,store}          traffic/{chain,ingest,siem,store}
ws
```

## `storage/store/`

The Hub's shared **DB query layer**. Two `.go` files at this path today (`store.go` + `catb_loader.go`) plus their tests — but the broader `store/` design ("one file per logical query group") is split across feature subpackages: `fleet/store/`, `identity/store/`, `jobs/store/`, `quota/store/`, `traffic/store/`, plus the higher-level `storage/hubstore/`. The pattern is consistent — hand-written SQL, no sqlc (CLAUDE.md "Go" section is explicit on this), `pgxmock` for unit tests and a small subset of integration tests against a transient DB.

When adding a new query: pick the feature subpackage that already owns the table (`fleet/store/` for thing/shadow rows, `identity/store/` for IAM rows, `jobs/store/` for the job-queue rows, etc.) or create a new file there if the surface is genuinely new.

## `handler/`

The Hub admin API's **shared HTTP scaffolding**. The package holds `routes.go`, `middleware.go`, `errors.go`, and a single `handler_test.go`. Per-feature handlers live under `<feature>/handler/` — `fleet/handler/`, `identity/handler/`, `observability/handler/` — and register themselves against the shared router exposed by `handler/routes.go`.

Convention: per-feature handlers do request parsing + IAM-action check + delegation to the matching `<feature>/store/`; business logic stays in the feature package. No db connection in handler scope — the pool comes via the injected store. Routes are mounted by `wiring.MountRoutes` (`packages/nexus-hub/cmd/nexus-hub/wiring/routes.go:105`), called from `main.go`; adding a new handler means: implement under `<feature>/handler/`, register in `wiring/routes.go`, wire IAM action, add a row to the relevant arch doc.

## `self/reg/`

Hub's **self-registration**. Hub treats itself as a Thing (`thing.type = 'nexus-hub'`, the constant `selfreg.ThingType` in `packages/nexus-hub/internal/self/reg/selfreg.go:22`) so the same registry / shadow / status contract applies to Hub as to AI Gateway, Compliance Proxy, Agent. `self/reg/` is the boot-time step that inserts the `thing` row for the current Hub instance, then runs the heartbeat loop.

Coupled to `fleet/manager/` (for the registry insert) and `self/shadow/` (for ongoing shadow updates).

## `self/shadow/`

Hub's **own shadow blob manager**. Companion to `self/reg/`. Where `self/reg/` does the one-time registration, `self/shadow/` runs the ongoing apply / report cycle for the Hub's own configuration keys. Same `OnConfigChanged` contract that data-plane Things use, just self-applied.

Includes a Postgres LISTEN-based notify channel so admin writes to Hub's own shadow trigger an immediate re-apply (vs polling). Test-mode injection points (`notifier` + `pooledListener` interfaces) let unit tests skip real Postgres LISTEN; production paths always use the real `pgxpool.Pool`.

## `jobs/consumer/`

The **MQ consumer side** of Hub. Owns the NATS JetStream subscribers under `nexus.event.*` and the db-writer fan-out. Files include `admin_audit.go`, `traffic.go`, `siem.go`, plus batch / message / manager scaffolding and their tests. Subjects (canonical list per `packages/nexus-hub/internal/alerts/eval/engine.go:33` and consumer registration):

- `nexus.event.ai-traffic` — traffic_events from AI Gateway.
- `nexus.event.compliance` — traffic_events from Compliance Proxy.
- `nexus.event.agent` — traffic_events from agents.
- `nexus.event.admin-audit` — admin-action audit rows from CP.
- `nexus.event.alert` — alert events raised by `alerts/engine/` (subject from `alerts/engine/raiser.go:57`).
- `nexus.event.diag` — diag uploads from `observability/opsmetrics/diag_writer.go:26`.

Each consumer terminates at the DB (Postgres insert via `<feature>/store/` or `storage/store/`) or at downstream dispatch (`alerts/client/`). Idempotency comes from NATS JetStream consumer ack semantics plus DB unique constraints — there are no dedicated `audit_dedup` / `traffic_dedup` Prisma tables.

Note: per-Thing operational metric samples do **not** flow through NATS — they ride the `metrics_sample` WebSocket frame to Hub directly. There is no `nexus.event.ops_metrics` subject.

## `ws/`

WebSocket transport for the **Hub side of `thingclient`**. Files: `conn.go`, `message.go`, `pool.go`, `server.go`, `signal.go` (plus matching tests). Accepts WebSocket connections from Things authenticated via a Bearer token — either the shared internal-service token or a per-device token hash looked up via `agentca.HashDeviceToken` (`packages/nexus-hub/internal/ws/server.go:186-207`). Multiplexes the change-signal channel + the pull-response channel + the heartbeat channel over one connection. The companion client side lives in `packages/shared/transport/thingclient/` (Tier-1, covered by `thing-config-sync-architecture.md`).

Configuration: ping cadence and frame size are package-level constants — `pingInterval = 30s` (`packages/nexus-hub/internal/ws/conn.go:21`, declared as a `var` so tests can shorten it) and `maxMessageSize = 64 KiB` (`conn.go:16`). The package terminates only the WS framing; protocol-level concerns (which keys to pull, when to signal) live in `fleet/manager/` + `config/`.

## `config/`

The Hub's **YAML config loader** (`packages/nexus-hub/internal/config/config.go`). Defines `HubConfig` and its sub-structs (`ServerConfig`, `DatabaseConfig`, `Redis`, `MQ`, `Consumers`, `Scheduler`, `Auth`, `AuthServer`, `AgentCA`, `OTEL`, `Log`, `Hub`, `Spill`), reads the YAML file, applies env-variable overrides, and validates required fields.

Change-signal dispatch (broadcast `config_changed` to connected Things) is not in this package — it lives in `ws/signal.go` (`SubscribeHubSignals` consumes the `nexus.hub.signal` subject and fans out to local WS pool members). Not to be confused with `packages/control-plane/internal/platform/configreconcile/` (CP-side drift watchdog) or `packages/shared/schemas/configtypes/` (hand-maintained Go mirrors of the Prisma config types).

## When you change one of these

- If a subpackage grows beyond ~6 .go files and gains its own architecture surface, promote it: add a row to `architecture-doc-triggers.md` pointing at a new `docs/developers/architecture/<service>/<name>-architecture.md` Tier-2 doc and remove the row from this card.
- If a subpackage changes API surface that crosses into `shared/`, §5 of `shared-package-architecture.md` (additive-only API stability) applies.

## Sources

- `packages/nexus-hub/internal/storage/store/` and per-feature `<feature>/store/` subpackages.
- `packages/nexus-hub/internal/handler/` and per-feature `<feature>/handler/` subpackages.
- `packages/nexus-hub/internal/self/reg/`
- `packages/nexus-hub/internal/self/shadow/`
- `packages/nexus-hub/internal/jobs/consumer/`
- `packages/nexus-hub/internal/ws/`
- `packages/nexus-hub/internal/config/`
- `packages/nexus-hub/internal/alerts/eval/engine.go` — canonical NATS subject map.
- `packages/nexus-hub/internal/alerts/engine/raiser.go` — `AlertFiredSubject` constant.
- `packages/nexus-hub/internal/observability/opsmetrics/diag_writer.go` — `DiagEventSubject` constant.
