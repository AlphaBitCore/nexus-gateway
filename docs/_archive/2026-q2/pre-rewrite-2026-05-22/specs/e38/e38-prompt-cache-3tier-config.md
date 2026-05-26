# E38 — Prompt Cache 3-Tier Config (Sub-Epic Refactor)

> Epic: 38 (sub-epic of E38 Prompt Cache Friendliness)
> Status: Draft
> Date: 2026-05-13
> Architecture impact: `docs/users/product/architecture.md` § "Prompt Cache 3-Tier Config (E38-S13)"
> SDD: `docs/developers/specs/e38/e38-s13-prompt-cache-3tier-config.md`
> OpenAPI: `docs/users/api/openapi/admin/e38-s13-prompt-cache-3tier-config.yaml`

---

## 1. Background

Prompt-cache configuration in E38 today is stored in two unrelated places:

- `system_metadata['prompt_cache']` — normaliser pipeline switch, adapter-keyed rule overrides, per-provider Anthropic cache-marker knobs, vestigial `global` block.
- `system_metadata['gemini_cache']` — single global blob holding `enabled / min_system_chars / ttl_seconds / circuit_breaker_*`, applied to all Gemini providers identically.

Two separate Hub shadow keys (`prompt_cache`, `gemini_cache`) propagate them to the AI Gateway, each handled by its own subscription path. The Gemini knob set is **global** by design — there is no schema slot for per-provider tuning of TTL, threshold, or enable flag.

Two operational issues result from this split:

1. **Silent UI ↔ runtime drift.** `UpdateGeminiCacheConfig` (handler at `admin_extras.go:1751`) calls `Hub.NotifyConfigChange` and discards the error. A Hub blip during a UI save left prod in the state where `system_metadata.gemini_cache.enabled = true` while `thing.desired.gemini_cache.enabled = false` for weeks; cache-skipped requests were counted by `nexus_aigw_gemini_cache_skipped_total{reason="disabled"}` but no operator noticed because the UI faithfully showed "ON". Verified 2026-05-13.
2. **No per-provider Gemini tuning.** A "production Gemini" and "canary Gemini" provider must share TTL, threshold, and enable state. If one of them needs `cache_enabled=false` for canary-style A/B, the only path today is global-off — disabling caching for every Gemini provider in the platform.

UX symptom: the Gemini cache settings render under each Gemini provider's detail page, decorated with the orange disclaimer "This configuration applies globally to all Gemini providers." Operators correctly read the UI as "this is a per-provider control" and are surprised by the disclaimer; the section is mis-located versus its actual scope.

## 2. Functional Requirements

### FR-1: Three-tier configuration model

| ID | Requirement | Priority |
|---|---|---|
| FR-1.1 | All cache configuration is stored in three concrete scope tiers — **global** (one row), **adapter family** (one row per `adapter_type` value), **per-provider override** (one row per `Provider.id` that has any override) — plus the existing Provider table that joins the latter two. | Must |
| FR-1.2 | Effective configuration for a given Provider is computed by the resolution chain `provider_override ?? adapter_default ?? global_default ?? code_baked_default`, evaluated per individual knob (a Provider may inherit some knobs while overriding others). | Must |
| FR-1.3 | Storage uses a `config JSONB` column per tier row, with the JSON document's shape governed by Go struct unmarshal validation. Adding a new cache knob does not require a DB migration. | Must |
| FR-1.4 | Each Tier-3 row's JSONB MUST only contain keys that are valid for the Provider's `adapter_type`. The CP handler validates on PUT and rejects 400 with a structured error if an admin tries to set Anthropic-only fields on a Gemini provider, or vice versa. | Must |
| FR-1.5 | Normaliser pipeline kill switch (`normaliser_enabled`) and cache master kill switch (`cache_master_kill_switch`) live only at Tier 1. They MUST NOT be overridable at Tier 2 or Tier 3. | Must |
| FR-1.6 | Normalisation Rule overrides (rule_id → `enabled`, `dry_run_always`) live at Tier 2 only, nested under `cache_adapter_config.config.rules.<rule_id>`. Per-provider rule override is **explicitly out of scope** for this story — rules affect cache-key computation (NormalizeKey runs before provider selection) and per-provider override would create cache-key inconsistency. | Must |

### FR-2: API surface

