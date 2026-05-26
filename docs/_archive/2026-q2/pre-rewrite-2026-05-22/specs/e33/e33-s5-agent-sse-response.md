# E33 S5 — Agent: Response-Side SSE Support + Dual Audit Pipeline

## Story

As an end-user device running the Nexus agent I need full MITM compliance handling on streaming AI responses (today the agent only inspects the first request and skips the response body entirely), so that desktop traffic to chatgpt.com / api.openai.com / api.anthropic.com produces the same dual-pipeline audit rows the centralized compliance-proxy produces.

## Scope

- `packages/agent/core/network/proxy/proxy.go:617-697` (`inspectResponse`) — replace today's "SSE ⇒ usage-only via `streaming.PassthroughWithAccumulator`; non-SSE ⇒ optional response inspector" path with the same three modes from S2:
  - `passthrough` ⇒ relay only (existing behavior).
  - `buffer_full_block` ⇒ buffer entire response, run response hook at stream end, optionally HTTP-451 on `fail_close`.
  - `chunked_async` ⇒ relay bytes in real time + accumulate via `ContentAccumulator` + run response hook per chunk.
- `packages/agent/core/network/intercept/handler.go:82-178, 294` — `ProcessRequest` and `ProcessResponse` thread both pipelines' results into the audit context. Remove the request-only ceiling memorialised in `feedback_no_response_inspection`.
- `packages/agent/core/observability/audit/...` — adopt `audit.Body` and the dual-pipeline columns. The agent's local SQLCipher queue stores the same envelope shape so the Hub `POST /api/internal/things/agent-audit` consumer parses it identically to compliance-proxy and ai-gateway events.
- `packages/agent/core/network/proxy/policy_resolver.go` (new) — same `ResolvePolicyByHost` shape as compliance-proxy but reads from the agent's local snapshot of `interception_domain` rows pushed via the existing Hub Category B `interception_domains` config key.
- `packages/agent/core/sync/configsync/streaming_compliance.go` (new) — adapter for the Hub Category A `streaming_compliance` shadow key.
- `packages/agent/core/spillstore/init.go` (new) — initialize `localfs` spill backend in the agent's per-OS data dir (`~/.local/share/nexus-agent/spill/` macOS/Linux, `%APPDATA%\nexus-agent\spill\` Windows) — root resolved via `packages/agent/core/platform`. Retention defaults inherit `system_metadata['spill_store.config']`.
- `packages/agent/core/network/relay/sse.go` (new — promoted from compliance-proxy's `internal/proxy/sse.go`) — actual implementation lives in `shared/streaming` (S2); this is just the agent-side wiring + per-OS spill init.
- Tests: `proxy_test.go` per-mode SSE tests using a stub upstream + stub hook executor; `audit_test.go` round-trip through SQLCipher → in-memory Hub mock.

## Tasks

1. Wire `streaming.PolicyResolver` into the agent proxy.
2. Replace `inspectResponse` with the three-mode dispatch.
3. Initialize `spillstore.localfs` rooted at the per-OS data dir; pass into the audit emitter.
4. Replace the agent's existing `audit.AuditEvent` with the shared dual-pipeline + Body schema.
5. Update Hub-side `agent-audit` HTTP handler to accept the new envelope (no schema change needed — Hub uses the same `mq.TrafficEventMessage` writer).
6. Drop the `memory: feedback_no_response_inspection` precondition from agent docs (it's no longer accurate after this story).

## Acceptance criteria

- A simulated chatgpt.com SSE flow through the agent produces a Hub `traffic_event` row whose `source = "agent"` and whose `request_hooks_pipeline` + `response_hooks_pipeline` are both populated.
- Agent's local NDJSON / SQLCipher queue persists rows in the same `Body` shape — verified by feeding one queued row through the Hub agent-audit handler in a unit test.
- The smoke script (`scripts/smoke-compliance-proxy-sse.sh`) optionally drives an agent path when `--with-agent` is passed, with the same assertions as compliance-proxy.
- `localfs` retention sweep runs in-process; agent shutdown does not orphan the goroutine.
