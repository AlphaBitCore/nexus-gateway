# SSE Response Cache — Design Spec

> Status: Draft (brainstorming complete, awaiting user review)
> Owner: AI Gateway team
> Date: 2026-05-06
> Related work: E28-S6 (canonical hub), E28-S7 (cross-format streaming transcoder)

## 0. TL;DR

Today the AI Gateway response cache (`internal/cache`) is non-streaming
only — `classifyCachePreLookup` returns `SKIP_STREAM` whenever
`isStream=true`. SSE is the production main path (Anthropic / Claude
Code, Cursor, every modern SDK that defaults to `stream:true`), so the
biggest cost lever the gateway has is currently turned off.

This spec adds streaming cache while **upgrading the non-streaming
cache to the same key formula and the same broker model**, so SSE
and non-SSE share **one architecture, one cache key recipe, and one
in-flight request-coalescing layer**. The two paths only diverge at
the very end (output encoding: SSE frames vs JSON).

The four primary cost levers:

1. **Streaming cache HIT** — repeated SSE prompts skip the upstream
   call entirely, replayed at full speed (no inter-frame sleep) while
   preserving the original chunk granularity (so SDK streaming UIs
   still see "progressive" arrival).
2. **In-flight request coalescing** (`streamcache.Broker`) — when
   N concurrent identical requests arrive while the first one is
   still pulling the upstream, all N share that one upstream
   connection. This is the largest single cost lever for SSE because
   a stream lasts 10–60 s and the collision window is ~100× wider
   than for non-streaming.
3. **Cross-ingress cache sharing** — the cache key is computed on
   the body **after** `Adapter.PrepareBody` (which translates
   OpenAI-shape → Anthropic-shape, rewrites `model` to
   `ProviderModelID`, strips reasoning-model–incompatible params,
   etc.). So the same prompt sent through OpenAI ingress vs Anthropic
   ingress to the same upstream model produces the same cache key.
4. **Hook-rule version safety** — *not via* fingerprinting in the key
   (rejected during brainstorming). Instead, response hooks **always
   run on every request**, including cache HIT, so rule updates take
   effect immediately. Cache stores raw upstream content; hooks are
   a per-request layer applied on top.

Status of the canonical-hub philosophy (E28-S6/S7): this spec
extends it. The cache layer joins the canonical bridge as another
"input goes in canonical → output comes out canonical" surface.

## 1. References

- Existing cache implementation: `packages/ai-gateway/internal/cache/cache.go`
- Streaming pipeline: `packages/ai-gateway/internal/streaming/{sse.go,live.go}`
- Cross-format streaming transcoder: `docs/sdd/e28-s7-streaming-transcoder.md`
- Canonical hub: `docs/sdd/e28-s6-canonical-hub-completeness.md`
- Adapter spec: `packages/ai-gateway/internal/providers/spec_adapter.go`
- Phase 4–7 in the proxy handler: `packages/ai-gateway/internal/handler/proxy.go`
- Forward-header allowlist (security adjacent): `packages/ai-gateway/internal/providers/spec_adapter.go:52` (and per-format extension in each `spec_*/spec.go`); see also the companion document
  `docs/superpowers/specs/2026-05-06-forward-header-allowlist-context.md`
  which is independent of this spec.

## 2. Goals

- **G1 — SSE responses cacheable.** Streaming requests on
  `/v1/chat/completions` and `/v1/messages` (and the cross-format
  variants enabled by E28-S7) can hit a cache.
- **G2 — Single-flight broker.** N concurrent identical streaming
  MISS requests collapse to **one** upstream call, with all N
  receiving the same chunk stream in real time.
- **G3 — Architecture parity.** Non-streaming and streaming paths
  share the cache layer, the broker layer, and the cache key
  formula. They diverge only at the encoding step (JSON vs SSE).
- **G4 — Hook integrity.** Compliance hooks run on every request,
  including cache HIT, so rule changes take effect with zero
  invalidation latency. Cache content is decoupled from any
  client-specific hook decision.
- **G5 — Cross-ingress / cross-alias cache sharing.** The cache
  key is computed on the post-`PrepareBody` body so requests that
  differ only in client-side model alias (`auto`, `gpt-4o`,
  `gpt-4o-2024-08-06`) or in ingress shape (OpenAI body vs
  Anthropic body, both targeting the same upstream model) hit the
  same cache entry.
- **G6 — Stream content fidelity on HIT.** Stream cache replay
  preserves the original chunk granularity from the producing
  upstream call. No "single frame with full text" replay (SDK UIs
  remain progressive); replay just runs at full speed because
  there is no inter-token inference latency.
