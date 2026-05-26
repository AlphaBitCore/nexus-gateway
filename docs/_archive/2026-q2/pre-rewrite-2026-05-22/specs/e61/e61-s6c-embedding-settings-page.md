# E61-S6c — Embedding Settings Page (singleton, mirror of AIGuardPage)

> Story: e61-s6c
> Epic: 61
> Status: Draft
> Requirements: `docs/developers/specs/e61/e61-smart-response-cache.md` §FR-3, §FR-7
> Architecture: `docs/developers/architecture/services/ai-gateway/response-cache-architecture.md` §3.2; `docs/developers/specs/e61/e61-s2-policy-schema-migration.md` §0 (two-layer split rationale)
> Blocked by: e61-s2 (`semantic_cache_config` table), e61-s3 T1a (ConfigStore + Admin API), e61-s5 (Provider system + embedding-probe endpoint)
> Blocks: e61-s6 (read-only chip + deep-link target), e61-s7 (smoke)

## User Story

As a Platform Admin, I want a dedicated **Cache Embedding Settings** page where I configure the fleet-wide embedding model once — provider, model, dimension probe — and where flipping the model triggers a Redis Vector index rebuild, so that all semantic-cache reads and writes use a consistent vector space and individual route owners never accidentally fragment the cache by picking different models.

## Background

Created 2026-05-19 from the E61 architecture review (E61-S2 §0). The
two-layer design splits L1 embedding infrastructure (this page) from
L2 per-route policy (E61-S6 Cache Settings page). This page **mirrors
AIGuardPage 1:1** so a reviewer reading both sees the same shape, and
the shared `<ProviderModelPicker>` component (committed 2026-05-19 as
6fb8d4a0) is reused as-is with `endpointType="embedding"`.

## Tasks

### T1 — Route registration

- T1.1 `packages/control-plane-ui/src/routes/shellRouteConfig.tsx`: add a new route entry under the existing "Settings" section, label `Cache Embedding`, path `/settings/cache-embedding`. IAM gate: `admin:semantic-cache.read` for view + `admin:semantic-cache.update` for the Save button (per E61-S2 T1a IAM catalog additions).
- T1.2 `packages/control-plane-ui/src/components/ui/Sidebar/Sidebar.tsx`: icon mapping for the new entry. Pick an icon that signals "embedding / vector / index" — `<Database>` from the icon library is acceptable.
- T1.3 i18n keys in 3 locales (`pages:settings.cacheEmbedding.*`) per CLAUDE.md i18n binding.

### T2 — Page layout (mirror AIGuardPage)

- T2.1 `packages/control-plane-ui/src/pages/settings/cacheEmbedding/CacheEmbeddingPage.tsx` (new) with structure:
    - `<PageHeader title="Cache Embedding" subtitle="...">`.
    - **Section 1 — Provider + Model picker**: `<ProviderModelPicker endpointType="embedding" providerLabel="..." modelLabel="..." helpText="...">` reading from `systemApi.listModels()` filtered to embedding providers. Identical shape to AIGuardPage's picker block.
    - **Section 2 — Index lifecycle warning banner** (red, always visible above the picker):
        > "⚠ Changing the embedding model triggers a **full rebuild of the semantic-cache index**. All existing semantic-cache entries will be invalidated and replayed on the next request. This is safe but means a transient cache-miss spike on the first ~5 minutes after save."
    - **Section 3 — Fleet kill switch**: a single `<Switch>` for `enabled` (defaults to off). When `embedding_provider_id` or `embedding_model_id` is null, the switch is force-disabled with a helper tooltip "Pick a provider + model first".
    - **Section 4 — Probe button**: when both fields are set, shows `<Button>Run Embedding Probe</Button>` calling `/admin/providers/:id/embedding-probe` (E61-S5 endpoint). On success, render the result chip inline with **latency-vs-budget context** (P4 Round-1 review): `Latency: 87ms / budget 100ms · Dimension: 1536 · Confirmed: text-embedding-3-small`. Latency value is colour-coded — green if <50ms, amber 50-90ms, red >90ms (relative to the 100ms hard timeout from `response-cache-architecture.md` §3.11). Tooltip on hover: "Embedding latency budget is 100ms — calls exceeding this hard-timeout fall through to the broker (L2 disabled per-request). Provider/network values near the budget signal that the L2 hit rate will be lower than capacity." On 4xx/5xx, render an error banner with the response message.
    - **Section 5 — Status panel**: read-only display of current L1 state — provider name, model name, dimension, fingerprint last-8-chars, updated_at, updated_by, `redis_index_name` (with tooltip explaining "Auto-versioned `:v1` → `:v2` → ... on every embedding-model swap to enable blue/green index switching — see response-cache-architecture.md §3.5"), and **circuit breaker state** (Round-1 review): `closed / half_open / open` with trip-count and last-trip timestamp. Mirror AIGuardPage's "what's currently active" panel idiom.
    - **Section 6 — Emergency disable** (P3 Round-1 review): a prominent red `<Button>Disable L2 fleet-wide</Button>` directly flips the `enabled` toggle to `false` and saves immediately (skipping the reindex modal — no model change). One-click safety net for operators who notice L2 poisoning traffic. Reverse is a routine save. The button has a `<ConfirmDialog>` ("This disables semantic cache cluster-wide. Existing requests in flight finish. New requests skip L2 immediately. Continue?") to prevent fat-finger.

    **Surfaced ALSO on the Cache ROI page header and on the Traffic Audit Drawer's cache-hit banner** as a "Disable L2 fleet-wide" inline action — operators don't have to navigate to find it during an incident. **Cross-file ownership clarification (Round-2 review)**: S6c owns the *action* (the PATCH that flips L1.enabled=false). The *button placements* outside this page (Cache ROI page header + audit drawer cache-hit banner) are delivered by their respective UIs: the Cache ROI page is updated as part of E61-S7 docs/UI pass (the page that consumes the `e61-s7-cache-roi-fields.yaml` data is the right place); the Traffic Audit Drawer modification is a focused PR on `trafficAuditDrawer.tsx` and is tracked as a separate small task. Both consumers import the same `useDisableL2FleetWide()` hook S6c exports.

