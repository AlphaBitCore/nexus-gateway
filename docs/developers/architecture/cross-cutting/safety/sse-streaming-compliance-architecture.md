# SSE Streaming Compliance Architecture

## What this doc covers

The end-to-end contract that makes a streaming Server-Sent-Events
response carry the same compliance + audit signal as a non-streaming
JSON response across all three ingress services (agent, compliance-
proxy, ai-gateway). Specifically:

- The 3 streaming modes (`passthrough`, `chunked_async`,
  `buffer_full_block`) and which service implements which.
- The `PreHookCallback` shared by all three so SSE hooks see a rich
  Registry-normalized payload instead of a flat-text fallback.
- The asymmetries between services (deliberate by design) and the
  visibility guards that make them obvious to operators.

This doc is the **single source of truth** for the SSE compliance
contract; per CLAUDE.md, code in the listed paths must be updated in
lockstep with this doc.

## Why this exists

The streaming pipelines (BufferPipeline + LivePipeline) run a
Registry-normalize step BEFORE the hook executor so every hook gets
the same canonical claim it gets on the non-stream path. Without it,
the compliance hook's `HookInput.Normalized` field would carry only a
flat-text concat of every SSE delta (`Kind="text"`), and hooks scoped
to AI traffic (rules that read `Normalized.Model`,
`Normalized.ToolCalls`, `Normalized.Reasoning`, etc.) would drop
through every match rule — making it impossible to:

- Block tool-call traffic by tool name.
- PII-redact assistant deltas that reference specific schema fields.
- Cost-classify cached-reasoning streams.
- Stamp `traffic_event.normalized_response` for SSE rows.

## Streaming modes

The admin policy is resolved exclusively through
`*streampolicy.Store` injected at boot via the shared
`streampolicy.BootStore` helper. The only source of truth is
`system_metadata['streaming_compliance.config'].default_mode` loaded
into the Store at startup. tlsbump's `WithStreamingPolicyStore(*Store)`
option lets the SSE handler read `Store.Get()` per-flow (always sees
the latest snapshot).

**Hub-push alignment (two-service, not three).** Agent and
compliance-proxy register a `streaming_compliance` shadow handler in
their configdispatch and are listed in
`ValidByThingType["compliance-proxy"]` and
`ValidByThingType["agent"]`; the Hub pushes the key to them and they
call `Store.ApplyShadowState` on each push. **ai-gateway does not**
receive Hub pushes for `streaming_compliance` — the key is absent from
`ValidByThingType["ai-gateway"]` and ai-gateway has no configdispatch
applier for it. ai-gateway's `streampolicy.Store` is boot-seeded from
`system_metadata` and is not updated at runtime. This is a deliberate
design boundary: ai-gateway's streaming mode is a boot-time service
configuration, not a live admin-tunable, because live stream-mode
changes on the ai-gateway path would be lossy for in-flight requests.

The admin's `streamingMode` policy (`agent_settings.streamingMode` /
per-domain override on `interception_domain`) resolves to one of
three modes via `shared/transport/streaming/policy.Resolve`:

| Mode | Behavior | Implemented by |
|---|---|---|
| `passthrough` | Bytes copied through without parsing; no hook executor, no audit normalize stamp (audit row carries empty `normalized_response`). Used for non-AI SSE / cert-pinned clients where compliance can't introspect. | `shared/transport/tlsbump/sse.go::handleSSEResponse case "passthrough"` (agent + compliance-proxy); `ai-gateway/internal/ingress/proxy/proxy_cache_passthrough.go::runPassthroughStream` (ai-gateway) — three-service consistent. |
| `chunked_async` (live) | Bytes copied through immediately; LivePipeline runs hook executor at every checkpoint (cumulative bytes hit `FirstInspectChars`, then every `ReinspectStepChars`). PreHook callback stamps `ci.Normalized` BEFORE each hook run. Low-latency + full compliance. | **Two implementations on purpose** — see "Two LivePipeline implementations" below. `shared/transport/streaming/live.go` (tlsbump-driven: agent + compliance-proxy transparent-forwarder shape); `ai-gateway/internal/platform/streaming/live.go` (cache-replay path with format transform + hold-back + Modify-rewrite + OpenAI-DONE toggle). |
| `buffer_full_block` (buffer) | Read full body into memory; run hook executor ONCE on complete content; replay to client only on Approve / Abstain; on a `block` (RejectHard) decision write a single error event. PreHook callback stamps `ci.Normalized` between Phase 1 (read) and Phase 2 (run hooks). Strongest enforcement; highest latency. | `shared/transport/streaming/buffer.go` — tlsbump callers (agent + compliance-proxy) + ai-gateway (`proxy_cache_buffer.go::runBufferStream`). Three-service consistent. **Limitation**: Modify (`redact`) decisions are not supported by this pipeline (no rewrite arm in Phase 3); ai-gateway logs WARN + treats as Approve. |

