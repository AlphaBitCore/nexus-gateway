# E3-S5 — Config Sync End-to-End Remediation

Status: draft → in-progress (P0)
Epic: E3 — Service Call Framework (Hub / Thing / Shadow)

## 1. Problem

Audit of `thing_config_template` + shadow-sync on main on 2026-04-22 found the
plumbing works for AI Gateway and Compliance Proxy but is largely broken for
the desktop agent, plus a handful of symmetric gaps across services.

### 1.1 Broken / missing per (thingType, configKey)

| thingType | configKey | Seeded | Admin UI | Admin push path | Reducer | Status |
|---|---|---|---|---|---|---|
| compliance-proxy | killswitch | ✅ | ✅ `InfraKillSwitchPage` | `pushConfigUpdate` | ✅ `killSwitch.Toggle` | OK |
| compliance-proxy | hook_config | ✅ | ✅ `HooksPage` | `admin_hooks.go` | ✅ `hookConfigCache.Reload` | OK (Cat B) |
| compliance-proxy | observability | ✅ | ❌ | — | ✅ `tp.Reconfigure` | No UI |
| compliance-proxy | payload_capture | ✅ | ❌ | — | ❌ no case | **No-op bug** |
| compliance-proxy | interception_domains | ✅ | ❌ | — | ✅ `accessChecker.SwapDomainAllowlist` | No UI |
| compliance-proxy | domain_allowlist | ✅ | ❌ | — | ✅ same | No UI |
| compliance-proxy | active_exemptions | ✅ | ✅ `ExemptionsPage` | break-glass | ✅ `ApplyActiveExemptions` | OK |
| compliance-proxy | alert_channels | ✅ | ❌ (only history page) | — | ✅ `ApplyAlertChannels` | No UI |
| compliance-proxy | alert_thresholds | ✅ | ❌ | — | ✅ `ApplyAlertThresholds` | No UI |
| compliance-proxy | alerting_thresholds | ✅ (**dup of above**) | — | — | ❌ reducer listens on `alert_thresholds` | **Name drift** |
| compliance-proxy | alert_custom_checks | ✅ | ❌ | — | ✅ `ApplyAlertCustomChecks` | No UI |
| ai-gateway | killswitch | ✅ | ✅ | ✅ | ✅ `killSwitchCtrl.Apply` | OK |
| ai-gateway | hook_config | ✅ | ✅ | ✅ | ✅ `hookConfigCache.Reload` | OK |
| ai-gateway | routing_rules | ✅ | ✅ | ✅ | ✅ `db.InvalidateRuleCache` | OK |
| ai-gateway | credentials | ✅ | ✅ | ✅ | ✅ `credManager.ClearCache` | OK |
| ai-gateway | quota_policies | ✅ | ✅ | ✅ | ✅ `policyCache.Load` | OK |
| ai-gateway | observability | ✅ | ❌ | — | ✅ `tp.Reconfigure` | No UI |
| ai-gateway | payload_capture | ✅ | ❌ | — | ❌ no case | **No-op bug** |
| ai-gateway | providers | ❌ | ✅ | ✅ | ✅ | **Missing seed** |
| ai-gateway | virtual_keys | ❌ | ✅ | ✅ | ✅ | **Missing seed** |
| ai-gateway | quota_overrides | ❌ | ✅ | ✅ | ✅ | **Missing seed** |
| agent | exemptions | ✅ | ✅ `AgentExemptionListPage` | ✅ | 🔴 `m.exemptions = nil` | **No-op (nil wire)** |
| agent | payload_capture | ✅ | ❌ | — | 🔴 `m.payloadCapture = nil` | **No-op** |
| agent | policy_rules | ✅ | 🟡 `/api/admin/policies` | ✅ | 🔴 `m.policyEngine = nil` | **No-op** |
| agent | interception_domains | ✅ | ❌ | — | 🔴 `m.domainApplier = nil` | **No-op** |
| agent | hook_config | ❌ | ✅ | ❌ (admin_hooks.go skips agent) | ❌ no case | **Missing everywhere** |

### 1.2 Cross-cutting issues

