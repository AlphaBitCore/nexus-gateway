---
doc: thing-config-sync-architecture
area: cross-cutting
service: foundation
tier: 1
updated: 2026-05-20
---

# Thing Config Sync Architecture

> **Tier 1 architecture doc.** Read this with `thing-model.md` whenever you touch the runtime config-sync flow: shadow desired/reported keys, `Cat A/B/C` classifications, change-signal dispatch, the pull path, or `thing_config_template`. The Thing data model itself is in `thing-model.md`.

Nexus Gateway uses a **pull-only** config-sync model across all five Thing types. Hub never pushes full state; it signals a change, and the Thing pulls just the keys that changed. This doc explains why, how, and the invariants that make it correct.

---

## 1. Why pull-only

A previous push-based design (with Redis pub/sub fanout) had three problems:
- **Hub did not know whether a Thing had applied a config.** Push semantics couldn't carry a receipt.
- **Cold-start divergence.** A Thing booting from snapshot missed any pushes that fired during the boot window.
- **Different code paths for "boot pull" vs "live push".** Two paths means two bugs.

Pull-only fixes all three. The change-signal is a kick; the pull is uniform across boot and live update; the receipt closes the loop.

## 2. Desired / reported shadow

Each Thing has a per-key shadow:

```
desired:  { hooks/v: 17, routing/v: 42, killswitch/v: 3, agent_settings/v: 8, … }
reported: { hooks/v: 17, routing/v: 42, killswitch/v: 3, agent_settings/v: 7, applyError: "…", processStartedAt: 2026-05-15T… }
```

Desired = what the admin wants. Reported = what the Thing currently has applied, plus per-key apply errors and `processStartedAt` (E27 outcomes). The gap is **drift**.

Versions are monotonic per key. The Thing reports the version it has applied, not the version it intends to apply.

## 3. Category A / B keys

Each shadow key is dispatched as one of two categories, by whether a `CatBLoader` is registered for it on the Hub side (`packages/nexus-hub/internal/storage/store/catb_loader.go`):

| Category | Wire shape | When used |
|---|---|---|
| **Cat A — inline** | Full value carried in the shadow JSON | Small, fast-path keys (kill switch toggle, emergency-passthrough flag, agent_settings). Read with no extra round-trip. |
| **Cat B — pull-on-signal** | Only `{ version, needsPull: true }` in the shadow | Mid-to-large keys (hook config, routing rules, payload_capture, streaming_compliance). Change-signal triggers an explicit pull. |

Per-instance overrides for any key are stored in `thing_config_override` and merged into `thing.desired` server-side (`packages/nexus-hub/internal/fleet/manager/override.go` `recomputeDesiredTx`). Receivers never see the override layer separately — they receive the merged desired-state value.

**Cat B invariant (binding).** Every Cat B key MUST carry `needsPull: true` in the shadow. A registered Cat B key without that flag is **invisible** to the pull path — the Thing never sees it. This caused the #91 prod bug (agent.desired missing 4 registered Cat B keys). See `feedback_thing_config_pull_model`.

**2026-05-21 — E72 extract-cache fleet config (Cat B).** Adds `response_cache.extract_config` to the ai-gateway desired-state surface. Carries `{enabled, ttlSeconds, applyFreshnessRules}` and is consumed by `packages/ai-gateway/cmd/ai-gateway/configdispatch/configdispatch.go` — the handler atomically swaps the live `Cache` so admins can disable cache or stop freshness rules from firing without a service restart.

## 4. The change-signal

When the admin updates a Cat B key via the CP admin API:

1. CP forwards the write to Hub (`POST /api/hub/shadow/...`).
2. Hub validates, persists to Postgres, increments the version.
3. Hub looks up affected Things and emits a change-signal over the existing WebSocket session.
4. Each Thing receives the signal — **a minimal payload** identifying which keys changed.
5. Each Thing pulls the new key values from Hub (`GET /api/hub/shadow/...`).
6. The Thing applies and emits a reported-state stamp.

For Cat A keys the payload itself rides on the desired-state JSON (no separate pull). Cat B keys carry only the version + `needsPull: true` and trigger a separate Hub HTTP fetch.

## 5. Apply path

A Thing applies a freshly pulled config by:

1. Validating against the local schema.
2. Atomic-pointer swapping the in-memory snapshot (`atomic.Pointer[...]`).
3. Stamping the reported state: `{ version, applied_at, processStartedAt, applyError: nil }`.

If apply fails, `applyError` is set and the previous in-memory snapshot is kept. Hub sees the failure on the next reported stamp and surfaces it as drift in the UI.

Atomic-pointer swaps are the convention because they avoid blocking in-flight requests on a hot-config reload.

## 6. Drift detection

`drift = desired.version > reported.version` (for the same key). Hub computes drift on every reported stamp and surfaces it in the CP "Config Sync" surface (terminology mapping in `thing-model.md` §10).

