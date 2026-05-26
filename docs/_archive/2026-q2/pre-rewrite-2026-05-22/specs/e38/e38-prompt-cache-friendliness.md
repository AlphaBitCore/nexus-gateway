# E38 — Prompt Cache Friendliness

> Epic: 38
> Status: Approved
> Date: 2026-05-07
> Spec seed: `docs/_archive/2026-q2/brainstorms/2026-05-06-prompt-cache-friendliness-brainstorm.md`
> Architecture impact: `docs/users/product/architecture.md` § "Prompt Cache Friendliness (E38)"

---

## 1. Background

The Nexus AI Gateway proxies LLM traffic to 20 provider adapters. For
multi-turn chat workloads (Claude Code, Cursor, agentic loops) the
upstream provider's own prompt cache is the highest-leverage cost
control: Anthropic charges 0.1× input price on cache reads; OpenAI
charges 0.5×. However, cache hit rates for Gateway-proxied traffic are
near 0% today because:

- **Root cause A**: per-request volatile bytes in the request body
  (e.g. Claude Code's `cch=<hex>` billing nonce) change the bytes
  seen by the provider on every turn, preventing prefix cache reuse.
- **Root cause B**: clients typically do not set `cache_control`
  markers, so the Anthropic provider never knows where to anchor its
  cache.

E38 fixes both root causes in phases, covering all 20 provider adapter
types, and adds the observability and cost-model layer needed to
measure and report savings.

---

## 2. Functional Requirements

### FR-1: Provider cache metric extraction (all phases)

| ID | Requirement | Priority |
|---|---|---|
| FR-1.1 | For every request to a Group A provider (anthropic, bedrock-Claude), extract `cache_creation_input_tokens` and `cache_read_input_tokens` from upstream responses (streaming and non-streaming) and persist to `traffic_event`. | Must |
| FR-1.2 | For every request to a Group B provider (openai, azure-openai, deepseek, glm), extract `prompt_tokens_details.cached_tokens` and persist to `traffic_event.cache_read_tokens`. | Must |
| FR-1.3 | For every request to a Group C provider (gemini, vertex) where a cache hit occurs, extract `cachedContentTokenCount` and persist to `traffic_event.cache_read_tokens`. | Should (Phase 4) |
| FR-1.4 | Extraction must work for both streaming (final usage frame) and non-streaming (usage object) responses. | Must |

### FR-2: Cost model

| ID | Requirement | Priority |
|---|---|---|
| FR-2.1 | A `provider_pricing` table stores per-model input/output/cache-write/cache-read prices. Seeded with official public prices at migration time. | Must |
| FR-2.2 | On every audit write, compute `cache_write_cost_usd`, `cache_read_savings_usd`, `cache_net_savings_usd` from token counts × prices and persist to `traffic_event`. | Must |
| FR-2.3 | Operators can override prices per Provider (enterprise contract discounts). | Should |

### FR-3: Nexus L1 cache key normalisation (L0)

| ID | Requirement | Priority |
|---|---|---|
| FR-3.1 | The Nexus L1 cache key is computed from a normalised body where rules marked `key_normalize_safe: true` have been applied. The upstream bytes are NOT modified. | Must |
| FR-3.2 | L0 runs on every request regardless of the global normaliser switch state. | Must |
| FR-3.3 | L0 failure must be fail-open: any panic or error falls back to using the original body for key computation. | Must |

### FR-4: Body normaliser rule engine (L3)

| ID | Requirement | Priority |
|---|---|---|
| FR-4.1 | A rule engine processes request bodies before upstream dispatch, stripping volatile non-semantic bytes per adapter_type. | Must |
| FR-4.2 | The engine is controlled by a global `normaliser_enabled` flag in `system_metadata`. Default: false. | Must |
| FR-4.3 | Rules are defined per `adapter_type` (providers.Format), not per domain or VK. | Must |
| FR-4.4 | Each rule has an `enabled` flag and a `dry_run_always` flag. When `dry_run_always=true` the engine records match statistics even when the rule is disabled. | Must |
| FR-4.5 | Fail-open: any rule panic, error, or timeout (>1 ms per rule) falls back to the original body. No request is ever blocked by the normaliser. | Must |
| FR-4.6 | Per-rule circuit breaker: 10 errors in 60 s → rule auto-disables with alert; manual re-enable required via Admin UI. | Must |
| FR-4.7 | Bundled rule: `anthropic / claude-code-cch-strip` — strips `cch=[0-9a-f]+;` from `system[*].text`. Enabled_by_default: false. | Must |
| FR-4.8 | Bundled rule: `openai / field-order-normalize` — canonical JSON field order for stable prefix bytes. Enabled_by_default: true. | Must |
| FR-4.9 | Config is loaded from `system_metadata` via Hub shadow push and hot-swapped without restart (atomic.Pointer). | Must |

### FR-5: cache_control marker injection (L4)

| ID | Requirement | Priority |
|---|---|---|
| FR-5.1 | For Group A providers (anthropic, bedrock-Claude), auto-inject `cache_control: {"type": "ephemeral"}` at up to three semantic boundaries: system[] end, tools[] end, messages[-2] end. | Must |
| FR-5.2 | Injection is controlled by a per-Provider `cache_marker_inject_enabled` flag. Default: false. | Must |
| FR-5.3 | Boundary 3 (messages history) is an additional sub-toggle, default off (Q4 decision). | Must |
| FR-5.4 | Injection never overrides or removes existing client-set `cache_control` markers. | Must |
| FR-5.5 | Total markers per request never exceeds 4 (Anthropic hard limit). Priority: Boundary 1 > 2 > 3. | Must |
| FR-5.6 | If the provider returns `invalid_request_error` referencing `cache_control`, auto-downgrade to fewer markers and retry. | Should |

### FR-6: Dry-run preview API (L2)

| ID | Requirement | Priority |
|---|---|---|
| FR-6.1 | `POST /api/admin/cache/preview` accepts a `traffic_event_id` and returns: the normalised body diff (unified diff format), estimated savings if rules were enabled. | Must |
| FR-6.2 | Dry-run telemetry: even when a rule is disabled but `dry_run_always=true`, record match rate per rule in `traffic_event` (via `normalised_strip_count/bytes` columns when `dry_run=true`). | Must |

### FR-7: Admin UI

| ID | Requirement | Priority |
|---|---|---|
| FR-7.1 | Cache Rules page: list all bundled rules by adapter_type, show enabled/disabled toggle, dry-run stats (match rate last 7d, estimated savings), risk badge. | Must |
| FR-7.2 | Provider → Cache Settings: L4 injection toggle, Boundary 3 sub-toggle. | Must |
| FR-7.3 | Global Settings → Cache: global normaliser on/off, L5 extended TTL toggle. | Must |
| FR-7.4 | Dashboard: cache hit rate by adapter type; net savings (this month / total); potential savings from disabled rules. | Should |

---

## 3. Non-Functional Requirements

| ID | Requirement |
|---|---|
| NFR-1 | Normaliser adds < 1 ms p99 latency on the hot path (regex + structural ops only). |
| NFR-2 | Config hot-swap via atomic.Pointer; no request is served with a partially-updated config. |
| NFR-3 | All new columns on `traffic_event` are nullable; missing data never fails the INSERT. |
| NFR-4 | Cost computations use Decimal arithmetic (no float rounding); results stored as `NUMERIC(12,8)`. |
| NFR-5 | Normaliser rule failures are isolated per-rule; one bad rule never disables the others. |
| NFR-6 | Go tests: `go test -race -count=1` green for all new packages. |

---

## 4. User Roles & Personas

| Role | Interaction |
|---|---|
| **Platform Admin** | Configures normaliser rules, enables/disables per-adapter type, views cost savings dashboard, sets Provider-level L4 toggle. |
| **Finance / CFO** | Views monthly net cache savings, break-even analysis, per-department cost allocation. |
| **Developer (VK user)** | No direct interaction; benefits transparently from improved cache hit rates. |
| **Compliance Officer** | Reviews dry-run diffs to verify normalised bodies do not alter semantic content. |

---

## 5. Constraints & Assumptions

- No VK-level configuration for normaliser rules (per Q2/Q3 decisions).
- The `cch=` strip rule applies to ALL requests through the `anthropic`
  adapter when enabled; operator ensures credentials are direct API keys.
- Session tracking is explicitly out of scope (AI Gateway is stateless).
- Gemini `cachedContent` lifecycle management (Phase 4) requires a
  separate SDD and is not in scope for Phase 1–3.
- Provider pricing table uses official public prices; accuracy of savings
  estimates depends on whether operator has overridden for discounted rates.
- L5 extended TTL (1h) is an Anthropic account-level beta feature; the
  Gateway sets `"type": "ephemeral"` vs `"type": "persistent"` but
  Anthropic controls whether 1h is granted for the account.

---

## 6. Glossary

| Term | Definition |
|---|---|
| **Prompt cache** | Provider-side KV store keying on request prefix bytes. Cache read = cheaper input price. |
| **cache_control marker** | Anthropic-specific JSON field `{"type": "ephemeral"}` placed at semantic boundaries to tell the provider to cache everything up to that point. |
| **Normaliser** | The Gateway component that strips volatile bytes and/or injects markers before upstream dispatch. |
| **L0 / cacheKeyNormalize** | Normalisation applied only to Nexus L1 cache key computation; does not alter upstream bytes. |
| **L3 / upstreamBodyNormalize** | Normalisation applied to bytes actually sent upstream; gated by global switch. |
| **L4 / marker injection** | Injection of `cache_control` markers into Anthropic requests; per-Provider toggle. |
| **Group A / B / C / D / E** | Provider grouping by cache mechanism (see brainstorm §4). |
| **cch= token** | Claude Code billing nonce in `system[*].text`; safe to strip for direct-API-key VKs. |
| **BEP** | Break-even point: minimum cache hit count for net savings to be positive. |

---

## 7. Priority (MoSCoW)

| Category | Items |
|---|---|
| **Must** | FR-1.1, FR-1.2, FR-1.4, FR-2.1, FR-2.2, FR-3.1–3.3, FR-4.1–4.9, FR-5.1–5.5, FR-6.1–6.2, FR-7.1–7.3 |
| **Should** | FR-1.3, FR-2.3, FR-5.6, FR-7.4 |
| **Could** | Phase 3 adapter coverage (moonshot, glm, remaining Group D) |
| **Won't (this epic)** | Gemini cachedContent lifecycle (Phase 4), semantic similarity cache, cross-tenant cache sharing |
