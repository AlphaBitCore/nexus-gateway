# Data-Plane Configuration Cache, Upstream HTTP, and Agent Transport Redesign

**Date:** 2026-04-26
**Status:** Approved (brainstorm complete; user delegated autonomous execution)

## 1. Overview

The data-plane services (`ai-gateway`, `compliance-proxy`, `agent`) today have **inconsistent and incomplete performance hygiene** along three axes:

1. **Configuration caching** — only `routing_rules`, `hook_config`, and `quota_policy` are cached in `ai-gateway`. `providers`, `models`, `credentials`, `virtual_keys`, `users`, `orgs`, `projects` are read directly from PostgreSQL on every `/v1` request, costing ~6 DB round-trips per request before reaching the upstream provider.
2. **Upstream HTTP client** — the provider-adapter layer in `ai-gateway` is well-tuned (pooled `http.Transport` via `specutil.NewHTTPClient`), but six other code paths use bare `&http.Client{}` literals or `http.DefaultClient`. The `agent` does a fresh `tls.DialWithDialer` per intercepted flow, with no connection reuse at all.
3. **Stale invalidation channels** — `HookConfigCache` still subscribes to Redis pub/sub for invalidation in addition to `thingclient` shadow push. The Hub-centric model is complete (per `CLAUDE.md` current-state summary), so the Redis pub/sub branch is dead weight.

This spec consolidates the fixes for all three under a single rewrite. The goals are:

- Eliminate per-request DB lookups for configuration data.
- Standardize upstream HTTP behaviour across all three data-plane services.
- Reduce TLS-handshake overhead in `agent` by replacing per-flow `tls.DialWithDialer` with a per-host HTTP/2 pool on a single shared `*http.Client`. (Agent goes direct to the original SNI/Host; see `2026-04-26-agent-transport-rewrite-design.md` for the Trinity-parallel topology rationale.)
- Collapse to a single invalidation channel (`thingclient` only).
- Add per-rule cache controls and observability for `response cache` and `ai-guard cache`.

Nexus Gateway is single-tenant on-prem (per project memory), so cross-tenant cache-key isolation is **not** required.

## 2. Key Decisions (from brainstorm)

