# AI Gateway Provider Adapters

*Audience: contributors adding or modifying a provider adapter.*

The AI Gateway uses one canonical format internally — OpenAI chat-completions shape — and speaks many provider wire formats externally. Adapters perform the translation in both directions. Eight binding rules (§3a) define exactly where each translation concern lives, preventing the cross-adapter case-statements and silent field drops that have caused production incidents. Every new adapter must conform to all eight rules before merging.

---

## The canonical = OpenAI rule (§3a Rules 1-2)

**Rule 1** — All internal flow sees the canonical form: router input, cache key, hook input, audit envelope, and request lineage. The canonical fields are exactly OpenAI's: `model`, `messages[]`, `max_tokens` / `max_completion_tokens`, `temperature`, `top_p`, `top_k`, `stream`, `stop`, `response_format`, `tools[]`, `tool_choice`, `parallel_tool_calls`, `metadata`, `stream_options`. Adding a new canonical field requires an architecture-doc update — adapters do not extend canonical unilaterally.

**Rule 2** — Each non-OpenAI adapter owns its full bidirectional translation. When adding an Anthropic, Gemini, Bedrock, or Cohere codec, `SchemaCodec.EncodeRequest` converts canonical → wire and `SchemaCodec.DecodeResponse` converts wire → canonical. The OpenAI adapter stays a pure identity codec; it never carries case-statements for "this came from Anthropic so do X". The asymmetry is intentional: OpenAI shape is the bus, every other shape adapter wires itself into the bus.

## Per-adapter wire quirks (§3a Rule 3)

Per-model HTTP-400 deprecations, parameter renames, and mandatory clamping belong in the adapter that talks to that wire — never in the shared `spec_adapter.go` wrapper.

- Anthropic's deprecation of `temperature`/`top_p`/`top_k` for extended-thinking models lives in [`packages/ai-gateway/internal/providers/specs/anthropic/codec/codec.go`](https://github.com/AlphaBitCore/nexus-gateway/blob/main/packages/ai-gateway/internal/providers/specs/anthropic/codec/codec.go) (`anthropicModelRejectsSamplingParams`).
- OpenAI's `gpt-5.x` / o-series `max_tokens → max_completion_tokens` rename + temperature strip lives in [`packages/ai-gateway/internal/providers/specs/openai/rewrites/rewrites.go`](https://github.com/AlphaBitCore/nexus-gateway/blob/main/packages/ai-gateway/internal/providers/specs/openai/rewrites/rewrites.go) (`ApplyReasoningRewrites`), wired via `AdapterSpec.PassthroughRewrite`.
- Moonshot's fixed-temperature requirement for `kimi-k2.5`/`kimi-k2.6` lives in `internal/providers/specs/compat/moonshot/rewrites.go`, also wired via `PassthroughRewrite`.

**Rule 7** — Every prefix-list rule must be backed by an observed HTTP 400. The comment above each switch documents the observation with a date and the exact error message. Speculative rules silently flatten caller intent.

## Extension namespace (§3a Rule 4)

Fields with no clean OpenAI mapping — Anthropic's `thinking` block, Gemini's `thinkingConfig`, Anthropic's `cache_creation_input_tokens`, Bedrock's `anthropic_version` — ride on the canonical body under `nexus.ext.<provider>.<key>`. The package is `providers/canonicalext/`; use `canonicalext.Get`, `canonicalext.Set`, `canonicalext.ScanUnsupported`, `canonicalext.WarnOnce`.

There is also a reserved `nexus.<flag>` sibling namespace for cross-provider behaviour controls:
- `nexus.dry_run` (bool, default `false`) — when `true`, the gateway short-circuits at the cache-lookup boundary, runs the estimator, and returns an empty-content response with populated `usage`. Helpers: `canonicalext.IsDryRun`, `canonicalext.SetDryRun`.

Opening a new `nexus.<flag>` key requires a schema update in `docs/users/api/openapi/` because every ingress codec must preserve it through canonicalization.

## Canonicalization contract (§3a Rule 5)

`SchemaCodec.EncodeRequest` expects canonical input — or codec-empty bytes for same-shape passthrough. Callers with an ingress-format body (Anthropic `/v1/messages`, Gemini `:generateContent`) **must** canonicalize first via `canonicalbridge.IngressChatToCanonical(ingress, body, target)`. Skipping canonicalization causes the OpenAI identity codec to forward the ingress body verbatim, which produces a 400 from the upstream (or silent partial parse with garbage output).

## Streaming parity (§3a Rule 6)

A codec rule that strips a parameter from non-streaming requests must also strip it from streaming requests — the upstream rejects both. The streaming session's pre-dispatch body construction goes through the same `PrepareBody` path, so this typically falls out for free. The gap usually surfaces on error-frame construction: when the gateway hand-builds an SSE error frame and forgets to emit it in the ingress format's envelope shape (§9.5 of the adapter architecture).

