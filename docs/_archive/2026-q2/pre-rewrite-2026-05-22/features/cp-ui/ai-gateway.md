# AI Gateway section — CP-UI feature doc

> Audience: operators managing how AI traffic is routed, billed, throttled, and cached. This is the most code-intensive section of the dashboard.

## Pages in this section

| Page | Path | IAM action | Purpose |
|---|---|---|---|
| Providers & Models | `/ai-gateway/providers` | `admin:provider.read` | Provider catalogue (OpenAI, Anthropic, Gemini, ...) + per-provider models, capabilities, pricing |
| Credentials | `/ai-gateway/credentials` | `admin:credential.read` | Provider API keys (encrypted at rest); test connection on create |
| Credential Reliability | `/ai-gateway/credential-reliability` | `admin:credential.read` | Per-credential health rollup: error class breakdown, 429 burst, circuit state |
| Routing Rules | `/ai-gateway/routing` | `admin:routing-rule.read` | Strategy-tree rules with match conditions and fallback chains |
| Virtual Keys | `/ai-gateway/virtual-keys` | `admin:virtual-key.read` | Application-side credentials; per-VK model restrictions + quota policy binding |
| Quota Policies | `/ai-gateway/quota-policies` | `admin:quota-policy.read` | Sliding-window budgets attached at org / project / VK scope |
| Quota Overrides | `/ai-gateway/quota-overrides` | `admin:quota-override.read` | Time-bound exceptions to policy limits |
| Cache | `/ai-gateway/cache` | `admin:prompt-cache.read` OR `admin:semantic-cache.read` | Fleet-wide cache configuration — embedding model, semantic cache tuning, freshness rules, provider prompt cache. Replaces the legacy `/ai-gateway/prompt-cache` + `/ai-gateway/cache-embedding` split (2026-05-20). |
| Passthrough | `/ai-gateway/passthrough` | `admin:passthrough.read` | E48 emergency-passthrough toggle, activation history, auto-revert status |

## Common workflows

- **Onboard a new provider** — Providers → "New Provider" wizard → fill manifest + endpoints → add a credential → test connection → add a model entry. Cross-ref `provider-adapter-architecture.md` §8 for the engineering side.
- **Rotate a provider key** — Credentials → select cred → "Update Key" → CP encrypts + Hub signals AI Gateway dirty-set; next request uses the new key. No restart. Cross-ref `credentials-architecture.md` §2.
- **Add a routing rule** — Routing Rules → "New Rule" wizard → choose strategy (Single / Fallback / LoadBalance / Conditional / A/B Split / PolicyNarrowing) → set match conditions → set fallback chain `onClass` rules. Admin guard rejects rules that can never match (E47-S8).
- **Issue a Virtual Key for a new app** — Virtual Keys → "New VK" → scope to org/project → restrict to specific models → optionally attach a quota policy → secret shown once. Test with `tests/lib/auth.sh`-style curl.
- **Activate emergency passthrough** — Passthrough → choose tier (org / provider / route) → reason free-text → expiry (max 8h, default 1h) → confirm. Hub propagates within seconds. Auto-revert at expiry.
- **Tune cache fleet-wide** — Cache → adjust embedding model / semantic threshold / freshness rules / provider prompt cache → save → next request uses new policy. All cache config is fleet-wide; routing rules carry no cache surface.

## Key API endpoints

```
/api/admin/providers           [GET/POST/PUT/DELETE]
/api/admin/credentials         [GET/POST/PUT/DELETE]; POST /:id/test
/api/admin/credential-reliability  [GET]
/api/admin/routing-rules       [GET/POST/PUT/DELETE]
/api/admin/virtual-keys        [GET/POST/PUT/DELETE]
/api/admin/quota-policies      [GET/POST/PUT/DELETE]
/api/admin/quota-overrides     [GET/POST/PUT/DELETE]
/api/admin/cache/global        [GET/PUT]                        // 3-tier prompt cache config — Tier 1 (global)
/api/admin/cache/adapters      [GET]                            // list adapter overrides
/api/admin/cache/adapter/{adapter_type}  [GET/PUT]              // Tier 2 (per-adapter)
/api/admin/cache/provider/{provider_id}  [GET/PUT/DELETE]       // Tier 3 (per-provider)
/api/admin/cache/effective     [GET]                            // resolved view across tiers
/api/admin/cache/overrides     [GET]                            // diff vs Tier 1
/api/admin/cache/preview       [POST]                           // dry-run a config change
/api/admin/semantic-cache/config         [GET/PUT]              // L2 semantic-cache singleton
/api/admin/semantic-cache/prewarm        [POST]                 // FAQ corpus pre-warm
// Both mounted in packages/control-plane/internal/ai/cache/handler/semanticcache.go:116-119.
/api/admin/passthrough         [GET/POST]; POST /reset; GET /history
```

