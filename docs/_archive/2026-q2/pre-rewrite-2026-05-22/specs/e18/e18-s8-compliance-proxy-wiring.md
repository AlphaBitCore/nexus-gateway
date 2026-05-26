# E18 — Story 8: Compliance Proxy — MQ Unification + Detector Wiring

## Context

Compliance Proxy currently (a) does not extract LLM signals and (b) writes `traffic_event` rows via direct DB INSERT (`packages/compliance-proxy/internal/audit/sql.go:buildInsertSQL`), bypassing the Hub DB writer. Both must change.

- **Write path unification:** publish to MQ `nexus.event.compliance`; delete the direct-INSERT code. Per CLAUDE.md no-backcompat: no parallel legacy path.
- **Detector wiring:** call `traffic.Adapter.DetectRequestMeta` / `DetectResponseUsage` around the existing hook pipeline and populate the new LLM signal fields on outgoing messages.

Depends on s1, s2, s3, s4, s6.

## User Story

**As a** compliance officer,
**I want** rows written by the Compliance Proxy to carry provider, model, API-key fingerprint, and usage — identical columns to AI Gateway rows,
**so that** I can answer "which provider keys are flowing through our reverse proxy and who is using them" with one SQL query.

## Tasks

### 8.1 Delete direct DB path

- Delete file `packages/compliance-proxy/internal/audit/sql.go` entirely.
- Delete any call sites that invoke it; replace with MQ publish (8.2).
- Remove the pgx dependency from audit construction — the proxy still needs pgx for `ConfigStore` hook-config reads, but no direct `traffic_event` writes.

### 8.2 Add MQ publisher

New file or extension of existing `packages/compliance-proxy/internal/audit/writer.go`:

```go
type Writer struct {
    publisher mq.Publisher  // shared/mq
    fallback  NDJSONFallback
    logger    *slog.Logger
}

func (w *Writer) Publish(ctx context.Context, evt *mq.TrafficEventMessage) {
    if err := w.publisher.Publish(ctx, "nexus.event.compliance", evt); err != nil {
        w.fallback.Append(evt) // NDJSON local disk
        w.logger.Error("mq publish failed, fell back to NDJSON", "err", err)
    }
}
```

- NDJSON fallback writes to `$NEXUS_PROXY_NDJSON_DIR/traffic-events-YYYY-MM-DD.ndjson` with rotation. Replay tooling is out of scope for this epic (see Requirements §2 "Out of scope").

### 8.3 Detector wiring — request side

File: `packages/compliance-proxy/internal/proxy/forward_handler.go`

After the adapter's `ExtractRequest` succeeds (around existing line ~115 per investigation) and before the hook pipeline runs:

```go
reqMeta := inst.Adapter.DetectRequestMeta(r, bodyBytes)
auditCtx.reqMeta = reqMeta

// Feed into HookInput (s1 interface for HookInput extension, TBD — see 8.5)
hookInput.Model = reqMeta.Model
hookInput.DetectedProvider = reqMeta.Provider
```

### 8.4 Detector wiring — response side

After response body buffering (around line ~310 per investigation) and the streaming accumulator finalization (for SSE) :

- **Non-streaming:** call `inst.Adapter.DetectResponseUsage(resp, respBody)`.
- **Streaming live / buffer modes:** the pipeline now owns a `streaming.UsageAccumulator` (from s4) that finalizes at end of stream. Extract `UsageMeta` from the accumulator.

Populate `TrafficEventMessage` fields: `PromptTokens`, `CompletionTokens`, `UsageExtractionStatus`, `ApiKeyClass`, `ApiKeyFingerprint` (from `reqMeta` via 8.3), `ProviderName`, `ModelName` (from `reqMeta.Provider` / `reqMeta.Model`).

### 8.5 `HookInput` extension (if needed)

Per investigation, `HookInput` is effectively read-only during pipeline execution today. Detectors run **before** the hook pipeline, not as hooks themselves. So `HookInput` just needs new read-only fields: `DetectedProvider`, `DetectedModel`, `ApiKeyClass`, `ApiKeyFingerprint`. Extend `packages/shared/policy/hooks/types.go:HookInput` — additive only.

(If s7 already extended `HookInput` for AI Gateway, this task is a no-op.)

### 8.6 Config

- Remove any DB connection strings from `compliance-proxy`'s traffic-writer config; keep the config path used by `ConfigStore`.
- Add config for MQ publish target (subject, pool size, retry policy). Default `nexus.event.compliance`.

### 8.7 Tests

- End-to-end proxy test: issue a CONNECT → TLS bump → forward a mock OpenAI request → assert one `TrafficEventMessage` appears on the MQ test harness with all LLM signal fields populated.
- NDJSON fallback test: simulate MQ publish failure → assert the event appears in NDJSON file and no panic.
- Regression: ensure existing compliance hooks (keyword filter, PII detector) still execute on the same `HookInput` shape.

## Acceptance Criteria

- `rg 'buildInsertSQL' packages/compliance-proxy` returns zero matches.
- `rg 'INSERT INTO traffic_event' packages/compliance-proxy` returns zero matches.
- Compliance Proxy no longer opens a pgx connection for traffic event writes (config read connection only).
- Every proxy request through an identified AI provider produces a `TrafficEventMessage` on `nexus.event.compliance` with the nine LLM signal fields populated.
- Streaming chat responses produce `usage_extraction_status = streaming_reported` on golden providers (Anthropic, Gemini) and `streaming_estimated` on OpenAI without `include_usage` (after s4 tokenizer wiring).
- `go test -race -count=1 ./packages/compliance-proxy/...` passes.

## Non-Goals

- NDJSON-to-MQ replay tooling.
- Renaming the `nexus.event.compliance` subject.
- Changing the three SSE modes (passthrough / live / buffer).