- **G7 — Modify the non-streaming cache too.** The non-streaming
  cache adopts the same key formula (G1 alias-collision bug is
  fixed) and the same broker (in-flight coalescing for non-stream
  traffic too, even if the win is smaller than SSE).

## 3. Non-goals

- **NG1 — Cross-stream/non-stream cache sharing.** A request with
  `stream:true` and the same prompt with `stream:false` are kept
  in **separate** cache entries. The `stream` field is intentionally
  retained in the cache key body. Reasons in §6.3.
- **NG2 — Cache content rewriting on HIT.** Cache stores raw upstream
  content. Per-client compliance Modify rewrites are applied by the
  per-request hook layer, every time, including on HIT.
- **NG3 — Hook fingerprint in cache key.** Considered and rejected
  in brainstorming. Hooks always run; key stays decoupled from hook
  rule version.
- **NG4 — Stream replay pacing.** No `time.Sleep` between replayed
  frames. Streaming HIT runs at I/O speed.
- **NG5 — Bedrock streaming.** Tracked under T-BEDROCK-STREAM in
  E28-S6; out of scope here. When that ships, Bedrock cache will
  drop in via the same broker without further design.
- **NG6 — Forward-header allowlist changes.** That is a separate
  product decision tracked in the companion document
  `2026-05-06-forward-header-allowlist-context.md`.
- **NG7 — Audit-storage redesign for cache HIT.** Existing
  `traffic_event` capture continues to work; we add a new
  `CacheStatus = HIT_LIVE` enum value to distinguish broker fan-out
  from a true Redis HIT.

## 4. Current state — relevant facts

These are the load-bearing facts you must keep in mind while
reading the rest. Everything in §6 builds on them.

### 4.1 Phase order in `proxy.go`

```
Phase 1 readBody                  → raw client body (JSON, ingress shape)
Phase 4 Routing                   → routeResult.Targets[] — RoutingTarget(s)
Phase 4.1 Cross-format gate
Phase 4.5 Quota
Phase 5 Request hooks             → may rewrite body
Phase 5.5 Cache lookup            → BuildKey(provider, model, body)
Phase 6 fetchUpstream
   ↓ specAdapter.Execute
   ↓   prepareBody:
   ↓     - rewritePassthroughModel:  body["model"] = ProviderModelID
   ↓     - applyOpenAIReasoningRewrites: strip reasoning-incompatible params
   ↓     - SchemaCodec.EncodeRequest:    cross-format translation
   ↓   final body sent to upstream
Phase 7/8 stream / non-stream response
```

`prepareBody` is currently a private function inside `specAdapter`.
This spec promotes it (see §6.4).

### 4.2 Existing cache key recipe

`cache.go:BuildKey`:

```go
key = SHA256("v1\nprovider=" + providerName +
             "\nmodel="    + modelID +
             "\nbody="     + body) // body = post-request-hook body
```

`provider` and `model` come from `routeResult.Targets[0]`. `body`
is the post-hook body, which still contains the **client-typed**
`"model"` string (e.g. `"auto"`, `"gpt-4o"`) — `model` rewriting
happens later in `prepareBody`, not before cache lookup. This is
the source of cache-collision gap **G1**: two clients with the
same prompt but different client-side model aliases miss each
other's cache.

### 4.3 Existing chunk model

Stream sessions emit `providers.Chunk`:

```go
type Chunk struct {
    Delta          string
    ReasoningDelta string
    ToolCallDeltas []ToolCallDelta
    Usage          *Usage
    Done           bool
    RawBytes       []byte
    NativeEvent    string
}
```

This is canonical (provider-side) — same shape regardless of
upstream protocol. The cache layer will store sequences of these.

### 4.4 Existing forward-header allowlist (security context)

Outbound requests to upstreams already strip every client header
except `accept`, `user-agent`, `content-type` (basic allowlist in
`spec_adapter.go:52`) plus per-spec extensions
(`anthropic-beta`, `openai-organization`, `x-goog-user-project`,
etc., declared in each `spec_*/spec.go`). Authorization is
re-applied by `Transport.ApplyAuth` from upstream credentials.
**Cache design does not change this**; this fact is recorded so
the security review of cache HIT/MISS paths is short.

## 5. Design decisions (locked)