- **Hub Cat B HTTP pull returns empty `{}`**: `GET /api/internal/things/config/:key?type=agent`
  currently just echoes `thing_config_template.state`, which for Cat B is empty.
  Agent's `policy_rules` / `interception_domains` / `hook_config` pulls fail to
  retrieve real data.
- **Hub cannot reach the business config tables** (`HookConfig`, `Policy`,
  `InterceptionDomain`). Hub only has its own thing DB schema today.
- **Agent auth for Cat B pulls** already works: `DeviceOrServiceAuth`
  middleware validates `Authorization: Bearer <device-token>` against
  `thing.metadata.deviceTokenHash`. No change needed there.

## 2. Goal

Every (thingType, configKey) pair that ships in seed must have:
1. A real data source (admin UI or system default)
2. A working admin push path (CP handler → Hub `/config/update`)
3. A working service-side reducer that applies or invalidates the cached
   state AND causes subsequent runtime reads to pick up the new value
4. Agent Cat B pulls return real aggregated data, not the empty template

## 3. Non-goals (this SDD)

- SSO for agent Hub connection — already handled via device enrollment + bearer
  token; not touching
- Replacing shadow protocol — scope is fixing the existing design
- Multi-region Hub coordination for config — orthogonal

## 4. Phases

### Phase P0-A — Agent reducer wiring

Fix the four nil interfaces so seeded agent keys actually apply.

Files:
- `packages/agent/core/rules/exemption/store.go` — add `SetConfig(json.RawMessage) error`
- `packages/agent/core/observability/audit/payload.go` — extract `payloadcapture.Store` with `Update(json.RawMessage) error`
- `packages/agent/core/rules/policy/engine.go` — add `Reload(ctx, json.RawMessage) error`
- `packages/agent/core/compliance/pipeline.go` — split into `ApplyDomainsOnly(raw) error` and `ApplyHookConfigState(raw) error`; keep old `ApplySnapshot` behind tests
- `packages/agent/cmd/agent/main.go` — wire all four into `configsync.NewManager(ManagerConfig{...})`

Acceptance:
- `go test ./packages/agent/...` green (including reducer unit tests with JSON fixtures that match the seed shapes)
- Manual smoke: Hub push killswitch-equivalent change to agent → `thing.reported` reflects applied state with the same content

### Phase P0-B — Agent hook_config end-to-end

Files:
- `tools/db-migrate/seed/seed.ts` — `agent.hook_config` row (Cat B, state `{}`)
- `packages/control-plane/internal/handler/admin_hooks.go` — 5 sites call `InvalidateConfig(ctx, "agent", "hook_config")` alongside ai-gateway and compliance-proxy
- `packages/agent/core/sync/configsync/manager.go` — new `HookConfigApplier` interface, `case "hook_config"` in `ApplyDesired` (HTTP pulls fresh state via `pullConfig` then applies)
- `packages/agent/core/compliance/pipeline.go` — `ApplyHookConfigState` implementation
- `packages/agent/cmd/agent/main.go` — `HookConfig: agentPipeline`
- `docs/developers/architecture/cross-cutting/foundation/service-call-framework.md §4.5` — add `hook_config (B)` to agent row

Acceptance:
- Admin edits a hook via HooksPage → Hub pushes to all three types (agent included) → agent pulls Cat B → `AgentPipeline.Resolver()` returns the new PolicyResolver → subsequent intercepted traffic runs the new hook chain

### Phase P0-C — Hub Cat B aggregated data loader

Approved direction: Hub connects directly to main DB (option A). Hub’s
dependency list already includes pgx to main-DB tables for Thing Registry;
this adds read-only access to business tables for Cat B assembly.

Files:
- `packages/nexus-hub/internal/storage/store/catb_loader.go` — new. `CatBLoader` interface (`Load(ctx, thingID) (state any, version int64, err error)`) + `Registry` keyed by (thingType, configKey)
- `packages/nexus-hub/internal/storage/store/catb_agent_hooks.go` — reads `HookConfig` rows for agent-scoped hooks, assembles `{hookConfigs: [...]}`
- `packages/nexus-hub/internal/storage/store/catb_agent_policy.go` — reads `Policy` table filtered by agent-applicable policies, assembles `{rules: [...]}`
- `packages/nexus-hub/internal/storage/store/catb_agent_domains.go` — reads `InterceptionDomain` + `InterceptionPath`, assembles snapshot shape
- `packages/nexus-hub/internal/handler/internal_things.go` — `SingleConfigPull` tries `CatBLoader` first, falls back to `thing_config_template.state` for Cat A
- `packages/nexus-hub/cmd/nexus-hub/main.go` — wire main-DB pgx pool into loader registry
- `packages/nexus-hub/internal/config/` — add `MainDBURL` config field
- `docs/users/product/architecture.md` — note Hub now reads main DB for Cat B assembly

