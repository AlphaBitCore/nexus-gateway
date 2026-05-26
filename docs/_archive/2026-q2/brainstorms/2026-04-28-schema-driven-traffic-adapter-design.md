---
title: Schema-Driven Traffic Adapter — Design
date: 2026-04-28
status: draft (brainstorm output, awaiting review)
author: nexus (collaborative session with Claude)
related:
  - docs/dev/architecture.md
  - docs/superpowers/specs/2026-04-14-hook-architecture-redesign.md
  - docs/superpowers/specs/2026-04-14-routing-architecture-redesign.md
  - docs/superpowers/specs/2026-04-14-pii-redaction-and-payload-storage-design.md
---

# Schema-Driven Traffic Adapter — Design

## 1. Background

### 1.1 Trinity service architecture

Nexus Gateway intercepts and processes AI traffic through three services. Each
sits at a different point in the network and has a different relationship to
the wire format, but **they all share the same `packages/shared/traffic/adapters`
codebase today**.

| Service | Deployment role | Relationship to wire | Examples |
|---|---|---|---|
| **ai-gateway** | **Explicit forward proxy** — clients call `https://nexus/v1/...` directly | Sees both inbound (client → nexus) and outbound (nexus → provider) wire; **may transform between formats** when a routing rule selects a provider whose protocol differs from the inbound one | OpenAI SDK calling `nexus/v1/chat/completions`, routed to Anthropic Claude |
| **compliance-proxy** | **Transparent TLS proxy** (MITM) on the network egress path | Sees real client→provider wire; passes it through possibly with rewrite (PII redaction, blocking); never changes the protocol | Employee uses official OpenAI SDK to `api.openai.com`; proxy intercepts, redacts PII, forwards |
| **agent** | **Endpoint interceptor** (loopback / TUN, OS-specific) | Same as compliance-proxy but runs on the user's device, so it can also see traffic from desktop apps and IDE plugins that never traverse the corporate network | Cursor, GitHub Copilot, Codeium, Claude Desktop, JetBrains AI, Continue, Aider |

### 1.2 What every service must do with the wire

Regardless of which service, the same five capabilities are needed to deliver
the product:

1. **Identification** — Which provider is this? Which model? Is it an LLM
   request at all (vs. telemetry or auth)? What's the API key class /
   fingerprint?
2. **Element extraction** — Pull out user text segments, tool calls,
   metadata (model, conversation_id), token usage; preserve everything else
   for audit (`Extra`).
3. **Hook execution** — Feed extracted content to the policy pipeline
   (PII detection, classification, quota, routing decisions, AI guard).
4. **Safety enforcement** — Apply the hook outcome: pass, block, redact
   (rewrite the wire), or route to a different provider.
5. **Cross-provider translation** — **Only in ai-gateway**, **only for
   providers in our curated catalog**. When a routing rule chooses a
   provider whose protocol differs from the inbound one, translate the
   request and response (including streaming) so the client SDK still
   parses the result correctly.

The first four apply to **all three services and all traffic classes**.
The fifth has a deliberately narrow scope (see §4.1).

### 1.3 Cross-provider translation — scope and degradation modes

> **This is the binding scope rule, called out at the top of the
> document because it shapes the whole architecture.**
>
> Cross-provider translation only happens in **ai-gateway**.
> compliance-proxy and agent **never** translate — they observe and
> may rewrite in-place (PII redaction), but they pass the original
> protocol straight through.
>
> Within ai-gateway, translation has **four modes** based on the
> source of the target-side schema, with different fidelity. The
> routing-rule editor surfaces the chosen mode and any lossy fields:

