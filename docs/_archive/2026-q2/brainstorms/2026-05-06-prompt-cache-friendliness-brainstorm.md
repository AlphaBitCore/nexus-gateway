# Prompt-Cache Friendliness — Architecture Design (E38)

> Status: Architecture Design (ready for Requirements / SDD)
> Owner: AI Gateway team
> Date: 2026-05-06 (revised 2026-05-07)
> Purpose: Design for making upstream provider prompt caches hit
> reliably through Nexus Gateway. Multi-turn LLM conversations are
> the primary motivating workload. This is the seed for E38.
> Covers all 20 adapter types in a phased rollout.
> Author note: open decisions Q1–Q8 at the end must be signed off
> before drafting Requirements.

---

## 0. Why this exists

Today's L1 cache (Nexus exact-match response cache, E35) effectively
never hits on chat-completion traffic because each turn of a
conversation appends history bytes — the request body for turn N is
strictly larger than turn N-1 and never matches. Confirmed with two
real Claude Code requests against `api.anthropic.com`:

- Row `66171646-bea2-4d46-a8f8-8f16528c0d9b` (turn 1, 110 656 B)
- Row `4a60a908-5307-475c-80e7-0f5694c8f474` (turn 2, 110 806 B)

`messages[]` differed (1 entry vs 3 entries) and `system[0].text`
differed because Claude Code injects a billing-side session token
`cch=<hex>` directly into the system prompt. Both rows show
`cache_status = MISS`.

The naive instinct ("just hash a normalized form") misses the
architectural point: the highest-leverage cache for chat completions
is **the upstream provider's own prompt cache** (Anthropic 0.1×
input cost, OpenAI 0.5× input cost). Building Nexus-side prefix
caches is reinventing the LLM provider's infrastructure. The
Gateway's job is to make the upstream cache work.

**Existing infrastructure note**: the `Usage` struct already carries
`CachedTokens *int` (populated by Anthropic, OpenAI, Gemini,
DeepSeek, GLM adapters), and `cache_creation_input_tokens` is
already stored in `nexus.ext.anthropic.*` by the Anthropic codec.
E38 builds on this foundation — it does not start from zero.

---

## 1. Reframe — what this epic is and is NOT

**Is**: a set of request-shape transforms and observability hooks
that make upstream provider prompt caches hit reliably under
real-world client behaviour.

**Is NOT**:
- A Nexus-side prompt cache that stores prefix tokens. Wrong layer.
- A semantic-similarity cache (embedding lookup → cached response).
  False-positive risk is unacceptable for chat correctness.
- A "compress / summarize the prefix" feature. Belongs in the
  client, not the Gateway.
- An override of `cache_control` markers the client already set.
  Respect explicit client intent.
- A modification of `messages[]` content. Touch only `system`,
  `tools`, `metadata`, and similar non-conversational fields.

---

## 2. Root cause taxonomy

