# E31 S7 — Runtime introspection across services

**Epic:** 31
**Story:** 7
**Status:** Implemented — 2026-04-27 (with follow-ups e31-s8/9/10/11/12 also merged)
**Requirements:** inline (operator diagnostic tooling; no separate requirements doc)

## User Story

As an operator changing data-plane config from the CP UI (kill switch, interception domains, virtual keys, hooks, providers, ...), I want to inspect each running service's **live in-memory configuration and cache contents on demand**, so that when "I changed X but it didn't take effect" happens I can verify in one click whether the service has actually applied the change — instead of grepping logs, comparing template versions, or restarting to see what was wrong.

The catalyst incident (2026-04-27) was a kill switch toggle that emitted `config_apply_success` in the proxy log but appeared not to take effect; root-causing took 30+ minutes of cross-service log comparison. With introspection, the same investigation is one HTTP call.

## Scope

### In

- A new `shared/runtimeintrospect` package providing a `Source` interface, a `Registry`, and an HTTP handler that returns one combined snapshot per service.
- Three services register sources and mount the handler on their existing admin/metrics port (auth gated by `InternalServiceToken`):
  - **compliance-proxy** — kill switch, all 8 subscribed config_keys, configcache categories, access checker domain snapshot, exemption store, payload capture store, runtime stats.
  - **ai-gateway** — all 10 subscribed config_keys, vk cache, policy cache (including OrgParents), provider/model catalog snapshots, hooks, ai-guard, payload capture, observability.
  - **nexus-hub** — alerts engine in-memory rules + channels, scheduler/job manager state, retention job state, thing registry summary, broadcast metrics, MQ consumer state.
- A Hub bridge endpoint `GET /api/internal/things/{id}/runtime` that:
  1. Looks up the thing's listen address + role from the thing registry.
  2. Reverse-HTTPs to `<thing-listen-address>/debug/runtime` with `InternalServiceToken`.
  3. Returns the snapshot to CP unchanged, plus thing meta (`desired_ver`, `reported_ver`, `last_seen_at`, `status`) so the UI can diff "what Hub thinks is desired" vs "what the thing actually has".
- A CP admin endpoint `GET /api/admin/nodes/{id}/runtime` that calls the Hub bridge, gated by a new IAM permission `admin:ReadNodeRuntime`.
- A CP UI **"Runtime State"** tab on the Nodes detail page (`/infrastructure/nodes/:id`) that renders the snapshot. Manual refresh button + 10s auto-refresh toggle. i18n keys for en/zh/es.
- Standard JSON shape for every service's snapshot (described in Section [Snapshot schema](#snapshot-schema)).
- Mandatory **secret redaction**: every `Source.Snapshot()` implementation MUST return secret-stripped data (API keys, provider credentials, signing keys, OAuth tokens, mTLS private keys, session cookies, raw JWT material). Length-only or `***` placeholders are acceptable; full values are not. Reviewers verify per-PR.
- Hub itself follows the same pattern but skips the bridge — it self-hosts `/debug/runtime` and CP calls it via the same Hub bridge endpoint convention with thing_id `hub` (special-cased: bridge short-circuits to local handler).

### Out

- **Mutating endpoints.** This story is read-only diagnostic. No `POST /debug/runtime/reload-cache` style operations.
- **Diff visualisation in v1.** UI shows "Hub desired_ver=37" vs "applied snapshot ver=37" side-by-side; structural JSON diff (key-by-key red/green) deferred.
- **Historical snapshots.** v1 returns only the live snapshot; no time-series, no audit log of past snapshots.
- **`iam/users`, `iam/roles`, `iam/policies`, `audit-logs`, `security/dsar`, `account`** menus — these are CP-managed entities without a data-plane in-memory replica.

### Originally out — now closed by follow-up SDDs

The four "gap" items the introspection surface deliberately exposed in
v1 have all been closed by follow-up stories merged alongside this one:

- **Agent introspection** — closed by **e31-s12** (NexusAgentUI Runtime tab via local IPC; Hub bridge unaffected because agents are still NAT-isolated).
- **Gap A (agent fleet kill switch)** — closed by **e31-s9**: agent's `configsync.Manager` now subscribes to `killswitch` and `connectionBridge.HandleConnection` short-circuits to passthrough while engaged.
- **Gap B (`interception_policies` dead key)** — superseded by **e31-s8**: the half-implemented InterceptionPolicy feature was deleted entirely (table, CRUD, frontend pages, both consumers).
- **Gap C (ai-gateway org cache invalidation)** — closed by **e31-s10**: a new `organizations` config_key is broadcast on Org create/update/delete; ai-gateway extends `case "quota_policies", "quota_overrides"` to include it and reloads `PolicyCache` (which rebuilds `OrgParents` and the policy list).
- **Gap D (agent dynamic OTEL)** — closed by **e31-s11** after a recorded compliance-review trade-off: agent's `configsync.Manager` now subscribes to `observability` and hot-swaps the `SwappableTracerProvider`.

## Architecture

### Data path (decision A2)

```
CP UI                  CP admin API           Hub                         Thing (proxy/gw/hub)
─────                  ────────────           ───                         ─────────────────────
GET /infrastructure   GET /api/admin/        GET /api/internal/          GET /debug/runtime
   /nodes/:id            nodes/:id/runtime     things/:id/runtime           (mounts handler from
   (Runtime tab)         │                     │                            shared/runtimeintrospect)
        │                ├─ IAM:               ├─ Lookup thing in
        │                │  admin:ReadNode     │  registry (listen
        │                │  Runtime            │  address + token)
        │                │                     ├─ Reverse HTTP →
        │                ├─ hubclient call ──> │  thing's admin port
        │                │                     │  with internal token
        │                ▲                     │
        ▲────────────────┘  pass-through       ▲────────────────────────────
        snapshot JSON                          { service, sources[], meta }
```

For `thing_id == "hub"` the Hub bridge short-circuits and serves the introspection locally (Hub is itself a Source-registering process).

**Why reverse HTTP and not WS-RPC**: thingclient protocol currently has no request/response channel; adding one is non-trivial and unnecessary for compliance-proxy/ai-gateway/hub which already advertise their listen addresses to Hub at registration time. Agent (which is behind NAT) is out of scope, so the WS-RPC need does not arise here.

### Authentication

- **Thing → Thing**: every `/debug/runtime` endpoint requires `Authorization: Bearer <InternalServiceToken>`. Same token already used for other internal admin endpoints; no new credential.
- **Hub → Thing**: Hub already holds `InternalServiceToken`; reuses for the reverse call.
- **CP → Hub**: existing CP↔Hub authentication (no change).
- **CP UI → CP**: existing session cookie + IAM permission `admin:ReadNodeRuntime` (new — mapped to existing `super-admins` role; introspection includes config redacted values but is still operator-grade material).

### Source contract (in `shared/runtimeintrospect`)

```go
package runtimeintrospect

// Source is a named contributor to a service's runtime snapshot.
// Implementations must return secret-redacted data — see SDD section
// "Mandatory secret redaction".
type Source interface {
    // Name uniquely identifies this source within the service. Convention:
    // "config.<key>" for thingclient config_keys, "cache.<category>" for
    // configcache categories, "runtime.<area>" for ad-hoc state.
    Name() string

    // Snapshot returns the source's current state. Errors are reported
    // per-source in the response and do not fail the whole call.
    Snapshot(ctx context.Context) (any, error)
}

type Registry struct { /* ... */ }

func (r *Registry) Register(s Source)
func (r *Registry) Snapshot(ctx context.Context) Response
func (r *Registry) Handler(token string) http.Handler  // mounts at /debug/runtime
```

### Snapshot schema