## Decoding through `shared/normalize` (§3a Rule 8)

Every `SchemaCodec.DecodeResponse` and streaming session's response parser delegates to `canonicalbridge.DecodeViaShared`, which calls the matching `shared/transport/normalize/codecs/<format>` Tier-1 normalizer. The wire-emission side (`EncodeRequest`, `PrepareBody`, per-model strip/rename/clamp rules) stays in each `spec_*/` package.

Adding a new field to an upstream's response shape means adding the alias once in `shared/normalize/<format>.go` — every consumer (AI Gateway, Compliance Proxy, Desktop Agent, Hub audit) picks it up at once.

## Adapter structure on disk

Each provider adapter under `packages/ai-gateway/internal/providers/specs/<name>/` is composed declaratively from four parts:

```go
type AdapterSpec struct {
    Format             Format          // wire family ("openai", "anthropic", ...)
    Transport          Transport       // BuildURL + ApplyAuth + Do + Probe
    SchemaCodec        SchemaCodec     // canonical ↔ wire translation
    StreamDecoder      StreamDecoder   // wraps SSE / chunked frames into StreamSession
    ErrorNormalizer    ErrorNormalizer // upstream 4xx/5xx → canonical ProviderError
    PassthroughRewrite func(payload map[string]any, modelID string) []string
    RequestShapes      []string        // "chat-completions" (default), "responses-api"
}
```

The generic `specAdapter` wrapper in `providers/dispatch/spec_adapter.go` turns this spec into the runtime `Adapter` the rest of the gateway calls. No per-provider `register.go` convention exists; every adapter is enumerated by `providers/builtins/`.

## Where code lives

| Layer | What it owns | Location |
|---|---|---|
| Per-adapter codec | canonical ↔ wire for this adapter; per-model strip/rename/clamp | `internal/providers/specs/<adapter>/codec/codec.go` |
| Per-adapter passthrough rewrite | `PassthroughRewrite` hook | `internal/providers/specs/<adapter>/rewrites/rewrites.go` |
| Canonical bridge | ingress → canonical → target wire (composes codecs); response: target canonical → ingress wire | `internal/execution/canonicalbridge/bridge.go` |
| Canonical extension namespace | `nexus.ext.<provider>.<key>` helpers | `internal/providers/canonicalext/` |
| Generic wrapper | `specAdapter` — composes `AdapterSpec` into `Adapter`; NOT per-adapter knowledge | `internal/providers/dispatch/spec_adapter.go` |
| Traffic adapter registry | 50+ API/web/IDE adapters for compliance proxy + agent | `packages/shared/traffic/adapters/` |

## Adding a new provider adapter

The checklist is in [`provider-adapter-architecture.md`](https://github.com/AlphaBitCore/nexus-gateway/blob/main/docs/developers/architecture/services/ai-gateway/provider-adapter-architecture.md) §8. Key steps:

1. Decide the wire family (OpenAI-shape, Anthropic-shape, Gemini-shape, or custom).
2. Create `packages/ai-gateway/internal/providers/specs/<name>/` with `spec.go`, `codec/codec.go`, `stream/`, `transport.go`, and `errors/`.
3. Register in `providers/builtins/`.
4. Add to seed (`tools/db-migrate/seed/seed.ts`).
5. Run `/adapter-conformance-check` — this validates §3a compliance before merge.

For non-JSON wire formats (binary protobuf, Connect-RPC, Google batchexecute), add a `NonJSONDetector` in `packages/shared/transport/normalize/extract/detector.go` rather than writing a fresh per-host adapter from scratch. The detector framework is the canonical reusable path; bypassing it creates parallel maintenance burden.

---

## Canonical docs

- [`provider-adapter-architecture.md`](https://github.com/AlphaBitCore/nexus-gateway/blob/main/docs/developers/architecture/services/ai-gateway/provider-adapter-architecture.md) — Full §3a Rules 1-8 with worked examples and the conformance-gap audit table
- [`normalization-architecture.md`](https://github.com/AlphaBitCore/nexus-gateway/blob/main/docs/developers/architecture/services/ai-gateway/normalization-architecture.md) — Three-tier normalization pipeline and `DecodeViaShared` delegation contract
- [`provider-coverage.md`](https://github.com/AlphaBitCore/nexus-gateway/blob/main/docs/developers/architecture/services/ai-gateway/provider-coverage.md) — Adapter status matrix across all provider surfaces

**Adjacent wiki pages**: [AI Gateway Overview](AI-Gateway-Overview) · [AI Gateway Providers And Models](AI-Gateway-Providers-And-Models) · [Canonical Vs Wire Format](Canonical-Vs-Wire-Format) · [AI Gateway Streaming](AI-Gateway-Streaming) · [Recipe Adding A Provider Adapter](Recipe-Adding-A-Provider-Adapter)