Acceptance:
- `GET /api/internal/things/config/hook_config?type=agent` returns the current hook list in the shape the agent expects
- `GET /api/internal/things/config/interception_domains?type=agent` returns full domain snapshot
- `GET /api/internal/things/config/policy_rules?type=agent` returns filtered policy ruleset
- Existing Cat A pulls (e.g. `killswitch`) still work (fall-through to template state)

### Phase P1 — Bug fixes

Files:
- `tools/db-migrate/seed/seed.ts` — delete `alerting_thresholds` row (duplicate); add `providers`, `virtual_keys`, `quota_overrides` for ai-gateway
- `packages/compliance-proxy/cmd/compliance-proxy/main.go` — add `case "payload_capture"` that forwards state to the capture store
- `packages/ai-gateway/cmd/ai-gateway/main.go` — same
- Find actual `payload_capture` consumers in both services. If no consumer exists, open a follow-up — do NOT leave a dead reducer.

Acceptance:
- `alerting_thresholds` no longer appears in DB or docs
- Admin can invalidate `payload_capture` and the service applies the new max_body_bytes / store flags

### Phase P2 — Admin UI pages for missing managed keys

In scope:
- `payload_capture` editor (per-service): toggle store_request_body / store_response_body, max_body_bytes
- `observability` editor (per-service): log_level, metrics_enabled, prometheus_path, tracing_enabled, sampling_rate
- `interception_domains` manager (CP + agent): CRUD + reorder
- `domain_allowlist` editor (CP): entry list CRUD
- `alert_channels` + `alert_thresholds` + `alert_custom_checks` manager (CP)

