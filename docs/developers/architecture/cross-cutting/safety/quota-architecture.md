# Quota architecture

Quota in the Nexus Gateway is **cost-based**: every request charges USD-cent counters against a chain of subjects (virtual key → user or project → organization walk-up). Counters live in Redis with an in-memory fallback, partitioned by daily / weekly / monthly periods. Admin policy + per-target override drive the enforcement decision; the AI Gateway pre-checks before upstream dispatch and async-reconciles actual cost on success.

All enforcement runs through the hierarchical policy/override system under `packages/ai-gateway/internal/policy/quota/`: a chain walk that supports five enforcement actions (`allow`, `reject`, `downgrade`, `notify-and-proceed`, `track-only`) and emits a `429 QUOTA_EXCEEDED` response on reject.

Anchor packages:

- `packages/ai-gateway/internal/policy/quota/` — engine, chain builder, policy + override cache, usage cache, enforcement.
- `packages/ai-gateway/internal/ingress/proxy/proxy.go` — pre-check + post-commit reconcile call sites + response header emit.
- `packages/ai-gateway/internal/ingress/envelope/usage.go` — `/v1/usage` handler that surfaces the VK's current limit + used + remaining to clients.
- `packages/control-plane/internal/ai/quota/handler/` — admin CRUD for policies + overrides.
- `tools/db-migrate/schema.prisma` — `QuotaPolicy`, `QuotaOverride`, `MetricRollup1h` models.

## 1. Subject chain

The pre-check assembles a **CheckChain** of levels in priority order, computed per request from the resolved `VKMeta`:

| Level | Condition | TargetType / TargetID |
|---|---|---|
| Virtual key | always | `virtual_key` / `meta.ID` |
| User | `meta.VKType` is `personal` or empty | `user` / `meta.OwnerID` |
| Project | `meta.VKType` is `application` | `project` / `meta.ProjectID` |
| Organization walk-up | every ancestor of `meta.OrganizationID` via `orgParents` map | `organization` / `<orgId>` |

The chain is evaluated end-to-end; each level looks up its policy + override independently. The **most restrictive Action wins** via a priority comparator (`reject > downgrade > notify-and-proceed > track-only > allow`). Two levels with the same Action collapse to one decision — the engine records all levels in `Decision.Levels` for the audit row.

User-level limits do not apply to application VKs, and project-level limits do not apply to personal VKs — the VKType branch in `BuildCheckChain` guarantees the chain excludes the irrelevant level.

## 2. Counter model + windows

Counters are **USD cents only** (`int64`). The pre-check converts estimated tokens × model pricing to cents; the post-commit reconcile converts actual usage × pricing the same way.

Three period types are supported:

- `daily` — `YYYY-MM-DD`
- `weekly` — `YYYY-Www` (ISO week)
- `monthly` — `YYYY-MM`

A counter is keyed by `(targetType, targetID, periodKey)` and is fixed-bucket — when the window rolls over the next request increments a brand-new key (no sliding window).

Storage is a `redis.UniversalClient` (standalone / sentinel / cluster shape) when Redis is configured; otherwise a thread-safe in-memory map is the fallback. The fallback is intentional for dev environments — the same engine code runs against either; quotas are correct per process but not shared across replicas.

## 3. Redis key + TTL

Key format: `quota:usage:{targetType}:{targetID}:{periodKey}` — `targetType` is one of the chain levels, `targetID` is the row UUID, `periodKey` matches the period type's date format above.

Atomic single-increment uses `INCRBY` + `EXPIRE` pipelined together; multi-level batches use a Redis pipeline so the chain commits in a single round-trip on the post-call reconcile.

TTL is computed once per key at first increment: start-of-next-period + a 1-hour buffer. The buffer covers reconcile traffic that arrives after the period boundary for requests that started before it.

## 4. Decision struct

The engine returns:

```go
type Decision struct {
    Allowed   bool
    Action    string       // "allow" | "reject" | "downgrade" | "notify-and-proceed" | "track-only"
    Message   string       // human-readable, e.g. "virtual_key quota exceeded: vk-… (45.00 / 50.00 USD)"
    QuotaID   string       // "override:<uuid>" or "policy:<uuid>"
    Levels    []CheckLevel // all levels evaluated; preserved for the audit row
    PeriodKey string       // the period that triggered the action
}
```

There is **no** `Remaining` or `Hint` field on `Decision`; the engine packs the usage detail into `Message` and lets the proxy handler surface it via response headers.

## 5. Action semantics

- `allow` — pass through; reconcile runs after success.
- `reject` — 429 `QUOTA_EXCEEDED`; request never reaches upstream; no reconcile.
- `downgrade` — request proceeds but the routing layer picks the cheapest model that still satisfies the cap. The proxy adds `X-Nexus-Quota-Downgrade: true` and `X-Nexus-Quota-Original-Model: <requested>` so clients see the substitution.
- `notify-and-proceed` — request proceeds; the proxy adds `X-Nexus-Quota-Warning: <Decision.Message>`; reconcile still runs.
- `track-only` — request proceeds, reconcile still runs, but the chain reports `Allowed: true` regardless of overage. Used for shadow / measurement deployments.

`Decision.Action` is the chain-wide collapsed action — the priority comparator picks the most restrictive across every level the request crossed.

## 6. Pre-check + post-commit reconcile

The hot path is a two-call pattern:

1. **Pre-check** (`QuotaEngine.Check`) runs before upstream dispatch. It reads each level's current period counter from Redis, computes `currentCents + estimatedCents > limitCents`, and returns the collapsed `Decision`. The estimated cost uses `CostEstimate{EstimatedInputTokens, MaxOutputTokens, InputPricePM, OutputPricePM}` — tokens come from the request-body tokeniser, prices from the resolved model.
2. **Post-commit reconcile** (`QuotaEngine.Reconcile`) runs async after a 2xx response: `go h.deps.QuotaEngine.Reconcile(...)`. It increments every level with the **actual** cents (from upstream usage stats) using the multi-level pipeline.

The pattern is a **soft reservation**: the pre-check does not write counters; counters only advance when the request actually succeeds. A rejected pre-check or an upstream 4xx/5xx leaves counters untouched, which is the refund mechanism — there is no explicit `Refund` call.

## 7. Override > policy precedence

Both `QuotaPolicy` and `QuotaOverride` rows live in Postgres; the AI Gateway holds a Hub-pushed cache that the policy-CRUD admin endpoints invalidate on every mutation.

Per level, the engine looks up override first (by `targetType + targetID`), then policy (by `scope + organizationId + vkType`). If the override's `enforcementMode` is empty, the policy's mode applies; same fallback for `periodType`. An override row with empty everything is effectively a no-op — the policy row drives.

Policies are ordered by `priority DESC`; the first matching row wins. `Enabled = false` rows are skipped at load time.

## 8. Admin CRUD

Routes register on the admin Echo group:

| Endpoint | IAM action | Notes |
|---|---|---|
| `GET /quota-policies` | `admin:quota-policy.read` | List with filters. |
| `POST /quota-policies` | `admin:quota-policy.create` | Insert + cache invalidate. |
| `GET /quota-policies/:id` | `admin:quota-policy.read` | |
| `PUT /quota-policies/:id` | `admin:quota-policy.update` | Update + cache invalidate. |
| `DELETE /quota-policies/:id` | `admin:quota-policy.delete` | Delete + cache invalidate. |
| `GET /quota-overrides` | `admin:quota-override.read` | |
| `POST /quota-overrides` | `admin:quota-override.create` | |
| `GET /quota-overrides/:id` | `admin:quota-override.read` | |
| `PUT /quota-overrides/:id` | `admin:quota-override.update` | |
| `DELETE /quota-overrides/:id` | `admin:quota-override.delete` | |

Every mutation calls `hub.InvalidateConfig("ai-gateway", "quota_policies")` (policies handler) or `…, "quota_overrides")` (overrides handler) so the AI Gateway picks up the new state on the next request.