| # | Inbound schema source | Outbound schema source | Mode | Fidelity |
|---|---|---|---|---|
| 1 | Cataloged | Cataloged | **Full translation** (Tier 2 complete) | ★★★★★ — every (source, target) pair has run real-SDK contract tests |
| 2 | Cataloged | **Pass-through** (customer declares the target accepts the source's wire) | **Passthrough** | ★★★★ — body is not parsed, only detect + audit; customer guarantees wire compatibility |
| 3 | Cataloged | **Customer schema** (customer-authored `build`, not yet in catalog) | **Best-effort translation** | ★★★ — engine uses cataloged source `extract` + customer `build`, but **no SDK contract test**; customer owns correctness |
| 4 | Cataloged | Unknown (no schema and not wire-compat) | **Refuse the route** | — |
| — | Web / IDE adapter | Anything | **Cannot occur** | Web/IDE adapters cannot be ai-gateway inbound (they are processed only by compliance-proxy / agent) |

**Curated catalog** = the set of provider adapters where we ship a
`build` schema section, run real-SDK contract tests in CI for every
(source, target) pair, and publish a per-pair compatibility
statement (which fields are lossy, which are unsupported, which are
stream-translatable).

**Customer schema** (mode 3) = build sections customers submit for
their internal LLM proxies / fine-tuned model gateways, loaded
through the signed registration flow (see §3.9). Customer schemas
**self-declare** their capability matrix and lossy-field policy;
the platform trusts the declarations but records everything to
audit (a customer-side capability mistake is the customer's
responsibility, but is forensically traceable). A customer schema
can be promoted into the catalog (mode 3 → mode 1) by passing
fixture coverage + SDK contract tests in CI.

**Pass-through** (mode 2) = the customer declares "my target
accepts the source's wire". The engine performs detection + audit
in ai-gateway but **does not parse the body**, forwarding the
original bytes to the target. This handles the very common case
where a customer's internal proxy is OpenAI-compat and no
translation is needed.

Consequences:
- **L5 Translator layer is ai-gateway-only.** compliance-proxy and agent
  do not load it.
- **Web-side and IDE-side adapters never need a `build` schema** — they
  only have `extract` and `rewrite`. They can never be ai-gateway
  inbound, and they cannot be a routing target.
- **The routing-rule editor must clearly indicate which mode the
  customer has chosen** — to prevent customers from believing they
  configured Pass-through when in fact the wire is incompatible,
  causing production breakage.

### 1.4 Traffic class taxonomy

Every adapter we ship belongs to exactly one class. Class determines which
decoders, which schema sections, and which testing rigor apply.

| Class | Wire characteristic | Spec source | Drift cadence | Translation eligibility |
|---|---|---|---|---|
| **API** | JSON request + (JSON response or SSE), well-known endpoints | Official OpenAPI (OpenAI, Anthropic, Gemini, Azure, Vertex, Bedrock); DeepSeek/GLM/Mistral/Grok-API/MiniMax — partial | Monthly | **Eligible** (curated catalog) |
| **Web** | JSON + SSE on private endpoints; occasionally protobuf-text form post (gemini.google.com); cookie auth | Reverse-engineered fixtures only | Weekly | **Not eligible** (extract + rewrite only) |
| **IDE / desktop App** | Mixed: gRPC + protobuf (Copilot, Codeium), private RPC (Cursor), wrapped Anthropic/OpenAI (Claude Desktop, Continue, Aider), private HTTPS (JetBrains AI) | None for proprietary clients; OSS clients have source | Daily (proprietary clients ship frequently) | **Not eligible** |

### 1.4.1 Business coverage asymmetry (load-bearing engineering-budget fact)

**The number of adapters that ai-gateway needs vs. what
compliance-proxy/agent need is asymmetric — and this is not an
engineering choice, it's a business reality.**

| Service | Business coverage scope | Typical count | Growth rate | Engineering mode |
|---|---|---|---|---|
| **ai-gateway** | Providers the enterprise **actively** chooses to onboard (curated catalog + customer's internal LLM) | **3–10 (typical enterprise)**, ≤ 30 (large multi-business-unit) | Monthly additions, customer-driven | Few and deep — must be high-fidelity, Mode 1 SDK contract tests; Mode 3 customer-owned |
| **compliance-proxy** | All external AI services employees **actually access**; the enterprise **cannot restrict** | **50–100+** | Weekly additions (BYOA reality) | Broad coverage — Tier 0 floor + best-effort schemas |
| **agent** | All desktop / IDE AI apps employees **actually use**; the enterprise **cannot restrict** | **50–100+** | Same as above | Same as above |

#### Why ai-gateway scope is naturally small

1. **Enterprise actively contracts the providers**: onboarding
   OpenAI / Anthropic / the company's internal LLM is an IT
   decision. Employees don't unilaterally configure new providers
   on ai-gateway.
2. **No business case for a "long tail" of providers**: the
   enterprise wants governance + routing + quota, not "integrate
   every LLM". LiteLLM's 200+ providers is an SDK-user use case,
   not an enterprise ai-gateway use case.
3. **Customer internal LLM proxies are the main extension point**:
   covered through customer schemas (Mode 3); also bounded —
   most customers have at most 1–3 internal models.

#### Why Web / App must be broadly covered

1. **Employees bring their own accounts, apps, devices** — the
   enterprise cannot mandate that an employee uses chat.openai.com
   instead of claude.ai or perplexity.ai.
2. **New AI products show up monthly** — chat.deepseek.com,
   grok.com, chat.mistral.ai, every cloud vendor's own chat —
   all caught up after the fact during 2025–2026.
3. **IDE plugins ship weekly**: Cursor / Copilot / Codeium
   protocol drift at the daily granularity is a fact, not a
   surprise; cataloging in advance is impossible.

#### Direct architectural and budget consequences

1. **Engineering effort allocation is roughly 1:9**: ai-gateway
   side does ~10 deep schemas (each with build + SDK contract
   tests + full fixtures); compliance-proxy/agent side does ~70+
   schemas (each only extract + minimum fixtures, no build); the
   total work is larger on the latter side but **per-adapter cost
   is smaller**.
2. **Tier 0 last-resort text extraction is the core defence on
   compliance-proxy/agent, not a fallback** — Web/App drift
   *is* the steady state; schemas always lag; Tier 0 is the
   working layer most of the time.
3. **The CASB 50K-app catalog comparison applies only to
   compliance-proxy/agent**, not to ai-gateway. Our ai-gateway
   catalog scale is 10–30, not 50K.
4. **Schema completeness priority**: ai-gateway must be 100%
   complete (with build); Web/App can be incremental (Tier 0
   floor preserves security capability).
5. **Customer extensibility (§3.9) is primarily an ai-gateway
   concern (customer schemas)**, not Web/App — Web apps are
   public, so platform maintenance is more appropriate than
   customer maintenance (customers don't have proprietary
   information about chat.openai.com).

### 1.5 Within-provider model variance

**This is an under-appreciated but severe fact: even within a single
provider, different models can have entirely different request /
response wire shapes.** The schema framework must dispatch on
**(provider, model-pattern, endpoint)** as a triple, not on provider
alone.

Concrete examples:

**OpenAI alone has at least 7 distinct wire shapes**:

| Models | Endpoint | Wire delta |
|---|---|---|
| `gpt-4o`, `gpt-4o-mini` | `/v1/chat/completions` | Standard Chat Completions |
| `o1`, `o3`, `o1-preview`, `o3-mini` | `/v1/responses` | Must use Responses API; **no `system` role** (use `developer` or `instructions`); **no streaming** in some early versions; no `temperature` / `top_p`; new `reasoning_effort` parameter; response carries `reasoning_tokens` |
| `gpt-3.5-turbo-instruct` | `/v1/completions` (legacy) | **`prompt` field instead of `messages`** — entirely different schema |
| `gpt-4o-realtime` | WebSocket | Different transport |
| `gpt-4o-audio` | `/v1/chat/completions` | `modalities:["text","audio"]` required; response carries `audio` field with base64 audio output |
| `text-embedding-3-*` | `/v1/embeddings` | Entirely different schema (no messages, vector array output) |
| `dall-e-3`, `gpt-image-1` | `/v1/images/generations` | Multipart or b64_json output |
| `whisper-1` | `/v1/audio/transcriptions` | **multipart/form-data** request, not JSON |
| `tts-1`, `tts-1-hd` | `/v1/audio/speech` | **Binary audio output**, not JSON |

**Anthropic**:
- `claude-3.5-sonnet`, `claude-3.7-sonnet` → standard Messages API
- `claude-3.7-sonnet` with thinking enabled → `thinking: {type, budget_tokens}` field added; response carries `thinking` content block
- Computer use models (`claude-3.5-sonnet-20241022` and later) → new `computer_20241022` / `computer_20250124` tool types with coordinates / screenshot fields
- Claude 4 → further new parameters

**Gemini**:
- `gemini-1.5-pro/flash` → standard `generateContent`
- `gemini-2.0-flash-thinking-exp` → `thinkingConfig` field
- `gemini-2.5-pro` → further new parameters, reasoning output
- `imagen-3` → entirely different endpoint

**Bedrock — extreme case**: same path
`/model/{provider}.{model_id}/invoke`, but body shape **entirely
determined by the model_id prefix**:

| model_id prefix | Body format |
|---|---|
| `anthropic.claude-*` | Anthropic Messages format (with top-level `model` field stripped) |
| `amazon.titan-*` | Titan format (`inputText`, `textGenerationConfig`) |
| `amazon.nova-*` | Nova format (Anthropic-like with deltas) |
| `meta.llama-*` | Llama format (`prompt`, `max_gen_len`) |
| `mistral.mistral-*` | Mistral format |
| `cohere.command-*` | Cohere format |
| `ai21.jamba-*` | AI21 format |

**Azure OpenAI**: path includes `deployment name + api-version`;
different api-versions support different field sets; o1 family has
additional Azure-specific restrictions on top of OpenAI's.

#### Implications for design

1. **Detection must dispatch on (host, path, model-pattern) as a
   triple.** "host + path" alone is insufficient — Bedrock's
   single-path multi-format case requires model_id prefix dispatch.
2. **An "OpenAI adapter" is actually a schema family.** At least 7
   schema entries are needed to cover OpenAI alone; they can share
   `components/schemas` references (DRY) but each is a distinct entry.
3. **The IR must carry model-capability flags.** At minimum:
   `supports_system_role`, `supports_streaming`, `supports_tools`,
   `supports_image_input`, `supports_audio_input`,
   `supports_reasoning`, `requires_max_tokens`, `max_context_window`.
4. **L5 Translator must enforce capability matching.** Example:
   inbound is OpenAI Chat Completions with image content, route
   target is o1 → o1 does not accept images; translator must refuse
   or fallback per the schema-declared policy.
5. **Routing-rule UX impact**: routing rules are not (provider →
   provider) but (source_model → target_model). Some pairs are
   simply incompatible; the routing-rule editor must surface this
   at save time, not at runtime translation failure.
6. **Detection ordering matters**: more specific patterns must match
   before less specific ones (e.g.
   `model_id ~ "anthropic.claude-3-5-sonnet-*"` before
   `model_id ~ "anthropic.*"`). Schema files need a `priority` field.

#### 1.5.1 Per-parameter capability differences (even when endpoint and body shape are identical)

Even with identical host, path, and body structure, **the parameter
subset accepted by different models within the same provider often
differs.** This is the most under-appreciated layer of the
capability model.

OpenAI `/v1/chat/completions` example:

| Parameter | `gpt-4o` family | `o1` / `o3` family | `gpt-4o-audio` | `gpt-4o-search-preview` |
|---|---|---|---|---|
| `temperature` | ✅ | ❌ (omit or fixed at 1) | ✅ | ❌ |
| `top_p` | ✅ | ❌ | ✅ | ❌ |
| `presence_penalty` / `frequency_penalty` | ✅ | ❌ | ✅ | ❌ |
| `logit_bias` / `logprobs` | ✅ | ❌ | ✅ | ✅ |
| `max_tokens` | ✅ | ❌ (use `max_completion_tokens`) | ✅ | ✅ |
| `max_completion_tokens` | ✅ | ✅ (recommended) | ✅ | ✅ |
| `reasoning_effort` | ❌ | ✅ | ❌ | ❌ |
| `modalities: ["audio"]` + `audio: {...}` | ❌ | ❌ | ✅ (required) | ❌ |
| `web_search_options` | ❌ | ❌ | ❌ | ✅ (required) |
| `n > 1` / `stop` / `seed` | ✅ | ❌ | ✅ | ✅ |
| `tools` | ✅ | partial | ✅ | ✅ |
| `stream` | ✅ | partial | ✅ | ✅ |
| `response_format: json_schema` | ✅ | partial | ❌ | ❌ |
| `parallel_tool_calls` | ✅ | partial | ✅ | ✅ |

Anthropic, Gemini, and Bedrock all show the same pattern: e.g. Claude
3.7 Sonnet's `thinking` mode is only valid when thinking is enabled;
Gemini 2.0 thinking models have `thinkingConfig` while others do not;
Bedrock's `amazon.titan-*` does not accept `anthropic.claude-*`'s
`max_tokens` but takes `textGenerationConfig.maxTokenCount` instead.

⇒ **Capability is not a boolean flag — it's a per-parameter handling
strategy**. Each parameter may take one of:

| Strategy | Meaning | Example |
|---|---|---|
| `accept` | Pass through | mainstream params |
| `reject_request` | Refuse the request if param is present | strict-mode unknown params |
| `silent_drop` | Strip the param without warning (known-no-op behaviour) | OpenAI o1 inbound containing `temperature` |
| `warn_drop` | Strip + emit a lossy-field warning | unsupported `response_format` |
| `rename_to(target)` | Rename | `max_tokens` → `max_completion_tokens` (4o → o1) |
| `clamp(min, max)` | Range-bound | `max_tokens` capped at context window |
| `force_default(value)` | Force a value | o1 `temperature` forced to 1 |
| `inject_required(value)` | Inject if missing on outbound | Anthropic's required `max_tokens` |
| `enum_subset(values)` | Restrict enum | `response_format.type` subset |
| `transform(fn)` | Invoke a built-in helper | `max_tokens` ↔ `textGenerationConfig.maxTokenCount` (Bedrock Titan) |

The schema declares a `parameters:` table per model-pattern; the
engine applies it during IR → wire build. This lets a single OpenAI
schema share the body structure (host, path, SSE format, main shape
all the same) while expressing per-model differences in the
`parameters:` table — without forking the whole schema per model.

#### 1.5.2 Further architectural impact

7. **L4 IR must preserve raw parameters with provenance**, so the
   build stage can filter against the target model's `parameters:`
   table; never drop fields at extract time.
8. **Routing-rule editor UX**: when a customer saves a route
   (source_model → target_model), the UI should display the
   parameter-transformation matrix (which fields drop / rename /
   clamp), letting the customer see lossy fields up front rather
   than discover them in production.
9. **Audit row**: lossy fields record not just the fact of removal
   but **why** (target model unsupported) and **the original value**
   (for post-hoc forensics).

### 1.6 Defense-in-depth tiers

**This is one of the design's load-bearing insights: we are an AI
security platform, not an AI SDK compatibility vendor. The floor
for "identify and intercept AI data" is not "perfectly parse the
protocol" — it is "extract any readable text from any traffic" so
keyword matching and AI security classifiers can run.**

Three tiers, ordered from minimum-viable to fully-structured:

| Tier | Capability | Applies to | Failure tolerance | Cost |
|---|---|---|---|---|
| **Tier 0 — last-resort text extraction** | Schema-independent generic heuristics that dump readable text from any wire | All traffic (the floor) | **Must not fail** — at worst returns an empty list, never errors | Very low |
| **Tier 1 — structured extraction** | Schema-driven extraction of segments / tool_calls / metadata / usage | Traffic with a registered schema | May degrade to Tier 0 | Medium |
| **Tier 2 — full bidirectional translation** | IR + build + capability matrix + SDK compatibility | Only ai-gateway cross-model routing, only inside the catalog | Must not fail in production (hard contract) | High |

#### Per-service real dependency

| Service | Depends on Tier 0 | Depends on Tier 1 | Depends on Tier 2 |
|---|---|---|---|
| **ai-gateway, no cross-model routing** | not strictly (audit/quota can use Tier 0/1, but the route decision itself does not need body parsing) | wants | not needed |
| **ai-gateway, cross-model routing** | floor | needed | **required** |
| **compliance-proxy** | **required** (security capability must not fail when the protocol drifts) | wanted, the more complete the better | never |
| **agent** | **required** (App-side protocols drift fastest) | wanted | never |

#### Engineering consequences

1. **Pressure on compliance-proxy and agent to "keep up with
   schemas" is significantly reduced.** Web field renames or
   Cursor protobuf field-number reshuffling do not, in the short
   term, disable PII / DLP / AI security detection — Tier 0
   carries the floor; keyword and AI classifiers still run.
   Schema completeness becomes "improving precision", not
   "preventing capability loss".
2. **Cases where ai-gateway does not perform cross-model
   translation can take a fast-path passthrough.** Detection only
   (host + model identification for the routing decision); the
   full schema engine is unnecessary. This shaves CPU off the
   majority of ai-gateway traffic.
3. **The truly hard kernel narrows to ai-gateway × cross-model
   routing × cataloged providers.** Engineering effort
   concentrates here; tolerance for the rest is higher.
4. **Tier 0 must exist and must never fail.** Implement it as part
   of the engine, parallel to the schema engine, so it still feeds
   hooks even when the schema engine errors or no schema is
   registered.

#### Tier 0 implementation outline

Generic heuristics, applied in order:
- HTTP body is JSON → recurse and dump every string value
  (including nested arrays / objects)
- HTTP body is SSE → split frames, treat the data portion as JSON
  per above; preserve event lines
- HTTP body is form-urlencoded → URL-decode and dump value strings
- HTTP body is protobuf-raw → run the L1 protobuf-raw decoder, dump
  string fields
- HTTP body is gRPC → unwrap gRPC framing, recurse into protobuf-raw
- HTTP body is binary with gzip / deflate / brotli magic →
  decompress and recurse
- HTTP body is straight UTF-8 / ASCII → treat as text
- otherwise → emit empty list, do not error

Output: `Tier0Text { all_text_segments: []string,
source_hint: "json_strings|sse_data|form|protobuf|grpc|gzip|raw_utf8|binary",
total_bytes_extracted: int }`.

Hooks evaluate keyword / AI security rules over the union of
Tier 0 ∪ Tier 1 outputs. Audit rows record the tier each hook
match came from, so it is forensically clear which detections were
recovered by the floor versus produced by structured extraction.

### 1.7 What an "adapter" must do today

Concretely, in the current `packages/shared/traffic/adapters/` codebase, every
adapter must implement (per `traffic.Adapter` interface):

- `ExtractRequest`, `ExtractResponse`, `ExtractStreamChunk` → produce
  `NormalizedContent { Segments, ToolCallSegments, Metadata, Extra }`
- `DetectRequestMeta` → provider, model, key class, key fingerprint
  (header-derived, not body-derived)
- `DetectResponseUsage` → token counts (or `non_llm` status)
- `RewriteRequestBody`, `RewriteResponseBody` → reverse-encode the wire
  with hook-modified content (PII redaction)

For ai-gateway with cross-provider routing, two additional capabilities are
required (not yet in the interface):

- `BuildRequest(IR, target_provider) → wire` — construct a target-format
  request from a canonical intermediate representation
- `BuildResponse(IR, target_provider) → wire` — construct a target-format
  response (including stream events) so the client SDK parses correctly

---

## 2. Problem

### 2.1 Hand-written adapter explosion under high coverage targets

Today: 18 adapters, all hand-written, ~150–400 lines of Go each, each with its
own `ExtractRequest`/`ExtractResponse`/`ExtractStreamChunk`/`Rewrite*`/`Detect*`.
Total ~5,000 lines of imperative parsing code, plus ~2,000 lines of test code.

Realistic coverage target broken down by service (see §1.4.1):

| Service | Target count | Per-adapter complexity |
|---|---|---|
| **ai-gateway** (catalog + customer routing targets) | ~10–15 typical / ≤ 30 large-enterprise | High — includes build + SDK contract tests |
| **compliance-proxy + agent** Web (chat.openai.com, claude.ai, gemini.google.com, chat.mistral.ai, chat.deepseek.com, perplexity.ai, grok.com, chatglm.cn, copilot.microsoft.com, …) | ~15–20 | Medium — extract + rewrite only; fail-soft is critical |
| **compliance-proxy + agent** IDE / desktop (Cursor, Copilot, Codeium, Claude Desktop, JetBrains AI, Continue, Aider, Cursor Tab, Windsurf, …) | ~15–20 | Medium-high — protobuf-raw + client-version binding |
| **compliance-proxy + agent** mainstream API (employees calling SDKs directly) | ~10 | Low — public spec |

⇒ Total target across the platform **~50–70 adapters**, but
**ai-gateway accounts for only 10–15**; the remaining **40+** sit
on compliance-proxy/agent. **Hand-written, both sides are linearly
expensive; under schema-driven, the Web/App side has higher drift
tolerance thanks to the Tier 0 floor — engineering budget tilts to
the high-fidelity ai-gateway side**.

At today's per-adapter cost (hand-written, ~1–2 engineer-days for
an OpenAI-compat, ~3–7 days for a non-trivial Web/IDE), reaching 70
adapters and keeping them healthy is a multi-quarter effort with
no end-state (new AI products keep emerging).

### 2.2 Web and App protocol drift outpaces hand maintenance

| Surface | Observed change cadence | Failure mode under hand-written adapter |
|---|---|---|
| OpenAI / Anthropic API | New optional field every 1–2 months (`reasoning_effort`, `prompt_cache_key`, `safety_identifier`, `web_search_options`, `prediction`, `modalities`, `audio`) | Audit silently misses new field; `Extra` collection compensates if `known_keys` list was kept current |
| Web (claude.ai, chat.openai.com) | Field renames / additions every 1–4 weeks | Extraction selector stops matching → `Segments` empty → audit row missing user content (silent compliance gap) |
| IDE / proprietary app (Cursor, Copilot) | Wire format changes per client release (~daily for Cursor, ~weekly for Copilot) | Same as Web, plus protobuf field-number reassignments break decode |

The compounding effect is the real problem: **18 adapters × monthly drift =
~one drift incident per week**, and each costs an engineer half a day to
diagnose and patch. At 70 adapters this becomes a full-time job.

### 2.3 Per-service duplication amplifies the cost 3×

The three services share the codebase but each has its own integration
points (hook pipeline, audit row schema, rewrite policy, error surface).
Today, every adapter change must be:

1. Implemented in `packages/shared/traffic/adapters/<name>/`
2. Exercised against ai-gateway request/response paths
3. Exercised against compliance-proxy MITM paths (different framing, body
   buffering, content-length handling)
4. Exercised against agent paths (cross-OS, possibly different TLS framing)

Even though it's "shared code", every change ships through three integration
surfaces. The effective per-adapter cost is closer to 3× the raw line count.

### 2.4 Industry has no public reference that fits us

Surveyed (with citations in §6):

- **LLM Gateway projects** (LiteLLM, Portkey, Cloudflare AI Gateway,
  Kong AI Gateway) — all hand-written per-provider classes; none process
  Web or IDE traffic; all assume the user is the SDK caller.
- **AI Security platforms** (WitnessAI, Lasso, Prompt Security) — bypass
  schema maintenance with ML intent classification; useful as a layer
  *on top of* extraction but not a substitute.
- **CASB / SaaS DLP** (Netskope CCI 50,000+ app catalog, Zscaler, Palo
  Alto Enterprise DLP) — proven that per-app inspection at scale is
  commercially viable, but their catalogs are closed-source trade
  secrets. **The comparison applies only to our compliance-proxy +
  agent side (Web/App with 50–70 + a long tail)**, not to ai-gateway
  (which is naturally bounded at ~10–15 mainstream providers; see
  §1.4.1).
- **Protocol parsing** (Wireshark dissectors, Suricata rules, Zeek scripts,
  OPA/Rego) — battle-tested "declarative DSL + host-language runtime"
  architecture, but for transport/network protocols, not application-layer
  LLM semantics.

⇒ **No public open-source schema-driven AI traffic adapter framework
exists.** The need is real (CASB validates the market), the architecture
is proven (protocol parsing validates the model), but the combination is
absent. This is both the opportunity and the warning sign.

---

## 3. Proposal: 5-layer schema-driven adapter

### 3.1 Goals

In priority order:

0. **Defense in depth** — Tier 0 last-resort text extraction
   **must never fail**; even when schemas have drifted, client
   versions are unknown, or no adapter is registered, hooks still
   receive readable text for keyword / AI security detection.
   This is the AI security platform's baseline contract — highest
   priority (see §1.6).
1. **Coverage scaling** — Adding a new adapter is writing a YAML schema +
   capturing fixtures, not writing Go code. Target: 80% of new adapters
   require zero Go.
2. **Drift resilience** — Unknown fields/events fail-soft into `Extra`
   instead of breaking extraction; upstream OpenAPI changes are detected
   in CI.
3. **Performance parity** — Schema engine ≤ 1.5× the latency of equivalent
   hand-written Go for the same extraction. Verified by CI benchmark
   gates.
4. **Wire-level non-destructiveness** — Rewrite must preserve byte-level
   layout of unmodified fields; failures fail-safe to passthrough; no
   half-rewritten bytes ever leave the proxy.
5. **Cross-provider translation correctness** — For ai-gateway curated-
   catalog routing, the translated wire must be parseable by the original
   client SDK (verified with real SDKs in CI).
6. **Customer extensibility** — Schema YAML is part of the deliverable;
   customers can add adapters for internal LLM proxies without source-
   code access; engine validates customer schemas to bound risk.
7. **Auditability** — DSL is intentionally restricted (not Turing-complete);
   every schema is signed and versioned; every accumulator state is
   bounded.

### 3.2 Non-goals

- **Replace hand-written adapters in one shot.** Migration is incremental
  and adapter-by-adapter.
- **Express arbitrary protocol logic in YAML.** State machines that don't
  fit the bounded primitive set (~7) live in registered Go custom
  accumulators, not in the schema.
- **Translate Web or IDE traffic across providers.** §1.3 — translation
  is ai-gateway + curated catalog only.
- **Replace the hook system, the routing engine, or the audit pipeline.**
  Adapters are upstream of all three; this design only changes how the
  adapter layer is structured.
- **Make schemas Turing-complete.** No loops, no recursion, no arbitrary
  expressions. Bounded primitives only.

### 3.3 Five-layer architecture

```
┌──────────────────────────────────────────────────────────────────┐
│ L5  Translator                            ai-gateway only        │
│     Four modes: Full / Pass-through / Customer-best-effort /     │
│     Refuse  (see §1.3 + §4)                                      │
├──────────────────────────────────────────────────────────────────┤
│ L4  Nexus IR                              all three services     │
│     NexusLLMRequest / NexusLLMResponse / canonical stream events │
│     model_capabilities flags + raw param preservation +          │
│     provenance                                                   │
├──────────────────────────────────────────────────────────────────┤
│ L3  Schema engine                         all three services     │
│     YAML compiled to typed Go runtime; zero-copy selectors;      │
│     byte-level rewrite; ≤ 1.5× hand-written Go; no IO;           │
│     bounded memory                                               │
├──────────────────────────────────────────────────────────────────┤
│ L2  Schema files                          per-adapter YAML       │
│     Sections: detect / extract / build (ai-gateway only) /       │
│       rewrite / parameters / metadata + fixture corpus           │
├──────────────────────────────────────────────────────────────────┤
│ L1  Decoder                               wire → canonical JSON  │
│     JSON / SSE-data / SSE-named / NDJSON / form-url /            │
│     gRPC / protobuf-raw / WebSocket                              │
├──────────────────────────────────────────────────────────────────┤
│ L0  Tier 0 last-resort text extraction    all three services     │
│                                           (must never fail)     │
│     Schema-independent generic text extraction                   │
│     Output: Tier0Text fed to keyword / AI security hooks         │
│     Continues to work even when L1-L5 fail or no schema is       │
│     registered                                                   │
└──────────────────────────────────────────────────────────────────┘
```

**Key design property: L3 only sees JSON.** Every weird wire format is
handled in L1 and converted into a canonical JSON event stream. This
prevents the DSL from sprouting protocol-specific primitives.

### 3.4 Layer details

#### L0 — Tier 0 last-resort text extraction

See §1.6 for the Tier 0 description. Engine implementation notes:

- **Must-not-fail contract**: any input → outputs `Tier0Text`;
  failures return an empty list, **never throw or return an
  error**. Hooks always receive a non-nil result.
- **Decoupled from L1–L5**: L0 runs in parallel with the schema
  engine and does not depend on schema-load state. Even when an
  adapter is not yet registered (a brand-new provider's first
  appearance), L0 still dumps text.
- **Bounded LOC**: L0 ≤ 500 LOC of Go total, including all
  heuristics (JSON whole-tree string scan, SSE frame split,
  protobuf-raw string-field extraction, gRPC framing, gzip /
  brotli / deflate decompression, UTF-8 sniff).
- **Audit annotation**: every hook keyword hit's audit row records
  a `text_source` of `tier0` or `tier1` (schema), so it is
  forensically clear which detections were rescued by the floor
  and which came from structured extraction.
- **Tier 0 + Tier 1 are unioned for hooks**, not chosen one or
  the other. Even when an adapter has a schema, hooks run against
  any extra text that Tier 0 finds (a useful safety net for a
  Web-side schema that has dropped a field).

#### L1 — Decoder

Goal: turn arbitrary wire bytes into a stream of canonical JSON events
that the L3 engine can process uniformly.

- **Frozen primitive set** (8 decoders). Each is a small, hand-written
  Go module (~100–250 LOC each). Adding a new decoder requires explicit
  design review and is rare.
- **`protobuf-raw`** is the most novel one: when the .proto file is not
  available (Cursor private RPC), decode by wire-type heuristics into a
  JSON tree keyed by field number with type tags
  (e.g. `{"_pb": {"1": {"type": "varint", "value": 42}, "2": {"type": "string", "value": "..."}}}`).
  This lets the schema author write selectors against field numbers
  without the runtime needing the schema definition.
- **SSE decoders** handle frame boundaries, multi-line data concatenation,
  comment lines (keep-alive), and the `[DONE]` literal sentinel. The
  data-only and named-events variants differ only in whether the `event:`
  line is preserved as a `_event` key on the emitted JSON.

#### L2 — Schema files

One YAML file per adapter, plus a mandatory fixture directory. Schemas
are versioned (`schema_version: 1`) and signed.

Sections:

- **`detect:`** — How to identify this adapter is the right one.
  **Three-dimensional dispatch**: host pattern, path pattern, and
  **model-pattern** (extracted from body or path). Each detect rule
  carries a `priority` (higher = more specific); the engine matches
  in priority order. A single OpenAI "family" schema is in fact
  multiple detect entries that share the body components but each
  has its own extract / build / rewrite hooks.
- **`extract:`** — Selector rules producing IR fields. Subdivided into
  `request`, `response_nonstream`, `response_stream`. Stream extraction
  declares accumulators (see §3.6 primitive set) and termination
  conditions.
- **`build:`** — IR → wire construction rules. **Only present for
  curated-catalog providers used by ai-gateway as routing targets.**
  Absent on Web/IDE schemas. Mirrors `extract` structurally.
- **`rewrite:`** — Byte-level patch rules for in-place modification
  (PII redaction). Each rewrite rule names which `extract` selector it
  inverts. Failure modes are explicit.
- **`metadata:`** — Header-derived fields (key class, key fingerprint,
  auth scheme). Limited primitives: literal match, prefix match,
  hash function name (`sha256`).
- **`x-nexus-known-keys:`** — Whitelist of known top-level keys.
  Anything outside is auto-collected into `Extra` (fail-soft).
- **`parameters:`** (NEW) — Per-model-pattern parameter handling
  table. Each parameter takes one of `accept` /
  `reject_request` / `silent_drop` / `warn_drop` /
  `rename_to(target)` / `clamp(min,max)` / `force_default(value)` /
  `inject_required(value)` / `enum_subset(values)` /
  `transform(helper)` (see §1.5.1). The engine applies the table
  during IR → wire build.

**Adapter family concept**: a single schema YAML may contain
**multiple model-pattern variants that share one set of
`extract` / `build` / `rewrite` body rules but each carry their own
`detect` and `parameters:` table**. This avoids forking OpenAI into
seven independent schemas just because there are seven wire variants.
Sketch:

```yaml
# openai-family.schema.yaml (sketch)
adapter_family: openai
shared:
  extract:    {...}    # shared body rules
  rewrite:    {...}
  build:      {...}
variants:
  - id: openai-chat-4o
    detect:
      host: api.openai.com
      path: /v1/chat/completions
      model_pattern: ^gpt-4o(-mini|-realtime|-audio|-search-preview)?
      priority: 100
    parameters:
      temperature:        accept
      reasoning_effort:   reject_request
      max_tokens:         accept
      max_completion_tokens: accept
  - id: openai-responses-o-series
    detect:
      host: api.openai.com
      path: /v1/responses
      model_pattern: ^(o1|o3)(-mini|-preview)?
      priority: 110
    parameters:
      temperature:        silent_drop
      top_p:              silent_drop
      presence_penalty:   silent_drop
      max_tokens:         rename_to(max_completion_tokens)
      reasoning_effort:   accept
  - id: openai-chat-audio
    detect: ...
    parameters:
      modalities:         inject_required(["text","audio"])
      audio:              inject_required({voice:"alloy", format:"mp3"})
  # ... other variants
```

Fixture directory: `<adapter>/fixtures/{request,response,stream}/*.{json,sse,bin}`,
each with a sibling `.expected.yaml` describing the expected IR output
plus a `variant_id` so fixtures group by model-pattern. CI re-plays
every fixture on every schema change.

#### L3 — Schema engine

A single Go package, target ≤ 1500 LOC, that:

1. **Loads schemas at startup** (or on signed schema reload). YAML →
   typed `CompiledSchema` struct: each selector pre-parsed into a
   gjson path closure, each accumulator instantiated as a `func(state, value)`,
   each event-dispatch built into a `map[string]Rule`.
2. **Validates schemas statically**: all selectors syntactically valid,
   all accumulator references resolvable, all `build` rules' field set
   matches IR field set, no cycles, all custom-accumulator references
   point to registered Go names.
3. **Executes per request/response**: walks the compiled tree, calls
   selectors, accumulates state, emits `NexusLLMRequest` / `NexusLLMResponse`.
4. **Bounded resources**: per-request memory cap, per-stream chunk
   count cap, per-selector step count cap. Engine refuses execution
   if a customer schema declares a higher bound than the platform allows.
5. **No IO**: engine doesn't fetch URLs, read files, call out. The one
   exception (image_url → base64 fetch) is delegated to a registered
   helper that the engine calls explicitly, with timeout and cache.

#### L4 — Nexus IR

The IR is **a model of LLM invocations, not a model of any specific wire
format**. It is the canonical representation that hooks, audit, and the
translator all consume; provider schemas map to and from it.

**Key principle**: at the extract stage, the IR **preserves all raw
parameters with provenance** (each parameter tagged with the schema
variant and selector it came from). **The engine never drops or
semantically transforms parameters at extract time.** All "drop /
rename / clamp / default-fill" transformations happen at the build
stage according to the target's `parameters:` table. Consequences:
(a) the same IR can be built into multiple different targets;
(b) audit rows record original value + transformation decision +
reason without information loss;
(c) the routing layer can perform capability compatibility checks
(see §4.1.1) without depending on extract-time ad hoc judgments.

`NexusLLMRequest`:
- Control params: `model`, `temperature`, `top_p`, `max_tokens`,
  `stop_sequences`, `seed`, `presence_penalty`, `frequency_penalty`,
  `response_format` (with target-support enum), `reasoning_effort`,
  `prompt_cache_markers` (extension)
- **`model_capabilities`** (NEW, important): a flag set describing
  the target model's accepted capabilities, enforced by L5 during
  translation. Minimum set: `supports_system_role`,
  `supports_streaming`, `supports_tools`, `supports_image_input`,
  `supports_audio_input`, `supports_reasoning`,
  `requires_max_tokens`, `max_context_window`,
  `supports_response_format_json`, `supports_temperature`. The
  capability table is declared per model-pattern in the adapter
  schema; the engine resolves it at runtime.
- `system_prompt`: string or array of typed parts
- `messages`: array of `{role, content_blocks[]}`
- `content_blocks`: typed union — `text`, `image{base64,media_type}`,
  `audio{...}`, `tool_use{id,name,input}`, `tool_result{tool_use_id,content}`,
  `refusal`
- `tools`: array of `{name, description, input_schema}`
- `tool_choice`: `auto | required | none | {tool_name}`
- `stream`: bool
- `extensions`: `map[string]any` for provider-specific fields that
  don't have a canonical place (vendor escape hatch, must be marked
  with provenance)

