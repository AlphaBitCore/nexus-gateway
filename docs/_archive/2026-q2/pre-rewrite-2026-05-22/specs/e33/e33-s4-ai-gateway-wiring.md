# E33 S4 — AI Gateway: Per-Provider Streaming Policy + Dual Audit Pipeline

## Story

As an AI gateway operator I need `ai-gateway` to honor a per-provider streaming compliance policy (mode + capture flags + spill enablement), run dual-pipeline hooks, and capture extracted bodies through the same `Body` container the other two data planes use.

## Scope

- `tools/db-migrate/schema.prisma` — `Provider` adopts the same Policy override columns: `streamingMode`, `streamingChunkBytes`, `streamingHookTimeoutMs`, `streamingFailBehavior`, `captureRequestBody`, `captureResponseBody`, `rawBodySpillEnabled` (all nullable). Migration generated.
- `packages/shared/schemas/configtypes/provider.go` — re-codegen.
- `packages/ai-gateway/internal/cache/layer/policy.go` (new) — `ResolvePolicyByProviderID(id string) streaming.Policy`. Reads from the existing `SnapshotCache[Provider]` plus the same global default as compliance-proxy.
- `packages/ai-gateway/internal/streaming/live.go` — replaced by the new shared `chunked_async` (S2). Wire `ContentAccumulator` driven by the per-provider extractor selected via `Provider.adapter_type` ⇒ `streaming/extract.Registry`.
- `packages/ai-gateway/internal/streaming/buffer.go` — replaced by the new shared `buffer_full_block`. Hook runs at stream end on assembled extracted content.
- `packages/ai-gateway/internal/handler/proxy.go:1401-1423` (and surrounding) — wire dual-pipeline emit. Today the response audit emit only carries `respResult`; replace with `(reqResult, respResult)`. Drop the single legacy fields on `AuditEvent`.
- `packages/ai-gateway/internal/observability/audit/...` — apply `audit.Body` for the two body fields; switch to `RequestHookDecision`, `ResponseHookDecision`, `RequestHooksPipeline`, `ResponseHooksPipeline` columns.
- `packages/ai-gateway/internal/handler/proxy.go` (request side) — capture the request `*hooks.CompliancePipelineResult` and thread it into the audit context the same way compliance-proxy now does, so SSE and non-SSE paths both produce dual-pipeline rows.
- `packages/ai-gateway/cmd/ai-gateway/main.go` — initialize `streaming.PolicyResolver` from `cachelayer`; pass to handler. Initialize `spillstore` from `system_metadata['spill_store.config']` (S1).
- Admin API surfaces: existing `/api/admin/providers/*` BFF gains write paths for the new columns. Implementation lives in `packages/control-plane/...` already, see S6 SDD.
- Tests: `streaming_test.go` covers all 3 modes for `chat_completions` SSE; `proxy_test.go` covers dual-pipeline emit on a non-streaming path; `policy_test.go` covers per-provider override.

## Tasks

1. Add Provider columns + migration; re-codegen.
2. Build `cachelayer/policy` resolver + invalidation hook off the existing `SnapshotCache[Provider]` reload path.
3. Replace `internal/streaming/live.go` and `buffer.go` with shims over `shared/streaming/{chunked_async,buffer_full_block}` driven by `policy.Mode`.
4. Thread the request-side pipeline result into the audit context; emit dual pipeline.
5. Convert ai-gateway's `audit.AuditEvent` to the new schema (Body container + dual pipeline).
6. Wire `spillstore.NewFromConfig` at startup; pass into the audit emitter for body persistence.
7. Run smoke against `/v1/chat/completions` SSE with the seeded VK (`nvk_2c9f98da65507c61d7f1ee6ccf8fd26e85d4460c4304540de381edd4966f9385`).

## Acceptance criteria

- `/v1/chat/completions` SSE call produces a `traffic_event` row where `request_hooks_pipeline` and `response_hooks_pipeline` are both populated, both `inline_*` body fields are non-empty (assuming default capture toggle is ON for the test provider), and `usage_extraction_status` reflects streaming token accounting.
- Provider override: setting `streamingMode='buffer_full_block'` on the test provider row + reloading via Hub shadow flips behavior on the next request without a service restart.
- Provider override: setting `captureRequestBody=false` produces a row with `kind:"absent"` on `request_body` while everything else continues to work.
- Smoke runs through the existing `e22-s1` payload-capture E2E test path with no regression.