The two adjacent fast-paths:

- **ConnectRPC** (`application/connect+proto`, Cursor / api2.cursor.sh):
  binary 5-byte framed payloads. SSE pipelines cannot parse; tlsbump
  routes to `streaming.PassthroughWithConnectRPCExtract` which tees
  payload bytes through the adapter's `ExtractStreamChunk` for audit
  while raw-relaying the wire. PreHook does NOT fire on this path
  (audit stamp uses `stampSSEResponseNormalized` at end-of-stream
  instead).
- **Cache replay** (ai-gateway): SSE bodies served from
  `cache/stream/subscription.go` flow through the same LivePipeline
  as upstream responses, with the cached canonical chunks re-encoded
  into the current ingress's wire shape by a `StreamTranscoder`.

## The `PreHookCallback` contract

The canonical type lives in `shared/policy/hooks/core/types.go`:

```go
type PreHookCallback func(rawBody []byte, ci *HookInput)
```

Both streaming packages re-export it as a type alias so callers don't
need to import `hookcore` directly:

- `shared/transport/streaming.PreHookCallback`
- `ai-gateway/internal/platform/streaming.PreHookCallback`

### Builder

The canonical builder is `responseprehook.Build(Options) hookcore.PreHookCallback`
in `packages/shared/transport/normalize/responseprehook`. Both
tlsbump's `buildSSEPreHookCallback` and ai-gateway's
`buildStreamPreHookCallback` delegate to it. The builder's
responsibilities:

1. Lowercase `AdapterID` (Registry routing keys are lower-case).
2. Strip Content-Type parameters (`text/event-stream; charset=utf-8` → `text/event-stream`); Registry routes by bare media type.
3. Derive `Stream = (Direction==DirectionResponse && bareCT startswith text/event-stream)`.
4. Call `Registry.Normalize(ctx, rawBody, meta)`.
5. On success, stamp `ci.Normalized = &payload`.
6. Fire optional `OnPayload(payload, rawBody)` for service-specific side-effects.

Returns `nil` when `Options.Registry == nil` — callers treat that as
"no normalize layer wired; keep the flat-text fallback".

### Registry is the only wire-format decode route in tlsbump

tlsbump's audit/normalize path dispatches exclusively through
`Registry.Normalize` (keyed Tier 1 → content-sniff Tier 1.5 → pattern
Tier 2 → structural Tier 3). There is no per-adapter direct dispatch:
when the Registry declines a body, tlsbump falls back to the adapter's
`ExtractRequest`/`ExtractResponse` **segment extraction only** — a PII
surface that yields the flat-text payload, not a structured decode.
Every production consumer (agent bridge, compliance-proxy forwarder)
wires the Registry, so the flat-text fallback is reachable only for
bodies no tier claims.

### Panic-safety

Both the inner `Registry.Normalize` call and the `OnPayload` callback
are wrapped in `recover()`. A panicking Tier 1/2/3 normalizer or
audit-stamp closure logs a WARN and drops the pre-hook; the SSE
pipeline continues normally. Rationale: losing one stream's
normalized payload is recoverable; losing the entire connection is
not.

### Pipeline integration

```
BufferPipeline:
    Phase 1: read full upstream body into rawBuf (TeeReader)
    -> preHook(rawBuf.Bytes(), checkpointInput)         <-- #90 wiring
    Phase 2: pipeline.Execute(ctx, checkpointInput)
    Phase 3: replay buffered events or write error

LivePipeline:
    For each upstream frame:
      append to accumulated
      if accumulated >= nextInspect:
        preHook(accumulated, ci)                         <-- #90/#91 wiring
        pipeline.Execute(ctx, ci)
        nextInspect = accumulated + ReinspectStepChars
```

LivePipeline fires PreHook at **every** checkpoint with the
**cumulative** bytes — so by end-of-stream the callback has seen the
full body and `ci.Normalized` reflects the latest claim. tlsbump
relies on this incremental stamping to keep
`auditInfo.ResponseNormalized` fresh without a separate end-of-
stream pass.

### Service-side closures

tlsbump and ai-gateway differ in their `OnPayload` use:

- **tlsbump** (`packages/shared/transport/tlsbump/sse.go::buildSSEPreHookCallback`):
  `OnPayload` JSON-marshals the payload into `auditInfo.ResponseNormalized`
  so the audit row's `normalized_response` column lands populated.
- **ai-gateway** (`packages/ai-gateway/internal/ingress/proxy/sse_prehook.go::buildStreamPreHookCallback`):
  No `OnPayload`. Audit stamping happens elsewhere in the ai-gateway
  hot path; the PreHook only needs to make hooks see the right
  Normalized.

The cross-service consistency test
(`packages/shared/transport/normalize/responseprehook/cross_service_consistency_test.go`)
asserts that both call shapes produce a bit-identical
`ci.Normalized` JSON for the same body × adapter × content-type.

## Two LivePipeline implementations (intentional, not drift)

"Three data planes share one shared.LivePipeline" is only half true:
`ai-gateway`'s live mode uses its own
`internal/platform/streaming.LivePipeline`, while tlsbump (`agent` +
`compliance-proxy`) uses `shared/transport/streaming.LivePipeline`.
The two impls serve different architectural roles; unifying them would
violate the "less is more" rule by pushing ai-gateway-only features
into `shared/` where `agent`/`compliance-proxy` would carry them as
dormant complexity.

**What's actually shared (the contract that matters):**

- `PreHookCallback` type (`shared/policy/hooks/core/types.go`) — single
  source of truth for the per-checkpoint normalize-before-hooks contract.
- `Decision` enum (`Approve` / `Abstain` / `BlockSoft` / `RejectHard` /
  `Modify`) — single source.
- `responseprehook.Build` — single builder, both impls call it.
- `nexus_streaming_modify_degraded_total{reason="buffer_mode"}` counter —
  emitted from `shared.BufferPipeline.Process`, fires for all three
  services when a Modify decision lands under buffer mode.
- `nexus_normalize_panic_total{location="registry"|"on_payload"}` and
  `nexus_prehook_normalize_drop_total{adapter}` counters — both emitted
  from `shared/transport/normalize/responseprehook`. Single source of
  truth for "PreHook saw an error and dropped the Normalized stamp";
  disjoint by design (the drop counter skips when the err is the panic
  sentinel) so admins can sum without double-counting. All three data
  planes route their pre-hook through `responseprehook.Build`.
- `streampolicy.Store` + `BootStore` — single Store shape, single boot
  helper, three-service aligned.
- Default fall-back arm: unknown enum → passthrough (matches tlsbump's
  `resolveStreamingMode`; pinned by `TestDispatchStreamMode_UnknownEnumFallsBackToPassthrough`).
- `passthrough` mode relay — single `shared/transport/streaming.Passthrough`,
  ai-gateway's `runPassthroughStream` is a thin caller.

**What's intentionally different (and why):**

| Feature | tlsbump's shared LivePipeline | ai-gateway LivePipeline | Why divergent |
|---|---|---|---|
| Format transform | none (transparent forward) | `TransformChunk` per-event (canonical → ingress wire) | ai-gateway routes between ingress shapes (chat-completions ↔ responses ↔ messages); tlsbump forwards the upstream wire verbatim |
| Hold-back semantics | none (immediate flush) | `HoldBack` config + Modify-rewrite of held buffer | ai-gateway needs to atomically rewrite leading deltas under Modify; tlsbump cannot rewrite already-sent bytes |
| Stream terminator | always emits `data: [DONE]` | `EmitOpenAIDone` toggle (off for /v1/messages — Anthropic SDK chokes on stray [DONE]) | ai-gateway speaks multiple ingress wire shapes |
| Callbacks | none | `OnCheckpoint` + `OnStreamRewrite` (rec audit + Modify slot count) | ai-gateway populates a richer audit row |

Pushing all four into `shared` would pollute the package that `agent` and
`compliance-proxy` consume — they don't need any of these features. The
shared contract surface above is what guarantees the two impls produce
equivalent admin-observable behavior; behavioral parity is pinned by the
3-pipeline consistency test
(`packages/ai-gateway/internal/platform/streaming/cross_pipeline_consistency_test.go`)
plus the metric/log assertions in `shared/transport/streaming/buffer_test.go`
and `proxy_cache_dispatch_test.go`.

## Asymmetries (intentional + visible)

### Buffer mode: Modify decisions degrade to Approve

ai-gateway honors `buffer_full_block` by routing the
SSE handler through `shared.BufferPipeline` when
`StreamingPolicy.Get().Mode == ModeBufferFullBlock`. One residual
asymmetry remains by architecture:

`shared.BufferPipeline.Process` Phase 3 handles `RejectHard` (the
`block` action — a single error event) and the default (Approve /
Abstain replay) but has no `Modify` arm — buffer mode replays the
buffered events verbatim, so a hook that returns `Modify` (`redact`)
with `ModifiedContent` cannot rewrite the body the way `LivePipeline`'s
held-back deltas can be edited mid-stream before the first flush. (A
`BlockSoft` decision, if one is produced internally, folds to the
`block` action and takes the same RejectHard arm — there is no soft
path.)

**Three-service unified degradation signal**: the
degradation is detected inside `shared.BufferPipeline.Process`
itself — when `result.Decision == Modify` arrives from the executor,
the pipeline:

1. Emits a `WARN` log line with the `requestId` and rejection reason.
2. Bumps the Prometheus counter
   `nexus_streaming_modify_degraded_total{reason="buffer_mode"}`.

Because all three data planes (`ai-gateway`, `compliance-proxy`,
`agent`) buffer through the same shared pipeline, the metric and log
fire from a single source of truth — Prometheus scrape job/instance
labels distinguish which data plane saw the degradation. The
ai-gateway `bufferModeExecutor` adapter (struct in
`proxy_cache_buffer.go`) is now a pure type bridge from
`StreamHookRunner` (func) to `PipelineExecutor` (interface); it owns
no log or metric.

**Admin-visible warning surface**: the Control Plane
streaming-compliance settings endpoint
(`GET /api/admin/settings/streaming-compliance`, response shape in
`packages/control-plane/internal/settings/handler/settings/streaming_compliance.go`)
returns a `warnings: []string` field populated by `modeWarnings()`
whenever `default_mode == "buffer_full_block"`. The single warning
string names the constraint AND the counter to watch. The
control-plane-ui `SettingsStreamingComplianceTab` renders these
warnings verbatim below the mode picker — admins see the constraint
on both GET and PUT responses, plus a pre-save advisory when they
change the dropdown locally. No tooltip or hover — these are
constraints admins MUST see, rendered inline.

Admins who need Modify (`redact`) rewrites must keep `streamingMode =
chunked_async` (the default). buffer mode is intended for the
strongest-enforcement `block` (RejectHard) use cases where
rewriting is not part of the policy.

### HoldBack=false ≠ passthrough

LivePipeline's `HoldBack=false` config writes deltas to the client
immediately (no pre-checkpoint accumulation). It still runs the hook
executor at every checkpoint AND fires the PreHook callback —
distinct from `passthrough` which runs neither. The
`TestLivePipeline_HoldBackFalse_StillRunsHooksAndPreHook`
(`ai-gateway/internal/platform/streaming/holdback_semantics_test.go`)
pins this contract.

## Adapter-ID routing contract

`responseprehook.Build` populates `Meta.AdapterType` from the
caller-supplied `Options.AdapterID`, lower-cased. The two ingress
sides feed different sources into that field:

| Side | Source | Example values |
|---|---|---|
| tlsbump (agent + cp) | `audCtx.adapter.ID()` from `traffic.Adapter` | `"openai-compat"`, `"chatgpt-web"`, `"cursor"`, `"github-copilot"`, … |
| ai-gateway | `target.AdapterType` from `RoutingTarget` (= `provcore.Format` string) | `"openai"`, `"anthropic"`, `"gemini"`, `"vertex"`, `"voyage"`, … |

Both sets are registered in `shared/transport/normalize/codecs/register.go`:

- The 16-entry `openAICompatible` list registers
  `OpenAIChatNormalizer` under each value (covers both Formats and
  traffic IDs sharing the OpenAI wire shape).
- `"openai-compat"` is registered alongside `"openai"` so agent
  traffic with `traffic.Adapter.ID()="openai-compat"` resolves to
  the same normalizer.
- `bedrock` aliases the Anthropic normalizer (it fronts Anthropic
  Messages today).
- Path-only fallbacks (`::/v1/chat/completions`, `::/v1/messages`,
  `::/v1/responses`, `::/v1/embeddings`) catch traffic where the
  adapter ID is a hostname (`"api.anthropic.com"`) instead of a wire
  key.
- Tier 3 GenericHTTPNormalizer is registered under the wildcard
  `"*:*:*"` key as the catch-all that prevents `ErrUnsupported`.

Two compile-time consistency tests pin this surface:

- `packages/ai-gateway/internal/providers/core/format_normalize_consistency_test.go::TestEveryAllFormatsHasTier1Normalizer`
  iterates `provcore.AllFormats()` and asserts each Format claims a
  Tier 1 codec (not Tier 2/3 fall-through). Catches adding a Format
  without a corresponding `codecs.RegisterDefaultAIBuiltins` entry.
- `packages/shared/traffic/adapters/adapter_id_resolves_test.go::TestEveryBuiltinAdapterIDResolvesThroughRegistry`
  iterates `BuiltinTrafficAdapterIDs()` and asserts every adapter
  produces a non-empty payload for both JSON request AND SSE
  response. Catches a Tier 1 normalizer that hard-errors on the
  wrong wire shape instead of returning `ErrUnsupported` early on
  `meta.Stream` to let the Registry walk continue to Tier 2/3.

## Failure modes the contract protects against

- **Flat-text PII over SSE.** A hook scoped to "block prompt-cache
  references" reads `Normalized.PromptCacheID` — flat-text fallback
  has none, so the rule never fired on streamed responses. PreHook
  fix: the rich Registry payload exposes the field.
- **NULL `normalized_response` on SSE rows.** Without the PreHook
  stamp, an SSE audit row lands with `normalized_response=NULL`
  because the runtime stamp runs only on non-stream responses.
  tlsbump's `OnPayload` closure stamps it incrementally as PreHook
  fires; ai-gateway stamps elsewhere in its audit hot path. Verified
  by the cross-service consistency test.
- **Silent fallback to Tier 3.** A Tier 1 normalizer that
  hard-errors on a wire shape (e.g. voyage on SSE bytes) makes the
  service silently fall to GenericHTTP. Tests assert no Tier 1 codec
  returns a HARD error for the wire shapes it gets in prod.
- **Per-service drift.** If each service maintained its own
  buildXxxPreHookCallback implementation, a bug fix in one would
  leave the others stale. Both delegate to `responseprehook.Build`;
  the cross-service consistency test asserts payload-equality across
  the two call shapes.
- **Panicking normalizer takes down a stream.** Without the recover
  wrappers, a spec parser bug that panics on a malformed body would
  crash the SSE goroutine and drop the connection. PreHook wraps both
  `Registry.Normalize` and `OnPayload` in `recover()`; a panic logs
  WARN and drops the pre-hook only, the stream continues.

## Implementation notes

**Text accumulation in LivePipeline and BufferPipeline:** The streaming accumulation loops in `live.go` (`pendingText`, `accumulatedAll`), `buffer.go` (`fullText`), and `connectrpc.go` (`all`) use `strings.Builder` to keep hot-path allocations O(n) rather than the O(n²) of `str += chunk`.

## Code anchors

- Canonical type:
  `packages/shared/policy/hooks/core/types.go::PreHookCallback`
- Canonical builder:
  `packages/shared/transport/normalize/responseprehook/responseprehook.go::Build`
- tlsbump pipeline (agent + compliance-proxy):
  `packages/shared/transport/tlsbump/sse.go` (`handleSSEResponse`,
  `buildSSEPreHookCallback`, `stampSSEResponseNormalized`)
- shared streaming primitives:
  `packages/shared/transport/streaming/{buffer.go,live.go,locked_buffer.go}`
- ai-gateway pipeline:
  `packages/ai-gateway/internal/ingress/proxy/{proxy_cache.go,sse_prehook.go}`
  `packages/ai-gateway/internal/platform/streaming/{live.go,prehook_test.go}` (compliance: LivePipeline + LiveConfig + Hook types + PreHook integration)
  `packages/ai-gateway/internal/platform/streaming/format/{parser.go,writers.go,extract.go}` (pure SSE wire primitives — Parser/Event/WriteEvent/WriteTypedEvent/WriteDone/WriteError/ExtractDeltaText/OpenAIStreamDeltaPayload; zero dependency on hookcore or streaming package, so the format surface can evolve independently of the hook executor)
  `packages/ai-gateway/internal/platform/streaming/format/parser_error_paths_test.go` (write-failure + io.EOF error paths: asserts that a downstream write error surfaces cleanly rather than silently swallowing the fault, and that io.EOF from the parser terminates cleanly without a panic)
- Codec registry: `packages/shared/transport/normalize/codecs/register.go`
- Streaming policy: `packages/shared/transport/streaming/policy/`
- Cross-service tests:
  `responseprehook/cross_service_consistency_test.go`
  `providers/core/format_normalize_consistency_test.go`
  `traffic/adapters/adapter_id_resolves_test.go`
  `ai-gateway/.../streaming/holdback_semantics_test.go`