| Root cause | Whose fault | Gateway can fix? |
|---|---|---|
| **A. Cacheable prefix carries varying bytes** (Claude Code's `cch=`, telemetry timestamps in `metadata`) | Client | **Yes**: per-adapter body normaliser |
| **B. Client never sets `cache_control` markers** | Client (many SDKs do not) | **Yes**: auto-inject markers at natural boundaries (Anthropic only) |
| **C. Different VKs hit different upstream API keys → cache fragmentation** | Operator / topology | Documentation + monitoring; not a code change |
| **D. Prompt below provider's auto-cache threshold** (OpenAI 1024 tokens) | Client | Cannot fix — provider design |
| **E. Default 5 min TTL expires before next turn** | Client / inactivity | **Partial**: admin can configure extended TTL (Anthropic beta) |

A and B are the primary culprits. C is education. D is unfixable.
E is a configuration knob, not a Gateway intelligence problem.

---

## 3. Multi-turn business scenarios

| Scenario | Prefix stability | Where Gateway helps |
|---|---|---|
| **Linear chat** (Claude Code, Cursor, ChatGPT clone) | High; messages append-only | Fix A (strip `cch=`) → cache hit jumps from 0 % to 95 %+ |
| **RAG Q&A** | Low; retrieved chunks change per turn | Limited. Best practice: put retrieval at messages-end, keep system stable |
| **Agentic tool loops** (Cursor agent, Claude Code agentic) | High once tool turns settle | A + B both help; system + tool-defs + completed turns all become cacheable |
| **Long-context compaction** (Claude Code auto-summary) | Cliff: prefix invalidated when client swaps history for a digest | Cannot fix. Surface as alert: "cache invalidation cliff detected for VK X" |
| **Prompt A/B testing** | Variant per arm | Not a problem to fix — expected behaviour |

Conclusion: A + B together cover linear-chat and agentic scenarios,
which is roughly **80 % of real customer workloads** by volume.

---

## 4. Provider cache capability matrix — all 20 adapter types

This is the authoritative reference for E38 phased delivery.
Adapter type names match `providers.Format` constants in
`packages/ai-gateway/internal/providers/types.go`.

### Group A — Explicit marker caching (Anthropic-style)
Providers in this group require `cache_control: {"type": "ephemeral"}`
blocks in `system[]`, `tools[]`, or `messages[]`. The provider
returns `cache_creation_input_tokens` and `cache_read_input_tokens`
in the usage envelope. Minimum cacheable prefix: 1024 tokens
(Claude 3.5 / 3 family), 2048 tokens (Claude 3 Haiku). Cache TTL:
5 minutes (default ephemeral), 1 hour (extended, Anthropic beta).

| Adapter type | Cache mechanism | write field | read field | Phase |
|---|---|---|---|---|
| `anthropic` | Explicit `cache_control` | `cache_creation_input_tokens` | `cache_read_input_tokens` | **1** |
| `bedrock` | Explicit `cache_control` (Claude models only; non-Claude Bedrock models: no prompt cache) | same as anthropic | same as anthropic | **2** |

**Already instrumented**: `spec_anthropic` codec already extracts
`cache_read_input_tokens` → `Usage.CachedTokens` and
`cache_creation_input_tokens` → `nexus.ext.anthropic.*`.
Bedrock Claude models use the same wire shape; a small extension to
`spec_bedrock` handles it in Phase 2.

### Group B — Automatic prefix caching (OpenAI-style)
Providers in this group cache prefixes automatically without any
client-side markers. The Gateway's job is purely to keep the prefix
bytes stable (consistent field ordering, volatile-byte stripping).
Cache is triggered at ≥1024-token prefix boundaries. Cost savings:
typically 50 % on cached input tokens. TTL: 5–10 minutes (provider
implementation detail; Gateway cannot control it).

| Adapter type | Cache mechanism | read field | Phase |
|---|---|---|---|
| `openai` | Automatic, no markers | `usage.prompt_tokens_details.cached_tokens` | **1** |
| `azure-openai` | Automatic (same as OpenAI) | same as openai | **2** |
| `deepseek` | Automatic prefix cache (DeepSeek V2/V3) | `usage.prompt_tokens_details.cached_tokens` | **2** |
| `glm` | Automatic (GLM-4 family) | `usage.prompt_tokens_details.cached_tokens` | **3** |

**Already instrumented**: `Usage.CachedTokens` is already populated
for `openai`, `deepseek`, `glm`. The adapter codecs parse
`prompt_tokens_details.cached_tokens`. E38 adds the write-side
counter (`CacheCreationTokens`) and wires both into
`traffic_event`.

### Group C — Content-register caching (Gemini-style)
Providers in this group use an out-of-band "cache object" API: the
client uploads a content blob, receives a `cacheId`, and then
references that `cacheId` in subsequent requests. This is a
fundamentally different model from Groups A and B — it requires a
client-side SDK change or a Gateway-side pre-warming proxy pattern.

| Adapter type | Cache mechanism | metric field | Phase |
|---|---|---|---|
| `gemini` | `cachedContent` API (out-of-band object) | `usageMetadata.cachedContentTokenCount` | **4** |
| `vertex` | Same as Gemini via Vertex AI | same as gemini | **4** |

**Already instrumented**: `Usage.CachedTokens` is populated from
`cachedContentTokenCount` in the Gemini codec when a cache hit
occurs. However, since E38 Phases 1–3 do not auto-inject cache
objects into Gemini (that requires a different architecture), the
Phase 4 design note covers this separately. Phase 4 scope: Gateway
acts as a cache-object manager — it uploads the stable prefix on
behalf of the caller and injects the `cacheId` into outgoing
requests. This is a significant feature and merits its own SDD.

### Group D — OpenAI-wire, no documented prompt cache
These adapters use the OpenAI wire shape (`IsOpenAIWireShape() ==
true`). None of them have published prompt-caching documentation as
of the architecture review date. The Gateway applies field-order
normalisation by default (low risk) and monitors response usage
fields for any emerging `cached_tokens` values.

| Adapter type | Wire shape | Cache support | Action |
|---|---|---|---|
| `mistral` | OpenAI | None documented | Normalise field order; monitor |
| `xai` | OpenAI | None documented | Normalise field order; monitor |
| `groq` | OpenAI | None documented (ultra-low latency, caching less critical) | Normalise field order; monitor |
| `perplexity` | OpenAI | None documented | Normalise field order; monitor |
| `together` | OpenAI | None documented | Normalise field order; monitor |
| `fireworks` | OpenAI | None documented | Normalise field order; monitor |
| `moonshot` | OpenAI | `prompt_cache_tokens` in usage (Kimi-specific) | Phase 3: add extraction |
| `minimax` | OpenAI | None documented | Normalise field order; monitor |

### Group E — Non-OpenAI wire, no prompt cache
These adapters have custom wire formats and no documented prompt
caching. Gateway applies no cache-specific transforms. Monitor
vendor roadmaps.

| Adapter type | Format | Cache support |
|---|---|---|
| `cohere` | Cohere native | None documented (Command R family) |
| `huggingface` | TGI / varies | Varies by model; no standard |
| `replicate` | Replicate native | Varies by model; no standard |

---

## 5. Six-layer capability stack

```
                ┌──────────────────────────────────────┐
                │ L7: Cost ROI Dashboard                │  net savings / BEP /
                │     (CFO-level visibility)            │  per-VK & dept breakdown
                └──────────────┬───────────────────────┘
                               │
                ┌──────────────┴───────────────────────┐
                │ L6: Quality monitoring + auto-revert  │  generation quality
                │     to dry-run                        │  regression detection
                └──────────────┬───────────────────────┘
                               │ depends on data
                ┌──────────────┴───────────────────────┐
                │ L5: Extended TTL opt-in               │  Anthropic 1h TTL (beta);
                │     global configuration              │  system-wide setting
                └──────────────┬───────────────────────┘
                               │ orthogonal
                ┌──────────────┴───────────────────────┐
                │ L4: Auto cache_control marker         │  Three-boundary injection:
                │     injection (Group A only)          │  system / tools / messages[-2]
                │     per-Provider on/off toggle        │  per-Provider configurable
                └──────────────┬───────────────────────┘
                               │ depends on
                ┌──────────────┴───────────────────────┐
                │ L3: Body normaliser rule engine       │  Root-cause-A fix.
                │     Global on/off switch.             │  Volatile-byte stripping +
                │     Rules keyed by adapter_type.      │  field-order normalisation.
                │     UI-configurable, dynamic reload.  │  Fail-open + circuit breaker.
                └──────────────┬───────────────────────┘
                               │ must measure first
                ┌──────────────┴───────────────────────┐
                │ L2: Dry-run / preview API             │  Admin pastes a sample
                │     + diff visualisation              │  request, sees what
                │     + "estimated savings" for         │  would change; also shows
                │     disabled rules                    │  savings if rule enabled
                └──────────────┬───────────────────────┘
                               │ depends on
                ┌──────────────┴───────────────────────┐
                │ L1: Provider cache metric extraction  │  Extend Usage struct;
                │     + traffic_event columns           │  wire all Group A/B/C
                │     + cost model                      │  adapters; net-savings $
                └──────────────┬───────────────────────┘
                               │ depends on
                ┌──────────────┴───────────────────────┐
                │ L0: Layered cache key design          │  cacheKeyNormalize separate
                │     (Nexus L1 key enhancement)        │  from upstreamBodyNormalize.
                └──────────────────────────────────────┘  Benefits + risks below.
```

**L1 must ship first.** Without provider-side cache visibility,
nothing above it can be measured, validated, or safely operated.

---

## 6. Critical architectural decisions

### 6.1 Where normalisation inserts in the request lifecycle

```
client request
  │
  ▼
readBody(maxRequestBytes)
  │
  ▼
resolveModel + auth + ratelimit
  │
  ▼
Hook pipeline (compliance hooks may rewrite body)
  │
  ▼
Routing → resolved RoutingTarget + adapter_type
  │
  ▼
spec_adapter.PrepareBody (provider-specific body shaping)
  │
  ├──[L0] ① cacheKeyNormalize
  │         Computes Nexus L1 cache key from normalized body.
  │         Does NOT alter upstream bytes.
  │         Safe to apply always — see §6.2 for risk analysis.
  │
  ▼
Nexus L1 cache lookup (exact-match by normalized key)
  │
  ├─ HIT → return cached response (cache_source = 'nexus')
  │
  ▼ MISS
  │
  ├──[L3] ② upstreamBodyNormalize (ONLY if global switch ON)
  │         Rule engine keyed by adapter_type.
  │         Strips volatile bytes, normalises field order.
  │         Fail-open: panic/timeout → log + forward original body.
  │         [L4] Auto-injects cache_control markers (Group A only,
  │              if per-Provider toggle is enabled).
  │
  ▼
Send to Provider
  │
  ▼
Response processing
  ├──[L1] ③ Extract cache_creation / cache_read tokens
  │         Streaming: accumulated from final usage frame.
  │         Non-streaming: from response usage object.
  ├──[L1] ④ Compute net_savings_usd, write to traffic_event
  └──[L1] ⑤ Update dry-run telemetry (even when rule disabled)
```

**Strict ordering invariant**: normaliser MUST run AFTER all
body-mutating hooks. Hook pipeline still sees the original client
body (PII detection, content extraction continue to work). A unit
test pins this order.

### 6.2 L0 — Layered cache key (cacheKeyNormalize)

**What it does**: before the Nexus L1 cache lookup, compute the
cache key from a normalised version of the request body. The
normalisation is the same stripping / ordering logic defined by L3
rules, but applied only to key computation — it never alters the
bytes forwarded upstream.

**Benefit**: Nexus L1 cache can hit even when the upstream body
varies in volatile fields. Example: two consecutive Claude Code
turns where only `cch=<hex>` differs. Without L0, both are Nexus L1
MISSes. With L0, if the rest of the body is identical, the second
turn hits the Nexus L1 cache — even if the L3 upstream normaliser
is disabled or the admin has not opted in.

**Risk and mitigation**: the only failure mode is a *false-positive
key collision* — two requests whose normalised keys collide but
whose expected responses differ. This is avoided by a strict
conservative rule: L0 may only strip fields that are provably
non-semantic (billing tokens, random nonces, opaque client
identifiers). Fields that could influence model output (system
content, messages, model ID, temperature, etc.) must never be
stripped from the key. The rule set for L0 is a subset of the L3
rule set, limited to `action: strip` rules marked
`key_normalize_safe: true` in the rule definition. Operators cannot
add new key-normalisation rules via UI without explicit `key_normalize_safe` sign-off.

**Default state**: L0 is always active once the normaliser rule
engine (L3) is deployed. It does not require the global L3 switch to
be on — the key computation is read-only and has no upstream side
effects.

### 6.3 Configuration model — adapter_type keyed, UI-configurable, dynamic

Rules are keyed by **`adapter_type`** (i.e. `providers.Format`),
not by domain or URL. The same Anthropic adapter may be reachable
at multiple base URLs (direct API, proxy, enterprise endpoint); the
caching logic should apply uniformly to all of them.

Configuration lives in `system_metadata`, pushed to the AI Gateway
via Hub shadow sync. Changes take effect on the next request without
a service restart.

The Admin UI exposes a **Cache Rules** page under Provider settings:
- List all rules for the selected adapter type
- Toggle each rule on/off (saved to `system_metadata` → Hub push)
- Show dry-run statistics: "X requests matched in last 7d, estimated
  $Y savings if enabled"
- Show per-Provider L4 toggle: "Auto-inject cache markers: ON/OFF"

**Three-layer override model** (outer layers override inner):

```yaml
# Layer 1: Bundled rules (compiled into binary; shipped with version)
bundled_rules:
  anthropic:
    - id: claude-code-cch-strip
      match:
        body_path: "system[*].text"
        regex: 'cch=[0-9a-f]+;'
      action: strip
      key_normalize_safe: true    # also used in L0 cacheKeyNormalize
      enabled_by_default: false
      dry_run_always: true        # record match stats even when disabled
      risk_level: medium          # displayed in UI
      risk_note: |
        Claude Code billing attribution token. Safe to strip for
        direct-API-key VKs. Do NOT enable for VKs whose upstream
        credential is a Claude Code Pro subscription key — Anthropic
        may use this token for usage attribution.

    - id: anthropic-metadata-userid-strip
      match:
        body_path: "metadata.user_id"
        regex: '.*'
      action: strip
      key_normalize_safe: false   # user_id COULD influence output (hypothetically)
      enabled_by_default: false
      dry_run_always: true
      risk_level: low

    - id: anthropic-cache-marker-inject
      type: cache_control_inject
      strategy: three_boundary       # system | tools | messages[-2]
      min_prefix_tokens: 1024
      max_existing_markers: 4        # Anthropic hard limit; never exceed
      enabled_by_default: false
      dry_run_always: false          # injection is L4; controlled by per-Provider toggle
      risk_level: medium
      risk_note: |
        Injects up to 3 cache_control markers at semantic boundaries.
        Respects existing client-set markers (never overrides or removes).
        Falls back gracefully if marker count would exceed provider limit.

  openai:
    - id: openai-field-order-normalize
      type: field_order_normalize    # canonical JSON field order for stable hashing
      key_normalize_safe: true
      enabled_by_default: true       # auto prefix cache; low risk
      dry_run_always: false
      risk_level: none

  azure-openai:
    - id: azure-openai-field-order-normalize
      type: field_order_normalize
      key_normalize_safe: true
      enabled_by_default: true
      risk_level: none

  bedrock:
    - id: bedrock-claude-cache-marker-inject
      type: cache_control_inject
      applies_when:
        model_pattern: "^(claude|anthropic\\.claude).*"   # Claude models only
      strategy: three_boundary
      min_prefix_tokens: 1024
      max_existing_markers: 4
      enabled_by_default: false
      risk_level: medium

  deepseek:
    - id: deepseek-field-order-normalize
      type: field_order_normalize
      key_normalize_safe: true
      enabled_by_default: true
      risk_level: none

  # Group D adapters: field-order normalisation only
  mistral:
    - id: mistral-field-order-normalize
      type: field_order_normalize
      key_normalize_safe: true
      enabled_by_default: true
      risk_level: none

  xai:
    - id: xai-field-order-normalize
      type: field_order_normalize
      key_normalize_safe: true
      enabled_by_default: true
      risk_level: none

  groq:
    - id: groq-field-order-normalize
      type: field_order_normalize
      key_normalize_safe: true
      enabled_by_default: true
      risk_level: none

  perplexity:
    - id: perplexity-field-order-normalize
      type: field_order_normalize
      key_normalize_safe: true
      enabled_by_default: true
      risk_level: none

  together:
    - id: together-field-order-normalize
      type: field_order_normalize
      key_normalize_safe: true
      enabled_by_default: true
      risk_level: none

  fireworks:
    - id: fireworks-field-order-normalize
      type: field_order_normalize
      key_normalize_safe: true
      enabled_by_default: true
      risk_level: none

  moonshot:
    - id: moonshot-field-order-normalize
      type: field_order_normalize
      key_normalize_safe: true
      enabled_by_default: true
      risk_level: none

  minimax:
    - id: minimax-field-order-normalize
      type: field_order_normalize
      key_normalize_safe: true
      enabled_by_default: true
      risk_level: none

  glm:
    - id: glm-field-order-normalize
      type: field_order_normalize
      key_normalize_safe: true
      enabled_by_default: true
      risk_level: none

  # Group E: no rules; monitoring only
  # cohere, huggingface, replicate: watch vendor roadmaps

# Layer 2: System-level overrides (pushed via Hub shadow; editable in Admin UI)
system_overrides:
  normaliser_enabled: true          # Global L3 on/off switch (Admin UI toggle)
  adapter_type_overrides:
    anthropic:
      claude-code-cch-strip: enabled
      cache_marker_inject_enabled: true    # L4 per-Provider toggle

# Layer 3: No VK-level granularity.
# Rationale: normalisation is a provider-wire concern, not a VK policy.
# VK-level customisation adds complexity without proportional benefit;
# operators who need different behaviour for a specific VK should use
# separate Provider entries with different credentials.
```

**Dynamic reload**: when the Admin UI saves a rule change, it writes
to `system_metadata` via the Control Plane admin API. The Hub
propagates the change as a shadow delta to the AI Gateway's
`thingclient.OnConfigChanged` callback. The normaliser rule engine
holds its config in an `atomic.Pointer`; hot-swap on callback with
zero downtime.

### 6.4 Three-boundary cache_control injection (L4)

**Why three boundaries** (vs. original single-boundary proposal):
Anthropic's official SDK best practice injects markers at each
stable semantic boundary, not just the system prompt end. Each
marker independently caches everything from the request start up to
that point.

```
Boundary 1 (highest ROI): system[] end
  → Covers static system prompt. Hits on every request in a session.

Boundary 2 (high ROI for agents): tools[] end (if tools present)
  → Tool definitions rarely change within a session.
  → Agent workloads: Cursor Agent, Claude Code agentic mode.

Boundary 3 (incremental): messages[:-1] end (if history > N tokens)
  → Caches conversation history up to the previous turn.
  → Only inject when accumulated history exceeds min_prefix_tokens.
```

**Injection rules**:
- Never override or remove existing client-set `cache_control`
  markers. Client intent takes precedence.
- Count existing markers first; only inject if total after injection
  stays ≤ 4 (Anthropic hard limit per request).
- Priority order when near the limit: Boundary 1 > 2 > 3.
- Placement: append `cache_control` field to the last content block
  at each boundary.

**Per-Provider toggle**: the L4 injection is enabled/disabled via
the `cache_marker_inject_enabled` flag on the Provider record (Admin
UI → Provider → Cache Settings). Default: off. This is
Provider-scoped, not VK-scoped.

### 6.5 `cch=` strip risk analysis

The `cch=<hex>` token injected by the Claude Code client into
`system[*].text` is a billing-attribution nonce used by Anthropic's
subscription billing system.

**Rule scope**: this rule is configured at the **adapter_type
level** (`anthropic`), not at VK level. When enabled, it applies
to all requests routed through any Provider whose `adapter_type` is
`anthropic`. There is no per-VK override.

**Consequence**: the operator enabling this rule is responsible for
ensuring that **all** upstream Anthropic credentials configured in
the system are direct API keys (pay-per-token). If any Provider
uses a Claude Code Pro subscription key, the operator must either:
(a) not enable this rule, or (b) move subscription-key Providers to
a separate system instance.

**Risk profile**:

| Upstream credential type | Strip safe? | Reason |
|---|---|---|
| Direct Anthropic API key (pay-per-token) | **Yes** | `cch=` is a no-op for per-token billing; stripping it has no effect on Anthropic's invoicing |
| Claude Code Pro subscription key | **No** | Anthropic uses this token for usage attribution to the subscription plan; stripping may cause unattributed usage, potentially triggering throttles or billing disputes |

**Risk mitigation stack**:
1. Rule is `enabled_by_default: false`.
2. `dry_run_always: true` — admin can observe match rate and
   savings estimate before enabling.
3. Admin UI shows a `risk_level: medium` badge and the full
   `risk_note`. The enable toggle requires a confirmation dialog:
   *"This rule applies to all Anthropic requests system-wide.
   Only enable if all your Anthropic credentials are direct API
   keys (pay-per-token), not Claude Code Pro subscription keys."*
4. Circuit breaker (see §6.7) auto-disables on upstream errors.

### 6.6 Audit data model

`Usage` struct gains one new field for the write side:

```go
// In providers.Usage
CacheCreationTokens *int  // tokens written to provider cache this request
                          // Anthropic: cache_creation_input_tokens
                          // Others: nil (provider does not expose write-side)
```

`traffic_event` gains eight new columns:

```sql
-- Provider-side cache token counts
ALTER TABLE traffic_event
  ADD COLUMN cache_creation_tokens   int,
  ADD COLUMN cache_read_tokens       int,

-- Normaliser audit
  ADD COLUMN normalized_strip_count  int,
  ADD COLUMN normalized_strip_bytes  int,
  ADD COLUMN cache_marker_injected   smallint,  -- 0-4: how many markers injected

-- Cost model (computed at write time from provider price table)
  ADD COLUMN cache_write_cost_usd    numeric(12,8),
  ADD COLUMN cache_read_savings_usd  numeric(12,8),
  ADD COLUMN cache_net_savings_usd   numeric(12,8);  -- savings - write_cost
```

`traffic_event_payload` does NOT store the normalised body. Reasons:
- Compliance reviewers need to see what the client actually sent.
- Storage: a normalised copy doubles payload size for no audit value.

Debug story: admin uses the L2 dry-run API to preview the diff for
any captured request (by `traffic_event_id`).

### 6.7 Failure modes — fail-open everywhere

A normaliser that panics or has a misconfigured rule MUST NOT block
the user request. Three layers of defence:

1. Per-rule timeout: 1 ms default (regex / structural — should never
   block). Exceeded → rule skipped, original bytes used.
2. Panic recovery: log, increment metric
   `nexus_aigw_normaliser_rule_panic_total`, forward original body.
3. Circuit breaker per rule: 10 errors in 60 s → auto-disable the
   rule, emit `nexus_aigw_normaliser_rule_tripped_total`, send admin
   alert. Re-enable requires manual toggle in Admin UI.

Cache savings are operator-friendly nice-to-haves; the user's
request flow is the contract.

### 6.8 Cost model and ROI visibility

Break-even analysis for Anthropic (Claude 3.7 Sonnet standard price):

```
Input token price:           $3.00 / 1M tokens
Cache write price (1.25×):  $3.75 / 1M tokens  ← additional cost
Cache read price  (0.10×):  $0.30 / 1M tokens  ← large savings

Break-even point:
  N hits needed = write_cost / (standard_price - read_price)
                = 3.75 / (3.00 - 0.30) = 3.75 / 2.70 ≈ 1.4 turns

→ Cache pays for itself after just 2 conversation turns.
```

The cost model is stored in a `provider_pricing` configuration table
(seeded per model, overridable for enterprise contracts). The gateway
computes `cache_write_cost_usd` and `cache_read_savings_usd` at
response-write time. `cache_net_savings_usd = savings - write_cost`.

**L7 Dashboard widgets**:
- Monthly net cache savings (per VK, per Provider, per department)
- Break-even status per Provider (hits required vs. hits observed)
- "Potential savings if disabled rules were enabled" (from dry-run
  telemetry, even when rules are off)
- Cache hit rate over time, by adapter type

### 6.9 Streaming vs non-streaming

Both share the same prefix structure; normalisation logic is
identical. Cache-hit metrics come from response usage on both. SSE
responses ship usage in a final `message_stop` / `[DONE]` frame.

The existing `spec_anthropic` streaming path already captures
`cache_read_input_tokens` from usage frames (confirmed in codec
tests). E38 extends this to also capture `cache_creation_input_tokens`
in the streaming path, which is currently stored only on non-
streaming responses.

---

## 7. Phased delivery plan (E38)

### Phase 1 — Foundation + highest-ROI adapters
Targets: `anthropic` (Group A) + `openai` (Group B).
These two cover ~80 % of real customer workloads.

| Story | Description | Effort |
|---|---|---|
| **e38-s1** | Add `CacheCreationTokens` to `Usage` struct; wire Anthropic streaming path for `cache_creation_input_tokens`; add 8 `traffic_event` columns; cost-model computation at write time; seed `provider_pricing` table | 3 d |
| **e38-s2** | L0 layered cache key: `cacheKeyNormalize` separate from `upstreamBodyNormalize`; unit test pinning order after hooks | 2 d |
| **e38-s3** | L3 normaliser rule engine framework: global on/off, adapter_type dispatch, per-rule circuit breaker, fail-open, dry-run mode, `dry_run_always` telemetry; bundled rules: Anthropic `cch=` strip + OpenAI field-order | 3 d |
| **e38-s4** | L2 dry-run preview API (`POST /api/admin/cache/preview`): takes a `traffic_event_id`, returns normalised body diff + estimated savings | 2 d |
| **e38-s5** | L4 three-boundary `cache_control` injection (Anthropic); per-Provider toggle in Admin UI; respects existing markers; marker-count guard | 3 d |
| **e38-s6** | Admin UI: Cache Rules page (list rules by adapter_type, toggle, dry-run stats, risk badge); Provider → Cache Settings (L4 toggle); Global Settings → Cache → Extended TTL toggle (L5) | 3 d |

**Phase 1 total ≈ 16 d.** After Phase 1, Claude Code traffic
should show prompt cache hit rates jump from ~0 % to ~90 %+ on
the dashboard.

### Phase 2 — AWS Bedrock + Azure OpenAI + DeepSeek

| Story | Description | Effort |
|---|---|---|
| **e38-s7** | Extend `spec_bedrock` to extract `cache_creation/read_input_tokens` for Claude models; add `bedrock-claude-cache-marker-inject` bundled rule | 2 d |
| **e38-s8** | Azure OpenAI field-order normalisation + `cached_tokens` extraction wire; DeepSeek `cached_tokens` (already in codec; just wire to `traffic_event`) | 1 d |

**Phase 2 total ≈ 3 d.**

### Phase 3 — Remaining Group B/D adapters + Moonshot

| Story | Description | Effort |
|---|---|---|
| **e38-s9** | GLM `cached_tokens` wire to `traffic_event`; Moonshot `prompt_cache_tokens` extraction; field-order rules for all Group D adapters (mistral, xai, groq, perplexity, together, fireworks, minimax) | 3 d |
| **e38-s10** | L7 ROI Dashboard (net savings / BEP / VK analysis / dept breakdown) | 3 d |
| **e38-s11** | L6 quality regression detection + auto-revert to dry-run | 3 d |

**Phase 3 total ≈ 9 d.**

### Phase 4 — Gemini/Vertex cachedContent (Gateway auto-managed lifecycle)
The Gemini `cachedContent` API requires a distinct Gateway-managed
cache-object lifecycle. The Gateway acts as a transparent cache-object
manager on behalf of callers — callers use standard chat completion
requests with no awareness of the caching layer.

**Gateway responsibilities**:
1. **Detect stable prefix**: on each request, extract the
   `system + tools + messages[:-1]` prefix and compute its hash.
2. **Cache object registry**: maintain a per-Provider, per-hash
   registry in Redis mapping `(provider_id, prefix_hash)` →
   `(cache_id, created_at, expires_at)`.
3. **Upload on miss**: if no live cache object exists for the prefix
   hash, call the Gemini `POST /v1beta/cachedContents` API to upload
   the prefix, store the returned `cacheId` and TTL in the registry.
4. **Inject on hit**: if a live cache object exists, inject
   `cachedContent: { name: "<cache_id>" }` into the outgoing request
   and strip the duplicated prefix fields.
5. **TTL management**: Gemini default TTL is 1 hour; Gateway refreshes
   (PATCH TTL) the cache object when it detects sustained traffic
   before expiry. Expired entries are pruned from the registry lazily.
6. **Invalidation**: if the prefix hash changes (system prompt update,
   tools change), the old cache object is deleted and a new one is
   uploaded.
7. **Observability**: emit `cache_creation_tokens` (upload cost) and
   `cache_read_tokens` (hit savings) into `traffic_event` using the
   same columns as Groups A/B.

This is significantly more complex than Phases 1–3 and requires its
own Requirements + SDD pass before implementation.

---

## 8. Risk matrix

| Risk | Prob | Impact | Mitigation |
|---|---|---|---|
| Strip `cch=` on subscription VK → Anthropic billing dispute | Medium | High | Default off; risk badge in UI; operator must explicitly declare VK type |
| Injection places marker at wrong semantic boundary → upstream rejects | Medium | High | Validate against Anthropic schema; auto-downgrade to fewer markers on `invalid_request_error` |
| Rule engine silently drops request bytes → wrong model output | Low | High | `key_normalize_safe` gate; conservative default; only non-semantic fields eligible |
| False-positive L0 cache key collision | Low | High | Conservative rule eligibility (`key_normalize_safe: true` only); property-based unit tests for collision resistance |
| Normaliser rules drift between AI-Gateway and Compliance-Proxy / Agent | High | Medium | Rules live exclusively in `system_metadata`; all services consume via shadow sync |
| Hook rewrites body after L0 key is computed → key inconsistency | High | Medium | Strict invariant: L0 runs AFTER all hook-pipeline mutations; unit test pins order |
| Gemini `cachedContent` object expires mid-conversation → silent miss | Medium | Medium | Phase 4 only; TTL tracking in Gateway; alert on expiry |
| Admin enables L3/L4 globally, then L6 quality regression fires | Medium | Medium | L6 auto-reverts to dry-run; surfaces alert; human re-enable required |

---

## 9. Open decisions for sign-off (Q1–Q8)

| # | Question | Recommended default |
|---|---|---|
| Q1 | L3 global switch default state at deploy? | **Decided**: Off. Turn on after operator reviews dry-run telemetry for one week. |
| Q2 | `cch=` rule scope. | **Decided**: adapter_type-level global toggle, no VK-level binding. UI confirm dialog states the rule applies system-wide to all Anthropic requests. Operator is responsible for ensuring all Anthropic credentials are direct API keys before enabling. |
| Q3 | Store normalised body in audit? | **Decided**: No. Store strip count + strip bytes only; debug via dry-run preview API by `traffic_event_id`. |
| Q4 | Three-boundary injection Boundary 3 (messages history): default on or explicit opt-in? | **Decided**: Opt-in (separate per-Provider sub-toggle). History marker has higher risk of misplacement. |
| Q5 | provider_pricing table: seed with public prices or operator entry? | **Decided**: Seed with official public prices; allow per-Provider override for enterprise-contract discounts. |
| Q6 | L5 extended TTL (1h) configuration scope. | **Decided**: Global setting (system_metadata). One toggle in Global Settings → Cache covers all Anthropic requests. |
| Q7 | Phase 4 Gemini cachedContent scope. | **Decided**: Gateway auto-manages cache object lifecycle (upload on miss, inject on hit, TTL refresh, invalidation on prefix change). Callers remain unaware. Registry in Redis. Requires own SDD before implementation. |
| Q8 | L3 circuit breaker re-enable strategy. | **Decided**: Manual only. Auto-re-enable could cause flapping; admin must investigate root cause before re-enabling. |

---

## 10. Things intentionally out of scope

- **Cross-tenant cache sharing**: providers key cache by API key;
  cannot share across tenants.
- **Self-hosted prefix cache** (vLLM, SGLang): we are a gateway, not
  an inference engine.
- **Embedding deduplication / semantic-similarity cache**: different
  correctness model; belongs to a separate smaller epic.
- **Compaction-aware re-warming**: Claude Code auto-summarises long
  context, invalidating the cache. Possible future feature: detect
  the cliff and optionally pre-warm. Deferred — high risk.

---

## 11. Hand-off notes for the next session

When picking this up:

1. Read this file in full.
2. Read `docs/dev/architecture.md` — especially the "Body Capture"
   section. E37 just landed; the request lifecycle in §6.1 above is
   current as of 2026-05-07.
3. Skim `packages/ai-gateway/internal/handler/proxy.go` between
   `PrepareBody` and the cache lookup to confirm the §6.1 insertion
   points are still accurate.
4. Read `packages/ai-gateway/internal/providers/spec_anthropic/codec.go`
   lines around `cache_read_input_tokens` and
   `cache_creation_input_tokens` — the extraction is already done;
   e38-s1 extends the `Usage` struct and wires the streaming path.
5. Read `packages/ai-gateway/internal/providers/types.go` — the
   `Usage` struct already has `CachedTokens *int`; e38-s1 adds
   `CacheCreationTokens *int` next to it.
6. Get user sign-off on Q1–Q8 before drafting Requirements.
7. Drafting order per CLAUDE.md: Architecture impact note →
   Requirements → SDD → OpenAPI → Code.
8. Phase 1 MVP path: s1 + s2 + s3 (partial: `cch=` strip only) +
   s5 ≈ 9 days. This is the minimum to show meaningful cache hit
   rate improvement for Claude Code / Cursor traffic.