| ID | Requirement | Priority |
|---|---|---|
| FR-2.1 | `GET /api/admin/cache/global` returns the singleton Tier-1 config row. | Must |
| FR-2.2 | `PUT /api/admin/cache/global` replaces the Tier-1 config row. Validates the JSON body against the Tier-1 Go schema, persists, and emits a Hub `NotifyConfigChange(ConfigKey: "cache_config")`. On Hub failure returns HTTP 502 with a structured "propagation pending" error — the body is committed to DB but operator action is required. | Must |
| FR-2.3 | `GET /api/admin/cache/adapter/:adapter_type` returns the Tier-2 row for the given adapter family. 404 if the adapter is unknown. | Must |
| FR-2.4 | `PUT /api/admin/cache/adapter/:adapter_type` upserts the Tier-2 row. Same validation + Hub semantics as FR-2.2. | Must |
| FR-2.5 | `GET /api/admin/cache/provider/:provider_id` returns the Tier-3 override row. 200 with `{}` if no override exists (NOT 404 — caller should treat absence as "no override"). | Must |
| FR-2.6 | `PUT /api/admin/cache/provider/:provider_id` upserts the Tier-3 row. Same validation + Hub semantics. Additionally validates that every key in the JSON body is appropriate for the Provider's `adapter_type` (per FR-1.4). | Must |
| FR-2.7 | `DELETE /api/admin/cache/provider/:provider_id` removes the Tier-3 row entirely (equivalent to "reset all fields to inherit"). Same Hub semantics. | Must |
| FR-2.8 | `GET /api/admin/cache/effective?provider_id=<id>` returns the merged effective config plus per-field source tag (`provider-override` / `adapter-default` / `global-default` / `code-default`). Used by the provider detail UI to render override badges. | Must |
| FR-2.9 | `GET /api/admin/cache/overrides` returns the list of all Providers that have a non-empty Tier-3 row, with each one's effective-vs-default diff. Used by the global prompt-cache page's "Active Overrides" panel. | Must |
| FR-2.10 | All endpoints are IAM-gated by `admin:prompt-cache.{read,update}`. No new IAM resource is carved out (FR-9). | Must |

### FR-3: AI Gateway runtime behaviour

| ID | Requirement | Priority |
|---|---|---|
| FR-3.1 | The gateway subscribes to a single Hub shadow key `cache_config`. The previous `prompt_cache` and `gemini_cache` shadow keys are deleted in the same release; no compatibility shim. | Must |
| FR-3.2 | The shadow payload is a single JSON document with three top-level objects: `global`, `adapters` (map keyed by adapter_type), `providers` (map keyed by provider_id). The gateway holds all three in memory and resolves per-provider effective config in O(1) at request time. | Must |
| FR-3.3 | The Gemini cachedContent manager evolves from a single `Manager` into a `ManagerSet` keyed by `provider_id`. Each constituent `Manager` reuses the same Redis client, HTTP client, and metrics namespace but holds its own resolved `Config` snapshot and its own circuit-breaker counters. | Must |
| FR-3.4 | When the shadow payload arrives, `ManagerSet.Reload(payload)` is invoked atomically: for each Gemini/Vertex Provider known to the gateway, the effective config is resolved and a `*Manager` is created (or reloaded) with that config. Providers removed from the Provider list have their `Manager` torn down. The hot path is unaffected during reload (atomic pointer swap per Manager). | Must |
| FR-3.5 | The hot path lookup at request time is `mgrSet.Get(providerID).Inject(ctx, ...)`. If a Provider's adapter_type is not `gemini`/`vertex`, `Get` returns a sentinel no-op Manager (or `nil` — caller handles). | Must |
| FR-3.6 | The normaliser engine's `Config` (loaded from the same shadow payload) is identical in shape to its current form (NormaliserEnabled + Rules + Providers + Global), so existing call sites in `proxy.go` continue to work after the shadow source change. | Must |

### FR-4: UI surfaces

| ID | Requirement | Priority |
|---|---|---|
| FR-4.1 | `/ai-gateway/prompt-cache` displays three sequential panels: (a) **Global Defaults** — `normaliser_enabled` + `cache_master_kill_switch` toggles. (b) **Adapter Family Defaults** — a tabbed view with one tab per adapter_type that has cache knobs (anthropic, bedrock, gemini, vertex); each tab shows the family's defaults + a Rules sub-section. (c) **Active Provider Overrides** — a table listing every Provider with a non-empty Tier-3 row, with each row showing which fields are overridden + a clickable jump to the Provider detail. | Must |
| FR-4.2 | `/ai-gateway/providers/:id` Cache tab renders only the fields applicable to the Provider's adapter_type. Each field shows: effective value, source badge (`Inherited from adapter default` / `Inherited from global` / `Overridden`), a "Reset to default" button, and the inherited default value in muted text below the input. | Must |
| FR-4.3 | The previous Gemini cache section UI (the orange "globally to all Gemini providers" notice) is deleted from the Provider detail tab. The new per-provider section reads from `/api/admin/cache/effective` and writes to `/api/admin/cache/provider/:id`. | Must |
| FR-4.4 | All user-visible strings on the redesigned pages are i18n-keyed across en/zh/es (per CLAUDE.md i18n binding). | Must |

