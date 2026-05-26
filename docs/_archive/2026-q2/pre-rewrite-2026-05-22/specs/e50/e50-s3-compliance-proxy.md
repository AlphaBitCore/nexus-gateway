# E50 — Story 3: Compliance Proxy Phase Instrumentation

## Context

Brings compliance-proxy's `traffic_event` rows to parity with ai-gateway's:
populates the 5 phase columns plus the `compliance-proxy` slice of
`latency_breakdown` (`conn_setup_ms`, `tls_handshake_ms`). Reuses S1's shared
`PhaseTimer` and the same `httptrace.ClientTrace` pattern as ai-gateway.

## User Story

**As a** platform operator looking at a compliance-proxy traffic event,
**I want** to see TLS-bump time, hook pipeline time, and provider time as
distinct phases on the row,
**so that** a slow proxy-routed request can be debugged without sifting through
TLS-handshake Prometheus histograms.

## Tasks

### 3.1 Wire `PhaseTimer` through `forward_handler.go`

`packages/compliance-proxy/internal/forward_handler.go` — at request entry
(existing `requestStart := time.Now()` near line 110), additionally construct
`timer := traffic.NewPhaseTimer()` and thread it through `AuditEvent` build.

Phase boundaries:

| Checkpoint | Phase |
|---|---|
| After CONNECT parse / inbound TLS bump prepared | `PhaseConnSetup` |
| TLS handshake completed (from `bump.go` — see 3.2) | `PhaseTlsHandshake` |
| After request hooks pipeline | (aggregated to `request_hooks_ms`) |
| After response hooks pipeline | (aggregated to `response_hooks_ms`) |

### 3.2 `bump.go` returns the TLS handshake duration

`packages/compliance-proxy/internal/bump.go` today records `metrics.TLSHandshakeMs`
in Prometheus only. Change the function signature to also return the elapsed
duration to the caller:

```go
// Bump performs the MITM TLS handshake against the incoming client; returns
// the negotiated *tls.Conn and the handshake duration (the latter so the
// caller can record it on the traffic_event row alongside the existing
// Prometheus histogram).
func Bump(...) (*tls.Conn, time.Duration, error)
```

Caller in `forward_handler.go` records via `timer.MarkBetween(PhaseTlsHandshake, d)`.
On connection reuse (no handshake), `d == 0` and the key is omitted by
`PhaseTimer.Snapshot()`.

### 3.3 Upstream TTFB + total via `httptrace.ClientTrace`

`packages/compliance-proxy/internal/upstream.go` — wrap the upstream request with
the same `httptrace.ClientTrace.GotFirstResponseByte` pattern as ai-gateway S2.2:

```go
trace := &httptrace.ClientTrace{
    GotFirstResponseByte: func() {
        ev.UpstreamTtfbMs = ptrInt(int(time.Since(sendStart).Milliseconds()))
    },
}
req = req.WithContext(httptrace.WithClientTrace(req.Context(), trace))
sendStart := time.Now()
resp, err := u.transport.RoundTrip(req)
// ... after resp.Body close:
ev.UpstreamTotalMs = ptrInt(int(time.Since(sendStart).Milliseconds()))
```

For SSE / streaming via `sse.go` (`streaming.Passthrough`, `LivePipeline`,
`BufferPipeline`): record TTFB on **first non-zero bytes** read from
`resp.Body`, same as ai-gateway S2.2. Record `UpstreamTotalMs` at stream close.
On client abort, stamp `LatencyBreakdown["stream_aborted"] = 1`.

### 3.4 Hooks aggregate columns

Both `request_hooks_pipeline` and `response_hooks_pipeline` JSONB already carry
per-hook `latencyMs`. Compute aggregates in `compliance-proxy/internal/audit/types.go`
emitter (where `RequestHooksPipeline` / `ResponseHooksPipeline` JSONB is
serialized today):

```go
ev.RequestHooksMs  = ptrInt(sumHookLatencies(reqPipeline.Results()))
ev.ResponseHooksMs = ptrInt(sumHookLatencies(respPipeline.Results()))
```

Bypass / SSE-only paths that don't run a pipeline write NULL (not 0).

### 3.5 AuditEvent + MQ wire

`packages/compliance-proxy/internal/audit/types.go` `AuditEvent` struct gains
the 5 fields (mirroring ai-gateway's `Record`). The MQ message produced by
`auditEmitter.Emit` / `EmitDual` carries them forward.

### 3.6 Unit tests

- `forward_handler_test.go`: fake handler walks a typical non-stream lifecycle
  and asserts all 5 columns plus the 2 JSONB keys are populated.
- `sse_test.go`: a fake provider response with controlled chunk timing —
  TTFB matches first chunk, not header.
- `bump_test.go`: handshake returns a non-zero duration on first MITM; 0 on
  reused connection.

## Acceptance Criteria

- A request that traverses the compliance-proxy in dev produces a
  `traffic_event` row with `source='compliance-proxy'`, all 5 columns
  non-NULL, and `latency_breakdown = {conn_setup_ms, tls_handshake_ms}`.
- A request through a reused TLS connection has `tls_handshake_ms` omitted
  from `latency_breakdown` (not stored as 0).
- `go test -race -count=1 ./packages/compliance-proxy/...` passes.
- `/test-compliance-proxy` smoke test report includes the new columns in its
  `traffic_event` cross-check.

## Non-Goals

- ai-gateway / agent (S2, S4).
- TLS handshake Prometheus histogram changes — that pipeline stays as today.
- Backfill (S5).
