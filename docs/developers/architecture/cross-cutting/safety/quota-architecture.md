# Quota architecture

Quota in the Nexus Gateway is **cost-based**: every request charges USD-cent counters against a chain of subjects (virtual key → user or project → organization walk-up). Counters live in Redis with an in-memory fallback, partitioned by daily / weekly / monthly periods. Admin policy + per-target override drive the enforcement decision; the AI Gateway pre-checks before upstream dispatch and async-reconciles actual cost on success.

All enforcement runs through the hierarchical policy/override system under `packages/ai-gateway/internal/policy/quota/`: a chain walk that supports five enforcement actions (`allow`, `reject`, `downgrade`, `notify-and-proceed`, `track-only`) and emits a `429 QUOTA_EXCEEDED` response on reject.

Anchor packages:

- `packages/ai-gateway/internal/policy/quota/` — engine, chain builder, policy + override cache, usage cache, enforcement.
- `packages/ai-gateway/internal/ingress/proxy/proxy.go` — pre-check + post-commit reconcile call sites + response header emit.
- `packages/ai-gateway/internal/ingress/envelope/usage.go` — `/v1/usage` handler that surfaces the VK's current limit + used + remaining to clients.
- `packages/control-plane/internal/ai/quota/handler/` — admin CRUD for policies + overrides.
- `tools/db-migrate/schema/` — `QuotaPolicy`, `QuotaOverride` (`gateway.prisma`); `MetricRollup1h` (`observability.prisma`).

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