### T3 — Save behaviour

- T3.1 `<Button onClick={save}>` PUTs `/api/admin/semantic-cache/config` with the picker's selection + the toggle. The server-side handler (E61-S3 T1a.5) recomputes the fingerprint and saves; on success returns the new row.
- T3.2 **Fingerprint-change confirmation modal**: when the local draft's (provider_id, model_id) differs from the server-loaded row, the Save click first opens a confirmation modal warning that the index will rebuild. Admin must explicitly confirm. Cancel = no save. The modal text:
    > "You're about to change the embedding model from **<old name>** to **<new name>**. This drops the current semantic-cache index and rebuilds it with the new model's dimension (<old dim>D → <new dim>D). Cached responses become unmatchable; the gateway will start writing fresh entries on the next eligible request. Continue?"
- T3.3 On confirm, show a toast "Configuration saved. Index rebuild in progress." and refetch the config (which surfaces the new fingerprint).
- T3.4 If the toggle is the only field changed (no provider/model change), no modal — direct save.

### T4 — Service layer

- T4.1 `packages/control-plane-ui/src/api/services/cache/semanticCacheConfig.ts` (new) exporting `semanticCacheConfigApi.getConfig()` + `.saveConfig(payload)` + `.runProbe(providerId)`. queryKey conventions per CLAUDE.md: `['admin', 'semantic-cache', 'config']`, `['admin', 'providers', 'embedding-probe', providerId]`.
- T4.2 Types in `packages/control-plane-ui/src/api/types.ts`: `SemanticCacheConfig` interface (id, embeddingProviderId, embeddingModelId, embeddingDimension, embeddingFingerprint, redisIndexName, enabled, updatedAt, updatedBy). Mirrors `AIGuardConfig`.

### T5 — Vitest tests

- T5.1 Renders the picker + warning banner + kill-switch (smoke).
- T5.2 Fingerprint-change modal opens when (provider, model) edited; does not open when only `enabled` edited.
- T5.3 Save button disabled while `enabled=true` but provider/model unset (defence against the impossible-state).
- T5.4 Probe button hidden until both provider + model are picked.
- T5.5 i18n key presence in all 3 locales (EN/ZH/ES).

### T6 — Acceptance criteria

- A1: Admin can land on `/settings/cache-embedding`, see the current L1 row, pick a different provider+model, see the rebuild warning, confirm, and observe the updated fingerprint after save.
- A2: When L1 has no provider/model set, the page renders the empty state + the toggle is locked off.
- A3: Probe button surfaces the embedding model's actual dimension within 5s for typical providers.
- A4: Changing only `enabled` (no provider/model change) does NOT open the rebuild-confirmation modal.
- A5: i18n + design-tokens + tsc + vitest all clean.
- A6: IAM gates work — a viewer-only IAM identity can load the page but the Save button is disabled with a tooltip explaining why.

## Out of Scope (S6c)

- The actual Redis Vector index rebuild logic — that's the Hub job consumer landing in E61-S3 T1a.3.
- Multi-tenant variants (per-org embedding model) — captured in E61-S2 §0 as v2 work; out of S6c.
- Migration from old per-route embedding fields to the new singleton — covered by E61-S2 T1.2 SQL UPDATE.

## Why this story exists (mirror of AIGuardPage)

| AIGuard analog | CacheEmbeddingPage |
|---|---|
| `ai_guard_config` singleton | `semantic_cache_config` singleton |
| Backend-mode radio (configured / external) | (no analog — embedding always uses a Provider row) |
| Provider + model cascading select | `<ProviderModelPicker endpointType="embedding">` |
| `backend_fingerprint` → cache flush on swap | `embedding_fingerprint` → Redis index flush on swap |
| ConfigCache hot-reload via shadow | ConfigCache hot-reload via shadow (E61-S3 T1a.2) |
| `/api/admin/ai-guard/config` GET/PUT | `/api/admin/semantic-cache/config` GET/PUT |
| AIGuardPage IAM: `admin:ai-guard-config.*` | This page IAM: `admin:semantic-cache.*` |
| Probe / classify-test panel | Embedding probe button |

A reviewer who knows ai-guard's settings page should be able to skim
this page in 60 seconds. The code structure follows the same file
layout (`AIGuardPage.tsx`, `AIGuardPage.module.css`,
`AIGuardPage.test.tsx`).