`scope` on a policy in admin API bodies is `user | vk | project | organization`; the engine-side cache + seed rows use `virtual_key` (the dimension prefix on `metric_rollup_1h`) for the VK level — admin POST/PUT payloads must use the `vk` short form. `vkType` narrows to `personal` or `application` (or null = both). `organizationId` narrows the policy to a single org; null = applies to all orgs.

See [iam-identity-architecture.md](../../services/control-plane/iam-identity-architecture.md) for the IAM verb model.

## 9. Backfill from rollups

On AI Gateway boot the engine backfills the current period's counters from the `metric_rollup_1h` table — the canonical post-call cost ledger. The backfill query reads `dimensionKey` rows for the three covered dimensions (`virtual_key=…`, `user=…`, `organization=…`) and seeds Redis with the sum of `billed_cost_usd` for the active period. The `project` dimension is not part of the boot backfill — project-level counters seed from live reconcile traffic only.

The rollup table is fed by the audit pipeline (see [audit-pipeline-architecture.md](../observability/audit-pipeline-architecture.md)). Backfill is read-only against the rollup; reconcile continues to write live counters into Redis.

## 10. Response surface for clients

429 envelope is the gateway `proxy_error` shape (see [error-taxonomy-architecture.md](./error-taxonomy-architecture.md)): `{"error":{"message":"…","type":"proxy_error","code":"QUOTA_EXCEEDED","hint":"…"}}`. The `message` carries `Decision.Message` (e.g. `virtual_key quota exceeded: vk-… (45.00 / 50.00 USD)`); the `hint` carries a static operator-action string (`Check usage or request a quota increase`).

Successful requests carry quota-meta headers when the chain stamped a VK-level limit. The engine populates `CurrentCents` + `LimitCents` + `PeriodKey` on `Decision.Levels[VK-level]` during Check; the proxy reads them after the request is allowed and emits:

- `X-Nexus-Quota-Used` + `X-Nexus-Quota-Limit` — VK-level current/limit in USD, when an override or policy applies at the VK level.
- `X-Nexus-Quota-Downgrade: true` + `X-Nexus-Quota-Original-Model: <requested>` — when the `downgrade` action selected a cheaper model.
- `X-Nexus-Quota-Warning: <Decision.Message>` — on the `notify-and-proceed` action.

The `/v1/usage` admin endpoint (`packages/ai-gateway/internal/ingress/envelope/usage.go`) calls `Engine.VKLimit(ctx, vkMeta)` to build the response's `quota` block from the same VK-level resolution, so clients see consistent numbers between the request-time headers and the usage summary.

Clients should treat these as observational; the request body is unaffected by either surface.

## References

- `packages/ai-gateway/internal/policy/quota/chain.go` — subject chain builder.
- `packages/ai-gateway/internal/policy/quota/enforcement.go` — engine `Check` + Decision + action priority.
- `packages/ai-gateway/internal/policy/quota/usage_cache.go` — Redis key format, `INCRBY` + TTL.
- `packages/ai-gateway/internal/policy/quota/policy_cache.go` — policy + override cache.
- `packages/ai-gateway/internal/policy/quota/types.go` — `CostEstimate` + `ActualUsage`.
- `packages/ai-gateway/internal/ingress/proxy/proxy.go` — pre-check, reconcile, response headers.
- `packages/ai-gateway/internal/ingress/envelope/usage.go` — `/v1/usage` handler quota block.
- `packages/control-plane/internal/ai/quota/handler/policies.go` — policy CRUD.
- `packages/control-plane/internal/ai/quota/handler/overrides.go` — override CRUD.
- `packages/shared/identity/iam/catalog_data.go` — `ResourceQuotaPolicy` + `ResourceQuotaOverride` IAM verbs.
- `tools/db-migrate/schema.prisma` — `QuotaPolicy`, `QuotaOverride`, `MetricRollup1h` models.
