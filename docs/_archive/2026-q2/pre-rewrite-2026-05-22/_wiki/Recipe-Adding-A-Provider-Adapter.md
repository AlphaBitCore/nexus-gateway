# Recipe Adding A Provider Adapter

*Audience: contributors onboarding a new LLM vendor or wire format into the AI Gateway.*

A provider adapter translates between Nexus's canonical OpenAI-shaped request/response bus and a specific vendor's wire format. Every adapter consists of a `SchemaCodec` (request encoding, response decoding), a `StreamDecoder` (SSE/chunked frame parsing), an `ErrorNormalizer` (4xx/5xx → canonical `ProviderError`), and a `Transport` (URL building, auth headers, HTTP execution). The `add-provider-adapter` skill (`Skill('add-provider-adapter')`) automates this checklist; the `adapter-conformance-check` skill (`Skill('adapter-conformance-check')`) audits an existing adapter against the 7 binding rules in `provider-adapter-architecture.md` §3a.

---

## Before writing code

Read [`provider-adapter-architecture.md`](https://github.com/AlphaBitCore/nexus-gateway/blob/main/docs/developers/architecture/services/ai-gateway/provider-adapter-architecture.md) in full, especially §3a (Rules 1-8). The rules are binding; violations have caused real production incidents. The short form:

- **Rule 1** — canonical format is OpenAI chat-completions shape. New canonical fields need an architecture-doc PR.
- **Rule 2** — each non-OpenAI adapter owns its full bidirectional translation; the OpenAI side is the identity codec.
- **Rule 3** — per-model wire quirks (HTTP 400 deprecations, parameter renames) live in the adapter's own package, wired via `AdapterSpec.PassthroughRewrite`.
- **Rule 4** — fields with no clean OpenAI mapping ride inside `nexus.ext.<provider>.<key>` via `canonicalext`.
- **Rule 5** — `SchemaCodec.EncodeRequest` receives canonical-or-empty input; ingress-format bodies must be canonicalized first via `canonicalbridge.IngressChatToCanonical`.
- **Rule 6** — streaming and non-streaming are both in scope; every codec rule applies to both paths.
- **Rule 7** — every prefix-list entry must cite an observed HTTP 400 (trace ID or direct test).
- **Rule 8** — `DecodeResponse` delegates to `canonicalbridge.DecodeViaShared`; only encoding stays in `spec_*/`.

---

## Step 1 — Decide the wire family

Three families cover most vendors:

- **OpenAI-shape** — DeepSeek, Moonshot, GLM, MiniMax, Groq, Fireworks, Together, xAI, Perplexity, Mistral, Cohere (OpenAI-compat surface), HuggingFace (OpenAI-compat).
- **Anthropic-shape** — Anthropic and downstream resellers.
- **Gemini-shape** — Google Gemini API and consumer Gemini surfaces.

Pick the closest family. If the vendor speaks a genuinely novel protocol (binary framing, Connect-RPC + Protobuf), add a `NonJSONDetector` in `packages/shared/transport/normalize/extract/detector.go` rather than building a full adapter from scratch — that is the canonical Tier-2 path.

## Step 2 — Implement the codec

Create `packages/ai-gateway/internal/providers/specs/<name>/codec/codec.go`. The codec implements `SchemaCodec`:

```go
type Codec struct{}

func (Codec) EncodeRequest(endpoint Endpoint, canonicalBody []byte, target CallTarget) (EncodeResult, error) {
    // canonical → vendor wire shape
    // For same-family passthrough, canonicalBody == nil; return zero EncodeResult.
    // Apply Rule 3 per-model quirks here.
    // Use canonicalext.Get for Rule 4 extension fields.
}

func (Codec) DecodeResponse(endpoint Endpoint, nativeBody []byte, contentType string) (DecodeResult, error) {
    // wire → canonical via canonicalbridge.DecodeViaShared (Rule 8).
    // Use canonicalext.Set for vendor-specific response fields.
    // Use canonicalext.WarnOnce for unrecognized canonical fields.
}
```

For non-streaming responses from OpenAI-compat vendors, `DecodeResponse` is often a one-liner delegating to `canonicalbridge.DecodeViaShared(FormatOpenAI, body)`.

## Step 3 — Implement the streaming session

Create `packages/ai-gateway/internal/providers/specs/<name>/stream/`. The session implements `StreamDecoder.Open`. For SSE-based providers reuse the shared SSE helper from `packages/shared/transport/streaming/sse.go`. For chunked HTTP/2 providers (Gemini, Cursor) reuse the chunked helper. The session emits canonical `CanonicalChunk` values; the hook pipeline and response cache both consume them.

## Step 4 — Implement transport and error normalizer

`transport.go` implements `Transport` (four methods: `BuildURL`, `ApplyAuth`, `Do`, `Probe`). `errors/errors.go` implements `ErrorNormalizer` — mapping vendor 4xx/5xx body shapes to `ProviderError{Class, Code, Message, RetryAfter}`. Cross-reference `error-taxonomy-architecture.md` for the full `ErrorClass` vocabulary (`ErrorClassRate429`, `ErrorClassContentFiltered`, `ErrorClassContextLength`, etc.).

## Step 5 — Build the AdapterSpec and register

Create `packages/ai-gateway/internal/providers/specs/<name>/spec.go`:

```go
func New() *AdapterSpec {
    return &AdapterSpec{
        Format:          FormatYourVendor,
        Transport:       newTransport(),
        SchemaCodec:     Codec{},
        StreamDecoder:   stream.New(),
        ErrorNormalizer: errors.New(),
        // If the vendor has per-model wire quirks, wire them here:
        // PassthroughRewrite: rewrites.Apply,
        // RequestShapes: []string{"chat-completions"}, // default; add "responses-api" only with empirical evidence
    }
}
```

Then register in `packages/ai-gateway/internal/providers/builtins/builtins.go` — add an entry to the provider set that boots the binary.

## Step 6 — Seed provider and models

Add the provider row and initial model catalog to `tools/db-migrate/seed/seed.ts`. Each model entry needs `provider`, `modelId`, `displayName`, capability flags (`supportsStreaming`, `supportsTools`, `supportsVision`, `supportsPromptCache`), and pricing (`inputPricePerMToken`, `outputPricePerMToken`). Pricing feeds the cost-estimation single source of truth — do not leave it as zero if the vendor publishes pricing.

## Step 7 — Map errors to ErrorClass

In `packages/ai-gateway/internal/providers/specs/<name>/errors/errors.go`, cover the vendor's rate-limit, content-filter, context-length, auth, and server-error shapes. Use string matching on the response body or HTTP status codes. Unmapped errors fall through to `ErrorClassUnknown`, which makes analytics less useful.

## Step 8 — Token-field stamp sweep (binding — 5 sites)

If the vendor returns new usage fields (e.g., Anthropic's `cache_creation_input_tokens`):

1. Add the field to `Usage` in `packages/ai-gateway/internal/providers/core/types.go`.
2. Add the column to `tools/db-migrate/schema.prisma` under `TrafficEvent` and generate the migration.
3. Stamp the field at all five sites in `packages/ai-gateway/internal/ingress/proxy/`: `proxy.go:handleNonStream`, `proxy_cache.go:handleStreamHit`, `proxy_cache.go:handleNonStreamHit`, `proxy_cache.go:handleStreamWithSubscription`, `proxy_cache.go:handleNonStreamWithSubscription`. Missing the four cache sites means all cache-hit traffic shows NULL on the new column.

## Step 9 — Smoke test (both stream and non-stream, per Rule 6)

```bash
# Non-streaming test:
curl -H "Authorization: Bearer <VIRTUAL_KEY>" \
     http://localhost:3050/v1/chat/completions \
     -d '{"model":"<new-model>","messages":[{"role":"user","content":"hi"}]}'

# Streaming test:
curl -H "Authorization: Bearer <VIRTUAL_KEY>" \
     http://localhost:3050/v1/chat/completions \
     -d '{"model":"<new-model>","stream":true,"messages":[{"role":"user","content":"hi"}]}'

# Confirm traffic_event rows have non-null usage:
docker exec postgres psql -U postgres -d nexus_gateway \
  -c "SELECT provider, model, total_tokens, cost_usd, error_class \
      FROM traffic_event ORDER BY emitted_at DESC LIMIT 3;"
```

If the vendor is reachable via the Anthropic or Gemini ingress (`/v1/messages`, `:generateContent`), run cross-format routing tests too — send requests via the non-native ingress and confirm the codec correctly bridges through canonical.

## Step 10 — Run the conformance check

```bash
go test -race -count=1 ./packages/ai-gateway/internal/providers/specs/<name>/...
go test -race -count=1 ./packages/ai-gateway/...
npm run check:arch-doc-triggers
```

Then invoke `Skill('adapter-conformance-check')` to sweep Rules 1-8 automatically. The skill checks for per-adapter logic leaked into `spec_adapter.go`, ingress bodies passed without canonicalization, error frames bypassing the helper, and prefix-lists missing observation comments.

---

## What links break if you skip this

- **Skipping Rule 5 (canonicalize before PrepareBody)**: cross-format routing (e.g., Anthropic ingress → your vendor target) forwards the Anthropic-shaped body verbatim to the vendor, producing HTTP 400. Same-family passthrough is not affected, making this gap easy to miss until a routing rule crosses families.
- **Skipping the token-field stamp sweep**: all traffic-event rows for cache-hit requests show NULL usage, breaking cost analytics and quota enforcement for cached responses.
- **Skipping error normalization**: all errors land as `ErrorClassUnknown`, disabling rate-limit retry logic and making the analytics "Errors by type" chart misleading.
- **Skipping the streaming test (Rule 6)**: codec parameter rules that strip or rename fields on non-stream requests silently miss the streaming path, sending disallowed parameters to the vendor in streamed calls.

---

## Canonical docs

- [`provider-adapter-architecture.md`](https://github.com/AlphaBitCore/nexus-gateway/blob/main/docs/developers/architecture/services/ai-gateway/provider-adapter-architecture.md) — §3a binding rules 1-8, the token-field stamp sweep, and the conformance gap history
- [`error-taxonomy-architecture.md`](https://github.com/AlphaBitCore/nexus-gateway/blob/main/docs/developers/architecture/cross-cutting/safety/error-taxonomy-architecture.md) — full `ErrorClass` vocabulary
- [`normalization-architecture.md`](https://github.com/AlphaBitCore/nexus-gateway/blob/main/docs/developers/architecture/services/ai-gateway/normalization-architecture.md) — canonical ↔ wire pipeline and the `DecodeViaShared` delegation contract

**Adjacent wiki pages**: [AI Gateway Provider Adapters](AI-Gateway-Provider-Adapters) · [Canonical Vs Wire Format](Canonical-Vs-Wire-Format) · [AI Gateway Streaming](AI-Gateway-Streaming) · [Recipe Index](Recipe-Index)