Drift is **not** an error by itself — it is a normal in-flight state. Drift becomes an alert when it persists beyond a threshold (per Thing type).

## 7. Three independent paths (binding audit invariant)

When you add or change a shadow key, **three independent paths must stay aligned**:

1. **`packages/control-plane/internal/platform/configreconcile/`** — the **CP-side drift watchdog**. Periodically compares CP's source-of-truth config tables against `thing.desired.<key>` and re-emits `Hub.NotifyConfigChange` to heal divergence. Runs in CP, not Hub. CP wiring at `packages/control-plane/cmd/control-plane/wiring/reconcile.go`.
2. **`tools/db-migrate/seed/seed.ts`** — the canonical Prisma seed used to bootstrap fresh dev DBs; the AlertRule / template rows for the seed baseline live in `tools/db-migrate/seed/data/seed-baseline.sql`.
3. **`tools/db-migrate/migrations/**`** — durable schema migrations that ship with each release.

Auditing only `migrations/` produces **false positives** — `configreconcile` and `seed.ts` can drift independently. Memory `feedback_thing_config_template_audit_paths` records audit #7 hitting exactly this.

When you review a change that touches `thing_config_template`, ask: does the new state appear in **all three** paths?

## 8. OnConfigChanged contract

Each Thing registers `OnConfigChanged` callbacks per key it cares about. When the pull path delivers a new value, the callback fires. The callback is responsible for:

- Validating the payload (idempotent).
- Atomic-pointer swap of the in-memory snapshot.
- Returning success / failure (translates to the reported stamp).

Callbacks are wired in service `main.go` files (`packages/<service>/cmd/<service>/main.go`). They must be registered before the WebSocket session is established, otherwise change-signals for that key will be lost.

A diag-pipeline-bypass corollary applies (memory `feedback_server_slog_sink_di_bypass`): after wiring SlogSink + `slog.SetDefault`, also reassign `logger = slog.Default()` or DI-injected loggers silently bypass the diag pipeline. Same shape of "register-then-forget" bug; verify the live snapshot is observable.

## 9. Cold-start

On boot a Thing:

1. Connects to Hub via `packages/shared/transport/thingclient` (server services: Bearer `INTERNAL_SERVICE_TOKEN`; agent: device cert + key over mTLS).
2. Receives its full shadow via the initial pull (all Cat A inline + a `needsPull` list for Cat B). The shadow is already the merge of `thing_config_template` and `thing_config_override` (server-side `recomputeDesiredTx`).
3. Pulls Cat B keys.
4. Applies and reports.
5. Enters the live change-signal loop.

This unifies the cold-start path with the live update path: same callbacks, same apply contract, same reported stamps.

## 10. Failure modes

| Failure | Behavior |
|---|---|
| WS link drops | Thing falls back to HTTP heartbeat; pull is HTTP. |
| Cat B `needsPull` flag missing | Pull never triggers; key never reaches the Thing (#91 incident). Fix: enforce `needsPull: true` on Cat B classification. |
| Apply error | Reported `applyError` set; previous snapshot retained; Hub surfaces drift. |
| Hub down | Thing keeps its last applied config; no new updates until reconnect. |
| Cold-start during admin write storm | Atomic versioning + monotonic pull ensures eventual convergence; ordering not guaranteed across keys but consistent per-key. |

## 11. Sources

- `packages/shared/transport/thingclient/` — client side (signal listen, pull, apply, report).
- `packages/shared/schemas/configtypes/` — typed shadow keys (hand-maintained).
- `packages/control-plane/internal/platform/configreconcile/` — CP-side runtime template reconcile + drift watchdog (wired in `packages/control-plane/cmd/control-plane/wiring/reconcile.go`).
- `packages/nexus-hub/internal/fleet/shadow/` + `internal/fleet/manager/` — server side (shadow store + change-signal dispatch).
- `packages/nexus-hub/internal/fleet/handler/hubapi/` — shadow CRUD HTTP API (e.g. `hub_api_overrides.go`).
- `tools/db-migrate/seed/seed.ts` + `seed/data/seed-baseline.sql` — canonical seed for `thing_config_template`.
- `tools/db-migrate/schema.prisma` — `Thing` (carries `desired` / `reported` / `reported_outcomes` JSONB columns), `thing_config_template`, `thing_config_override`. There is no `thing_shadow` / `thing_shadow_reported` table; the shadow lives on the `thing` row itself.

## 12. Cross-references

- `thing-model.md` — Thing data model (this doc is the runtime companion).
- `multi-endpoint-coordination-architecture.md` — golden flows that traverse the sync path end-to-end (e.g., admin enables hook → Thing applies).
- `mq-architecture.md` — MQ is not in the config sync path; sync is HTTP+WS only.
- CLAUDE.md "Hub-centric model" + "Kill switch" + "Redis cache-only" facts.