`NexusLLMResponse`:
- `id`, `model`
- `output_blocks`: same typed union as request `content_blocks`
- `stop_reason`: canonical enum (`end_turn | max_tokens | tool_use |
  stop_sequence | content_filter | error`)
- `usage`: `{input_tokens, output_tokens, total_tokens, cached_tokens,
  reasoning_tokens, audio_tokens}`

`NexusLLMStreamEvent`: canonical event taxonomy used by L5 for stream
translation:
- `response.start` (with model, id)
- `output_block.start { index, type }`
- `output_block.text_delta { index, delta }`
- `output_block.tool_use_args_delta { index, delta }` (raw partial JSON)
- `output_block.stop { index }`
- `response.usage { ... }`
- `response.stop { stop_reason }`
- `response.error { ... }`

Each provider's `extract` schema maps real wire events → canonical events;
each `build` schema maps canonical events → real wire events. This is the
key abstraction that makes O(N) schemas work instead of O(N²).

#### ai-gateway pass-through fast-path (short-circuit before L5)

Not all ai-gateway traffic needs to run the full schema engine.
**When the routing rule determines that no cross-model /
cross-format translation occurs**, the engine takes a fast-path:

- Detection only (identify provider + model for the routing
  decision itself)
- Tier 0 text extraction runs (for hook security checks)
- **No body is parsed into IR; no build happens**
- Bytes are forwarded to the target unchanged