| # | Decision | Rationale |
|---|---|---|
| D1 | Store streaming responses as **chunk timelines** (sequences of canonical `ChunkRecord`s); store non-streaming responses as **canonical response JSON**. Two distinct entry types, one cache. | Stream HIT must preserve chunk granularity for SDK UI progressivity. Non-stream HIT does not need chunks. |
| D2 | **Hook always runs.** Cache HIT, broker fan-out, and MISS all run the response hook pipeline per-request. Cache stores raw upstream content. | A `(rules, response)` pair does not determine the hook decision — `(rules, response, IP, VK, org, region, request_id)` does. Skipping the hook on HIT is a compliance hazard. Also avoids the audit gap of HIT requests "not appearing" in compliance event streams. |
| D3 | **Cache key uses post-`PrepareBody` final body.** `Adapter.PrepareBody` is promoted from a private method to part of the `Adapter` interface. Phase 5.5 calls `PrepareBody` once before lookup; the resulting bytes are reused if MISS to skip a duplicate `PrepareBody` inside `Execute`. | Final body reflects what the upstream actually receives. This naturally absorbs alias normalization, parameter stripping, and cross-format translation, fixing G1 and gaining cross-ingress sharing. |
| D4 | **Cache key retains `provider`, `ProviderModelID`, and the `stream` field** in body. | Provider is never in body — must be explicit. ProviderModelID is in body for OpenAI/Anthropic but not for Gemini/Vertex/Bedrock (path-param model) — must be explicit for safety. `stream` is retained because streaming and non-streaming upstream endpoints are not always semantically equivalent (different URLs, different protocols, different rate buckets). |
| D5 | **Single broker abstraction for stream and non-stream MISS.** Per-cache-key `Broker` owns the upstream connection plus a chunk ringbuffer; subscribers (HTTP requests with the same key) join via reference count; cancel-on-zero-subscribers; persist on upstream success. | Architecture parity between SSE and non-SSE. Non-streaming broker is a degenerate case (one terminal "chunk"). Same code, lower maintenance. |
| D6 | **No "leader" concept in the broker.** First subscriber kicks the upstream; subsequent subscribers join the ringbuffer. Any subscriber leaving (including the "first") just decrements ref-count. Upstream is owned by the broker, not by any subscriber. | Eliminates leader-failover state-machine complexity present in alternatives A/B/C/D from brainstorming. Robust against the most common failure (first client closes connection in Claude Code-style usage) without dropping siblings. |
| D7 | **Replay vs broker subscription share an interface.** A `ChunkSubscription` interface abstracts both "live broker subscription on MISS" and "cached chunk replay on HIT". The transcoder + LivePipeline + writer code path is identical for HIT and MISS. | Single SSE downstream pipeline, one set of tests for the post-`ChunkSubscription` stages. |
| D8 | **No backward compatibility for cache entries.** v1 entries are abandoned on schema bump. Pre-GA, no users; CLAUDE.md "no backward compatibility" rule applies. | Schema evolution can use `"v2\n"` prefix in `BuildKey`; on first lookup, v1 entries are not found, get evicted by TTL. |
| D9 | **Cache write condition: upstream normal termination + canonical schema validity.** Streaming: terminator chunk arrived (`Done=true`) and assembled canonical response has at least usage and stop_reason. Non-streaming: HTTP 2xx and `SchemaCodec.DecodeResponse` succeeded. Hook decisions do not gate cache write. | Cache is upstream ground truth. Per-request hook outcome is per-request. Decoupled. |
| D10 | **Hot-flush stream replay (G6).** No `time.Sleep` between replayed chunks. Each chunk is a separate `Write+Flush` so SDK UIs see the natural multi-chunk boundary. | Speed equals "no inference delay" while preserving fidelity. |

## 6. Architecture

### 6.1 Three-layer model

```
┌─ Per-request layer (each HTTP request) ───────────────────────────┐
│ readBody → routing → Phase 5 request hook                         │
│         ↓                                                          │
│ adapter.PrepareBody(req)  ← promoted to public; pure function;    │
│         ↓                  no network call                         │
│ key = BuildKey(target.ProviderName, target.ProviderModelID,       │
│                canonicalize(finalBody))                            │
│         ↓                                                          │
│ cache.Lookup(key)                                                  │
│   HIT (isStream)  → StreamEntry → replayChunkSubscription         │
│   HIT (!isStream) → ResponseEntry → ingress encoder → JSON write  │
│   MISS            → broker.Subscribe(key, leaderFn)                │
│                      → if first subscriber: stamp MISS,            │
│                          leaderFn pulls upstream                   │
│                      → else (broker already exists for this key):  │
│                          stamp HIT_LIVE, join the live ringbuffer  │
└────────────────────────────────────────────────────────────────────┘
                  ↓ (only on MISS)
┌─ Broker layer (per cache key, in-flight) ─────────────────────────┐
│ First subscriber triggers leaderFn → upstream session              │
│ Chunks pushed to ringbuffer + notify all subscribers               │
│ ref-count == 0 with stream not done → cancel upstream              │
│ Upstream normal terminate → AssembleCanonical → cache.Store        │
│ Upstream error → broadcast error frame; do not write cache         │
└────────────────────────────────────────────────────────────────────┘
                  ↓
┌─ Per-subscriber layer (back to per-HTTP-request) ─────────────────┐
│ ChunkSubscription (interface; same shape for HIT replay & MISS)   │
│         ↓                                                          │
│ if isStream:                                                       │
│   chunkSSEReader → transcoder (E28-S7) → LivePipeline             │
│     → response hook (always runs) → cappedTeeWriter → SSE write   │
│ else:                                                              │
│   accumulateChunks → canonical response → ingress encoder         │
│     → response hook (always runs) → JSON write                    │
└────────────────────────────────────────────────────────────────────┘
```