> **Delivery status (2026-04-22)** — E25-S1 delivered the
> `interception_domains` admin UI (`/compliance/interception-domains` with
> inline path sub-table) plus `domain_allowlist` as a derived projection
> (no separate admin surface; CP's reducer cascades the invalidation).
> See `docs/developers/specs/e25/e25-s1-interception-domains-admin.md`. Combined with
> E22-S1 (`payload_capture`) the two payload/observability-style keys
> and the domain pair are now covered; `observability` and the unified
> alerting pages remain the open P2 items.

New UI routes (tentative):
- `/infrastructure/payload-capture`
- `/infrastructure/observability`
- `/compliance/interception-domains`
- `/compliance/domain-allowlist`
- `/compliance/alerting`

Each page follows the existing pattern used by `InfraKillSwitchPage` /
`HooksPage`:
1. `GET /api/admin/<resource>` to list/fetch
2. `POST|PUT` to mutate
3. CP handler writes to DB (Cat B) or directly calls `pushConfigUpdate`
   (Cat A)
4. CP handler calls `h.Hub.InvalidateConfig(ctx, thingType, configKey)` on
   success

Acceptance: each page covered by at least one vitest integration test that
asserts the resulting admin → Hub → service push path works end-to-end.

### Phase P3 — Documentation sync

Files:
- `docs/developers/architecture/cross-cutting/foundation/service-call-framework.md §4.5` — update tables for all four
  thingTypes with the real key set
- `docs/developers/architecture/cross-cutting/foundation/thing-model.md` — cross-check terminology boundary for any new
  keys
- `docs/users/product/architecture.md` — Hub main-DB dependency

## 5. Risks

- **Hub main-DB schema coupling** — Hub now needs to know the business
  schema. Mitigation: isolate loaders behind the `CatBLoader` interface so
  schema changes touch one file per key.
- **Cat B pull latency** — Hub needs to round-trip to DB on every pull.
  Mitigation: cache per (thingType, configKey, version) in Hub’s Redis.
  Out of scope for P0, open a follow-up.
- **Dead reducer for `payload_capture`** — if no consumer exists after P1
  research, shipping a reducer is dishonest. Ship only with real consumer
  wired.

## 6. Rollout order

P0-A → P0-B → P0-C are three separate commits, merged in that order. P1
follows in one commit. P2 is per-page commits. P3 is a final doc-only
commit.

## 7. Verification

After each phase:
- `go build ./...` + `go test -race -count=1` on touched packages green
- `npx tsc --noEmit` + `npx vitest run` on control-plane-ui green
- Manual smoke via `./scripts/dev-start.sh` + admin login + clicking the
  relevant config surface + inspecting `thing` table for desired/reported
  divergence

## 8. Out-of-scope follow-ups

- Hub Redis cache for Cat B aggregated payloads
- Per-agent-user / per-device-group scoped config overrides (today seed is
  per-thingType only)
- Schema generator / SDK for `payload_capture` / `observability` templates
  so the admin UI forms can render from a shared definition

## P0 status

- P0-A (agent reducer wiring): landed
- P0-B (agent hook_config end-to-end): landed
- P0-C (Hub Cat B aggregated data loader): landed — `CatBLoader`/`CatBRegistry` +
  three agent loaders in `packages/nexus-hub/internal/storage/store/catb_*`; wired
  into `SingleConfigPull` via `handler.RouteConfig.CatB`; Hub reuses its
  existing DB pool (decision 1A; no new env, no second connection).
  `policy_rules` scopes per-agent via `DeviceGroupMembership` (decision
  2A) — an agent with no memberships gets `{}` (no-op) so local yaml
  defaults survive; an agent in an empty-policy group gets authoritative
  `{"rules":[]}`.

## P1 status

- Seed cleanup: dropped three dead rows (`compliance-proxy.payload_capture`,
  `compliance-proxy.alerting_thresholds`, `ai-gateway.payload_capture`) — no
  runtime consumers in either service (alerting moved to shared/alertclient
  in 2026-04; `payload_capture` never had reducer wiring). Re-seed these
  when consumer wiring lands per §8.
- Seed additions: `ai-gateway.providers`, `ai-gateway.virtual_keys`,
  `ai-gateway.quota_overrides` (all Cat B, `state: {}`) — mirrors the
  existing admin push path (`admin_providers.go` / `admin_vk_approval.go` /
  `admin_quota_overrides.go` already call `InvalidateConfig` for them; the
  AG reducer has `case` entries; before this change a fresh AG never had
  these keys in `thing.desired` until the first admin write).
- Docs sync: `docs/developers/architecture/cross-cutting/foundation/service-call-framework.md §4.5` rewritten to match
  shipped seeds for compliance-proxy and ai-gateway. Alerting keys removed
  with an explicit "do not re-introduce" note pointing at the unified
  alerting design (`docs/developers/specs/e21/e21-s1-unified-alerting.md`). Agent and
  control-plane tables were already correct.

## P1 follow-up (E22-S1, 2026-04)

- `payload_capture` has been re-seeded for both `compliance-proxy` and
  `ai-gateway` as Cat B placeholders (`state: {}`) now that
  [`docs/developers/specs/e22/e22-s1-payload-capture-end-to-end.md`](../../../../docs/developers/specs/e22/e22-s1-payload-capture-end-to-end.md)
  lands the runtime consumers (shared `payloadcapture.Store`, per-service
  reducer cases, and the CP admin endpoint at
  `/api/admin/settings/payload-capture`). The authoritative config lives
  in `system_metadata["payload_capture.config"]`; the shadow row only
  carries a version bump to trigger the reducer.

## P1 follow-up (E24-S1, 2026-04)

- `agent.payload_capture` has now been re-seeded (also a Cat B
  placeholder, `state: {}`) and is backed by a real reducer: the agent
  constructs a `payloadcapture.Store` at boot, routes its
  `ApplyShadowState` into `configsync.Manager` via a new
  `PayloadCapture` slot, and exposes `ReadBodyCap()` to the platform
  MITM layer so the buffered body read honours the admin-configured
  ceiling. Hub's `AgentPayloadCaptureLoader` reads the same
  `system_metadata["payload_capture.config"]` row as CP and AG, so one
  admin edit now fans out to all three data-plane services via a single
  shadow invalidation (`compliance-proxy`, `ai-gateway`, `agent`).
