# E37-S3 — Stream / Non-Stream × Request / Response Coverage

Status: draft
Epic: E37 — Payload Capture Unification
Story: S3 — Close the four-quadrant coverage matrix uniformly across
       AI-Gateway, Compliance-Proxy, and Agent

## 1. Problem

After E22 the capture matrix has gaps:

| Service | Req non-stream | Req stream | Resp non-stream | Resp stream (SSE) |
|---|---|---|---|---|
| AI-Gateway | ✅ proxy.go:267 | ✅ same buffer | ✅ proxy.go:1569 | ⚠️ only when cache leader (proxy_cache.go:536); non-cache stream branch silently uncaptured |
| Compliance-Proxy | ⚠️ no clip; no spill awareness | ⚠️ same | ⚠️ same | ⚠️ same |
| Agent | ✅ MITM relay buffer | ✅ same | ✅ MITM relay buffer | ❌ excluded (`SSE excluded` comment in `intercept/payload_capture.go`) |

Combined with S1 (unified threshold) and S2 (agent pre-signed
spill), this story fills every cell.

## 2. Goal

After this story lands, every cell in the matrix above is ✅. A
single capture wrapper drives streaming on AI-Gateway across both
cache-on and cache-off paths. Compliance-Proxy applies
`MaxInlineBodyBytes` like the other services. Agent SSE responses
flow into the audit envelope.

## 3. Non-goals

- Incremental disk-streaming spill (capturing as bytes flow in
  rather than buffering then deciding). Out of scope; in-memory
  bounded by `perObjectCap` is sufficient for current SSE traffic
  per F3.4.
- Capturing on the client wire (post-tee). Capture is the captured
  copy only; the wire copy is unchanged.

## 4. Design

### 4.1 Unified streaming tee

`packages/ai-gateway/internal/handler/proxy.go::cappedTeeWriter`
is renamed to `streamCaptureTee` and moved to a small helper file
`stream_capture.go` so both cache-on and cache-off paths import it
from the same site. Semantics:

```go
// streamCaptureTee mirrors bytes written through an http.ResponseWriter
// into an in-memory buffer for end-of-stream audit. The buffer is
// bounded by `hardCap` (defaulted to spill.perObjectCap, default
// 256 MiB); bytes past hardCap continue to flow on the wire but the
// buffer no longer grows and Truncated flips true.
//
// The tee never inspects MaxInlineBodyBytes itself — that decision
// lives in EmitBody at stream end. hardCap is purely an OOM guard.
type streamCaptureTee struct {
    http.ResponseWriter
    hardCap   int64
    written   int64
    buf       []byte
    truncated bool
}

func newStreamCaptureTee(w http.ResponseWriter, hardCap int64) *streamCaptureTee
func (t *streamCaptureTee) captured() []byte
func (t *streamCaptureTee) truncatedBeyondCap() bool
```

Where `hardCap` comes from `cfg.Spill.PerObjectCap` (existing YAML
field). The proxy handler holds a `payloadcapture.Store` and the
spill `FactoryConfig` already; both are available at construction.

### 4.2 AI-Gateway: cache-off stream path

Today `handleStream` (or whatever the non-cache stream entry is)
calls `lp.Process(ctx, sseReader, w, hookCtx)` directly. Wrap the
writer:

```go
captureCfg := h.payloadCaptureConfig()
var tee *streamCaptureTee
if captureCfg.StoreResponseBody {
    tee = newStreamCaptureTee(w, h.spill.PerObjectCap())
    w = tee
}
lp.Process(r.Context(), sseReader, w, hookCtx)
if tee != nil {
    rec.ResponseBody = tee.captured()
    rec.ResponseTruncated = tee.truncatedBeyondCap()
}
```

The cache-on path in `proxy_cache.go:536` migrates to the same
helper; the inline `newCappedTeeWriter` definition is deleted.

### 4.3 AI-Gateway: `EmitBody` wiring (cache + non-cache)

The audit writer's `recordToMessage` call at `audit.go:577-583`
already calls `EmitBody`. With S1's `MaxInlineBodyBytes`, the
threshold is the runtime config, not the YAML constant. No code
move needed beyond the rename.

### 4.4 Compliance-Proxy: `EmitBody` with `MaxInlineBodyBytes`

`packages/compliance-proxy/internal/compliance/emitter.go`:

```go
type AuditEmitter struct {
    // …
    spill          spillstore.SpillStore
    payloadStore   *payloadcapture.Store
    // (delete spillThreshold; comes from payloadStore now)
}

func (e *AuditEmitter) buildEvent(...) audit.AuditEvent {
    // …
    pcCfg := e.payloadStore.Get()
    threshold := pcCfg.MaxInlineBodyBytes
    ctx, cancel := context.WithTimeout(context.Background(), spillEmitTimeout)
    defer cancel()
    requestBodyContainer := spillstore.EmitBody(
        ctx, e.spill, threshold,
        requestBody, requestContentType,
        eventID, "request", false, e.logger)
    responseBodyContainer := spillstore.EmitBody(
        ctx, e.spill, threshold,
        responseBody, responseContentType,
        eventID, "response", false, e.logger)
    // …
}
```

`packages/compliance-proxy/cmd/compliance-proxy/init.go` injects
the existing `payloadcapture.Store` (already wired for the
`StoreRequestBody` / `StoreResponseBody` flags) into the emitter.

The `emitter.WithSpillStore(store, threshold)` setter is
deleted; replace with `emitter.WithSpillStore(store *spillstore.SpillStore)`.
The threshold comes from the runtime config now.