| # | Decision | Choice | Rationale |
|---|----------|--------|-----------|
| 1 | Cache abstraction | Two generic types: `SnapshotCache[T]` and `KeyCache[K,V]` | Snapshot suits small full-table data with atomic swap; KeyCache suits large per-key data with LRU + TTL. Forcing both into one type compromises both reading patterns. |
| 2 | `providers` / `models` / `credentials` / `routing_rules` / `hook_config` / `quota_policy` storage | `SnapshotCache[T]` (eager full-table load + `atomic.Pointer` swap) | Each table is at most a few hundred rows per customer. Full snapshot is tens of KB; lock-free reads. |
| 3 | `virtual_keys` / `users` / `orgs` / `projects` storage | `KeyCache[K,V]` (`hashicorp/golang-lru/v2` + per-key TTL + singleflight) | VK count can reach tens of thousands per customer; full-table push has unacceptable write amplification. Lazy LRU + push-driven evict is the right shape. |
| 4 | VK denormalization | **Do not** denormalize User/Org/Project fields into the VK cache row | Org/Project changes would cascade-invalidate all VKs of that org → write amplification returns. Three independent caches cleanly mirror CP CRUD boundaries. |
| 5 | TTLs | Snapshot: no TTL (push-driven); VK: 30s; User/Org/Project: 5min | VK 30s bounds revoke latency; User/Org/Project change rarely and have no security-critical staleness. |
| 6 | Invalidation channel | `thingclient` only — delete Redis pub/sub branch from `HookConfigCache` | Single source of truth; Hub already provides HTTP fallback for offline scenarios. Per project policy: no backwards compatibility shims. |
| 7 | New `thingclient` message form | `{ key, op: "invalidate", ids: [...] }` for `KeyCache` invalidation | Avoids pushing the entire VK table on every revoke; only the affected key list. |
| 8 | Quota counter caching | **Do not** add in-memory layer above Redis | Counters must be atomic across multiple `ai-gateway` replicas; in-memory layer would let same-instance concurrent requests under-count and break low-RPS limits. |
| 9 | `model pricing lookup` (quota downgrade path) | Fold into `model` snapshot cache | Pricing is already a column on the `Model` row; loading the model row covers it. |
| 10 | HTTP client unification | Move `specutil.NewHTTPClient` to `packages/shared/transport/httpclient`; replace all bare clients | Three services + shared sub-packages share the same need; six existing offenders should converge. |
| 11 | `compliance-proxy` SIEM `HTTPSink` use of `http.DefaultClient` | Replace with `shared/httpclient` factory | Sharing a global transport across unrelated callers is the worst-practice pattern. |
| 12 | Agent transport | Replace per-flow `tls.DialWithDialer` with a single process-lifetime `*http.Client` (per-host HTTP/2 pool) on the MITM relay outbound side. Agent terminates at the original SNI/Host directly — Trinity topology is parallel, not chained, so there is no "upstream gateway" hop driven by the agent. (Full design: `2026-04-26-agent-transport-rewrite-design.md`.) | Agent intercepts may produce thousands of short flows; per-host H2 multiplex absorbs concurrency to a single provider host. Network-level redirection (DNS, PAC, transparent proxy) — when configured by ops — is invisible to the agent and unaffected. |
| 13 | Agent escape path / whitelist | **Removed.** The original "gateway hot path + origin escape" two-pool model has been dropped along with the gateway-forwarding premise. The single client handles every host; the byte-level `Relay()` fallback in `MITMRelay` (rare, non-HTTP traffic) keeps a one-shot un-pooled `tls.Dial` of its own. | Without a "default = gateway" path there is no "exception = origin" path either. |
| 14 | Cross-tenant cache isolation | **Not added** (single-tenant deployment) | Misread during brainstorm; product is on-prem single-tenant per project memory. |
| 15 | `response cache` granularity | Add `cache_enabled bool` and `cache_ttl_seconds int` columns to `RoutingRule` | Per-rule control; enables turn-off-for-PII-routes scenarios without disabling globally. |
| 16 | `ai-guard cache` granularity | Sufficient as-is (`AIGuardConfig.CacheTTLSeconds` per config; TTL=0 disables) | Already has the right knob; surfaces in metrics is the only gap. |
| 17 | `response cache` HIT behaviour wrt audit hook | Run audit hook **non-blocking** even on HIT; quota stays at zero | Compliance customers must see all requests in audit even when the response was a cache hit. Quota is correctly zero because no upstream cost was incurred. |
| 18 | DNS pre-warm / TLS session resumption | Out of scope this round | Diminishing returns vs. the items above; revisit only if production telemetry justifies. |
| 19 | Redis at-rest encryption | Out of scope (deployment-layer concern) | Not a code change. |
| 20 | Lint enforcement | `forbidigo` rule banning bare `&http.Client{}` literals and `http.DefaultClient` in production paths | Prevents regression after this cleanup. |

## 3. Scope

### In Scope

- New package `packages/shared/storage/configcache` providing `SnapshotCache[T]` and `KeyCache[K,V]`.
- Wiring all data-plane reads of `providers`, `models`, `credentials`, `routing_rules`, `hook_config`, `quota_policy`, `virtual_keys`, `users`, `orgs`, `projects` through the new caches.
- New package `packages/shared/transport/httpclient` (promoted from `ai-gateway/internal/providers/specutil/`).
- Replacement of all six known bare `http.Client` / `http.DefaultClient` usages with the shared factory.
- Agent transport rewrite: single shared `*http.Client` with per-host HTTP/2 pool replaces per-flow `tls.DialWithDialer` in `MITMRelay`. No two-pool model (the prior "gateway hot path + origin escape" framing was dropped — see paired spec `2026-04-26-agent-transport-rewrite-design.md`).
- Control Plane: emit `NotifyConfigChange` for the new tables (`providers`, `models`, `virtual_keys`, `users`, `orgs`, `projects`); support new `invalidate-by-id` message form.
- Hub: extend shadow message handler to deliver `invalidate-by-id` to data plane.
- Removal of Redis pub/sub branch from `shared/compliance/config_cache.go`.
- DB schema: add `cache_enabled`, `cache_ttl_seconds` to `RoutingRule`; one Prisma migration; update Go struct codegen.
- Response cache: respect per-rule `cache_enabled` and `cache_ttl_seconds`; run audit hook non-blocking on HIT.
- Prometheus metric `nexus_cache_hits_total{cache, rule_id}` for both response and ai-guard caches; cache-size and miss counters in same family.
- `forbidigo` lint config banning bare HTTP client literals.
- Quota: `model pricing lookup` folded into `model` snapshot cache; verify Redis pool size config (PoolSize, MinIdleConns) is reasonable.
- Update `docs/dev/architecture.md` — add a "Data-plane caches" subsection summarizing the snapshot/key cache split and the invalidation channel.