```json
{
  "service": "compliance-proxy",
  "thing_id": "proxy-...",
  "thing_version": "0.1.0",
  "process_started_at": "2026-04-27T18:00:00+08:00",
  "snapshot_taken_at": "2026-04-27T19:05:23+08:00",
  "sources": {
    "config.killswitch":           { "ok": true, "value": { "enabled": false, "applied_ver": 33, "applied_at": "..." } },
    "config.interception_domains": { "ok": true, "value": { "applied_ver": 37, "applied_at": "...", "domains": ["api.openai.com", "..."] } },
    "cache.allowlists":            { "ok": true, "value": { "size": 8, "loaded_at": "...", "entries": ["..."] } },
    "runtime.kill_switch":         { "ok": true, "value": { "enabled": false, "last_toggled_at": "...", "last_changed_by": "hub-shadow" } },
    "runtime.active_tunnels":      { "ok": true, "value": 3 }
  }
}
```

The CP API additionally wraps this with `meta`:

```json
{
  "snapshot":  { /* the above */ },
  "meta": {
    "thing_status": "online",
    "last_seen_at": "...",
    "hub_desired_ver":  46,
    "hub_reported_ver": 46,
    "hub_desired":  { "killswitch": { ... } },
    "hub_reported": { "killswitch": { ... } }
  }
}
```

This lets the UI render three columns side-by-side per config_key: **Hub desired** | **Hub reported** | **Service applied snapshot**.

## Tasks

### T1. `shared/runtimeintrospect` package

- New package: `Source` interface, `Registry`, JSON envelope (`Response`), HTTP `Handler(token)`.
- Auth: bearer token check (constant-time compare); 401 on mismatch, 403 on missing.
- Per-source error isolation: a panicking or erroring `Snapshot()` produces `{"ok": false, "error": "..."}` for that source; other sources still serve.
- Unit tests: register, snapshot, redact contract verification helper, handler auth, panic isolation.

### T2. compliance-proxy sources

Register the following sources in `cmd/compliance-proxy/main.go` (after each component is constructed):

| Source name | Implementation hook | Notes |
|---|---|---|
| `config.killswitch` | `killSwitch.IntrospectSnapshot()` (new method) | Snapshot returns `{enabled, last_toggled_at, history_capacity, history_count}`. |
| `config.active_exemptions` | `exemptionStore.IntrospectSnapshot()` | Strip exemption-rule secrets if any. |
| `config.hook_config` | `hookConfigCache.IntrospectSnapshot()` | Redact hook auth headers. |
| `config.interception_domains` | `cacheManager.IntrospectCategory(CategoryInterceptionDomains)` | Use accessChecker.DomainSnapshot revision. |
| `config.domain_allowlist` | `cacheManager.IntrospectCategory(CategoryAllowlists)` | |
| `config.observability` | `cacheManager.IntrospectCategory(CategoryObservability)` | Redact OTEL bearer if any. |
| `config.payload_capture` | `payloadCaptureStore.IntrospectSnapshot()` | |
| `cache.allowlists` | sub-source of CategoryAllowlists with size + loaded_at | |
| `runtime.kill_switch` | already covered by `config.killswitch` — alias for UI grouping; skip duplicate |
| `runtime.active_tunnels` | `connManager.ActiveCount()` | |
| `runtime.access_checker` | `accessChecker.IntrospectSnapshot()` | Domain snapshot revision + counts. |

Mount handler at `/debug/runtime` on the existing metrics/admin port.

### T3. ai-gateway sources