### FR-5: Reconcile / drift detection

| ID | Requirement | Priority |
|---|---|---|
| FR-5.1 | A new CP package `internal/configreconcile/` runs a goroutine every 60s. For the shadow key `cache_config` (plus `kill_switch`, `agent_settings`, `aiguard`, `virtual_keys` — Tier-7 scope) it compares the CP DB source of truth against `thing.desired.<key>` on each online thing of the relevant type. On drift, it logs a structured warning, increments `cp_config_drift_total{config_key,thing_type,thing_id}`, and re-invokes `Hub.NotifyConfigChange` once. | Must |
| FR-5.2 | The reconcile goroutine is started in `main.go` after Hub client init, with graceful shutdown on context cancellation. | Must |
| FR-5.3 | Per CLAUDE.md "production code, no mocks, no defer": the reconcile job ships in this story — not deferred to a later release. | Must |

### FR-6: NotifyConfigChange error capture (fix-all)

| ID | Requirement | Priority |
|---|---|---|
| FR-6.1 | Every CP handler that calls `Hub.NotifyConfigChange` MUST capture both return values and either retry with backoff or return HTTP 502 to the caller. The current "fire-and-forget" anti-pattern (return values discarded) is removed everywhere it appears, including but not limited to: `admin_extras.go:1690` (cache normaliser), `admin_extras.go:1751` (gemini cache), `admin_virtual_keys.go:37` (VK invalidate), `admin_aiguard.go:201` (aiguard config). | Must |
| FR-6.2 | After this story, the only handlers that may discard `NotifyConfigChange` errors are read-only or out-of-band auxiliary paths (none currently exist). A lint check or code review checklist enforces this going forward. | Should |

### FR-7: Migration / clean-up

| ID | Requirement | Priority |
|---|---|---|
| FR-7.1 | Per CLAUDE.md dev-phase rule "no data-migration code": the new tables are created fresh with seeded sensible defaults. The two old `system_metadata` rows (`prompt_cache`, `gemini_cache`) are deleted in the same migration. No compatibility shim reads from the old shape. | Must |
| FR-7.2 | The two old shadow keys (`prompt_cache`, `gemini_cache`) are deleted from `thing.desired` for the ai-gateway thing in the migration. They are not re-emitted in any future code path. | Must |
| FR-7.3 | The 9 stale offline dev `thing` rows (`thing-gw-01`, `thing-cp-01`, `thing-proxy-01`, `agent-dev-*`, `agent-<hex>` from 5-10 onwards) are DELETEd in the same migration so the drift view shows only real prod nodes after this release. | Should |

### FR-8: Audit log

| ID | Requirement | Priority |
|---|---|---|
| FR-8.1 | Every PUT/DELETE to a cache config endpoint writes an audit row via the existing `audit.EntryFor` pattern, with `iam.ResourceSettings` (or new `iam.ResourcePromptCache` if/when carved) and the before/after JSONB diff in `AfterState`. | Must |

### FR-9: IAM impact

| ID | Requirement | Priority |
|---|---|---|
| FR-9.1 | No new IAM resource is created. The existing `prompt-cache` resource with `{read, update}` verbs covers all new endpoints. Managed policies that grant `admin:prompt-cache.update` continue to work without modification. The `super_admin` policy already grants this; per-role policies are unchanged. | Must |
| FR-9.2 | Per CLAUDE.md "API / menu / route changes require IAM impact review (binding)": this story's diff is reviewed against `packages/control-plane-ui/src/routes/shellRouteConfig.tsx` (no sidebar change), `packages/control-plane/internal/iam/managed.go` (no policy seed change), and `tools/db-migrate/seed/seed.ts` (no IAM seed change). | Must |

## 3. Non-Functional Requirements

| ID | Requirement | Priority |
|---|---|---|
| NFR-1 | Per-request hot path overhead introduced by the 3-tier resolution layer is < 1 µs (3 map lookups + JSON struct merge once per shadow reload, not per request). | Must |
| NFR-2 | Shadow payload size at steady state ≤ 32 KB for 25-Provider prod (well within the 1 MB Hub WebSocket frame limit). | Must |
| NFR-3 | Reconcile job adds < 50 ms of DB load per minute under normal conditions; less than 1 second under maximum drift scenario. | Must |
| NFR-4 | All new endpoints respond p50 < 50 ms, p99 < 200 ms under nominal load (single-row DB read + JSON marshal). | Must |
| NFR-5 | Migration applies on prod's ~50 MB DB in < 5 seconds. Single-migration deploy path (per `prod-deploy` skill) is used; full `prisma migrate deploy` is not invoked. | Must |
| NFR-6 | Concurrent writes to the same tier row (e.g. two admins editing the same adapter defaults) are serialised by row-level locking; last writer wins is acceptable but neither write is lost as a whole. | Must |
| NFR-7 | The 3-tier model gracefully handles unknown keys in stored JSONB (e.g. a knob removed from code but still present in DB): Go unmarshal silently drops them; no crash. New knobs added in code that are absent from DB fall back to the struct's zero value. | Must |