Eligible when:
- Inbound model = outbound model (no routing), or
- Inbound schema = outbound schema and the mode is Pass-through
  (§1.3 mode 2)

Benefit: most ai-gateway traffic (no routing or same-provider
routing) takes the cheapest path, with performance close to a
naive proxy plus text extraction.

#### L5 — Translator (ai-gateway only)

Four modes (per the §1.3 table):

- **Mode 1 — Full translation** (cataloged → cataloged): full IR
  build, all capability checks, plus SDK contract test coverage.
- **Mode 2 — Pass-through**: takes the fast-path above; does not
  enter L5.
- **Mode 3 — Customer best-effort** (cataloged → customer schema):
  customer provides the build schema; the engine runs the same
  IR build pipeline, but **without SDK contract tests**; the
  customer owns correctness; the capability matrix is the
  customer's declaration; the platform trusts but fully audits.
- **Mode 4 — Refuse**: rejected by the routing-rule editor or at
  runtime with an error response.

Stream translation flow inside Mode 1 / 3: source-format chunks →
L1 decoder → L2 extract → canonical events → L2 build (target) →
wire chunks. Non-streaming: routine `IR → target.build →
wire_out`. The translator's state carries open output_block
indices, partial tool_use args buffers, and any target-required
wrapping events.

**Translation contract**: round-trip `A → IR → B → IR → A` must equal
identity modulo declared lossy fields. Lossy fields are explicitly listed
in the schema's `build.lossy_fields:` and surface as warnings, not errors.