### Out of Scope

- DNS pre-warm / custom Resolver.
- TLS session resumption configuration.
- Redis at-rest encryption.
- AI-Guard backend redesign (multi-backend, fallback chains).
- Multi-tenant cache-key isolation (single-tenant product).
- Removal of `compliance-proxy`'s HookConfigCache TTL — keeping the 2min TTL as backstop is harmless; only the Redis pub/sub branch is removed.
- Any frontend (`control-plane-ui`) changes for surfacing per-rule cache toggles. UI work tracked separately when the routing-rules editor is next touched.

## 4. Architecture

### 4.1 New package: `packages/shared/storage/configcache`

Two generic primitives, both producing the same interface for consumers (read-side methods + an `Invalidate` method).

```go
package configcache

// SnapshotCache holds a full table in memory and serves lock-free reads
// via an atomic pointer. Reload is full-table replacement.
type SnapshotCache[T any] struct {
    loader  func(ctx context.Context) (map[string]T, error)
    snap    atomic.Pointer[map[string]T]
    log     *slog.Logger
    onLoad  func(size int) // metrics hook
}

func NewSnapshotCache[T any](loader func(ctx context.Context) (map[string]T, error), opts ...SnapshotOption) *SnapshotCache[T]
func (s *SnapshotCache[T]) Load(ctx context.Context) error
func (s *SnapshotCache[T]) Get(id string) (T, bool)
func (s *SnapshotCache[T]) All() map[string]T
func (s *SnapshotCache[T]) Reload(ctx context.Context) error // alias for Load; used by invalidate

// KeyCache is per-key lazy LRU + TTL + singleflight. It does not preload.
type KeyCache[K comparable, V any] struct {
    loader func(ctx context.Context, key K) (V, error)
    lru    *lru.Cache[K, entry[V]]
    sf     singleflight.Group
    ttl    time.Duration
    log    *slog.Logger
    onHit  func()
    onMiss func()
}

func NewKeyCache[K comparable, V any](loader func(ctx context.Context, key K) (V, error), capacity int, ttl time.Duration, opts ...KeyOption) *KeyCache[K, V]
func (k *KeyCache[K, V]) Get(ctx context.Context, key K) (V, error)
func (k *KeyCache[K, V]) Invalidate(keys ...K)
func (k *KeyCache[K, V]) Purge()
```

`entry[V]` carries `(value, loadedAt)` so TTL is enforced on `Get`. Errors during load are not cached; misses retry next call. `singleflight` collapses concurrent misses on the same key into one DB query.

### 4.2 Wiring (per table)

