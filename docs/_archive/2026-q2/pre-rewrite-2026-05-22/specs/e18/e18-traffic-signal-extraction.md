# E18 — Unified LLM Traffic Signal Extraction

**Status:** Draft — 2026-04-21
**Epic:** 18
**Depends on:** E-prior audit table consolidation (2026-04-14), shared hook framework, shared traffic adapter framework

## 1. Business Goal

Enterprise customers need complete visibility into AI traffic across the organization, not only the portion that flows through the AI Gateway `/v1/*` endpoints. Today, the AI Gateway writes `provider_name`, `model_name`, `prompt_tokens`, and `completion_tokens` to `traffic_event`, but the Compliance Proxy and the Agent do not — even though they see the same traffic after TLS bump / MITM decryption.

This epic closes that gap: the three data planes produce the same LLM signal columns, powered by a single shared detection library. Customers gain:

- Unified cost attribution across gateway traffic, reverse-proxy-routed traffic, and employee direct-to-provider traffic captured by the Agent.
- Per-API-key fingerprinting that lets compliance teams see which keys are burning spend without logging the keys themselves.
- Provider/model/token/status columns queryable from a single table, regardless of which data plane produced the row.

## 2. Scope

### In scope

- Extension of `packages/shared/traffic/Adapter` with `DetectRequestMeta` / `DetectResponseUsage`.
- Promotion of Compliance Proxy's SSE engine (`internal/streaming`) to `packages/shared/transport/streaming`.
- Addition of Agent response-side inspection (response buffering + SSE accumulation), previously absent.
- Nine provider detect adapters: OpenAI, Anthropic, Gemini, Azure OpenAI, MiniMax, AWS Bedrock, Vertex AI, Zhipu GLM, DeepSeek; plus a `generic` fallback.
- Three new columns on `traffic_event`: `api_key_class`, `api_key_fingerprint`, `usage_extraction_status`.
- Unification of the audit write path: Compliance Proxy's direct DB INSERT is removed; all three data planes flow through MQ to the Hub DB writer.
- Control Plane UI columns for the new fields on Analytics and Traffic pages.
- Admin OpenAPI endpoints exposing the new fields on traffic query responses.

### Out of scope

- Client-side body decryption for TLS connections the data planes do not already bump.
- Real-time streaming cost alerting (future epic).
- NDJSON-to-MQ replay tooling for Hub-unreachable scenarios (operational tooling backlog).
- Per-request audit of each SSE frame (only final usage is recorded).

## 3. User Roles & Personas

| Role | Need met by this epic |
|---|---|
| **Compliance Officer** | Query `traffic_event` by `api_key_fingerprint` to see which provider keys employees are using outside the sanctioned Gateway path. |
| **Cost Analyst** | Aggregate `estimated_cost_usd` grouped by `api_key_fingerprint` and `provider_name` across `source` = `(ai-gateway, compliance-proxy, agent)` to get the full organizational cost picture. |
| **Security Engineer** | Detect anomalous patterns: previously unseen `api_key_class`, keys appearing on employee devices that should only be in Gateway credentials, traffic to providers outside the allowed list. |
| **Platform Admin** | Monitor extraction reliability via `usage_extraction_status` distribution; high `parse_failed` rate indicates detector regression or new provider format. |
| **Data-Plane Developer** | Add a new provider with one adapter file that all three data planes automatically pick up, without touching three separate detection codepaths. |

## 4. Functional Requirements

### F1 — Shared Traffic Adapter detection interface (MUST)

The `packages/shared/traffic.Adapter` interface gains two read-only methods: `DetectRequestMeta(req, body) RequestMeta` and `DetectResponseUsage(resp, body) UsageMeta`. The result types carry the fields defined in F4/F5. Adapters MUST NOT mutate the request, response, or body.

### F2 — Provider coverage (MUST)

Adapters implementing both detect methods MUST exist for: OpenAI, Anthropic, Google Gemini, Azure OpenAI, MiniMax, AWS Bedrock, Google Vertex AI, Zhipu GLM, DeepSeek. A `generic` fallback adapter MUST return `Provider = "unknown"` and `UsageMeta.Status = non_llm` unless host-pattern matching identifies a supported provider.

### F3 — Data-plane wiring (MUST)