**SDK compatibility tests**: for every `(source, target)` pair in the
curated catalog, CI runs the official SDK against the translated wire
and asserts no parse errors, no field mismatches, expected tool-call
parsing, expected stream completion.

### 3.5 Per-service consumption model

| Layer | ai-gateway | compliance-proxy | agent |
|---|---|---|---|
| **L0 Tier 0 last-resort text** | **✅ (floor, must never fail)** | **✅ (primary security floor)** | **✅ (primary security floor)** |
| L1 Decoder | All decoders | All decoders | All decoders |
| L2 `extract` | ✅ | ✅ | ✅ |
| L2 `build` | ✅ (cataloged + customer routing targets) | ❌ | ❌ |
| L2 `rewrite` | ✅ | ✅ | ✅ |
| L2 `detect` / `metadata` | ✅ | ✅ | ✅ |
| L2 `parameters` | ✅ | (display only, not applied) | (same) |
| L3 engine | Full | Full | Full |
| L4 IR | Full (extract + build) | Extract only (audit / hooks consume IR; never built back to wire) | Extract only (same) |
| L5 Translator | ✅ (4 modes) | ❌ | ❌ |
| ai-gateway fast-path | ✅ (no-route / Pass-through modes) | N/A | N/A |

**One schema YAML serves three services.** Adding `OpenAI Chat Completions`
schema delivers identification, extraction, audit, hook input, PII rewrite
across all three services; in ai-gateway it additionally enables
cross-provider routing as a target.

### 3.6 Performance strategy

