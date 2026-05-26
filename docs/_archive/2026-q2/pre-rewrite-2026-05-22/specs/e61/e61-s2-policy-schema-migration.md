# E61-S2 — Policy Schema Migration + Audit Constants + ConfigKey Wiring

> Story: e61-s2
> Epic: 61
> Status: Draft
> Requirements: `docs/developers/specs/e61/e61-smart-response-cache.md` §FR-1.2, §FR-1.4, §FR-1.5, §FR-5
> Architecture: `docs/developers/architecture/services/ai-gateway/response-cache-architecture.md` §5; `docs/developers/architecture/cross-cutting/foundation/configuration-architecture.md` (§7 catalog + 14-layer rename sweep)
> Blocked by: none (foundational)
> Blocks: e61-s1 wire-up (skip-reason audit constants), e61-s3, e61-s4, e61-s6

## User Story

As a Gateway Admin, I want the per-route response-cache policy to carry separate Extract and Semantic configurations, with a master `skip_time_sensitive` toggle, so that I can tune each tier independently and the schema is ready to grow as future epics (E69, E70, E71) layer on more knobs.

## §0 — Architecture: two-layer split (L1 infrastructure + L2 policy)

**Decision** (2026-05-19): semantic cache configuration splits across two storage layers so embedding-model choice (infrastructure) stays decoupled from cache behaviour (policy).

| Layer | Storage | Owner | Change frequency | Cross-route shared? |
|---|---|---|---|---|
| **L1 — Embedding infrastructure** | `semantic_cache_config` singleton table (mirror of `ai_guard_config`) | Platform / ops | rare (1-2 model upgrades/year) | YES, fleet-wide (one Redis Vector index; vector space must match) |
| **L2 — Cache policy** | `routing_rule.response_cache_policy.semantic` JSONB | Per-route owner / compliance | frequent (toggle, threshold tuning) | NO — each route independent |

**Why split, not per-route embedding choice**: vectors produced by different embedding models occupy different vector spaces and use different dimensions (e.g., OpenAI text-embedding-3-small = 1536D, nomic-embed-text = 768D). Cosine similarity is undefined across spaces, so a per-route choice would force one Redis index per (model, dimension) tuple. A fleet-wide singleton uses one index with one fingerprint and lets every route share the same `vary_by`-partitioned cache.

**Effective-enabled cascade** the L2 reader evaluates per request:

```
effective_semantic_enabled(route)
    = L1.enabled                          AND
      L1.embedding_model_id IS NOT NULL    AND
      L2.enabled
```

The L1 `enabled` flag is the fleet-wide kill switch (incident response — flip to false to disable semantic cache everywhere instantly). L2 `enabled` is per-route opt-in.

**Fingerprint-driven index lifecycle**: when admin saves L1 with a different `embedding_model_id`, the L1 ConfigStore recomputes `embedding_fingerprint = sha256(provider_id || ':' || model_id || ':' || dimension)`; ConfigCache observes the change on next reload, emits a `semantic_cache.invalidate_all` job (E61-S3 task), and the Redis Vector index is dropped + recreated before the next write. Mirrors `ai_guard_config.backend_fingerprint`.

**Out of L1's scope** (deliberately per-route, no plans to ever promote): `threshold`, `embed_strategy`, `vary_by`, `allow_cross_model`. These are policy / risk / scope decisions owned by the route, not the platform.

**Forward-compat with multi-tenant**: when Nexus goes SaaS, `semantic_cache_config` grows an `org_id` PK column; the Redis index name format becomes `nexus:semantic-cache:v1:org:<id>`. L2 is already per-route which is already org-scoped via routing rule → org mapping. Zero L2 changes needed.



## Tasks

### T1 — Prisma migration

- T1.1 New migration directory under `tools/db-migrate/migrations/` (per CLAUDE.md unique-timestamp rule):
    - `<UTC>_e61_response_cache_dual_tier/migration.sql`