Every mutation emits `admin_audit` (cross-ref `admin-audit-log-coverage.md` §2).

## Traffic Event drawer — Costs breakdown (2026-05-21)

Every Traffic Event row's detail drawer renders a **3-section Costs breakdown** showing where the money went:

1. **Upstream provider cost** — predicted spend at current `Model` prices, decomposed into per-component rows (uncached input × `inputPricePerMillion`, cache_read × `cachedInputReadPricePerMillion`, cache_creation × `cachedInputWritePricePerMillion`, output × `outputPricePerMillion`). On gateway HIT rows the only rows that render are `uncached + output` (no upstream call → no provider-cache concept). Math closes to `estimated_cost_usd` exactly.
2. **Nexus internal-ops** — `embedding_cost_usd` (semantic-cache lookup) and `ai_guard_cost_usd` (internal ai-guard backend only — external-URL backends never bill us). The caption changes between `internalOpsNoteCounted` and `internalOpsNoteExcluded` based on the `excludeInternalOpsFromBilledCost` setting in `nexus-hub.yaml`.
3. **vs no-gateway baseline** — what this request would have cost without any gateway (= prompt × full input + completion × full output, no cache discounts). Subtract net actual paid to see total savings from gateway + provider-cache combined.

Sub-microdollar amounts (< $1e-6) render in scientific notation on the Traffic Event drawer (`packages/control-plane-ui/src/pages/traffic/audit-drawer/trafficAuditDrawer.tsx:58`) + the Traffic list cost column (`packages/control-plane-ui/src/pages/traffic/list/TrafficTab.tsx:202`), via the audit-grade `formatUsdSci` helper (`packages/control-plane-ui/src/lib/format.ts:204-219`) — e.g., `$3.0e-7` instead of the `<$0.000001` floor. All other surfaces (rollups, analytics, provider/VK usage) keep `formatUsd` (same file, lines 175-187), which floors to `<$0.000001` to avoid scientific notation on aggregated totals.

Architecture spec: `cost-estimation-architecture.md` § 3.3 (estimated_cost semantic), § 6.5 (3 HIT × MISS cases), § 6.6 (internal-ops cost).

## Failure modes & gotchas

- **Credential test on create fails** — surface the provider's exact error; do not silently disable. Common: bad key (`auth`), org-mismatched key (`auth`), rate-limited test (`Rate429`).
- **Routing rule "always 0-match"** — admin guard rejects before persist (E47-S8).
- **Quota policy hierarchy** — a child-policy that exceeds the parent-org cap binds at the parent. Surface "parent-bound" in the UI to avoid confusion.
- **Prompt Cache write requires `cache_control` markers** — for Anthropic, the request author marks segments; the route policy controls TTL but not the boundary placement. UI shows last-seen segment count.
- **Passthrough mandatory expiry** — UI rejects manual entry > 8h; default 1h. Auto-revert is non-negotiable (E48). Audit trail is always emitted regardless of bypass.
- **`credential-reliability` lag** — health rollup is computed by a Hub job at 5-minute cadence. The page shows "Last computed at" badge.

## Architecture references

- `docs/developers/architecture/services/ai-gateway/provider-adapter-architecture.md` — Providers + Models
- `docs/developers/architecture/cross-cutting/safety/credentials-architecture.md` — Credentials + Reliability
- `docs/developers/architecture/services/ai-gateway/routing-architecture.md` — Routing Rules + admin guard
- `docs/developers/architecture/cross-cutting/safety/quota-architecture.md` — Quota Policies + Overrides
- `docs/developers/architecture/services/ai-gateway/prompt-cache-architecture.md` — Prompt Cache page
- `docs/developers/architecture/cross-cutting/safety/error-taxonomy-architecture.md` — fallback `onClass` semantics
- `docs/developers/architecture/cross-cutting/foundation/multi-endpoint-coordination-architecture.md` §4 — Passthrough end-to-end flow