### 4.5 Agent: SSE response capture

Today `agent/internal/proxy/proxy.go::inspectResponse` already
buffers SSE bytes via `cappedBuffer` for tier-2 usage extraction
(`bodyBuf := &cappedBuffer{cap: maxBodyBytes}` at line ~683).
Today the audit pipeline ignores those bytes for SSE.

The change:

1. `inspectResponse` returns the captured bytes regardless of
   stream / non-stream (it already does for non-stream; extend
   for stream).
2. `intercept/payload_capture.go::CaptureResponseBody` accepts
   bytes for both stream and non-stream. The "SSE excluded"
   comment is removed.
3. The agent's audit event stamps `ResponseBody` (or
   `ResponseSpillRef` per S2) just as for non-stream.

The platform layer's `cappedBuffer` is replaced by a thin wrapper
that respects the same `perObjectCap` semantic the gateway uses;
mid-stream truncation flips a flag the agent reads at completion.

### 4.6 Agent: `maxBodyBytes` parameter rename

`agent/internal/proxy/proxy.go::MITMRelay` and its callees take a
`maxBodyBytes int64` parameter today. Its actual purpose is the
inspection / capture buffer cap. With S1's renames the local
variable becomes `inspectBodyCap`; the source value comes from
`cfg.Spill.PerObjectCap` (via the `BodyReadCapper` interface in
`agent/cmd/agent/main.go`).

This decouples the agent's per-flow buffer ceiling from the
admin's `MaxInlineBodyBytes`. The previous coupling was
accidental — the admin's truncation cap was being used as the
per-flow buffer cap, which meant a 64 KiB admin setting silently
clamped streaming captures. After this story, the per-flow cap
is `perObjectCap` (256 MiB) and `MaxInlineBodyBytes` only
governs inline-vs-spill.

### 4.7 Reader path (consumer)

No change beyond what S2 covers. `decodeBodyEnvelope` in
`admin_traffic.go` already round-trips raw and base64 inline
encodings; SSE flows through as base64-inline (no spill needed if
small) or as a spill ref (large), and the resolver from S2 returns
both as JSON-string for the UI.

## 5. Tasks

- T1 — Move `cappedTeeWriter` → `streamCaptureTee` into
  `ai-gateway/internal/handler/stream_capture.go`. Extend to take
  `hardCap` from `cfg.Spill.PerObjectCap()`.
- T2 — Wire the tee on the cache-off stream branch (`proxy.go`
  streaming entry). Confirm `Flush()` / `Unwrap()` are forwarded
  so SSE chunked encoding still works.
- T3 — Compliance-Proxy: inject `payloadcapture.Store` into the
  emitter; pass `pcCfg.MaxInlineBodyBytes` into `EmitBody`. Delete
  `emitter.spillThreshold`.
- T4 — Agent: drop `SSE excluded` exception; make
  `CaptureResponseBody` symmetric across stream / non-stream.
  Plumb captured bytes from `inspectResponse` into the audit
  event regardless of `Stream` flag.
- T5 — Agent: rename `maxBodyBytes` parameter on `MITMRelay` /
  `inspectRequest` / `inspectResponse` to `inspectBodyCap`. Source
  it from `cfg.Spill.PerObjectCap()` instead of the admin
  `MaxInlineBodyBytes`. Update tests that exercise this parameter.
- T6 — Add four integration tests, one per quadrant, that exercise
  AI-Gateway with capture-on:
  - Non-stream JSON request → inline body row.
  - Stream JSON-Lines request → inline body row (small).
  - Non-stream JSON response → inline body row.
  - SSE response → inline body row (≤ 256 KiB) or spill row (>).
- T7 — Add the same four tests for Compliance-Proxy (with TLS-bumped
  traffic to a fake upstream) and for Agent (with a synthetic
  `MITMRelay` flow).

## 6. Acceptance criteria

- AC1 — A 5 KiB SSE response through AI-Gateway with cache off
  produces a `traffic_event_payload` row with non-NULL
  `inline_response_body`.
- AC2 — A 1 MiB SSE response through AI-Gateway with cache off
  produces a row with non-NULL `response_spill_ref` and NULL
  `inline_response_body`.
- AC3 — A 5 KiB SSE response through Agent's MITM relay produces
  the same row shape as AC1.
- AC4 — Compliance-Proxy with capture on emits a row whose
  `inline_request_body` matches the captured request bytes
  (within `MaxInlineBodyBytes`) and whose `request_truncated` is
  false when size ≤ threshold.
- AC5 — `git grep "SSE excluded"` returns zero hits.
- AC6 — `git grep cappedTeeWriter` returns zero hits.
- AC7 — All four data-plane test suites pass under `go test
  -race -count=1`.

## 7. Risks

- **R1 — Memory pressure on large SSE streams**: 256 MiB per
  in-flight stream worst case. Mitigated by `spill.perObjectCap`
  YAML, by F3.4's truncation flag, and by typical real-world SSE
  (5–500 KiB).
- **R2 — Test flake on streaming**: SSE timing sensitivity. The
  integration tests use synthetic upstreams with deterministic
  flush points, not real provider traffic.
- **R3 — Compliance-Proxy rebinding**: injecting a new dependency
  (`payloadcapture.Store`) changes the construction order in
  `init.go`. Mitigated by following the existing pattern (the
  store is already constructed for the `StoreRequest/Response`
  flags) and by integration tests that exercise the full path.
