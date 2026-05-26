# E50 — Story 2: AI Gateway Phase Instrumentation

## Context

Instruments the ai-gateway request lifecycle to populate the 5 new
`traffic_event` columns plus the `ai-gateway` slice of `latency_breakdown`
JSONB. Reuses the shared `PhaseTimer` from S1 and wraps the outbound HTTP
transport with `httptrace.ClientTrace` for accurate TTFB.

## User Story

**As a** platform operator looking at an ai-gateway traffic event,
**I want** to see exactly how much of the request time was spent in our
hooks/routing/adapters versus the upstream provider,
**so that** when a customer complains about latency I can pinpoint the
contributor in seconds instead of grepping Prometheus.

## Tasks

### 2.1 Wire `PhaseTimer` through `proxy.go` request handler

`packages/ai-gateway/internal/proxy/proxy.go` — at handler entry (existing
`start := time.Now().UTC()` near line 238), additionally construct
`timer := traffic.NewPhaseTimer()` and thread it through the request context
or `Record` struct so every phase boundary can mark it.

Phase boundaries (one `timer.Mark(...)` call each):

| Checkpoint location | Phase recorded |
|---|---|
| After auth/VK validation (`vkauth.Validate` returns) | `PhaseAuth` |
| After quota / rate-limit check | `PhaseQuota` |
| After routing decision returns (`router.Resolve` returns) | `PhaseRouting` |
| After request hooks pipeline returns | (aggregated to `request_hooks_ms` column — not JSONB) |
| After cache lookup returns | `PhaseCacheLookup` |
| After request format adapter call | `PhaseReqAdapter` |
| After response format adapter call | `PhaseRespAdapter` |
| After response hooks pipeline returns | (aggregated to `response_hooks_ms` column — not JSONB) |

`PhaseRouting` is *additionally* sourced from the existing `routing_trace`
stage durations for consistency — if the new in-line measurement diverges from
the routing trace's `SUM(stages[*].DurationMs)` by more than 1ms in tests, fail
the test (the routing trace is the canonical record for the stage view; the
column is a denormalized aggregate).

### 2.2 Upstream TTFB + total via `httptrace.ClientTrace`

`packages/ai-gateway/internal/execution/executor/executor.go` — wrap the outbound
`http.Request` with `httptrace.WithClientTrace`:

```go
trace := &httptrace.ClientTrace{
    GotFirstResponseByte: func() {
        rec.UpstreamTtfbMs = ptrInt(int(time.Since(upstreamSendStart).Milliseconds()))
    },
}
req = req.WithContext(httptrace.WithClientTrace(req.Context(), trace))
upstreamSendStart := time.Now()
resp, err := transport.RoundTrip(req)
// ... after body read completes (resp.Body close in defer):
rec.UpstreamTotalMs = ptrInt(int(time.Since(upstreamSendStart).Milliseconds()))
```

For streaming (`LivePipeline`, `chunkSSEReader`): record `UpstreamTtfbMs` on the
**first non-zero bytes** observed in `chunkSSEReader.Read()` rather than
`GotFirstResponseByte` — `GotFirstResponseByte` fires when headers arrive, which
for SSE precedes the first event chunk by a non-trivial amount in some providers.
Mark this difference in the test plan.

For streaming `UpstreamTotalMs`: record at upstream `resp.Body` close, which
for SSE is end-of-stream OR client abort. On abort, stamp
`LatencyBreakdown["stream_aborted"] = 1` (using `1` as a boolean-ish marker
to keep the JSONB schema int-valued).

### 2.3 Hooks aggregate columns

Both `request_hooks_pipeline` and `response_hooks_pipeline` JSONB structures
already carry per-hook `latencyMs`. In the `recordToMessage` helper
(`packages/ai-gateway/internal/observability/audit/audit.go`), compute:

```go
rec.RequestHooksMs  = ptrInt(sumHookLatencies(rec.HooksPipeline, "request"))
rec.ResponseHooksMs = ptrInt(sumHookLatencies(rec.HooksPipeline, "response"))
```

`sumHookLatencies` returns 0 if the slice is empty (which becomes a NULL column
on emit — bypass rows should have NULL, not 0, to avoid false positives in P95
queries).

### 2.4 Audit Record extension

`packages/ai-gateway/internal/observability/audit/audit.go` `Record` struct adds:

```go
UpstreamTtfbMs    *int
UpstreamTotalMs   *int
RequestHooksMs    *int
ResponseHooksMs   *int
LatencyBreakdown  traffic.LatencyBreakdown
```

`recordToMessage(rec *Record) *mq.TrafficEventMessage` copies these onto the
outbound `TrafficEventMessage`. The `PhaseTimer.Snapshot()` populates
`LatencyBreakdown` at the end of the handler before message build.

### 2.5 Response header for ops debugging

The existing `x-nexus-aigw-latency-ms` header (set on non-streaming responses)
remains unchanged. Add three sibling headers for ops:

- `x-nexus-aigw-upstream-ttfb-ms`
- `x-nexus-aigw-upstream-total-ms`
- `x-nexus-aigw-overhead-ms` (`= total - upstream_total`, computed on the fly)

Per existing spec, streaming responses omit `x-nexus-aigw-latency-ms` because
client-side wall-clock makes the value meaningless mid-stream. The three new
headers are likewise **omitted for streaming** (the value can be reconstructed
from the `traffic_event` row).

### 2.6 Unit tests

- `proxy_test.go`: a fake handler exercises a typical lifecycle and asserts
  all 5 columns are populated, that `request_hooks_ms` matches the JSONB sum,
  and that `LatencyBreakdown` contains exactly the 6 ai-gateway keys.
- `executor_test.go`: a fake `RoundTripper` that delays headers vs first byte
  vs body close — assert TTFB and total are recorded correctly.
- Streaming-path test (`livepipeline_test.go`): SSE reader returns chunks with
  artificial delays — assert `upstream_ttfb_ms` matches first chunk arrival,
  not header arrival.

## Acceptance Criteria

- A live request through the dev gateway produces a `traffic_event` row with
  all 5 new columns non-NULL and `latency_breakdown` containing keys from the
  ai-gateway set.
- For a request that hits the prompt cache, `latency_breakdown.cache_lookup_ms`
  is non-zero (e.g. 1-5ms).
- `our_overhead_ms` computed at read time as `latency_ms - upstream_total_ms`
  is consistently ≥0 across 100 sample requests.
- `go test -race -count=1 ./packages/ai-gateway/...` passes.
- `/smoke-gateway` covers a non-stream + SSE + 2-turn-cache run; the report
  asserts the new columns are populated on every model.

## Non-Goals

- compliance-proxy / agent instrumentation (S3, S4).
- Admin API or UI rendering of the new fields (S6).
- Backfill of pre-E50 rows (S5).