1. **AI Gateway** calls the detector in its request hook stage (after VK resolution, before policy hooks) and response hook stage. `api_key_fingerprint` = `SHA256(vk)[:8]` where `vk` is the caller's presented virtual key.
2. **Compliance Proxy** calls the detector in its forward handler after TLS bump and request/response body buffer. `api_key_fingerprint` = `SHA256(real_provider_key)[:8]` read from the decrypted request's auth header.
3. **Agent** calls the detector in its MITM relay after body buffer for both request and response directions. `api_key_fingerprint` source identical to Compliance Proxy.

### F4 — Request metadata fields (MUST)

`DetectRequestMeta` returns:

- `Provider string` — `openai` / `anthropic` / `gemini` / `azure` / `minimax` / `bedrock` / `vertex` / `glm` / `deepseek` / `unknown`.
- `Model string` — the raw model identifier extracted from request body (`$.model` for OpenAI-shape, equivalent for other providers) or URL path (Azure, Vertex).
- `Path string` — the HTTP request path.
- `ApiKeyClass string` — the provider-identifying key prefix fragment (see F6), empty for requests with no recognized key.
- `ApiKeyFingerprint string` — `SHA256(key)[:8]` lowercase hex. Empty if no key present.

### F5 — Response usage fields (MUST)

`DetectResponseUsage` returns:

- `PromptTokens *int` — nil when unknown (distinguished from zero).
- `CompletionTokens *int` — nil when unknown.
- `Status string` — one of `ok`, `streaming_reported`, `streaming_estimated`, `streaming_unavailable`, `parse_failed`, `no_body`, `non_llm`.

The pipeline MUST write NULL (not zero) for unknown token counts. Analytics queries MUST therefore exclude NULL rows when averaging or summing tokens, and MUST not treat NULL as zero cost.

### F6 — `ApiKeyClass` recognition (MUST)

| Class value | Matched against |
|---|---|
| `sk-ant-` | Anthropic `x-api-key` header |
| `sk-proj-` | OpenAI project-scoped key on `Authorization: Bearer` |
| `sk-` | OpenAI non-project key on `Authorization: Bearer` |
| `AIza` | Google Gemini `x-goog-api-key` header (or query param `?key=`) |
| `nvk_` | Nexus virtual key on `Authorization: Bearer` (Gateway-issued) |
| `azure-api-key` | Azure `api-key` header (no recognizable prefix — class labels the header type) |
| `aws-sigv4` | AWS Bedrock SigV4 `Authorization: AWS4-HMAC-SHA256 ...` (no raw key in transit) |
| `gcp-oauth` | Vertex AI `Authorization: Bearer <short-lived OAuth token>` |
| `glm-jwt` | Zhipu GLM JWT on `Authorization: Bearer` |

`ApiKeyClass` is a classification label, not a raw prefix extraction — the detector MUST NOT emit bytes that contain key secret material.

### F7 — SSE streaming usage extraction (MUST)

`packages/shared/transport/streaming` MUST accumulate streaming responses and extract usage via three tiers:

- **Tier 1 (`streaming_reported`)**: parse server-emitted usage frames. Anthropic: `message_delta.usage`; OpenAI: trailing `usage` frame before `data: [DONE]` (present when caller set `stream_options.include_usage`); Gemini: `usageMetadata` in any chunk.
- **Tier 2 (`streaming_estimated`)**: when Tier 1 yields no usage, run a tokenizer over captured prompt and completion text (tiktoken for OpenAI-shape, Anthropic tokenizer for Anthropic, SentencePiece for Gemini).
- **Tier 3 (`streaming_unavailable`)**: body truncated, parse failed, or tokenizer unavailable — record status, leave tokens NULL.

### F8 — Audit write-path unification (MUST)

Compliance Proxy MUST publish traffic events to `nexus.event.compliance` via `shared/mq` and MUST NOT write directly to the `traffic_event` table. The existing direct INSERT code path (`packages/compliance-proxy/internal/audit/sql.go`) MUST be deleted. Per CLAUDE.md development-phase policy, no parallel-legacy path is kept. Local NDJSON fallback for Hub unreachability is retained.

### F9 — Schema migration (MUST)

A Prisma migration adds three columns to `traffic_event`:

- `api_key_class TEXT` (nullable)
- `api_key_fingerprint TEXT` (nullable)
- `usage_extraction_status TEXT` (nullable, CHECK constraint on allowed values)

Plus an index on `(api_key_fingerprint, timestamp)` WHERE `api_key_fingerprint IS NOT NULL`.

The MQ message schema (`packages/shared/transport/mq/messages.go:TrafficEventMessage`) gains the same three fields. The Hub DB writer's INSERT SQL MUST bind them.

### F10 — Agent response inspection (MUST)

The Agent's MITM relay MUST be extended to buffer response bodies (10 MB cap, matching Compliance Proxy) and to drive SSE frames through the shared accumulator. `intercept/handler.ProcessResponse` MUST be wired into the relay flow. Agent-side usage extraction MUST produce the same accuracy tiers as Compliance Proxy.

### F11 — Admin API exposure (MUST)

Admin analytics and traffic-log OpenAPI specs under `docs/users/api/openapi/` MUST expose the three new fields on `traffic_event` response bodies. The Control Plane admin handler MUST include them in both list and detail responses.

### F12 — UI surface (SHOULD)

Control Plane UI analytics and traffic pages SHOULD render new columns for `api_key_class`, `api_key_fingerprint` (abbreviated display), and `usage_extraction_status`. All strings MUST be localized across `en`, `zh`, `es` per the i18n mandate.

## 5. Non-Functional Requirements

### NF1 — Performance budget (MUST)

- Request-side detection: p95 added latency ≤ 2 ms per request for bodies ≤ 64 KB. Larger bodies MAY add proportional latency; the 10 MB cap bounds the worst case.
- Response-side detection non-streaming: p95 added latency ≤ 3 ms per response.
- SSE accumulation MUST NOT block frame passthrough; per-frame processing ≤ 200 μs on typical hardware.

### NF2 — Memory bound (MUST)

- Request and response buffers capped at 10 MB each per request; the data plane MUST reject-or-skip (not OOM) on exceed.
- SSE live-mode accumulator MUST flush at configurable checkpoint size (default 500 chars); buffer-mode cap at 8 MB.

### NF3 — Security and privacy (MUST)

- `ApiKeyFingerprint` MUST be a cryptographic hash (SHA-256) truncated to 8 bytes. The detector MUST NOT emit the key itself, not even in logs or error paths.
- `ApiKeyClass` MUST be a pre-defined classification label (see F6), not a variable-length raw prefix that could leak secret bytes.
- Body reads by the detector are bounded to the fields declared in a **Data Access Declaration** (`docs/developers/specs/e18/e18-s1-adapter-interface.md`): `$.model`, `$.stream`, `$.usage.*` for response bodies; auth header prefix byte-range for request metadata.

### NF4 — Availability (MUST)

- Detection failure (parse error, unknown provider) MUST NOT block request forwarding in any data plane. Status column records the failure; traffic continues.
- Hub MQ unreachable MUST NOT block traffic. NDJSON fallback on local disk is written; no data plane blocks for persistence.

### NF5 — Backpressure isolation (SHOULD)

- Tokenizer estimation (Tier 2) MAY run on a bounded worker pool decoupled from the request path, with per-request deadline. If the deadline expires, status becomes `streaming_estimated_timeout` (sub-state of `streaming_unavailable`).

### NF6 — Observability (MUST)

- Each data plane exposes Prometheus metrics: `nexus_{service}_llmdetect_duration_seconds` (histogram), `nexus_{service}_llmdetect_status_total{status=...}` (counter, labels include `provider`), `nexus_{service}_streaming_extractor_tier_total{tier=...}` (counter).

## 6. Constraints and Assumptions