| Table | Cache type | Loader query | Invalidation key in `OnConfigChanged` |
|---|---|---|---|
| `Provider` | `SnapshotCache[ProviderRow]` | `SELECT * FROM "Provider"` | `providers` (full reload) |
| `Model` | `SnapshotCache[ModelRow]` | `SELECT * FROM "Model"` | `models` (full reload) |
| `Credential` | `SnapshotCache[CredentialRow]` | `SELECT * FROM "Credential"` | `credentials` (full reload) |
| `RoutingRule` | `SnapshotCache[RoutingRuleRow]` (already exists; migrate to shared abstraction) | `SELECT * FROM "RoutingRule" WHERE enabled=true` | `routing_rules` (full reload) |
| `HookConfig` | `SnapshotCache[HookConfigRow]` (already exists; migrate to shared abstraction; remove Redis pub/sub) | `SELECT * FROM "HookConfig" WHERE enabled=true` | `hook_config` (full reload) |
| `QuotaPolicy` + `QuotaOverride` | `SnapshotCache[*]` (already exists; migrate to shared abstraction) | existing PolicyCache loader | `quota_policy` (full reload) |
| `VirtualKey` | `KeyCache[hash → VKRow]` | `SELECT * FROM "VirtualKey" WHERE keyHash=$1` | `virtual_keys` invalidate-by-hash |
| `User` | `KeyCache[id → UserRow]` | `SELECT * FROM "User" WHERE id=$1` | `users` invalidate-by-id |
| `Org` | `KeyCache[id → OrgRow]` | `SELECT * FROM "Organization" WHERE id=$1` | `orgs` invalidate-by-id |
| `Project` | `KeyCache[id → ProjectRow]` | `SELECT * FROM "Project" WHERE id=$1` | `projects` invalidate-by-id |

Existing call sites are rewritten to consult the cache. The `store/*.go` direct query helpers stay (used by loaders and by Control Plane writes via different code path), but proxy hot-path code consults the cache exclusively.

### 4.3 Invalidation message contract

`shared/thingclient.OnConfigChanged` continues to receive `desired map[string]ConfigState` where `ConfigState.State` is opaque JSON. We extend the convention so that for `KeyCache`-backed configs the `State` payload is one of:

```json
// Initial / full reset: instructs cache to purge
{ "op": "purge" }

// Targeted invalidation
{ "op": "invalidate", "ids": ["id1", "id2"] }
```

For `SnapshotCache`-backed configs (existing behaviour) the `State` either contains the full payload inline or an opaque marker; on receipt the consumer calls `cache.Reload(ctx)`.

The `Hub` server-side keeps storing per-key `desired` JSON (existing shadow model) and keeps `desired_ver` semantics. The CP-side helper `hubClient.NotifyConfigChange(key, payload)` is extended with a sibling `hubClient.NotifyInvalidate(key, ids)` for the new message form.

### 4.4 Removal of Redis pub/sub

`shared/compliance/config_cache.go` currently runs a Redis subscriber goroutine in addition to `thingclient`. That goroutine, the related Redis client wiring in both `ai-gateway` and `compliance-proxy`, and any leftover publish calls in Control Plane handlers are deleted. The `HookConfigCache` keeps its 2-minute TTL as a backstop on its own timer.

After this change, **no service publishes config-invalidation events to Redis**. Redis remains in use solely for: sessions, IAM cache, rate-limiting counters, response cache body, quota counters, and desired-state cache (Hub-side).

### 4.5 New package: `packages/shared/transport/httpclient`

```go
package httpclient

type Config struct {
    Timeout                time.Duration // default 30s
    DialTimeout            time.Duration // default 10s
    KeepAlive              time.Duration // default 30s
    MaxIdleConns           int           // default 200
    MaxIdleConnsPerHost    int           // default 50
    MaxConnsPerHost        int           // default 100
    IdleConnTimeout        time.Duration // default 90s
    TLSHandshakeTimeout    time.Duration // default 10s
    ResponseHeaderTimeout  time.Duration // default 60s
    ForceHTTP2             bool          // default true
}

func New(cfg Config) *http.Client
func NewProbe() *http.Client // short-timeout variant for health checks
```

The factory builds a tuned `http.Transport` (h2 enabled via `http2.ConfigureTransport`), wraps it in `*http.Client`, returns it. Each call site that needs different parameters (probe, webhook, alerting) constructs once at startup and reuses.

Replacements:

| Existing call site | Replacement |
|---|---|
| `ai-gateway/internal/providers/specutil/http.go` | Re-exports from `shared/httpclient` (keep package as façade or delete) |
| `ai-gateway/cmd/ai-gateway/main.go:182` (webhook hook client) | `httpclient.New(httpclient.Config{Timeout: webhookTimeout})` |
| `ai-gateway/internal/handler/proxy.go:48` `NewUpstreamClient()` | `httpclient.New(...)` with explicit Dial/TLS config |
| `ai-gateway/internal/hooks/webhook.go:45` | `httpclient.New(httpclient.Config{Timeout: timeout})` |
| `shared/thingclient/http.go:25` | `httpclient.New(httpclient.Config{Timeout: 10 * time.Second})` |
| `shared/alertclient/client.go:54` | `httpclient.New(httpclient.Config{Timeout: cfg.HTTPTimeout})` |
| `compliance-proxy/internal/siem/sinks.go:98` (HTTPSink) | `httpclient.New(httpclient.Config{Timeout: 10 * time.Second})` (replace `http.DefaultClient`) |

### 4.6 Agent transport rewrite

> **Updated 2026-04-26.** The original framing of this section assumed
> the agent forwards intercepted flows to an upstream gateway and built
> a two-pool ("hot" gateway path + "escape" origin pool) model around
> that. Per the Trinity topology in `docs/dev/architecture.md`
> the agent terminates at the **original SNI/Host** directly and is
> unaware of any downstream redirection (DNS, PAC, transparent proxy)
> that ops may configure. The full redesign is in
> `docs/superpowers/specs/2026-04-26-agent-transport-rewrite-design.md`;
> what follows is the current, condensed version.

Today `packages/agent/core/network/proxy/proxy.go:291` calls `tls.DialWithDialer` for every intercepted flow. The replacement:

- **Single pool, no routing.** Agent holds **one** process-lifetime `*http.Client` constructed via `httpclient.New(httpclient.Config{ForceHTTP2: On(), H2ReadIdleTimeout: 30*time.Second, ...})`. All intercepted HTTPS requests are repackaged as outbound `http.Request` objects on this client, with the URL set to the original `SNI:port`. `http.Transport` does the per-host pooling and HTTP/2 multiplex automatically.
- **No "gateway" / "origin" split, no whitelist.** Topology (provider-direct vs compliance-proxy vs ai-gateway) is decided by ops at the network layer (DNS rewrite, PAC, transparent proxy) and is invisible to the agent.
- **Byte-level fallback preserved.** When the first decrypted byte stream cannot be parsed as HTTP (rare: HTTP/0.9, raw TCP-over-TLS, malformed clients), `MITMRelay` keeps the existing `Relay(clientTLS, serverTLS)` byte-pump branch with its own one-shot `tls.DialWithDialer` (unpooled).
- **MITM cert chain unchanged.** Agent continues to MITM client traffic with its locally-generated CA; only the outbound dial of the relay changes.
- **Streaming response handling unchanged.** HTTP/2 carries SSE/NDJSON natively; the existing `streaming.Accumulator` consumes `io.Reader` over the new path.

The Agent's existing `MITMRelay` keeps its public signature; only the outbound side is rewritten. There is no `relayBackend` interface in the implementation — the prior draft introduced one to front the two-pool model, which has been dropped.

### 4.7 Per-rule cache control

`RoutingRule` schema delta:

```prisma
model RoutingRule {
  // ... existing fields ...
  cacheEnabled      Boolean @default(true)  // disables response cache for this rule
  cacheTtlSeconds   Int     @default(0)     // 0 means inherit global TTL
}
```

`response cache` lookup flow change:

```
Lookup begins
  → resolve route (existing)
  → if matched rule.cacheEnabled == false → CacheStatusSkipPolicy, no lookup
  → else use rule.cacheTtlSeconds if > 0 else global TTL on Store
```

`response cache` HIT side-effect change:

```
On HIT:
  → return cached body (existing)
  → spawn non-blocking goroutine with bounded timeout (2s) to:
    - iterate hooks where `category == 'audit'` only; skip transform/redact/policy hooks
    - any returned response transformations from these audit hooks are discarded (the cached body is already final and must not be mutated)
    - emit traffic_event with cache_status=hit and zero token counts
  → quota.Reconcile is NOT called (zero tokens)
```