## 4. User Roles & Personas

- **Platform Operator (super_admin):** Tunes cache behavior for the entire platform. Uses `/ai-gateway/prompt-cache` to set global and adapter defaults; uses provider detail pages to set overrides.
- **AI Gateway Engineer (developer):** Reads effective config via API/UI when debugging cache miss issues. Cross-references shadow state via `prod-debug` skill or runtime introspection.
- **Read-only Auditor (viewer):** Has `admin:prompt-cache.read` only. Can browse all 3 tiers + effective view + overrides list but cannot save.

## 5. Constraints & Assumptions

- **CLAUDE.md dev-phase:** No backward-compatibility shims; old endpoints deleted in the same release; old DB rows deleted in the same migration; no `@deprecated` markers retained; no "Phase 1 keeps old behavior" plans.
- **No mocks / no stubs / no TODO in production code.** Test code may use Vitest/Go test doubles as appropriate.
- **English only** for all repository text (docs, comments, UI strings, commit messages).
- The set of cache knobs is bounded and code-defined: admin cannot create new knobs at runtime. Adding a knob is a code change to the relevant Go struct + UI form + i18n keys.
- The set of adapter families with non-trivial cache behavior is currently `{anthropic, bedrock, gemini, vertex}`. Other adapter types (openai, deepseek, moonshot, etc.) have no admin-tunable cache config and render the "fully provider-managed" info card on their provider detail page.
- The set of Normalisation Rules is code-baked; the DB only stores `enabled` and `dry_run_always` overrides.

## 6. Glossary

| Term | Definition |
|---|---|
| **Tier 1 / Global** | The singleton `cache_global_config` row. Contains knobs that affect every Provider regardless of adapter, e.g. the normaliser pipeline switch. |
| **Tier 2 / Adapter Family Defaults** | One row per `adapter_type` in `cache_adapter_config`. Contains the default knobs applied to every Provider of that adapter family, plus the rule override map. |
| **Tier 3 / Provider Override** | One row per Provider in `cache_provider_config` that has at least one knob overridden. Absence of a row = "fully inherits from Tier 2 + Tier 1". |
| **Effective Config** | The result of merging Tier 1 → Tier 2 → Tier 3 in order for a specific Provider. Each knob's value comes from the rightmost tier where it appears. |
| **Source Tag** | The label `provider-override` / `adapter-default` / `global-default` / `code-default` indicating which tier supplied a specific knob's effective value. |
| **Rule Override** | An entry in `cache_adapter_config.config.rules.<rule_id>` setting `enabled` / `dry_run_always` for that rule. The rule's other metadata (regex, body_path, adapter_type) is code-baked. |
| **ManagerSet** | The AI Gateway's per-provider Gemini cachedContent manager pool, `map[provider_id]*Manager`. Replaces the current singleton `Manager`. |
| **Shadow key `cache_config`** | The single Hub WebSocket key that carries the full 3-tier config payload from CP to AI Gateway. Replaces the old `prompt_cache` + `gemini_cache` key pair. |

## 7. Priority Summary (MoSCoW)

- **Must (MVP):** FR-1.* (entire 3-tier model), FR-2.1–FR-2.9 (all endpoints except the reset-single-field affordance, which is implemented via DELETE then re-PUT), FR-3.* (gateway runtime), FR-4.1–FR-4.3 (UI redesign), FR-5.* (reconcile), FR-6.1 (fire-and-forget fix all 4 sites), FR-7.1–FR-7.2 (clean migration), FR-8.1 (audit), FR-9.* (IAM).
- **Should:** FR-4.4 (i18n keys — graceful degradation possible if a key is missing), FR-6.2 (linter), FR-7.3 (orphan thing cleanup — operational nicety).
- **Could:** A per-knob revision history table for "who changed what when" beyond the audit log. Not in this story; deferred to a separate ops-driven request.
- **Won't (this story):** Per-provider rule override (rejected on cache-key correctness grounds — see FR-1.6). Will be revisited only if a non-key-safe rule with legitimate per-provider differentiation need arises.

---

## 8. Open Questions

None at spec time. All decisions are recorded as ADRs in the SDD.