| Source name | Implementation hook |
|---|---|
| `config.providers` | provider catalog snapshot |
| `config.models` | model catalog snapshot |
| `config.credentials` | credential cache (redact secret material — `key_length` + `last_4` only) |
| `config.virtual_keys` | vk cache (redact raw key bytes — `vk_id` + `vk_type` + scope only) |
| `config.routing_rules` | routing engine rule list |
| `config.quota_policies` | policy cache snapshot |
| `config.quota_overrides` | override list |
| `config.hook_config` | hooks cache |
| `config.observability` | OTEL cfg (redact bearer) |
| `config.payload_capture` | payload capture cfg |
| `config.aiguard_config` | AI Guard cfg |
| `cache.policy_cache.org_parents` | OrgParents map snapshot — **explicitly highlight gap C in this snapshot's metadata field** (e.g. `"invalidation_signal": "none — see SDD e31-s7 gap C"`) so it's discoverable in the UI. |
| `runtime.routing_engine` | routing engine state (active rule count, last reload time) |

Mount handler on the AI Gateway metrics port.

### T4. nexus-hub sources

| Source name | Implementation hook |
|---|---|
| `runtime.alerts.rules` | in-memory rule list (redact channel webhook URLs/tokens) |
| `runtime.alerts.channels` | channel registry (redact webhook secrets) |
| `runtime.scheduler` | job manager state (job count, last_run_at, next_run_at per job) |
| `runtime.retention_jobs` | data retention + rollup retention config + last_run stats |
| `runtime.thing_registry` | counts by type/status; do NOT enumerate per-thing detail (that's already on the Nodes list page) |
| `runtime.config_broadcast` | last broadcast per (thing_type, config_key): timestamp, version, things_notified |
| `runtime.mq_consumers` | consumer manager status |

Mount handler on the Hub admin port.

### T5. Hub bridge `GET /api/internal/things/:id/runtime`

- Lookup thing by id; 404 if not found, 503 if `status != online`.
- For `thing_id == "hub"`, serve self snapshot directly (no reverse HTTP).
- Otherwise: HTTP GET `<thing.listen_address>/debug/runtime` with `Authorization: Bearer <InternalServiceToken>`, 5s timeout, 1 retry on connection error.
- Wrap response with `meta` block (see [Snapshot schema](#snapshot-schema)).
- Errors: 502 if reverse HTTP fails, 504 if timeout, propagate 401/403 if token rejected (indicates token rotation drift).

### T6. CP admin endpoint `GET /api/admin/nodes/:id/runtime`

- IAM gate: new permission `admin:ReadNodeRuntime`. Bind to existing `super-admins` role (and `compliance-admins` viewer-style if appropriate — discuss in code review).
- Pass-through to Hub bridge; CP does not interpret or transform the snapshot.
- 401 if unauthenticated, 403 if IAM denies, 502/503/504 propagated from Hub.

### T7. CP UI Runtime State tab

- New tab on `/infrastructure/nodes/:id` page.
- Layout: per-source card; for `config.*` sources, render 3 columns (Hub desired / Hub reported / Service applied).
- Refresh button (manual) + auto-refresh toggle (10s interval, off by default).
- Empty/error states: per-source error displayed inline (don't fail the whole page).
- i18n keys in `pages.json` for en/zh/es. Technical terms (config_key names, "snapshot", "applied_ver") stay in English.
- `useApi` queryKey: `['admin', 'nodes', 'runtime', nodeId]`.

### T8. OpenAPI spec

`docs/users/api/openapi/admin/e31-s7-runtime-introspection.yaml` — paths:
- `GET /api/admin/nodes/{id}/runtime` (CP admin)
- `GET /api/internal/things/{id}/runtime` (Hub internal — documented for completeness)
- `GET /debug/runtime` (per-thing — documented but service-local)

Each with response schema reflecting the JSON envelope above.

### T9. Architecture doc

`docs/users/product/architecture.md` — add a new section **"Runtime introspection"** describing the data path, source contract, and the 4 known gaps (A/B/C/D) explicitly, with links to the gaps' future SDDs.

### T10. Tests

- `shared/runtimeintrospect`: unit tests (T1).
- Per service: smoke tests that the registry has the expected source names registered (table-driven), so a refactor that drops a source from registration breaks CI.
- Hub bridge: unit test for routing (`hub` short-circuit, listen-addr lookup, reverse HTTP error mapping).
- CP API: integration test calling Hub via fake; permission denied path.
- CP UI: vitest for tab rendering with mock data + error state.
- E2E manual verification per [Acceptance Criteria](#acceptance-criteria).

### T11. Verify (the original problem)

Reproduce the 2026-04-27 kill switch incident with introspection in place:
1. Toggle kill switch in CP UI.
2. Open Nodes detail → Runtime State tab → confirm `config.killswitch.value.enabled` flips immediately, `applied_ver` advances.
3. If it doesn't flip, the introspection snapshot pinpoints which layer broke (Hub desired_ver vs reported_ver vs applied snapshot).

## Acceptance Criteria

1. CP UI Nodes detail page has a "Runtime State" tab visible to `super-admins`.
2. For each MVP service (compliance-proxy / ai-gateway / nexus-hub), the tab renders ≥1 source per row in the [Coverage Matrix v3](#coverage-matrix-v3-snapshot) (modulo agent which is out of scope).
3. For every config_key the service subscribes to, the snapshot shows `applied_ver` and a value structurally equivalent (after redaction) to `thing.desired[key]`.
4. The Hub `meta` block lets the user spot disagreement between Hub desired_ver and the service's applied_ver in one screenshot.
5. Toggling kill switch in another tab and clicking Refresh updates the snapshot within one HTTP roundtrip.
6. No secret material (API keys, provider credentials, OAuth tokens, mTLS private keys, session cookies) appears anywhere in the snapshot. PR review checks this per-source.
7. A panicking Source for one entry does not break the rest of the snapshot — that source returns `{"ok": false, "error": "..."}`.
8. `go test -race -count=1 ./packages/shared/runtime/runtimeintrospect/... ./packages/{compliance-proxy,ai-gateway,nexus-hub}/...` passes.
9. CP UI vitest passes; new keys exist in en/zh/es with matching key counts.
10. The 4 known gaps (A/B/C/D in [Out](#out) section) are surfaced in the architecture doc, NOT silently fixed in this story.

## Coverage Matrix v3 (snapshot)

| # | CP UI menu | thingclient key | DB table | AI-GW | Comp-Proxy | Agent (out) | Hub |
|---|---|---|---|---|---|---|---|
| 1 | config/providers | `providers` | Provider | ✅ | – | – | bridge |
| 2 | config/models | `models` | Model | ✅ | – | – | bridge |
| 3 | security/credentials | `credentials` | Credential | ✅ | – | – | bridge |
| 4 | security/virtual-keys + settings/personal-vks | `virtual_keys` | VirtualKey | ✅ | – | – | bridge |
| 5 | config/routing | `routing_rules` | RoutingRule | ✅ | – | – | bridge |
| 6 | config/quota-policies | `quota_policies` | QuotaPolicy | ✅ | – | – | bridge |
| 7 | config/quota-overrides | `quota_overrides` | QuotaOverride | ✅ | – | – | bridge |
| 8 | config/hooks + rule-packs | `hook_config` | HookConfig + RulePack | ✅ | ✅ | (gap, out) | bridge |
| 9 | compliance/interception-domains | `interception_domains` + `domain_allowlist` | InterceptionDomain | – | ✅ | (out) | bridge |
| 10 | compliance/exemptions | `active_exemptions` / `exemptions` | ComplianceExemption | – | ✅ | (out) | bridge |
| 11 | agent-exemptions | (agent-only) | AgentExemption | – | – | (out) | bridge |
| 12 | infrastructure/kill-switch | `killswitch` | thing.desired | – | ✅ | (gap A, out) | bridge + writes |
| 13 | infrastructure/diag-mode | (per-thing window) | thing_diag_mode_window | ✅ | ✅ | (out) | bridge + writes |
| 14 | settings/ai-guard | `aiguard_config` | ai_guard_config | ✅ | – | – | bridge |
| 15a | (setup wizard, no menu) | `observability` | – | ✅ | ✅ | (gap D, out) | bridge |
| 15b | settings/observability/retention | (Hub-internal) | metric_ops_retention_config | – | – | – | ✅ self-host |
| 16 | (implicit setting) | `payload_capture` | per-thing | ✅ | ✅ | ✅ (e31-s12) | bridge |
| ~~17~~ | ~~config/policies~~ | ~~`interception_policies`~~ | ~~InterceptionPolicy~~ | – | – | – | – |
| 18 | settings/device-auth | `auth` (agent only) + Hub self | – | – | – | ✅ self-host |
| 19 | alerts/rules + alerts/channels | (Hub-internal) | AlertRule, AlertChannel | – | – | – | ✅ self-host |

## Risks

- **Snapshot size.** Aggregating providers + models + virtual_keys + routing_rules + hooks for ai-gateway can produce a multi-MB JSON. Mitigation: Source implementations return summaries (counts + selected fields) and offer `?detail=full` query param for the rare case operators want full enumeration; default omits per-row detail beyond what's needed to verify "is the right thing in cache".
- **Secret leakage via misimplemented Source.** A new Source author forgetting to redact provider API keys leaks them to anyone with `admin:ReadNodeRuntime`. Mitigation: SDD section "Mandatory secret redaction" + a `runtimeintrospect/redaction_test.go` table-driven helper that scans returned JSON for high-entropy strings matching common API-key patterns and fails the test (best-effort heuristic, not security boundary). PR review remains the authoritative gate.
- **Performance under repeated polling.** UI auto-refresh at 10s × N admin tabs could pressure the snapshot path if any Source does heavy work. Mitigation: Source implementations must be O(in-memory state); no DB queries inside `Snapshot()`. Enforced by code review.
- **Endpoint exposed on a port reachable from non-admin networks.** Both compliance-proxy and ai-gateway have their metrics/admin port bound by config; in some deployments these are LAN-reachable. Mitigation: introspection requires `InternalServiceToken` (same as other admin endpoints); operators are responsible for not binding the admin port to public interfaces. Documented in `docs/operators/ops/`.
- **Out-of-scope gaps re-surface as bug reports.** Once the UI shows "ai-gateway has cached `OrgParents` but no invalidation signal exists" prominently, gap C will be reported as a bug. That's the point — but make sure each gap's UI surface includes a link to the future fix SDD so the issue tracker doesn't fill with duplicates.

## References

- Catalyst incident: 2026-04-27 conversation log (kill switch + interception_domains apply succeeded but didn't take effect; required restart).
- Architecture: `docs/users/product/architecture.md` — Runtime introspection section (added by T9).
- Related current-state code: `packages/shared/transport/thingclient/` (config push), `packages/shared/storage/configcache/` (cache categories), `packages/{compliance-proxy,ai-gateway}/cmd/*/main.go` (existing OnConfigChanged switch cases used as authoritative coverage source).
- Follow-up SDDs (all merged alongside this story):
  - **Gap A** — agent fleet kill switch: `docs/developers/specs/e31/e31-s9-agent-fleet-killswitch.md`
  - **Gap B** — InterceptionPolicy deleted entirely (the half-implemented feature was removed rather than completed): `docs/developers/specs/e31/e31-s8-delete-interception-policy.md`
  - **Gap C** — `organizations` config_key + ai-gateway OrgParents invalidation: `docs/developers/specs/e31/e31-s10-aigw-organizations-invalidation.md`
  - **Gap D** — agent dynamic OTEL config (compliance trade-off recorded): `docs/developers/specs/e31/e31-s11-agent-dynamic-otel.md`
  - Agent local introspection (NexusAgentUI Runtime tab): `docs/developers/specs/e31/e31-s12-agent-local-introspection.md`
