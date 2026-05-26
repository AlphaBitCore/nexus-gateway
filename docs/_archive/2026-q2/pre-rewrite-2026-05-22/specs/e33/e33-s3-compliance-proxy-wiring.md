# E33 S3 — Compliance Proxy: SSE Pipeline + Dual Audit + Per-Domain Policy

## Story

As a compliance officer I need `compliance-proxy` to capture both request and response bodies for every PROCESSed flow (SSE included), run both request- and response-stage hook pipelines, and persist both pipelines' executions to the audit row, with the streaming behavior chosen per intercepted domain.

## Scope

- `packages/compliance-proxy/internal/proxy/forward_handler.go:243-256, 374-501` — request pipeline keeps running as today, but its `*hooks.CompliancePipelineResult` is now propagated into `audCtx.requestPipelineResult` so the post-upstream emit can include it. Response-side pipeline keeps running but the audit emit gains `requestPipelineResult` parameter.
- `packages/compliance-proxy/internal/proxy/sse.go` — replace the existing `live`/`buffer`/`passthrough` switch with the new `Policy`-driven dispatch:
  - `passthrough` ⇒ `streaming.Passthrough` (today's behavior).
  - `buffer_full_block` ⇒ buffer entire response, run response pipeline, optionally HTTP-451 the client when `fail_close` + reject.
  - `chunked_async` ⇒ relay bytes in real time to client AND accumulate via `ContentAccumulator`; run response pipeline per chunk + once at stream end; the client's stream is never blocked.
  - In every branch, call `emitAudit` with the **`audCtx`** (carrying request body + request pipeline result) so the request side never disappears from the row.
- `packages/compliance-proxy/internal/proxy/sse.go:emitAudit` — signature changes to `(audCtx *requestAuditCtx, respInput *hooks.HookInput, respResult *hooks.CompliancePipelineResult, statusCode int, requestStart time.Time, usage traffic.UsageMeta, extractedReq audit.Body, extractedResp audit.Body)`. The body fields become full `audit.Body` discriminators.
- `packages/compliance-proxy/internal/compliance/emitter.go:Emit` — accept `requestPipelineResult` + `responsePipelineResult` (both nullable). Populate `RequestHookDecision`, `ResponseHookDecision`, `RequestHooksPipeline`, `ResponseHooksPipeline` on the `AuditEvent`. Drop the legacy `hookDecision` single-value path. The bodies are written through `audit.Body` (S1).
- `packages/compliance-proxy/internal/proxy/policy_resolver.go` (new) — `ResolvePolicyByHost(host string) streaming.Policy`. Reads `interception_domain` for the matched host (already in `domainpolicy.Engine`) plus the global default from `system_metadata['streaming_compliance.config']`; merges via `streaming/policy.Resolve`.
- `packages/compliance-proxy/internal/configloader/streaming_policy.go` (new) — loads global default + per-domain override columns from PG; refreshed on the existing thingclient shadow callback for `streaming_compliance` and `interception_domains`.
- `packages/compliance-proxy/internal/proxy/listener.go:147` — proxy options grow `policyResolver streaming.PolicyResolver`. Drop the global `streamingMode` option (resolution moves to per-host).
- `packages/compliance-proxy/cmd/compliance-proxy/init.go:308-426` — initialize policy resolver. Drop the YAML `streamingMode` config (replaced by DB-driven global default + per-domain override). Keep YAML envelope but make the value an explicit fallback if Hub config is unreachable at boot.
- `tools/db-migrate/schema.prisma` (incremental) — `interception_domain` adds: `streaming_mode String?`, `streaming_chunk_bytes Int?`, `streaming_hook_timeout_ms Int?`, `streaming_fail_behavior String?`, `capture_request_body Boolean?`, `capture_response_body Boolean?`, `raw_body_spill_enabled Boolean?`. Migration generated.
- `tools/db-migrate/codegen-go-models.json` — re-emit `interception_domain` Go struct.
- `packages/shared/schemas/configtypes/interception_domain.go` — re-codegen.
- Tests: `policy_resolver_test.go` (NULL inheritance per column), `forward_handler_test.go` (request body + request pipeline result reach emitter on SSE path), `sse_test.go` (per-mode end-to-end with stub upstream).

## Tasks

1. Add the new `interception_domain` columns + migration; re-codegen.
2. Build `policy_resolver` + thingclient shadow callback.
3. Refactor `forward_handler.go` to thread `requestPipelineResult` into `audCtx` and onward to all emit call sites (success, error, reject, exempt).
4. Rewrite `sse.go` to dispatch on `Policy.Mode` and call the new `emitAudit` signature with both bodies and both pipeline results.
5. Rewrite `compliance.AuditEmitter.Emit` for dual-pipeline semantics.
6. Drop the `streamingMode` YAML field (it now comes from DB); preserve env-var override for dev escape.
7. Run end-to-end against a real `chatgpt.com` flow + a non-SSE synthetic flow.

## Acceptance criteria

- A `chatgpt.com` `/backend-api/f/conversation` flow under `chunked_async` mode produces a `traffic_event` row with:
  - `request_hooks_pipeline` length ≥ 5 (the seeded enabled request hooks for `COMPLIANCE_PROXY` ingress).
  - `response_hooks_pipeline` length ≥ 1 (response-content-safety + pii-outbound).
  - `traffic_event_payload.inline_request_body` non-empty containing the user's prompt.
  - `traffic_event_payload.inline_response_body` non-empty containing the assistant's completion (assembled via `chatgpt-web` extractor).
  - No `audit/mq_writer: marshal failed` ERROR in the same window.
- Same flow under `buffer_full_block` mode + `fail_close` + a hook that hard-rejects ⇒ HTTP 451 to client; row records both pipelines.
- Same flow under `passthrough` mode ⇒ row exists with both pipelines empty (kind=`absent` for the bodies); no hook executions; no extractor runs.
- `policy_resolver` unit tests: full inheritance, single-column override, full override.