| Optimisation | Mechanism | Expected gain |
|---|---|---|
| Schema compilation | YAML → typed Go runtime at startup (or schema reload) | Eliminates per-request parse overhead |
| Zero-copy selectors | Reuse gjson byte-paths; never `json.Unmarshal` a whole body unless explicitly needed | Bounded allocation |
| Byte-level rewrite | Patch only the byte ranges of overwritten string fields; do not re-marshal JSON | Preserves byte order / spacing; ~10× faster than re-marshal |
| Streaming SSE parse | Frame-by-frame; never buffer the whole stream | Latency parity with hand-written |
| Accumulator pool | `sync.Pool` for stream-state objects | GC pressure reduction at high QPS |
| Static dispatch table | Compiled accumulator dispatch — no reflection | < 10 ns per chunk dispatch overhead |
| Sniff short-circuit | Non-AI domain identified via SNI / Host → bypass the engine entirely | ~99% of cluster traffic at zero cost |

**Performance contract** (CI-enforced):
- Request extraction p50 < 200 µs, p99 < 1 ms (excluding upstream RTT)
- Stream chunk parse < 50 µs / chunk
- Rewrite added latency < 10% vs. passthrough
- Schema engine vs. hand-written Go: ≤ 1.5× per benchmark fixture

DSL restriction is what makes these targets achievable: no Turing
completeness → static analysis → JIT-style dispatch → predictable
performance, similar to how eBPF achieves predictable kernel-side
performance through verifier + JIT.

### 3.7 Safety / non-destructive guarantees

Layered as five contracts that the engine implementation must enforce:

1. **Wire byte fidelity** — Rewrite preserves byte-level layout of every
   field that wasn't explicitly modified, including key order and
   whitespace. JSON re-marshal is never used for rewrite paths.
2. **Protocol fidelity** — HTTP headers, Content-Length, Transfer-Encoding,
   SSE frame boundaries, gRPC framing are all preserved unless explicitly
   declared mutable in the schema.
3. **Failure mode** — Any failure in decode, extract, or rewrite results
   in passthrough of the original wire plus an error log. The engine
   **never emits half-rewritten bytes**. This is a hard invariant.
4. **Translation fidelity** — `build` warns or refuses (configurable
   per-pair) when an IR field has no representation in the target
   protocol. Round-trip tests in CI catch silent loss.
5. **Schema safety** — Customer-supplied schemas pass static validation,
   are signed, run under platform-imposed bounded-resource limits;
   custom Go accumulators must be registered at platform build time
   (no plugin loading at runtime).

### 3.8 Maintenance & drift governance

- **Per-schema fixture corpus** (mandatory). At least N (suggested: 5)
  real wire captures per adapter, each with `.expected.yaml`. CI
  re-plays on every schema change. Schemas without sufficient fixtures
  fail CI.
- **Upstream OpenAPI tracking** (API class only). For OpenAI, Anthropic,
  Gemini, etc., a CI job pulls upstream OpenAPI git changes weekly,
  diffs against our `x-nexus-known-keys`, and opens issues for new
  fields. This converts "schema drift surveillance" from manual to
  automated.
- **Production unknown-field telemetry** (Web / IDE class). Audit
  pipeline reports the share of `Extra` fields per adapter. When the
  ratio crosses a threshold (e.g. > 10% of requests have unknown
  top-level keys), ops gets alerted to refresh the schema.
- **Customer-facing client version pinning** (IDE class). Cursor /
  Copilot adapters bind to a client version range; capture mechanism
  records observed client versions for drift correlation.

### 3.9 Customer extensibility

**The primary use case is ai-gateway-side customer schemas
(internal LLM proxies / fine-tuned model gateways)** — that is the
customer's actual business-driven need. Web/App adapters are public
applications, so platform maintenance is more appropriate than
customer maintenance (customers have no proprietary information
advantage on chat.openai.com etc.).

- Schema YAML format is part of the documented public surface.
- Customers can author schemas for internal LLM proxies, fine-tuned
  model gateways, and bespoke API surfaces. Typical scale: 1–3
  customer schemas per customer.
- Customer schemas are loaded through a signed registration flow with
  the same static validator and bounded-resource limits as platform
  schemas.
- Customer schemas **cannot** declare custom Go accumulators — only
  reference platform-registered accumulators. This bounds the
  attack surface.
- Customer schemas **can** be ai-gateway routing targets in Mode 3
  (best-effort, §1.3); the customer owns correctness; on passing
  catalog promotion review they upgrade to Mode 1 (full fidelity).

---

## 4. Cross-provider translation (ai-gateway only)

### 4.1 Scope (binding)

Translation happens **only in ai-gateway** and **only when both source
and target providers are in our curated catalog**. The catalog is the
set of adapters where we ship a `build` section, real-SDK contract
tests, and a per-pair compatibility statement.

| Inbound | Outbound | Translation? |
|---|---|---|
| OpenAI SDK → catalog provider (OpenAI / Anthropic / Gemini / Bedrock / Vertex / DeepSeek / GLM / Mistral / Grok-API / Azure / Cohere / Cerebras / Groq / …) | Catalog | ✅ Yes |
| Anthropic SDK → catalog provider | Catalog | ✅ Yes |
| Gemini SDK → catalog provider | Catalog | ✅ Yes |
| Customer SDK speaking customer schema → catalog provider | Catalog | Not until customer schema enters the catalog |
| Any inbound → Web/IDE adapter | — | ❌ Web/IDE adapters cannot be routing targets |
| compliance-proxy / agent traffic | — | ❌ Translation never runs in these services |

### 4.1.1 Model-level capability check (pre-routing gate)

Before cross-provider translation runs, **the routing layer enforces
a capability compatibility check**. Routes that fail the check are
refused at edit time (in the routing-rule editor) or runtime, rather
than letting translation silently produce incorrect bytes.

| Inbound feature | Target capability missing | Handling |
|---|---|---|
| Image content block in inbound | target `supports_image_input=false` (e.g. o1) | **Refuse the route**; error response; routing-rule editor lint warning |
| Inbound requests streaming | target `supports_streaming=false` | Per-schema declaration: refuse / downgrade to non-stream / buffered fallback |
| Inbound contains `system` role | target `supports_system_role=false` (e.g. some early o1 variants) | Auto-migrate system content to `instructions` or first user message per the target schema's rule |
| Inbound requires tools | target `supports_tools=false` | Refuse the route |
| Context length exceeds target's `max_context_window` | — | Refuse the route + reason in the response |
| Inbound contains `response_format=json_object` | target `supports_response_format_json=false` | Warn + drop (lossy) |
| Inbound contains `temperature` | target `supports_temperature=false` (o1 family) | Silent drop (known no-op behaviour, no warning) |

**Convention**: a routing rule is valid iff source-model inbound
capabilities ⊆ target-model capabilities. The Control Plane's
routing-rule editor loads the catalog's capability matrix and
performs UX validation; customers see incompatibilities at save
time, not at runtime.

### 4.2 Translation complexity tiers

Field-level mapping difficulty between OpenAI Chat Completions and
Anthropic Messages illustrates the tiers (full mapping table lives in
the schema):

| Tier | Examples | Implementation |
|---|---|---|
| **Trivial** | `temperature`, `top_p`, same name same semantics | Schema rename / passthrough |
| **Structural** | `system` field promotion (top-level vs. inside messages); `content` string ↔ `[{type:text, text}]` | Schema structural rule |
| **Enum mapping** | `stop_reason` ↔ `finish_reason`; `tool_choice` shapes | Schema enum table |
| **Required-vs-optional** | `max_tokens` (Anthropic required, OpenAI optional) | Schema default-fill rule |
| **JSON serialisation** | `function.arguments` (string of JSON) ↔ `tool_use.input` (object) | Schema-invoked built-in `parseJSON` / `stringifyJSON` |
| **External IO** | `image_url:{url}` ↔ `image:{source:{type:base64,media_type,data}}` | **Code helper** (URL fetch with cache + timeout) |
| **Hard mismatch** | `response_format`, `seed`, `logit_bias`, `frequency_penalty` not supported by target | Schema declares lossy field; warn or refuse per pair config |
| **Stream protocol translation** | Anthropic SSE state machine (`content_block_start/delta/stop`) ↔ OpenAI flat chunks ↔ OpenAI Responses API named events | **Code (canonical event stream as bridge)**, see §4.4 |

### 4.3 What schema does, what code does

| Capability | Mechanism |
|---|---|
| Field rename, structural transform, enum map, default fill | Schema (declarative rule) |
| **Per-parameter handling (drop / rename / clamp / inject / force)** | **Schema `parameters:` table (§1.5.1 + §3.4 L2); engine applies per target model_pattern** |
| **Per-model capability compatibility check** | **Schema declares capability flags; routing layer enforces at save / runtime (§4.1.1)** |
| `parseJSON` / `stringifyJSON` for tool args | Schema invokes built-in helper |
| URL fetch (image base64, file content) | Registered Go helper, schema invokes by name |
| Lossy field policy (warn / refuse) | Schema declares; engine enforces |
| Stream event reordering / state machine | **Canonical event stream as IR** — both source and target schemas declare the mapping, engine bridges through canonical events, no per-pair custom code |
| Tool-call ID correlation across formats | Engine maintains per-conversation ID map |

### 4.4 Stream translation challenge

This is the hardest part of the project. The asymmetries between
formats are real:

- **OpenAI Chat Completions**: data-only SSE, flat chunks; `delta.content`
  string accumulation; tool_calls accumulated by index field; final usage
  in a `choices: []` chunk; literal `[DONE]` terminator.
- **OpenAI Responses API**: named-events SSE, 30+ event types, sequence-
  number ordered, hierarchical (`output_item` → `content_part` →
  `output_text`).
- **Anthropic Messages**: named-events SSE state machine; content blocks
  open/close by index; inside-block delta type discriminates `text_delta`
  vs `input_json_delta` (tool args); message_delta carries final stop
  reason and usage.
- **Gemini**: candidate-array form on `streamGenerateContent?alt=sse`;
  each frame is a complete candidate fragment.

**Approach**: every adapter's `extract` decomposes its native stream
events into the canonical event stream (`output_block.start`,
`output_block.text_delta`, `output_block.tool_use_args_delta`,
`output_block.stop`, `response.usage`, `response.stop`). The translator
buffers and reorders the canonical stream as needed for the target's
`build` schema, which describes how to emit the target's native frames
from canonical events.

This means stream translation is O(N) schemas (each provider declares
both directions to/from canonical events) instead of O(N²) per-pair
custom translators.

**Required validation**: round-trip and SDK contract tests are
non-negotiable. Without them, a subtle stream translation bug silently
breaks a customer's production traffic.

---

## 5. Risks and mitigations

### 5.1 Stream protocol translation correctness

**Risk**: A subtle bug in event reordering or buffering causes the
target SDK to mis-parse, produce empty content, or miss tool calls.
This is the highest-impact failure mode in the whole project.

**Mitigations**:
- Canonical event stream as the bridge (single point of conversion).
- Mandatory round-trip tests for every catalog pair.
- Real SDK integration tests: official OpenAI / Anthropic / Gemini /
  Vertex / Bedrock SDKs run against the translator output, asserting
  parse success and semantic equivalence.
- Property-based testing on canonical event streams (random valid
  sequences, both directions).
- Per-pair "compatibility certificate" gates production rollout.

### 5.2 IR coverage drift

**Risk**: Provider adds a feature (prompt caching marker, reasoning
budget, structured output mode) that the IR can't represent; either
field is silently dropped on translation, or extraction loses fidelity.

**Mitigations**:
- IR designed as **max-coverage of major providers**, with explicit
  `extensions: map[string]any` for provider-specific fields.
- IR versioned; provider schemas declare minimum IR version.
- CI job tracks upstream feature announcements and opens IR-extension
  proposals.
- Audit row preserves raw `Extra` so even unmodelled fields are
  recoverable post-hoc.

### 5.3 App-side protobuf without schema (Cursor)

**Severity: Medium** (Tier 0 floor means short-term schema lag does
not disable security capability — keyword / AI security detection
still runs).

**Risk**: Without a `.proto` file, schema authors must reference
field numbers by hand; Cursor renumbers fields → structured
extraction breaks.

**Mitigations**:
- `protobuf-raw` decoder produces JSON keyed by field number with
  type tags.
- Schema authors capture multiple versions of fixtures across observed
  client versions.
- Adapter binds to a client version range; out-of-range traffic falls
  to a generic "unknown protobuf" handler that audits with `Extra`
  but produces no Segments.
- **Tier 0 text extraction continues to work**: string fields inside
  protobuf-raw are dumped by L0 and fed to hooks; even when the
  whole adapter is broken, security detection can still catch PII
  / keywords.
- Client version drift telemetry surfaces in ops dashboards.

### 5.4 Customer-supplied schema safety

**Risk**: A malformed or hostile customer schema causes panics, OOM,
infinite loops, or gives the customer the ability to read other
customers' data.

**Mitigations**:
- Static validator before load; rejects unknown primitives, unbalanced
  resource declarations, references to unregistered accumulators.
- Bounded resources at runtime (memory cap per request, selector step
  cap, accumulator state cap).
- Customer schemas can never reference custom Go accumulators (only
  platform-registered ones).
- Customer schemas can never declare a routing target (no translation
  into customer formats until an adapter passes the curated-catalog
  contract review).
- Schemas signed at registration; signature verified at load.

### 5.5 Performance regression vs. hand-written code

**Risk**: Schema engine is slower than hand-written adapters in some
case, defeating the "performance parity" goal.

**Mitigations**:
- Per-fixture benchmark in CI with a hard 1.5× ceiling vs. hand-
  written reference; PR that breaches fails CI.
- Profiling included in the engine package; pprof endpoints exposed
  in dev mode.
- Hot paths (selector resolution, accumulator dispatch) hand-tuned
  with sync.Pool, static dispatch tables, and zero-allocation gjson
  paths.

### 5.6 SDK compatibility on translated wire

**Risk**: Translated wire is technically valid by spec but the target
SDK's parser is stricter than the spec and rejects it.

**Mitigations**:
- Real SDK integration tests in CI for every catalog pair (Python,
  TypeScript, Go SDK at minimum for major providers).
- Per-pair "known-incompatible field" list, surfaced in routing rules
  so customers see the limitations before depending on them.
- Conservative defaults: when in doubt, refuse translation rather than
  ship questionable bytes.

### 5.7 Web-side fingerprint risk under rewrite

**Risk**: Rewriting fields in claude.ai or chat.openai.com traffic
changes byte fingerprint, triggers anti-bot detection, and locks out
the user account.

**Mitigations**:
- Web-side schemas default rewrite to disabled.
- Even when enabled, byte-level rewrite preserves layout (no JSON
  re-marshal); minimum-edit-distance writer.
- Per-adapter rewrite-allowed flag, defaulting off.
- Documentation makes the trade-off explicit to ops.

### 5.8 DSL expressiveness creep

**Risk**: Each new provider tempts adding "just one more" primitive;
DSL bloats; static analysis weakens; performance regresses.

**Mitigations**:
- Primitive set frozen at design time at ≤ 7 accumulators and
  ≤ 8 decoders.
- Adding a primitive requires explicit design-doc review.
- New provider quirks first-resort: handle in L1 normalisation, not
  L3 primitive set.
- Last-resort escape: registered Go custom accumulator (≤ 30 LOC),
  one per adapter cap, total ≤ 5 across the platform target.

### 5.9 Model behaviour drift (provider changes capabilities)

**Risk**: A provider upgrades a model so that a previously
`silent_drop`'d parameter is suddenly accepted (or vice versa).
Example: OpenAI adds `temperature` support to the o1 family; our
`silent_drop` keeps stripping the field, and customer behaviour
silently degrades.

**Mitigations**:
- The capability matrix is diffed against vendor docs / changelog
  monthly (in parallel with the OpenAPI ingestion job).
- Real-SDK integration tests run weekly and automatically catch
  parameter compatibility shifts.
- Capability tables live in the schema; customers can override
  with signature.
- Audit rows record "what we transformed", so drift is forensically
  reconstructible.

### 5.10 Silent lossy parameter handling

**Risk**: A routing rule translates a `gpt-4o` inbound to `o1`;
`temperature` is `silent_drop`'d; the customer is unaware. The
customer expected strict temperature; in production they see
quality regression with no error.

**Mitigations**:
- Routing-rule editor UX explicitly surfaces the
  (source_model → target_model) parameter transformation matrix;
  customers see the lossy list before saving.
- Audit rows record each lossy transformation (field, original
  value, strategy, reason).
- High-sensitivity parameters (`temperature`, `seed`) default to
  `warn_drop` rather than `silent_drop`, so customers see them in
  audit; `silent_drop` is reserved for known-no-op fields (e.g.
  o1 ignoring `top_p` is a complete no-op).
- Customers can override the default policy at the schema layer.

### 5.11 Customer schema build errors (Mode 3)

**Risk**: A customer-supplied build schema (Mode 3) has bugs; the
translation produces invalid wire; the target provider rejects or
returns degraded responses. There is no SDK contract test
backing the customer schema, and the platform has not pre-validated
it.

**Mitigations**:
- Customer schemas not yet promoted to the catalog show the label
  "best-effort, customer-owned" in the routing-rule editor;
  customers must accept the disclaimer before saving the route.
- Audit rows mark `mode = customer_best_effort`, so when
  errors occur, the customer can immediately localise to their
  own schema.
- The platform offers a customer-schema self-test toolkit (local
  fixture replay + dry-run contract test framework) so customers
  can run their own tests before going live.
- A customer can request catalog promotion: enough fixtures +
  platform-run SDK contract tests; on pass, the route is upgraded
  to Mode 1 (Full translation).

### 5.12 Three-service integration regressions

**Risk**: A schema works in ai-gateway but breaks in compliance-proxy
because of buffering / framing differences across services.

**Mitigations**:
- Engine is decoupled from service integration; service-side wraps
  the engine identically.
- Three-service contract tests: every adapter is exercised against
  ai-gateway, compliance-proxy, and agent test rigs.
- Service-specific concerns (TLS framing, content-length, agent OS
  differences) live in the service shim, never in the schema.

---

## 6. Industry comparison