### 6.2 Cache key (v2 formula)

```go
// In Phase 5.5, before lookup:
finalBody, err := adapter.PrepareBody(req) // pure function
if err != nil { /* 4xx upstream-encode error */ }

canonical := canonicalizeJSON(finalBody)
//   - sort JSON object keys recursively (stable across SDK
//     serialisations)
//   - retain "stream" / "stream_options" fields (NG1)
//   - do NOT manually rewrite "model" — PrepareBody did it already
//   - do NOT strip OpenAI reasoning params — PrepareBody did it
//     already

key = "nexus:cache:" + sha256Hex(
    "v2\n" +
    "provider=" + target.ProviderName +
    "\nmodel="  + target.ProviderModelID +
    "\nbody="   + canonical
)
```

`canonicalizeJSON` is the only addition to the body-shaping logic.
Everything else is delegated to `PrepareBody`. This is the smallest
correct recipe.

The `"v2\n"` schema prefix lets us bump again later without a
migration. Existing v1 entries simply do not get matched; they
expire by TTL.

### 6.3 Cache entry types

Two value types, one Redis namespace:

```go
package cache

// StreamEntry — value for streaming cache hits.
type StreamEntry struct {
    Provider  string
    Model     string         // ProviderModelID
    Chunks    []ChunkRecord  // in order, canonical (no provider-native bytes)
    Usage     providers.Usage
    CachedAt  time.Time
    Schema    string         // e.g. "stream/v1"
}

// ChunkRecord is the cache-friendly encoding of providers.Chunk.
// We deliberately drop RawBytes (those are ingress-specific bytes;
// the transcoder will re-encode for the current ingress at HIT).
type ChunkRecord struct {
    Delta          string                    `json:"d,omitempty"`
    ReasoningDelta string                    `json:"r,omitempty"`
    ToolCallDeltas []providers.ToolCallDelta `json:"t,omitempty"`
    Usage          *providers.Usage          `json:"u,omitempty"`
    Done           bool                      `json:"done,omitempty"`
    NativeEvent    string                    `json:"e,omitempty"` // optional
}

// ResponseEntry — value for non-streaming cache hits.
type ResponseEntry struct {
    Provider          string
    Model             string
    CanonicalResponse []byte // assembled canonical response JSON
    Usage             providers.Usage
    CachedAt          time.Time
    Schema            string         // e.g. "response/v1"
}
```

Stored in Redis as JSON (or `gob`/`msgpack` if size becomes an
issue — start with JSON for debuggability). Discriminator: each
entry's `Schema` field. Lookup decodes the discriminator first to
choose the right struct.

Size budget: stream entries can be ~3–10× a non-streaming entry's
size at the same prompt (one record per chunk, but each record
carries only deltas, not full text). For typical Claude-style
responses (50–500 chunks), entry size is well under the existing
`MaxBodyBytes` payload-capture cap.

**Hard cap on stream entries**: refuse to write a stream entry whose
serialised size exceeds a configurable limit (default 1 MiB,
matching the response-cache convention). Oversized streams are
served live but skip cache write; record this with a structured-log
event and a `cache_writes_skipped_total{reason="too_large"}` metric
so operators can see if the cap is too tight.