Counters are **USD cents only** (`int64`). The pre-check converts estimated tokens × model pricing to cents. The post-commit reconcile does **not** recompute cost from tokens × price — it charges the single canonical per-request cost the AI Gateway cost pipeline already computed once (`rec.EstimatedCostUsd`), the same value persisted to `traffic_event.estimated_cost_usd`, summed into `billed_cost_usd` by the Hub rollup, and re-seeded by the boot Backfill (§9). See [§2a](#2a-single-canonical-price-source) — one price source, one cost value, every path.

<a id="2a-single-canonical-price-source"></a>
### 2a. Single canonical price source

There is exactly **one** price authority: the **Model table**
(`Model.inputPricePerMillion` / `outputPricePerMillion` /
`cachedInputReadPricePerMillion` / `cachedInputWritePricePerMillion`). The
retired `provider_pricing` table is gone — both the cost pipeline
(`cache/layer/pricing.go` reads the Model snapshot) and the quota engine
(`store.GetModel` / `store.FetchModelPricing`) resolve every rate from it.

The cost is computed **once per request**, cache-aware (prompt-cache read/write
tokens decomposed at their own rates by `proxy_cachecost.go::computeCacheCosts`),
and lands in `rec.EstimatedCostUsd`. That single number flows, unchanged, to:

1. `traffic_event.estimated_cost_usd` (the persisted ledger),
2. the Hub rollup's `billed_cost_usd` — a **passthrough** of `estimated_cost_usd`
   for success + non-cache rows (the rollup never re-prices; see
   [metrics-rollup-architecture.md](../observability/metrics-rollup-architecture.md)),
3. the live quota counter via `Reconcile` (`ActualUsage.CostUSD = rec.EstimatedCostUsd`), and
4. the boot Backfill seed, which reads `metric_rollup_1h.billed_cost_usd` (§9).

Because all four read the same source and the same computed value, the live
enforcement counter and the rolled-up ledger cannot disagree for a given model,
including across a gateway reboot (audit F-0163). Before this unification the
reconcile recomputed `PromptTokens × InputPricePM + CompletionTokens ×
OutputPricePM`, which omitted cache-token decomposition and over-charged
prompt-cache-heavy traffic relative to `billed_cost_usd` — so the counter dropped
when the Backfill re-seeded from the cheaper billed figure on restart.

Reconcile converts actual cost to **milli-cents** and carries a per-subject sub-cent **remainder** across calls before committing whole cents to the counter. Without the carry, a sub-cent-per-call cost (`$0.009` mini / flash / haiku-class + embeddings) truncates to `0` cents every reconcile and the live counter never advances — unbounded intra-period under-enforcement. The carry map is keyed by subject (`targetType:targetID`) and bounded by the active-subject working set (it is not multiplied by period: a period rollover resets the subject's remainder in place, dropping at most `<1` cent of residual, which is negligible against a cost cap). The carry is per-process; on restart or across replicas each process keeps its own remainder, so the residual loss is at most `<1` cent per process per subject per period.

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
- `downgrade` — request proceeds but the routing layer picks the cheapest model that still satisfies the cap. The downgrade **budget** is the remaining headroom under the tightest enforced level — `min(LimitCents − CurrentCents)` across every level that carries a limit, floored at 0 — so the substituted model fits beneath every cap rather than an arbitrary fraction of the estimate. The selector (`SelectCheapestIndex`) **skips any candidate without a price row** (`store.ModelPricing.Priced == false`): an unpriced model prices to `$0` and would otherwise win the cheapest-fits comparison, then re-price to 0 and slip past the very cost cap that triggered the downgrade. A genuinely free model (a price row with zero rates, `Priced == true`) stays selectable; only a missing price row is rejected — the downgrade boundary fails closed exactly like the primary-model guard (§6, audit F-0348). If every candidate is unpriced, the selector returns `-1` and the request is rejected with `429 QUOTA_EXCEEDED`. After selecting the cheaper model the proxy **re-resolves the quota input/output prices from that model**, so the async reconcile and the row's `estimated_cost_usd` reflect what was actually run, not the original (more expensive) target. The proxy adds `X-Nexus-Quota-Downgrade: true` and `X-Nexus-Quota-Original-Model: <requested>` so clients see the substitution.
- `notify-and-proceed` — request proceeds; the proxy adds `X-Nexus-Quota-Warning: <Decision.Message>`; reconcile still runs.
- `track-only` — request proceeds, reconcile still runs, but the chain reports `Allowed: true` regardless of overage. Used for shadow / measurement deployments.

`Decision.Action` is the chain-wide collapsed action — the priority comparator picks the most restrictive across every level the request crossed.

## 6. Pre-check + post-commit reconcile

The hot path is a two-call pattern:

1. **Pre-check** (`QuotaEngine.Check`) runs before upstream dispatch. It reads each level's current period counter from Redis, computes `currentCents + estimatedCents > limitCents`, and returns the collapsed `Decision`. The estimated cost uses `CostEstimate{EstimatedInputTokens, MaxOutputTokens, InputPricePM, OutputPricePM}`, where the token figures are a deliberately-conservative heuristic, **not** the billed amount:
    - **Input** — `utf8.RuneCount(body) / 3` (floored at 1). Rune-based rather than byte-based so CJK text, where one rune maps to multiple tokens, is not under-counted; it over-estimates plain English, which is the safe direction for a cost cap.
    - **Output** — the caller's `max_tokens` when pinned (the provider cannot exceed it), otherwise a fixed `4096` default. An omitted `max_tokens` whose real completion exceeds 4096 is under-reserved at pre-check, but the post-commit reconcile corrects the counter to the actual usage, so the only exposure window is a single in-flight request.

    The pre-check is therefore an approximation; the authoritative figure is always the reconciled actual cost (step 2). A routed model with **no price row configured** (both `InputPricePM` and `OutputPricePM` unset — distinct from a model priced at 0, which is genuinely free) would estimate `$0` and bypass every cost cap; when a cost limit is actually enforced for the caller the pre-check **fails closed** with `429 QUOTA_MODEL_UNPRICED` rather than serving unaccounted spend. The same fail-closed guarantee extends to the `downgrade` boundary: an unpriced **downgrade-to** candidate is skipped, never selected and re-priced to 0 (§5, audit F-0348).
2. **Post-commit reconcile** (`QuotaEngine.Reconcile`) runs async after a 2xx response: `go h.deps.QuotaEngine.Reconcile(...)`. It increments every level with the single canonical per-request cost (`ActualUsage.CostUSD = rec.EstimatedCostUsd`, the cache-aware Model-table cost from §2a — **not** a second tokens × price recomputation). Each level is incremented under its **own** stamped period key — `Check` stamps a per-level `PeriodKey`, so a mixed-period chain (e.g. VK monthly + org daily) advances each counter under its own period rather than collapsing every level onto the first enforcing level's period (which would silently stop the off-period levels from enforcing). Levels that commit the same whole-cent amount under the same period collapse into one multi-level pipeline call (the common single-period case); divergent periods or sub-cent remainders split into separate pipeline calls.

The pattern is a **soft reservation**: the pre-check does not write counters; counters only advance when the request actually succeeds. A rejected pre-check or an upstream 4xx/5xx leaves counters untouched, which is the refund mechanism — there is no explicit `Refund` call.

## 7. Override > policy precedence

Both `QuotaPolicy` and `QuotaOverride` rows live in Postgres; the AI Gateway holds a Hub-pushed cache that the policy-CRUD admin endpoints invalidate on every mutation.

Per level, the engine looks up override first (by `targetType + targetID`), then policy (by `scope + organizationId + vkType`). Each unspecified override field inherits from the matching policy: a blank `enforcementMode` inherits the policy's mode, a blank `periodType` inherits the policy's period, and a blank (zero) `costLimitCents` inherits the policy's cost cap. The cost fallback is load-bearing — without it a blank-cost override would set `limitCents = 0`, skip enforcement, and silently **shadow** (disable) the policy cost cap at that level. An override row with empty everything is effectively a no-op — the policy row drives. The same inheritance applies in `Engine.VKLimit` so the `/v1/usage` quota block and the request-time headers report the inherited cap rather than "no limit".

Policies are ordered by `priority DESC`; the first matching row wins. `Enabled = false` rows are skipped at load time.

An override may carry an optional `expiresAt`. It is a *temporary* exception (it has a `reason`), so the column lets it self-revert (audit F-0161): `policy_cache.Load` selects only `WHERE "expiresAt" IS NULL OR "expiresAt" > NOW()`, so an expired override drops out of the enforcement cache on the next load and the target falls back to its applicable policy. `NULL` means the override never expires (the default for existing rows). The admin API rejects an `expiresAt` that is not in the future on create/update, and the update form clears it (restoring a permanent override) via the `expiresAtMode: "_inherit"` sentinel, mirroring the cost/mode/period inherit affordance.

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

Every mutation calls `hub.InvalidateConfigE("ai-gateway", "quota_policies")` (policies handler) or `…, "quota_overrides")` (overrides handler) so the AI Gateway picks up the new state on the next request. The CP DB commits first (source of truth), then the push runs; **a push failure returns HTTP 502 with a `propagation_error` envelope** (`code: HUB_PROPAGATION_FAILED`) and the success audit row is suppressed, so the admin retries instead of believing a new spend cap took effect while the fleet still enforces the old one.

`scope` on a policy in admin API bodies is `user | vk | project | organization`; the engine-side cache + seed rows use `virtual_key` (the dimension prefix on `metric_rollup_1h`) for the VK level — admin POST/PUT payloads must use the `vk` short form. `vkType` narrows to `personal` or `application` (or null = both). `organizationId` narrows the policy to a single org; null = applies to all orgs.

See [iam-identity-architecture.md](../../services/control-plane/iam-identity-architecture.md) for the IAM verb model.

## 9. Backfill from rollups

On AI Gateway boot the engine backfills the current period's counters from the `metric_rollup_1h` table — the canonical post-call cost ledger. The backfill query reads `dimensionKey` rows for all four enforcement-chain dimensions (`virtual_key=…`, `user=…`, `project=…`, `organization=…`) and seeds Redis with the sum of `billed_cost_usd` for the active period. The `project` dimension must be included — the enforcement chain adds a project level and reconcile increments the live project counter, so omitting it from the boot seed would let a Redis cold-start reset the project counter to `0` and grant a full extra budget of project overspend until live traffic re-accumulates.

The rollup table is fed by the audit pipeline (see [audit-pipeline-architecture.md](../observability/audit-pipeline-architecture.md)). Backfill is read-only against the rollup; reconcile continues to write live counters into Redis.

## 10. Response surface for clients

429 envelope is the flat gateway shape (see [error-taxonomy-architecture.md](./error-taxonomy-architecture.md)): `{"error":{"message":"…","type":"rate_limit_error","code":"QUOTA_EXCEEDED","hint":"…"}}` — `type` is derived from the HTTP status, `code` stays the Nexus discriminator. The `message` carries `Decision.Message` (e.g. `virtual_key quota exceeded: vk-… (45.00 / 50.00 USD)`); the `hint` carries a static operator-action string (`Check usage or request a quota increase`).

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
- `tools/db-migrate/schema/` — `QuotaPolicy`, `QuotaOverride` (`gateway.prisma`); `MetricRollup1h` (`observability.prisma`).