The audit-hook-on-hit spawning uses a `errgroup`-bounded background pool to avoid unbounded goroutine fan-out; if the pool is saturated, the hit still returns but emits a `nexus_cache_hit_audit_dropped_total` counter.

### 4.8 Metrics

New Prometheus counters (in `nexus` namespace):

```
nexus_cache_hits_total{cache="response", rule_id="..."}
nexus_cache_hits_total{cache="aiguard", config_id="..."}
nexus_cache_misses_total{cache="response", rule_id="..."}
nexus_cache_misses_total{cache="aiguard", config_id="..."}
nexus_cache_size{cache="snapshot_providers"}
nexus_cache_size{cache="snapshot_models"}
nexus_cache_size{cache="snapshot_credentials"}
nexus_cache_size{cache="key_virtual_keys"}
nexus_cache_size{cache="key_users"}
nexus_cache_size{cache="key_orgs"}
nexus_cache_size{cache="key_projects"}
nexus_cache_invalidations_total{cache="...", reason="push|ttl|manual"}
nexus_cache_hit_audit_dropped_total{cache="response"}
nexus_httpclient_requests_total{client="...", code="..."}
```

`rule_id` cardinality is bounded by the routing-rule count (low, low hundreds in practice), safe to label.

### 4.9 Lint enforcement

Add to root `.golangci.yml`:

```yaml
linters:
  enable:
    - forbidigo

linters-settings:
  forbidigo:
    forbid:
      - p: '^http\.DefaultClient$'
        msg: "use packages/shared/transport/httpclient.New instead of http.DefaultClient"
      - p: '&http\.Client\{'
        msg: "use packages/shared/transport/httpclient.New instead of bare http.Client literals"
    exclude-godoc-examples: false
    analyze-types: true
```

Test files are exempt by default `_test.go` exclusion.

## 5. Implementation Sequence