1. **Pre-GA, no back-compat (per CLAUDE.md):** Compliance Proxy's direct DB INSERT path is deleted, not deprecated. The new schema and new MQ fields ship together in one migration.
2. **Full MITM assumed:** All three data planes already terminate TLS on intercepted traffic (Agent via mimic cert, Compliance Proxy via TLS bump, AI Gateway as the origin endpoint). No client-side decryption work is in scope.
3. **Single-tenant enterprise:** Per-customer `api_key_fingerprint` collision risk is acceptable (8 bytes = 64-bit space, 10^9 unique keys per customer leaves negligible collision probability under birthday bound). Cross-tenant sharing is not a concern for this product.
4. **English-only artifacts:** All docs, code comments, commit messages, and UI strings added by this epic are in English. UI strings are localized to zh/es via the i18n system, not authored in those locales.
5. **Legal clearance out of band:** The decision to read request/response body fields (`$.model`, `$.stream`, `$.usage.*`) and auth header prefixes is a product/compliance-layer decision confirmed before this epic. The SDD includes a Data Access Declaration for downstream deployment review.
6. **Hub DB writer is the single-point schema owner:** Only `packages/nexus-hub/internal/jobs/consumer/traffic.go:insertTrafficEventSQL` inserts into `traffic_event` after this epic. Data planes publish messages; they do not own DDL knowledge.

## 7. Glossary

| Term | Definition |
|---|---|
| **LLM signal** | Provider, model, API key class, API key fingerprint, and token usage fields extracted from AI traffic, stored on `traffic_event`. |
| **Virtual key (VK)** | A Gateway-issued credential (prefix `nvk_`) presented by internal callers; mapped to real provider credentials internally by the AI Gateway. |
| **Api key class** | Classification label for the kind of API key seen on a request (e.g. `sk-ant-`, `nvk_`, `azure-api-key`). Not a raw prefix — it contains no secret bytes. |
| **Api key fingerprint** | `SHA256(key)[:8]` as lowercase hex. A non-reversible 8-byte identifier stable per key; enables aggregation without storing the key. |
| **Usage extraction status** | Enum describing how (or whether) token usage was extracted: `ok`, `streaming_reported`, `streaming_estimated`, `streaming_unavailable`, `parse_failed`, `no_body`, `non_llm`. |
| **Detect adapter** | A provider-specific implementation in `packages/shared/traffic/adapters/<provider>` that implements `DetectRequestMeta` and `DetectResponseUsage`. |
| **Streaming accumulator** | The component in `packages/shared/transport/streaming` that parses SSE frames and extracts usage via Tier 1 / Tier 2 / Tier 3. |
| **Data Access Declaration** | A section in the SDD documenting exactly which body fields and header byte ranges the detector reads, for deployment-time compliance review. |

## 8. Priority (MoSCoW)

### Must

F1, F2 (coverage of the 9 providers), F3, F4, F5, F6, F7, F8, F9, F10, F11, NF1, NF2, NF3, NF4, NF6.

### Should

F12 (UI surface — landed in this epic but can ship behind a flag if core wiring risks schedule), NF5.

### Could

- Retroactive fingerprint computation on pre-epic `traffic_event` rows (large table; probably skipped).
- SIEM bridge enrichment with the new fields (handled automatically once columns exist, but per-customer SIEM contract updates are out of scope).

### Won't (this epic)

- NDJSON-to-MQ replay tooling.
- Per-frame streaming audit (only final usage recorded).
- Raw key decryption for TLS connections not already MITM'd.
- New tokenizer for providers that lack a public tokenizer (status falls to `streaming_unavailable`).

## 9. Acceptance Criteria (epic-level)

1. All 9 provider adapters implement the detect interface; unit tests with golden request/response fixtures pass under `go test -race -count=1`.
2. `traffic_event` has `api_key_class`, `api_key_fingerprint`, `usage_extraction_status` columns in PostgreSQL; Prisma migration is reversible in dev.
3. Three data planes (AI Gateway, Compliance Proxy, Agent) produce traffic events carrying all nine LLM signal columns; integration tests confirm Hub inserts rows with fields populated.
4. Compliance Proxy direct-DB INSERT code is removed; `rg buildInsertSQL packages/compliance-proxy` finds no matches.
5. Agent MITM relay buffers response bodies and drives SSE through `shared/streaming`; end-to-end test captures a streaming chat response and records prompt/completion tokens with status `streaming_reported`.
6. Admin analytics API returns the three new fields; OpenAPI spec is regenerated and consumed by Control Plane UI.
7. Virtual-key fingerprint is computed and recorded on every AI Gateway row; analytics query grouped by fingerprint distinguishes between AI Gateway rows and Proxy/Agent rows via the `source` filter.
8. No new `TODO` / `FIXME` / placeholder code in production paths. All code is English. Each SDD story's acceptance criteria is satisfied.