- T1.2 Migration SQL — **two-layer split** between embedding infrastructure (singleton, L1) and per-route policy (L2). See the architecture rationale in §0 below.
    - **L1**: new singleton table `semantic_cache_config` (mirrors `ai_guard_config` exactly):
        ```sql
        CREATE TABLE IF NOT EXISTS semantic_cache_config (
            id                      TEXT PRIMARY KEY DEFAULT 'singleton',
            embedding_provider_id   TEXT REFERENCES "Provider"(id) ON DELETE SET NULL,
            embedding_model_id      TEXT REFERENCES "Model"(id)    ON DELETE SET NULL,
            embedding_dimension     INT,
            -- Fingerprint = sha256(provider_id || ':' || model_id || ':' || dimension).
            -- L1 ConfigCache compares on load; mismatch → emit semantic_cache.invalidate
            -- job → Redis FT.DROPINDEX + FT.CREATE before the next write. Mirrors the
            -- ai_guard_config.backend_fingerprint flush trigger.
            embedding_fingerprint   TEXT NOT NULL DEFAULT '',
            redis_index_name        TEXT NOT NULL DEFAULT 'nexus:semantic-cache:v1',
            enabled                 BOOLEAN NOT NULL DEFAULT false,
            updated_at              TIMESTAMPTZ NOT NULL DEFAULT NOW(),
            updated_by              TEXT
        );
        INSERT INTO semantic_cache_config (id) VALUES ('singleton')
        ON CONFLICT (id) DO NOTHING;
        ```
    - **L2**: rewrite the `routing_rule.response_cache_policy` JSONB shape. The `semantic` sub-object **does NOT** carry embedding_provider_id / embedding_model_id — those are L1's responsibility:
        ```sql
        UPDATE routing_rule
        SET response_cache_policy = jsonb_build_object(
            'extract', jsonb_build_object(
                'enabled',  response_cache_policy->'enabled',
                'ttl',      response_cache_policy->'ttl',
                'vary_by',  CASE
                              WHEN (response_cache_policy->>'vary_by_vk')::boolean THEN 'vk'
                              WHEN (response_cache_policy->>'vary_by_user')::boolean THEN 'user'
                              ELSE 'none'
                            END
            ),
            'semantic', jsonb_build_object(
                'enabled',                       false,
                'threshold',                     0.96,
                'embed_strategy',                'system_plus_last_user',
                'vary_by',                       'vk',
                'allow_cross_model',             false,
                'max_entry_bytes',               262144,   -- 256 KiB; oversize entries skip L2 write
                'embedding_cost_ceiling_usd_per_day', null -- runaway-cost protection; null = no cap
            ),
            'skip_time_sensitive', true
        )
        WHERE response_cache_policy IS NOT NULL;
        ```
    - Add column `traffic_event.embedding_cost_usd DECIMAL(20,10) NULL`. Default null (most rows pre-E61 won't have one).
    - Add column `traffic_event.embedding_model_id TEXT NULL REFERENCES "Model"(id) ON DELETE SET NULL`. Records which embedding model produced the embedding for this request (Cache ROI per-model breakdown stays correct after L1 model swap; without this column the historical ROI loses model attribution). Foreign key uses SET NULL on delete so model-removal doesn't break old rows.
- T1.3 Prisma schema (`tools/db-migrate/schema.prisma`):
    - `model SemanticCacheConfig { ... @@map("semantic_cache_config") }` with the columns above. Regenerate Go codegen.
    - `TrafficEvent.embeddingCostUsd Decimal? @map("embedding_cost_usd") @db.Decimal(20, 10)`.

### T1a — IAM impact (per CLAUDE.md "API / menu / route changes require IAM impact review")

- T1a.1 `packages/shared/identity/iam/catalog_data.go`: add `{Name:"semantic-cache", Service:ServiceGateway, Verbs:[VerbRead,VerbUpdate]}` + `ResourceSemanticCache` convenience var. Same pattern as `prompt-cache` (already in the catalog). **Scope clarification (Round-2 review)**: this resource governs BOTH (a) the L1 embedding singleton (`/api/admin/semantic-cache/config`) AND (b) the fleet-wide time-sensitive freshness rule list (`/api/admin/cache/time-sensitive-patterns` + `/test` + per-rule PUT — S6 T5e). The resource name "semantic-cache" remains because both control surfaces share the same operator persona (Platform/Ops, not per-route owners) and the same blast radius (cluster-wide). A dedicated `cache-freshness-rules` resource was considered and rejected — would force admins to wire two separate IAM policies for the same operational role.
- T1a.2 `packages/control-plane/internal/identity/iam/managed.go`: NexusViewer fixture gains `admin:semantic-cache.read`.
- T1a.3 No new admin endpoint is required for the per-route policy (uses existing routing-rule PATCH); a new `/api/admin/semantic-cache/config` GET/PUT covers the L1 singleton — gated on `iamMW(iam.ResourceSemanticCache.Action(iam.VerbUpdate))` for writes.
- T1.3 Update Prisma schema declarative (`tools/db-migrate/schema.prisma`):
    - `TrafficEvent.embeddingCostUsd Decimal? @map("embedding_cost_usd") @db.Decimal(20, 10)`.
    - `TrafficEvent.embeddingModelId String? @map("embedding_model_id") @db.Text`.
    - `TrafficEvent.embeddingModel  Model?  @relation("EmbeddingModelOnTraffic", fields: [embeddingModelId], references: [id], onDelete: SetNull)` — the relation name disambiguates from `Model`'s existing back-reference on routing-rule rows.
    - Regenerate Go codegen.
- T1.4 Update seed (`tools/db-migrate/seed/seed.ts`) to write the new policy shape for the dev-seed routing rules. Also seed a placeholder `semantic_cache_config` row (id='singleton') with `enabled=false` and a `provider=openai, model=text-embedding-3-small` reference IF the dev seed has an OpenAI provider row; this lets developers enable L2 locally just by flipping `enabled=true` rather than picking provider+model from scratch.

### T2 — Audit constants

- T2.1 `packages/ai-gateway/internal/platform/audit/audit.go`:
    - Add `GatewayCacheSkipReasonTimeSensitive GatewayCacheSkipReason = "time_sensitive"`.
    - Add `GatewayCacheSkipReasonOversizeForEmbedding GatewayCacheSkipReason = "oversize_for_embedding"`.
    - Add failure-mode skip reasons (per `response-cache-architecture.md` §6):
      - `GatewayCacheSkipReasonValkeyUnavailable     = "valkey_unavailable"`
      - `GatewayCacheSkipReasonEmbeddingTimeout      = "embedding_timeout"`
      - `GatewayCacheSkipReasonEmbeddingProviderError = "embedding_provider_error"`
      - `GatewayCacheSkipReasonEmbeddingDimMismatch  = "embedding_dim_mismatch"`
      - `GatewayCacheSkipReasonSemanticSearchError   = "semantic_search_error"`
      - `GatewayCacheSkipReasonSemanticSearchTimeout = "semantic_search_timeout"`
      - `GatewayCacheSkipReasonSemanticReindexInProgress = "semantic_reindex_in_progress"`
      - `GatewayCacheSkipReasonSemanticUnavailable   = "semantic_unavailable"`
      - `GatewayCacheSkipReasonEmbeddingCircuitOpen  = "embedding_circuit_open"`  (Round-1 review — circuit breaker)
      - `GatewayCacheSkipReasonEmbeddingBudgetExceeded = "embedding_budget_exceeded"`  (Round-1 review — per-route daily ceiling)
    - The pre-existing `GatewayCacheKindSemantic = "semantic"` constant (line 173) stays — code paths in S4 / S5 finally write it.
- T2.2 `packages/ai-gateway/internal/platform/audit/audit_test.go`:
    - Add tests asserting the two new skip reasons round-trip through `audit.Record` → DB row → read-back.

### T3 — configKey constants + Hub shadow wiring

- T3.1 `packages/shared/schemas/configkey/`:
    - Add typed constants for:
        - `response_cache.time_sensitive_patterns` (Category B — cluster-wide list, pushed to all ai-gateway Things).
        - The per-route `response_cache_policy.*` keys are already shadow-managed via the routing-rule shadow; no new keys here.
    - Update `ValidByThingType` to include the new key for `thing_type = "ai-gateway"`.
    - Update `TypedRegistry` with the schema for the rule-list payload (matches the `freshness.Rule` shape from S1).
- T3.2 Hub seed (`tools/db-migrate/seed/data/...`):
    - Seed `system_metadata` row for `response_cache.time_sensitive_patterns` with the S1 seed JSON.
- T3.3 ai-gateway `OnConfigChanged` callback:
    - On `response_cache.time_sensitive_patterns` change → reload rules → atomically swap the `*freshness.Detector` via the singleton wrapper.
    - On routing-rule shadow change → reload routing snapshot (existing path).

### T4 — yaml + env defaults

- T4.1 `packages/ai-gateway/ai-gateway.dev.yaml`: add a default block (commented) showing the new policy shape — purely documentary; runtime values come from the DB.
- T4.2 `.env.example` at repo root: no new env vars (no secrets in this story).
- T4.3 Verify the configuration-architecture.md §6.5 14-layer rename sweep does NOT apply here (we're adding fields, not renaming) — no `npm run check:rename` needed.

### T5 — Tests

- T5.1 Prisma migration round-trip: seed an old-shape row, run migration, assert the new shape matches expectations. Lives in `tools/db-migrate/test/`.
- T5.2 audit-constant round-trip tests as in T2.2.
- T5.3 configKey schema validation tests: assert that a malformed `time_sensitive_patterns` payload is rejected at shadow-write time.
- T5.4 ≥95% coverage on the touched Go files.
- T5.6 — **L1-precondition validator** (Round-2 review). When the policy CRUD handler accepts a payload with `semantic.enabled=true`, validate that L1 is also configured:
    - `semantic_cache_config.embedding_model_id IS NOT NULL`. If null → reject `400 invalid_semantic_l1_absent` with body `{"error":"Cannot enable semantic cache on a route while the fleet-wide embedding model is unconfigured. Visit Settings → Cache Embedding to configure it first."}` (S6 T5.1a shows the banner that prevents this in the UI; the API validator is the backstop).
    - The validator does NOT check `semantic_cache_config.enabled` — admin is allowed to configure routes ahead of flipping the fleet kill switch. The effective-enabled cascade in `response-cache-architecture.md` §3.2 handles the runtime gating.
    - Implementation: extend the `validateVaryByCompat` helper from T5.5 into `validateSemanticPolicy` covering both checks.

- T5.5 — **`vary_by=user` validation gate**. When the policy CRUD handler accepts a payload with either `extract.vary_by="user"` OR `semantic.vary_by="user"`, validate that the routing rule can actually populate `user_id` on the request. The validator checks:
    - The rule's resolved chain includes an upstream-auth step that stamps `user_id` on the audit Record (today: SSO+JWT path, ID-token path, or admin-API path stamps it; raw VK-only path does NOT).
    - If no `user_id`-stamping step is present, the handler returns `400 invalid_vary_by` with body `{"error":"vary_by=user requires an upstream-auth path that stamps user_id; route '<name>' uses VK-only auth which has no user identity."}`.
    - Implementation: add `validateVaryByCompat(rule, policy)` helper called inside the existing routing-rule PUT handler. Unit-test with VK-only route + vary_by=user → reject; SSO route + vary_by=user → accept.
    - Reason: a route silently behaving as `vary_by=none` when admin set `user` is a debugging nightmare — admin sees the config but cache reuses across users anyway. Fail fast at save time.

## Acceptance Criteria

- A1: Existing routing rules' `response_cache_policy` JSONB rows are rewritten in place to the new shape on migration; no row is dropped, no null values introduced where the old shape had a value.
- A2: `traffic_event.embedding_cost_usd` column exists, type `DECIMAL(20,10) NULL`, indexed-not-required (no query plan depends on it yet).
- A3: Both new skip-reason constants flow through `audit.Record` → Prisma row → admin API → CP-UI without serialization loss.
- A4: configKey constants pass the `npm run check:configkey-coverage` lint (if it exists; otherwise lint passes manually).
- A5: Hub shadow push of a modified `time_sensitive_patterns` payload triggers `OnConfigChanged` in ai-gateway within ≤5s and the new rules are visible to the next request.
- A6: No reference to `vary_by_user` / `vary_by_vk` (old flat field names) remains in non-migration Go code after S2 ships.

## Out of Scope (S2)

- The actual algorithm that uses the new skip reasons — wired in S1 + S4.
- L2 read/write logic — S3 + S4.
- UI controls for the new policy fields — S6.
- Embedding provider seed — S5.
