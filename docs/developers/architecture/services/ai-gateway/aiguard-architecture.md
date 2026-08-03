# AI-Guard architecture

AI-Guard is a judge-model classifier: it sends a piece of content to an LLM "judge" and returns a structured decision — approve, reject, soft-block, or modify — with a confidence, labels, and span-level redaction suggestions. The AI Gateway serves it as an HTTP service; a compliance policy reaches it through the `webhook-forward` hook (see [hook-architecture.md](hook-architecture.md)). The implementation lives in `packages/ai-gateway/internal/policy/aiguard`.

## 1. Endpoints

`MountAIGuardRoutes` exposes two routes:

- **`POST /v1/ai-guard/classify`** — the native classify API, guarded by the internal service token. Its body is a `Request` (`detector_type`, `content`, optional `messages`, and a `context` block); it returns a `Response` on success, `400` for malformed JSON or a missing required field, `503 backend_unavailable` when the judge call fails, and `500` otherwise.
- **`POST /v1/ai-guard/compliance-webhook`** — the contract a `webhook-forward` hook speaks, so an existing webhook rule can call AI-Guard with no translation proxy. It accepts the webhook payload, extracts the content to classify by preference order (`normalizedContent` → embedded messages → a direct text field → request metadata → a raw-JSON fallback), classifies under `detector_type = compliance_webhook`, and returns a webhook-shaped response (`decision`, `reason`, `reasonCode`, `redactions`).

## 2. The classify pipeline

`Classify` funnels both endpoints through `classifyImpl`, a single ordered pipeline that is also the sole emission point for AI-Guard audit events:

1. **Input staging** — when the request carries structured `messages`, `inputstaging.Plan` selects the subset that fits the judge's context window and joins it into `content`. The configured strategy defaults to `full_truncated` — the judge's value is violation coverage, and dropping middle turns would blind it to content assembled across turns; under budget pressure inputstaging's built-in budget enforcement drops the oldest messages first and tail-truncates a still-oversized newest turn, so the forwarded content always fits. The budget is the judge model's context limit (fallback 8192 tokens when unconfigured) minus the measured judge prompt template minus 512 reserved for the reply. Overflow is fail-open: it is logged and counted, but the budget-bounded content still goes to the judge rather than blocking the request.
2. **Validation** — `detector_type` and `content` are required; their absence is a caller-contract `400`, not a backend failure.
3. **Cache key** — the content is normalized and hashed with the detector type and the backend fingerprint into a cache key.
4. **Cache lookup** — a hit returns immediately, stamped `cache_hit`, and still emits an audit event.
5. **Prompt render** — the configured prompt template is rendered with the detector type, content, upstream tags, and target provider/model.
6. **Backend call** — the judge is called under a timeout (default 30 seconds). A timeout or transport error becomes a `BackendUnavailable` (mapped to `503`) and emits a failure audit event.
7. **Persist + audit** — the response metadata is stamped, the result is cached when a TTL is configured, and a final audit event records the decision, latency, and (when available) token counts and cost.

## 3. Backends

A `Backend` is anything that turns a prompt into a `Response`. The configured `backendMode` selects one of two:

- **`configured_provider`** (`AdapterBackend`) — resolves a configured provider/model through the provider-target resolver, picks the matching adapter, and calls it with a canonical OpenAI chat body requesting a JSON object response (see [provider-adapter-architecture.md](provider-adapter-architecture.md)). Because the gateway owns this call, it stamps the judge's prompt/completion tokens and, via a price lookup against the in-memory model snapshot, the `ai_guard_cost_usd`.
- **`external_url`** (`ExternalBackend`) — calls an operator-supplied judge endpoint (`{url}/chat/completions`) with an optional bearer credential and custom headers, translating the configured Nexus model to its provider model id. It deliberately does not record cost: the gateway cannot know what an external classifier charges, so the cost fields stay zero and the audit row stores NULL.

Both backends parse the judge's reply through the shared decoder.

## 4. Request, response, and redactions

`content` is a flat text projection of the material to inspect; callers holding a `NormalizedPayload` pass its joined text so the judge sees the conversation in turn order. The `Response` decision is one of `approve` / `reject_hard` / `block_soft` / `modify`. Rather than returning a rewritten body, the judge returns `redactions` — each a byte span into `content` with an action (`redact` / `strip` / `replace`) and a replacement. The caller maps those spans back onto its canonical payload using the same `TransformSpan` framework that hook redactions use (see [normalization-architecture.md](normalization-architecture.md)), so a judge suggestion and a hook redaction are applied uniformly.

## 5. Decoding the judge output

Judge models do not reliably return clean JSON, so `DecodeJudgeOutput` handles three shapes: raw JSON, markdown-fenced JSON, and prose with an embedded object (extracted by brace-matching). It rejects a decision outside the allowed set, clamps confidence to `[0, 1]`, normalizes labels (trimmed, lowercased, deduplicated, sorted) for stable tag emission, and sanitizes redactions — dropping spans with invalid offsets and defaulting a missing action to `redact`.

## 6. Configuration

`AIGuardConfig` carries the backend mode, the provider/model or external URL/credential/headers, the prompt template, the judge timeout, the cache TTL, the input strategy, the model context limit, and a backend fingerprint. The fingerprint is folded into the cache key, so swapping the judge backend or model makes cached judgments under the old fingerprint unreachable rather than serving stale verdicts.

`ConfigCache` holds the config on the hot path behind a TTL with a single-flight load. It is invalidated push-style when the Hub pushes the `ai_guard` shadow key, so an admin edit takes effect immediately and the TTL is a backstop. Config loading is fail-open: when the loader fails and a prior snapshot exists, the cache keeps serving it rather than failing the hot path, surfacing an error only when no config has ever loaded.

## 7. Audit and cost

Every classify attempt — success, cache hit, or failure — emits a `TrafficEvent` through a sink that writes into the gateway's audit pipeline with `InternalPurpose = "ai-guard"`, so these internal classifier rows are distinguishable from customer traffic and hidden from billing displays by default. Token counts and `ai_guard_cost_usd` are stamped only on a fresh `configured_provider` call; cache hits, failures, and `external_url` calls leave them zero, which the writer persists as NULL. The cost stamping is described alongside the other cost fields in [cost-estimation-architecture.md](cost-estimation-architecture.md).

## References

- `packages/ai-gateway/internal/policy/aiguard/classify.go` — classify pipeline, input staging, audit emission
- `packages/ai-gateway/internal/policy/aiguard/types.go` — `Request`, `Response`, `Redaction`, `Metadata`
- `packages/ai-gateway/internal/policy/aiguard/backend_provider.go` — configured-provider backend and cost stamping
- `packages/ai-gateway/internal/policy/aiguard/backend_external.go` — external-URL backend
- `packages/ai-gateway/internal/policy/aiguard/decoder.go` — judge-output parsing and sanitization
- `packages/ai-gateway/internal/policy/aiguard/cache.go` — classify cache and key derivation
- `packages/ai-gateway/internal/policy/aiguard/config_cache.go` — config hot-path cache
- `packages/ai-gateway/internal/ingress/proxy/classify/classify.go` — classify and compliance-webhook HTTP handlers
- `packages/ai-gateway/cmd/ai-gateway/wiring/aiguard.go` — backend construction by mode, traffic sink
- `packages/ai-gateway/cmd/ai-gateway/wiring/thingclient.go` — AI-Guard route mounting
- `packages/shared/storage/configstore/aiguard.go` — `AIGuardConfig` schema
