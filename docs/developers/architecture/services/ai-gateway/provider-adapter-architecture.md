# Provider adapter architecture

> Full provider adapter architecture write-up (§3a Rules 1-8, per-adapter walkthroughs) is queued in the docs-backfill program. This page currently captures only the AdapterSpec interface contract — the part that interacts with the endpoint typology unification (E87). Full canonical↔wire translation rules + per-adapter detail follow in a later commit.

## AdapterSpec interface (the codec contract)

Every provider adapter under `packages/ai-gateway/internal/providers/specs/<name>/` returns an `AdapterSpec` (`packages/ai-gateway/internal/providers/core/spec.go`) that the generic `specAdapter` composes into a runtime `Adapter`. The interface methods take `typology.WireShape` as the per-call dispatch parameter:

```go
type Transport interface {
    BuildURL(target CallTarget, shape typology.WireShape, stream bool) (string, error)
    ApplyAuth(r *http.Request, target CallTarget) error
    Do(ctx context.Context, r *http.Request) (*http.Response, error)
    Probe(ctx context.Context, target CallTarget) (*ProbeResult, error)
}

type SchemaCodec interface {
    EncodeRequest(shape typology.WireShape, canonicalBody []byte, target CallTarget) (EncodeResult, error)
    DecodeResponse(shape typology.WireShape, nativeBody []byte, contentType string) (DecodeResult, error)
}

type StreamDecoder interface {
    Open(r io.ReadCloser, shape typology.WireShape) (StreamSession, error)
}
```

The `shape` parameter tells the adapter which of its native wire shapes the call is for (e.g. OpenAI codec dispatches `WireShapeOpenAIChat` to chat-completions encoding, `WireShapeOpenAIResponses` to responses-API encoding, `WireShapeOpenAIEmbeddings` to embeddings encoding). The adapter rejects unsupported shapes with an error.

See [endpoint-typology-architecture.md](../../cross-cutting/foundation/endpoint-typology-architecture.md) for the full WireShape catalogue and the `(Format, WireShape)` projection model.

## Bridge dispatch contract

`canonicalbridge.Bridge` is the AI Gateway's canonical↔wire translation layer. It picks the target adapter by `provcore.Format` (one Format per adapter family) and dispatches the codec call with the adapter's native `WireShape` resolved via `chatWireShapeForFormat` / `embeddingsWireShapeForFormat` (lockstep-tested helpers in `bridge.go`). Adding a new adapter requires:

1. Define the adapter's `AdapterSpec` (Format + Transport + SchemaCodec + StreamDecoder).
2. Add the new Format to `chatWireShapeForFormat` / `embeddingsWireShapeForFormat` (or accept the OpenAI-family default for OpenAI-shape-compatible providers).
3. Add the new `typology.WireShape` constant if the adapter speaks a non-OpenAI shape (e.g. `WireShapeBedrockEmbeddings` was added for the Bedrock Titan/Cohere embedding wire).
4. Add the rule to `packages/shared/transport/typology/defaults.go` if a Nexus ingress path delivers requests in that wire shape.

## §3a binding (full rules in a later commit)

Per `CLAUDE.md` Mandatory rules: "Adapter format-translation follows `provider-adapter-architecture.md` §3a (binding)". The 8 §3a rules govern canonical OpenAI shape, non-OpenAI canonical↔wire translation, per-model wire quirks, extension fields via `canonicalext`, ingress canonicalization, streaming+non-streaming parity, and prefix-list comment policy. The full §3a write-up + per-adapter examples land in the next docs-backfill commit for this page.