| System | Coverage | Schema-driven? | Cross-provider translation? | Web/IDE? | Open source? | Relevance to us |
|---|---|---|---|---|---|---|
| LiteLLM | API only, 200+ providers | ❌ Per-provider Python class (BaseConfig + transformation.py) | ✅ Via OpenAI-format IR | ❌ | ✅ | IR pattern is borrowable; implementation pattern is what we are leaving behind |
| Portkey | API only, 250+ models | ❌ Per-provider TypeScript class | ✅ | ❌ | ✅ | Same as LiteLLM |
| Cloudflare AI Gateway | API only | ❌ Native providers hand-coded; "Custom Provider" feature only supports URL passthrough — no declarative transform | Limited (unified API, not full translation) | ❌ | ❌ | Even Cloudflare did not solve declarative transform, validating that this is genuinely hard |
| Kong AI Gateway | API only | Partial (Lua plugin per provider) | Partial | ❌ | ✅ | Plugin pattern as a structural reference |
| WitnessAI | API + some Web | ❌ Uses ML intent classification instead of schema extraction | N/A | ✅ partial | ❌ | ML is a layer above extraction, not a substitute |
| Lasso Security | API + browser | ❌ LLM-as-a-Judge | N/A | ✅ partial | ❌ | Same |
| Netskope CASB | 50,000+ SaaS apps | ⚠️ Closed catalog | N/A | ✅ generic | ❌ | Validates per-app schema route at scale; commercial viability proven |
| Palo Alto Enterprise DLP | SaaS | ⚠️ App-ID + classification | N/A | ✅ generic | ❌ | Same |
| Wireshark dissectors | Network protocols | ✅ Lua + C decoder | N/A | N/A | ✅ | "Declarative DSL + host-language decoder" pattern reference |
| Suricata / Zeek | Network IDS | ✅ Rule DSL + C engine | N/A | N/A | ✅ | Same |
| OPA / Rego | Policy | ✅ Declarative DSL + Go engine | N/A | N/A | ✅ | Bounded-resource declarative engine reference |

**Bottom line**: nobody is publishing what we want to build. LLM Gateway
projects don't process passive traffic. AI Security and CASB vendors do
but keep their catalogs closed. Protocol parsing projects validate the
architecture but operate on lower-layer protocols. The combination of
(API + Web + IDE) × (extract + rewrite + translate) × open source × three-
service share is unique to our positioning.

---

## 7. Open questions (need decision before spec freeze)

These were raised in brainstorming but not yet locked. They go into the
plan-mode review with the user.

1. **IR scope** — Do we model OpenAI Responses API events as first-class
   in the IR, or treat it as a translation-only target? Recommendation:
   first-class (its hierarchical model is more expressive than Chat
   Completions and absorbs Anthropic content_blocks naturally).
2. **Build-section ownership for non-curated providers** — A customer
   adapter may want `build` for internal use without entering the
   curated catalog. Recommendation: allow `build` declaration but disable
   it as a routing target unless explicitly catalog-promoted.
3. **`Extensions` field discipline** — How strict on requiring a known
   provenance for `extensions:` keys? Recommendation: require
   `extensions[provider_id].field_name` shape; flat keys rejected.
4. **Migration order** — Which existing adapters migrate first?
   Recommendation: OpenAI Chat Completions → DeepSeek/GLM/Grok-API
   (free OpenAI-compat copies) → Anthropic (validate state-machine
   primitive set) → claude.ai (validate Web fail-soft) → Cursor or
   Copilot (validate IDE protobuf-raw).
5. **Custom Go accumulator ceiling** — Is 5 platform-wide a hard cap?
   Recommendation: yes; over-budget → redesign primitive set, not
   relax cap.
6. **Performance contract enforcement** — Hard fail in CI, or just
   warn? Recommendation: hard fail with a documented waiver process.
7. **Capability matrix source of truth** — Who maintains "o1
   does not accept temperature"-style parameter compatibility
   tables? Recommendation: the platform catalog maintains a
   baseline matrix (hand-curated from vendor docs + automated
   SDK testing); customers may override at the schema layer (e.g.
   their fine-tuned model accepts extra parameters); the engine
   merges catalog ⊕ customer overrides; customer overrides must
   be signed.
8. **Routing-rule editor lossy disclosure depth** — When a
   customer saves a route (source_model → target_model), what
   level of UX surfaces the drop / rename / clamp matrix?
   Recommendation: show all by default, customer must check a
   confirmation box to save; an "advanced mode" can later skip
   confirmation.
9. **Tier 0 extraction depth** — How deep should L0 attempt to
   decode (gzip / brotli / protobuf-raw / nested base64)? Cost
   versus recall trade-off. Recommendation: default to one
   level of gzip / brotli plus top-level JSON whole-tree scan;
   customers can configure a deeper mode (protobuf-raw + nested
   decompression), default off for cost control.
10. **Customer best-effort mode platform review level** — When a
    customer submits a Mode 3 build schema, does the platform
    do only static validation, or does it also require a minimum
    fixture coverage? Recommendation: static validation + at
    least one fixture round-trip (locally executed) + a customer
    sign-off; do not require platform-run SDK contract tests
    (those are the catalog-promotion gate).
11. **Catalog size cap and growth strategy** (driven by §1.4.1) —
    The ai-gateway catalog is naturally ≤ 30 by business reality,
    but what is the platform-supported cap? Recommendation: a
    soft cap of 50 (including customer schemas), beyond which an
    "expansion review" is required; this prevents one large
    customer from pulling the platform into the "200+ provider"
    engineering mode.
12. **Compliance-proxy/agent schema priority order** — Among the
    50–70 Web/App coverage targets, which come first?
    Recommendation: rank by enterprise-user access volume; the
    top 10 (chat.openai.com, claude.ai, gemini.google.com,
    Cursor, Copilot, etc.) must have schemas; the long tail
    relies on Tier 0 floor; add schemas as customer-reported hit
    rates / keywords reveal need.

---

## 8. Next steps

1. **User reviews this document.** If approved with possibly some open-
   questions resolutions, proceed.
2. **Write the implementation plan** via the writing-plans skill,
   following the mandatory workflow (architecture impact assessment →
   requirements doc → SDD → OpenAPI for any new APIs → code → tests →
   verify).
3. **Spike** (after plan), with the validation scenarios:
   - **L0 Tier 0**: feed an unregistered mock provider's wire
     (random JSON / SSE / protobuf-raw) and confirm hooks still
     receive keyword hits.
   - **Adapter family**: a single OpenAI family schema covering
     `gpt-4o` (Chat Completions) + `o1`/`o3` (Responses API +
     parameter drop/rename matrix) + at least one embeddings or
     audio variant.
   - **Anthropic Messages** (catalog, state-machine validation).
   - **claude.ai** (Web, fail-soft + Tier 0 floor validation).
   - **Cursor or GitHub Copilot** (IDE, protobuf-raw + Tier 0).
   - **Cross-model routing** (`gpt-4o` → `o1`, same provider
     different model): parameter transformation + capability
     check + audit recording.
   - **Cross-catalog routing** (`gpt-4o` → mock customer schema,
     Mode 3): validate the best-effort path + audit mode label.
   - **ai-gateway pass-through fast-path**: validate the
     non-cross-model case skips L5 with performance close to a
     naive proxy.
   - **OpenAI ↔ Anthropic full SDK contract test** (Mode 1).
   - **Benchmark**: schema engine vs. hand-written OpenAI adapter,
     ≤ 1.5×.
   Spike output: a runnable engine, family schema and variants,
   populated fixtures, benchmark numbers, and a CI report that
   covers all of the above.
4. **Migration**: incremental, adapter by adapter, behind a per-
   adapter feature flag that selects schema vs. legacy code path.
   Pre-GA per project policy means no parallel-run period in
   production code; the legacy adapter is removed in the same change
   that adds the schema.

---

## Appendix A — Sources consulted

- OpenAI: [Create Chat Completion](https://developers.openai.com/api/reference/resources/chat/subresources/completions/methods/create), [Chat Completions streaming events](https://developers.openai.com/api/reference/resources/chat/subresources/completions/streaming-events), [Streaming responses guide](https://developers.openai.com/api/docs/guides/streaming-responses), [Responses API streaming events](https://community.openai.com/t/responses-api-streaming-the-simple-guide-to-events/1363122)
- LiteLLM: [Provider integration](https://docs.litellm.ai/docs/provider_registration/), [OpenAI-compatible providers](https://docs.litellm.ai/docs/contributing/adding_openai_compatible_providers)
- Portkey: [Gateway repo](https://github.com/Portkey-AI/gateway)
- Cloudflare: [AI Gateway providers](https://developers.cloudflare.com/ai-gateway/usage/providers/), [Custom providers](https://developers.cloudflare.com/ai-gateway/configuration/custom-providers/)
- WitnessAI: [Product page](https://witness.ai/product/)
- Lasso Security: [Product page](https://www.lasso.security/)
- Netskope CASB: [Product page](https://www.netskope.com/products/casb)
- Palo Alto: [Enterprise DLP for ChatGPT](https://docs.paloaltonetworks.com/enterprise-dlp/administration/configure-enterprise-dlp/enterprise-dlp-and-ai-apps/create-a-security-policy-rule-for-chatgpt)