**TTL** is unchanged from today's response cache (`defaultTTL = 1 *
time.Hour`, configurable). Stream and non-stream entries share the
same TTL knob — one config field, one Redis-side expiry behavior.

**Redis key prefix** stays `nexus:cache:` for both entry types. The
schema discriminator inside the value disambiguates them. We do not
introduce per-kind prefixes because the v2 key formula already keeps
stream and non-stream entries on disjoint hashes (the `stream` field
in the body differs).

### 6.4 `Adapter.PrepareBody` promotion

Today (`spec_adapter.go:196`):

```go
func (a *specAdapter) prepareBody(req Request) ([]byte, []string, error) {
    // ...
}
```

Refactor:

```go
// in providers/adapter.go
type Adapter interface {
    Execute(ctx context.Context, req Request) (*Response, error)
    Probe(ctx context.Context, target CallTarget) (*ProbeResult, error)

    // PrepareBody is the pure-function part of Execute up to but
    // excluding the network call. Returns the final body sent to
    // upstream and the list of in-place rewrites applied (for the
    // x-nexus-coerced header). Idempotent; no side effects.
    PrepareBody(req Request) (finalBody []byte, rewrites []string, err error)
}
```

`Execute` then becomes a thin wrapper:

```go
func (a *specAdapter) Execute(ctx context.Context, req Request) (*Response, error) {
    body, rewrites, err := a.PrepareBody(req)
    if err != nil { return nil, /* 4xx */ }
    // ... existing URL build, header forward, ApplyAuth, Do, decode
}
```

Phase 5.5 calls `PrepareBody` once for the lookup; on MISS, the
already-prepared body bytes are passed back into a new
`ExecuteWithBody(ctx, req, body, rewrites)` to avoid a second
encode.

This is a structural refactor but adds zero new logic. The handful
of `prepareBody` callers all live inside the providers package.

### 6.5 `streamcache.Broker`

New package: `packages/ai-gateway/internal/cache/stream`.

```go
package streamcache

// ChunkSubscription is the read side, used by both HIT replay and
// MISS broker subscription.
type ChunkSubscription interface {
    // Next returns the next chunk in order, or io.EOF when the
    // stream finished cleanly, or a *providers.ProviderError on
    // upstream failure / broker-broadcasted error.
    Next(ctx context.Context) (providers.Chunk, error)
    // Close releases the subscription. On the broker path, this
    // decrements the broker's ref-count.
    Close() error
}

// Broker owns one upstream stream session per cache key.
type Broker struct {
    // unexported fields: ringbuffer, subscribers map, mu sync.Mutex,
    // upstreamCtx + cancel, finalChunks []providers.Chunk for replay
    // window, etc.
}

// Registry deduplicates Broker by cache key.
type Registry struct { /* sync.Map[key]*Broker */ }

// LeaderFn launches the upstream call and returns a StreamSession
// the broker will pump into the ringbuffer.
type LeaderFn func(ctx context.Context) (providers.StreamSession, error)

