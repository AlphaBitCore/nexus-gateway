# E18 — Story 9: Agent Request-Side Detector Wiring

## Context

The Agent already buffers the first HTTP request after MITM and runs the hook pipeline against it (`packages/agent/core/network/intercept/handler.go:ProcessRequest`). It does not yet populate LLM signal fields in its audit event. This story adds request-side detection. Response-side inspection is a separate story (s10) because it requires broader changes to the relay loop.

Depends on s1, s2, s3.

## User Story

**As a** security engineer,
**I want** Agent rows in `traffic_event` to record provider, model, API-key class, and API-key fingerprint for every intercepted AI request,
**so that** I can see which employees are hitting which providers with which keys, regardless of gateway routing.

## Tasks

### 9.1 Adapter lookup in agent

File: `packages/agent/core/network/intercept/handler.go`

The existing `ProcessRequest` method already identifies the matching adapter via `DomainSnapshot`. After `inst.Adapter.ExtractRequest` succeeds:

```go
reqMeta := inst.Adapter.DetectRequestMeta(req, body)
hookInput.DetectedProvider = reqMeta.Provider
hookInput.DetectedModel = reqMeta.Model
hookInput.ApiKeyClass = reqMeta.ApiKeyClass
hookInput.ApiKeyFingerprint = reqMeta.ApiKeyFingerprint
```

### 9.2 Audit event extension

File: `packages/agent/core/observability/audit/event.go`

Add the three new LLM signal fields plus the existing provider/model/tokens that the Agent has not been writing:

```go
type Event struct {
    // ... existing fields ...

    ProviderName          string
    ModelName             string
    ApiKeyClass           string
    ApiKeyFingerprint     string
    // PromptTokens / CompletionTokens / UsageExtractionStatus are populated in s10
}
```

### 9.3 Upload path

The Agent uploads audit events to Hub via `POST /api/internal/things/agent-audit` (per `architecture.md` — agent has no direct MQ access). The upload handler on the Hub side republishes to `nexus.event.agent` (or inserts equivalent messages into the MQ for the DB writer).

- Update the JSON payload schema of `POST /api/internal/things/agent-audit` to include the new fields.
- Update the Hub endpoint to translate received agent events into `TrafficEventMessage` with the new fields.

### 9.4 Handler at Hub

File: `packages/nexus-hub/internal/api/agent_audit.go` (or equivalent path — locate at implementation time).

Map received Agent `Event` JSON to `TrafficEventMessage` and publish to `nexus.event.agent`. Preserve `api_key_class` / `api_key_fingerprint` / `provider_name` / `model_name` in the translation.

### 9.5 Tests

- Unit test: Agent intercepts a request with `Authorization: Bearer sk-ant-test` → audit event has `api_key_class = "sk-ant-"` and `api_key_fingerprint = SHA256("sk-ant-test")[:8]`.
- Integration test: Agent request-side intercept produces an upload payload with populated LLM signal fields; Hub translates and publishes to MQ; DB writer inserts row with fields present.

## Acceptance Criteria

- `go test -race -count=1 ./packages/agent/...` passes.
- Agent audit events carry `provider_name`, `model_name`, `api_key_class`, `api_key_fingerprint` for every request that matches a known provider adapter.
- Non-AI traffic (non-matching adapter → generic) results in empty strings for these fields (not null from DB perspective — the `omitempty` JSON tag + `nullIfEmpty` in s6 handles it).
- Hub upload handler rejects malformed payloads but accepts the new fields transparently.

## Non-Goals

- Response body inspection (s10).
- SSE handling (s10).
- Tokenizer estimation (s10 via s4).
- Changing the upload transport (still HTTP, not direct MQ).
