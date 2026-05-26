# E18 — Story 7: AI Gateway Detector Wiring + Virtual Key Fingerprint

## Context

AI Gateway already populates `provider_id`, `provider_name`, `model_id`, `model_name`, `prompt_tokens`, `completion_tokens` via its internal `providers/` adapter and `audit/audit.go`. It does **not** yet populate `api_key_class`, `api_key_fingerprint`, or `usage_extraction_status`. This story wires those in by calling the shared `traffic.Adapter` detector alongside the existing provider adapter, and computing a fingerprint of the caller's virtual key.

Depends on s1, s2.

## User Story

**As a** cost analyst,
**I want** AI Gateway rows to carry a stable fingerprint of the virtual key that made the call,
**so that** I can aggregate spend per internal caller without storing the VK itself in analytics tables.

## Tasks

### 7.1 VK fingerprint at auth

File: `packages/ai-gateway/internal/handler/proxy.go`

Right after virtual-key authentication resolves the VK (the existing auth middleware or handler), compute:

```go
vkFingerprint := traffic.ApiKeyFingerprint(presentedVK)
vkClass := "nvk_"  // all Gateway-issued VKs use this prefix class
```

Attach both to the request-scoped audit context (the `audit.Record` under construction).

### 7.2 Detector call

In the request handling path, after the request body is read (existing `readBody` at `handler/proxy.go:112`), additionally invoke the shared traffic adapter matching the upstream provider:

```go
detectAdapter := traffic.Adapters.ForProvider(route.ProviderType)  // new registry lookup by provider
reqMeta := detectAdapter.DetectRequestMeta(r, bodyBytes)
```

Write `reqMeta.Provider` / `reqMeta.Model` into the audit record where the existing provider/model fields flow. The `ApiKeyClass` / `ApiKeyFingerprint` from `DetectRequestMeta` are **ignored** for AI Gateway rows — the VK fingerprint computed in 7.1 wins (AI Gateway semantics).

### 7.3 Response usage path

The existing provider adapter (`packages/ai-gateway/internal/providers/`) produces a `Metadata` struct with token counts. Translate it into a `traffic.UsageMeta` and record `UsageExtractionStatus`:

- Non-streaming with complete `Metadata`: `ok`
- Streaming with base stream session completing with usage: `streaming_reported`
- Streaming completing without usage: `streaming_unavailable` (Tier-2 estimation is not enabled for AI Gateway in this epic — AI Gateway's traffic already has VK attribution and provider-reported usage is near-100%; Tier-2 is a proxy/agent concern)
- Parse failures or non-LLM traffic reaching the gateway: `parse_failed` or `non_llm`

### 7.4 Audit writer — `packages/ai-gateway/internal/observability/audit/audit.go`

Add fields to `audit.Record`:

```go
ApiKeyClass           string
ApiKeyFingerprint     string
UsageExtractionStatus string
```

Propagate into the outgoing `TrafficEventMessage` in the MQ publish path.

### 7.5 Tests

- Unit test: request with VK `nvk_test_abc123` produces record with `api_key_class = "nvk_"` and `api_key_fingerprint = SHA256("nvk_test_abc123")[:8]`.
- Unit test: streaming chat response without usage frame → `usage_extraction_status = "streaming_unavailable"`.
- Integration test: end-to-end request through the proxy handler produces a `TrafficEventMessage` with all three new fields populated.

## Acceptance Criteria

- `go test -race -count=1 ./packages/ai-gateway/...` passes.
- Traffic events written by AI Gateway carry all three new fields for every request where a VK is presented.
- `api_key_fingerprint` is stable across multiple requests with the same VK (deterministic hash verification).
- A request rejected at VK auth (no valid VK) produces either no traffic event (preferred — auth failures not in scope for cost analytics) or one with `api_key_class = ""` and `api_key_fingerprint = ""` (acceptable if current code path emits an event for rejected requests).

## Non-Goals

- Backfilling past rows with fingerprints (s5 non-goal).
- Tier-2 tokenizer estimation in AI Gateway (the upstream provider always emits usage for the providers we support, barring provider outages).
- Changing the existing `providers/` adapter interface — it remains the routing/translation layer.