// Subscribe returns a ChunkSubscription for the given key plus a
// boolean indicating whether this call was the first subscriber
// for that key (i.e. the one that triggered leaderFn).
//
// If no Broker exists, a new one is created and leaderFn is called
// to start the upstream; isFirstSubscriber is true. If a Broker already
// exists, the caller joins it; isFirstSubscriber is false. Previously-
// buffered chunks are delivered in order before live ones.
//
// The proxy handler uses isFirstSubscriber to set audit.CacheStatus:
// true → "MISS"; false → "HIT_LIVE".
func (r *Registry) Subscribe(ctx context.Context, key string, leaderFn LeaderFn) (sub ChunkSubscription, isFirstSubscriber bool, err error)
```

Lifecycle invariants (D6):

- Upstream is owned by the broker, not by any subscriber. Ref-count
  reaching zero triggers `cancel()` on the upstream context.
- Subscriber `Close()` (HTTP request cancel, finished delivering,
  hook reject) decrements ref-count.
- Upstream normal termination (`Done=true` chunk received): broker
  flushes the ringbuffer to subscribers, calls `cache.Store`, then
  closes itself.
- Upstream error (`*providers.ProviderError`): broker broadcasts
  the error to all live subscribers (each formats its own
  ingress-specific error frame); cache is NOT written. Broker
  closes.
- Subscriber arriving after broker closed normally: Registry returns
  a nil broker; the proxy retries the lookup and either gets a HIT
  (just-written entry) or restarts a new broker. This race is
  benign and self-correcting.

Hook execution (D2) is not the broker's concern. The broker only
distributes chunks. Each subscriber runs its own LivePipeline +
hook pipeline + writer.

**Non-streaming brokers** are the same `Broker` type with a
degenerate ringbuffer of one terminal chunk. The non-streaming
`leaderFn` calls `adapter.ExecuteWithBody`, decodes the response
into a single `providers.Chunk` with `Done=true` carrying the
canonical response in `Delta` (or a small wrapper field on
`ChunkRecord`), pushes that one chunk, and closes the broker. All
subscribers see exactly one `Next()` returning that chunk, then
`io.EOF`. No new abstractions; the streaming broker code path
handles non-streaming as a one-chunk special case. (Implementation
note: the wrapper field name on `ChunkRecord` is left to the
implementation phase — what matters is that one canonical concept
flows through.)

### 6.6 Replay subscription (HIT path)

```go
// internal/cache/replay.go (new)
func NewReplaySubscription(entry *cache.StreamEntry) ChunkSubscription
```

A trivial `[]ChunkRecord` cursor; `Next` returns the next record
converted to `providers.Chunk` until the slice is exhausted, then
`io.EOF`. Implements the same `ChunkSubscription` interface as
broker subscriptions, so the downstream pipeline (chunkSSEReader →
transcoder → LivePipeline → hook → writer) is identical.

### 6.7 Hook integration

Per D2, all three paths (HIT, broker fan-out, MISS) run the
response-hook pipeline. The existing `streaming.LivePipeline`
already runs hooks per-subscriber; nothing changes there.

For non-streaming HIT, we currently skip hooks (`proxy.go:388–429`).
We change that path to run the response hook pipeline before
writing to the client.

This adds a hook evaluation cost on every HIT. Hook eval is
typically ms-scale; the upstream-call savings dwarf it.

### 6.8 `CacheStatus` enum extension

`audit.CacheStatus` gets one new value:

```go
const (
    CacheStatusHitLive CacheStatus = "HIT_LIVE" // joined an in-flight broker; no upstream call by this request
)
```

Final enum after this spec ships:
- `HIT` — Redis hit, broker not involved (entry already persisted).
- `HIT_LIVE` — *new*: this request joined an in-flight broker for
  the same key. No upstream call was triggered by this request, but
  the response was streamed/assembled live from the broker's
  upstream session rather than from a Redis entry.
- `MISS` — this request was the broker leader (or no other
  subscribers); this request triggered an upstream call.
- `DISABLED`, `SKIP_NO_CACHE` — unchanged.
- **`SKIP_STREAM` is removed**; it has no remaining call site after
  this spec ships.

Cost attribution: `HIT` and `HIT_LIVE` both count as "this request
did not pay for an upstream call". Dashboards can roll them up if
desired, or keep them separate to distinguish "warm-cache hit" from
"in-flight coalesced hit".

The `x-nexus-cache` response header surfaces the same enum
to clients for debuggability.

## 7. Interactions with other subsystems

### 7.1 Routing

Cache key includes `target.ProviderName` and `target.ProviderModelID`
from `routeResult.Targets[0]`. Effects:

- Weighted/sticky/region-based routing: same body routed to two
  different targets produces two different cache keys, so each
  target's cache is independent. Correct.
- Failover: if Targets[0] fails and Targets[1] succeeds, the cache
  key uses Targets[0]'s provider/model but the actual content was
  produced by Targets[1]. **This is preserved current behavior**;
  audit columns (`RoutedProviderID`, `RoutedModelID`) track the
  actual target. Acceptable in semantically-equivalent failover
  configurations; flagged here for future consideration.
- Broker per cache key: a single weighted-routing client population
  fan-outs into separate brokers per chosen target, which is the
  intended behavior.

### 7.2 Hooks

- **Request hooks** (Phase 5): output becomes part of the body
  hashed in cache key. Deterministic hooks → shared cache.
  Non-deterministic hooks (e.g. timestamp injection) → no cache
  sharing; this is a configuration choice, not a cache bug.
- **Response hooks** (per-subscriber): run on every request,
  including HIT. Modify rewrites are applied to the client-bound
  bytes only; cache stores raw upstream content. Reject responses
  block the client but do not poison the cache (cache write is
  decoupled, gated only by upstream normal termination).

### 7.3 E28-S7 cross-format streaming transcoder

The cache-HIT path on streaming uses canonical chunks and runs
them through the same `transcoder` selected by E28-S7's
`Bridge.NewStreamTranscoder(ingress, target)`. Both HIT and MISS
share the transcoder, so cross-format support extends to cache
HIT for free as soon as E28-S7 ships.

### 7.4 Quota

Unchanged. Quota check is Phase 4.5, before cache lookup. On HIT,
quota.Reconcile is still called with the cached usage values —
preserving the current behavior where billing reflects the cost
the user "would have paid" had this request been served fresh.
Whether HIT requests are charged is an org-policy concern handled
at analytics time.

### 7.5 Audit / payload capture

- `traffic_event.cache_status` will surface `HIT`, `HIT_LIVE`,
  `MISS`, `MISS_FOLLOWER`, `DISABLED`, `SKIP_NO_CACHE`.
- Payload capture: HIT path captures the bytes written to client
  (post-hook rewrite). Same as today's non-streaming HIT.
- Streaming HIT: existing `cappedTeeWriter` already mirrors bytes
  to `rec.ResponseBody`; no change needed.
- Stream entries in Redis are not stored in `traffic_event` — they
  live only in the cache layer.

### 7.6 Forward-header allowlist

No interaction. The allowlist is enforced by `specAdapter.Execute`
upstream of cache. Cache neither inspects nor stores upstream
request headers. Safety guarantee: cache content cannot leak
client-specific headers because they were already stripped before
the upstream call.

## 8. Security

### 8.1 What the cache stores

- Stream entry: canonical chunks (text deltas, tool-call deltas,
  reasoning deltas, usage). **No request body**. **No request
  headers**. **No VKs**. No client IP. No authentication state.
- Response entry: canonical response JSON (same content
  cardinality as a non-stream upstream response). **No request
  body in entries**. The cache key SHA-256 is non-reversible.

### 8.2 Cross-tenant data leakage

Cache key includes the post-`PrepareBody` body, which includes the
prompt content. Prompt collisions across tenants therefore share
cache entries. This is **the same posture as today's non-stream
cache** — by design, equal upstream input ⇒ equal upstream output
⇒ shareable cache.

If the deployment includes prompts with tenant-identifying content
(e.g. system prompts that name the tenant), the natural body-hash
divergence keeps tenants apart. If prompts are tenant-agnostic
(rare in real usage), shared cache is a feature, not a bug.

Tenant-isolation must be enforced at the **input** (via VK + ACL +
hooks), not at the cache layer.

### 8.3 Hook bypass risk

D2 (hooks always run) closes the audit-bypass concern: every
request gets a fresh response-hook evaluation, including HIT.
Compliance event emission is per-request, not per-cache-write.

### 8.4 Forward-header context

(See §7.6.) Existing allowlist already strips VK / Authorization /
internal `x-nexus-aigw-*` headers before the upstream call. This
spec does not alter that posture.

## 9. Migration

Pre-GA, no backward compatibility. On deploy:

- Existing v1 cache entries (non-streaming) become unreachable
  (their key has `"v1\n"` prefix; new lookups use `"v2\n"`).
  TTL evicts them. No data loss.
- The `audit.CacheStatusSkipStream` constant is removed; any audit
  rows already stamped with it remain valid as historical data.
- The new `audit.CacheStatusHitLive` and `CacheStatusMissFollower`
  constants are introduced. Analytics dashboards can roll these up
  with `HIT` and `MISS` for backward-compatible aggregates if
  desired.

## 10. Test matrix

### 10.1 Unit tests

- `cache.BuildKey` v2 formula: alias normalisation, cross-ingress
  equivalence, JSON-key ordering invariance, stream/non-stream
  separation, provider/model collision safety.
- `Adapter.PrepareBody` for each spec: round-trip determinism,
  passthrough vs cross-format, OpenAI reasoning-model param
  stripping. (Mostly already tested today; refactor preserves it.)
- `cache.StreamEntry` / `ResponseEntry` JSON round-trip: schema
  field discriminator, chunk-record `omitempty` correctness.
- `streamcache.Broker`: subscribe/unsubscribe, ringbuffer ordering,
  late-joiner replay, ref-count → cancel, upstream error
  broadcast, upstream normal terminate → cache.Store, race between
  broker close and new subscriber.
- `cache.ReplaySubscription`: chunk-by-chunk emission, EOF on
  exhaustion, Close idempotency.

### 10.2 Integration tests

- End-to-end stream HIT: warm cache via one request, second request
  arrives with same key, response is byte-equal at the canonical
  layer to the first.
- End-to-end stream broker fan-out: two concurrent requests, one
  upstream call observed (via httptest stub), both clients receive
  the full stream.
- Cross-ingress sharing: OpenAI ingress + Anthropic provider
  prompt P → cache. Anthropic ingress + Anthropic provider same
  prompt P → HIT, response transcoded back to Anthropic shape.
- Hook always runs: hook stub counts invocations; HIT request
  produces +1 invocation, HIT_LIVE same.
- Failure isolation: subscriber A's hook rejects; subscriber B
  on the same broker receives the full stream.
- Upstream error mid-stream: all subscribers receive an
  ingress-specific error frame; cache is not written; subsequent
  request misses.
- Non-stream broker: two concurrent identical non-stream requests;
  one upstream call; both receive the same JSON.

### 10.3 Parity / regression tests

- E28-S6 round-trip golden tests stay green (stream HIT path goes
  through transcoder same as live).
- Forward-header allowlist test: HIT paths do not introduce any
  new outbound header forwarding.

## 11. Metrics & observability

Prometheus counters (`nexus_aigw_cache_*`):

- `cache_lookups_total{result="hit"|"hit_live"|"miss"|"skip_no_cache"|"disabled"}` (one label per `audit.CacheStatus` value, lowercased)
- `cache_writes_total{kind="stream"|"response"}`
- `cache_broker_subscribers` (gauge, current ref-count sum)
- `cache_broker_active` (gauge, number of in-flight brokers)
- `cache_replay_chunks_total` (counter, chunks replayed on HIT)
- `cache_entry_bytes_bucket` (histogram, persisted entry size)

Structured logs at INFO for broker creation/close, at DEBUG for
subscriber join/leave.

## 12. Engineering estimate

≈ 1.5–2 engineer-weeks, broken down:

- `Adapter.PrepareBody` interface promotion + spec_adapter
  refactor: 0.5 day
- `cache` package upgrades (v2 key, two entry types, JSON
  schema discriminator): 1 day
- `streamcache` new package (Broker, Registry, Subscription,
  RingBuffer, replay): 4 days
- Proxy handler integration (Phase 5.5 rewrite, HIT/MISS
  branching, non-stream broker integration): 2 days
- Audit / metrics / `CacheStatus` enum updates: 1 day
- Test matrix (§10): 3 days
- Doc + spec-vs-code review: 0.5 day

## 13. Risks & mitigations

| Risk | Likelihood | Severity | Mitigation |
|---|---|---|---|
| Broker introduces a leak (subscribers accumulate, ref-count miscounted) | M | H | Strict `defer Close()` in handler; Registry exposes a metric for `cache_broker_subscribers`; integration test for ref-count under cancel storms. |
| Stream entries grow large (long Claude streams with many chunks) | M | M | Cap `StreamEntry.Chunks` length at e.g. 2,000 or `MaxBodyBytes`-equivalent total bytes; refuse to cache larger streams (treat as MISS-write-skip). |
| `PrepareBody` is not idempotent for some adapter | L | H | Unit-test idempotency: `PrepareBody(req) == PrepareBody(req)` byte-for-byte across all `spec_*` packages, plus a bench gate (must be < 1 ms per call). |
| Hook fingerprint absence allows stale-rule cache | (intentional) | (acceptable) | Hooks always run; mitigated by D2. |
| Failover skews cache attribution | L | L | Documented in §7.1; audit columns track actual target separately from cache key target. |
| Cross-tenant prompt collision shares a cache entry | L | L | Same as today's non-stream cache; documented in §8.2. |

## 14. Out of scope (deferred)

- Bedrock streaming cache (T-BEDROCK-STREAM, E28-S6).
- Embeddings cache for streaming (no streaming wire; non-streaming
  embeddings cache already covered by today's response cache and
  inherits the v2 key formula via this spec).
- Per-tenant `Cache-Control: max-age` overrides via
  `x-nexus-aigw-max-age` header (potential follow-up).
- Cache analytics dashboard (cost-saved attribution per VK / per
  org).
- Forward-header allowlist runtime-configurability — separate
  document `2026-05-06-forward-header-allowlist-context.md`.

## 15. Code anchors (cheat-sheet for the writing-plans phase)

- `packages/ai-gateway/internal/cache/cache.go` — extend with
  `StreamEntry`, `ResponseEntry`, v2 `BuildKey`, replay constructor.
- `packages/ai-gateway/internal/cache/stream/` (new) — Broker,
  Registry, Subscription, RingBuffer.
- `packages/ai-gateway/internal/providers/adapter.go` — promote
  `PrepareBody` to interface.
- `packages/ai-gateway/internal/providers/spec_adapter.go` — split
  `Execute` into `PrepareBody` (pure function, exposed on the
  interface) plus `ExecuteWithBody(ctx, req, body, rewrites)` (the
  network-side remainder, called both by `Execute` and by the proxy
  handler when a cache MISS already has the prepared body in hand).
- `packages/ai-gateway/internal/handler/proxy.go` Phase 5.5 — call
  `PrepareBody`, build v2 key, branch HIT(stream)/HIT(non-stream)/
  MISS+broker.
- `packages/ai-gateway/internal/handler/proxy.go::handleStream` —
  replace `chunkSSEReader` source from `result.Stream` to a
  `ChunkSubscription`.
- `packages/ai-gateway/internal/observability/audit/audit.go` — extend
  `CacheStatus` enum.
- `tests/parity/` — extend with cross-format streaming HIT vs MISS
  byte-equivalence (canonical layer).

End of spec.