Order of work (these are sequence, not compatibility phases — each step lands fully and the previous step's surface goes away in the same commit):

1. `packages/shared/storage/configcache` package + unit tests (`SnapshotCache`, `KeyCache`, table-driven concurrent tests, TTL expiry test, singleflight collapse test).
2. `packages/shared/transport/httpclient` package; verify no behavioural drift vs. existing `specutil`; replace `specutil` callers and delete `specutil/http.go` (keep the package only if other helpers live there).
3. Replace six bare `http.Client` / `DefaultClient` callers with the new factory.
4. Migrate `routing_rules` and `hook_config` (already cached) to `SnapshotCache`. Verify behaviour parity. Delete bespoke cache implementations.
5. Add `SnapshotCache` for `providers`, `models`, `credentials`. Wire into `provtarget.Resolver` and `credentials.Manager`. Delete direct DB calls from hot path.
6. Add `KeyCache` for `virtual_keys`, `users`, `orgs`, `projects`. Wire into `vkauth.lookupVK` and any user/org/project resolution sites.
7. Migrate `quota.PolicyCache` to `SnapshotCache`. Fold model pricing lookup into the `models` snapshot.
8. Hub: extend message handler to support `op: invalidate, ids: [...]` payload form.
9. Control Plane: emit `NotifyConfigChange` (full reload) for `providers`, `models`; emit `NotifyInvalidate` (by-id) for `virtual_keys`, `users`, `orgs`, `projects`.
10. Delete Redis pub/sub branch from `shared/compliance/config_cache.go` and remove now-unused Redis pub client wiring in `ai-gateway` and `compliance-proxy`.
11. `RoutingRule` schema: add `cacheEnabled`, `cacheTtlSeconds`; Prisma migration; regenerate Go types; default values backfill.
12. Response cache: per-rule honouring + audit-hook-on-hit.
13. Agent transport rewrite (single shared `*http.Client` with per-host H2 pool; no escape pool — see `2026-04-26-agent-transport-rewrite-design.md`).
14. Prometheus metrics + lint rules.
15. Update `docs/dev/architecture.md` "Data-plane caches" subsection.
16. Verification: curl against `ai-gateway` with the seed VK; check returned body, `traffic_event` row, `nexus_cache_*` metrics.

## 6. Test Plan

### 6.1 Unit tests

- `configcache.SnapshotCache`: load, get-existing, get-missing, reload, concurrent-read-during-reload, atomic swap visibility.
- `configcache.KeyCache`: load, hit, miss, TTL expiry, singleflight (10 concurrent miss → 1 loader call), invalidate-by-key, invalidate-multi.
- `httpclient`: `New(default)` produces transport with expected pool params; `NewProbe` short-timeout; H2 negotiation enabled.
- `RoutingRule` cache-enabled false → `response cache.Lookup` returns `CacheStatusSkipPolicy`.
- Audit-hook-on-hit: mock cached body, mock audit hook, assert hook called once asynchronously.

### 6.2 Integration tests

Limited; the data-plane has no formal integration test harness today. Verification is via curl scripts (Section 6.3).

### 6.3 Manual verification (post-implementation, executed by Claude before reporting done)

Using seed VK `nvk_71a3418d79126ad1a8bac8d6930850b208b56e6b7c016eb000702912ac35cbc4`:

1. Start data-plane services (`ai-gateway`, `compliance-proxy`), restart if necessary.
2. Curl 1: `POST /v1/chat/completions` with the VK; assert 200 + valid OpenAI-shaped response.
3. Curl 2: same payload again; assert 200 + `X-Cache: HIT` header (cache hit) AND `traffic_event` row count incremented by 2 (audit-on-hit fired).
4. Query Prometheus `/metrics` endpoint of `ai-gateway`; assert `nexus_cache_hits_total{cache="response"} >= 1` and `nexus_cache_size{cache="key_virtual_keys"} >= 1`.
5. Query Postgres `traffic_event` table for the two events and confirm `cache_status` column reflects miss→hit.
6. Curl 3: `GET /v1/models` to exercise the `Model` snapshot cache path; assert success.
7. Sanity-check `ai-gateway` logs for "loaded N providers", "loaded N models", "loaded N credentials" entries on startup.

Pass criteria: all curls succeed; metrics are non-zero on the expected counters; `traffic_event` reflects the activity.

## 7. Risks & Mitigations

| Risk | Mitigation |
|---|---|
| `KeyCache` TTL expiry races with concurrent eviction-by-push | `KeyCache.Invalidate` and `Get` both take the LRU lock; the LRU library is internally synchronized. TTL check is read-after-fetch from the LRU; double-load is acceptable (singleflight collapses anyway). |
| New `invalidate` message form lands at older data-plane binaries | Pre-GA, no backwards compat. Old binaries are not deployed. |
| Snapshot reload on a busy hot path briefly stalls | Reload computes new map then `atomic.Pointer.Store`; readers see either old or new fully-populated map, never a partial. |
| Agent H2 long connection breaks under aggressive idle-timeout middleboxes | Configure H2 PING frames via `http2.Transport.ReadIdleTimeout = 30s`; transport will detect dead connection and reconnect transparently. |
| `forbidigo` rule false positives in test files | Test file exclusion is the linter default; if a test legitimately needs `http.DefaultClient` (it should not), use `// nolint:forbidigo` with comment. |
| Removing Redis pub/sub leaves a `compliance-proxy` deployed without `thingclient` connectivity stuck on stale config | `thingclient` HTTP fallback already covers this; if both WS and HTTP fallback are dead the service has bigger problems. The 2min `HookConfigCache` TTL is the final backstop. |

## 8. Open Items (deferred to plan/implementation)

- Exact pool capacity numbers for `KeyCache` per table — choose during implementation based on table size estimates. Defaults: VK 10000, User 1000, Org 100, Project 1000.
- H2 transport `ReadIdleTimeout` value — start with 30s, tune via observability.
- Prometheus label cardinality cap on `rule_id` — if a customer creates >500 rules this could become a memory concern; flag if seen in production.
