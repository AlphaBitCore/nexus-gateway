# E33 — Compliance Body Capture + Streaming Pipeline Overhaul

Status: In progress
Epic owner: Gateway / Compliance
Related: E18 (Traffic signal extraction), E22 (Payload capture end-to-end), E27 (Compliance exemption), E29 (Rule Pack runtime).

## 1. Background

End-to-end testing on `chatgpt.com` SSE traffic (`POST /backend-api/f/conversation`) surfaced six defects in the compliance audit pipeline that are individually patchable but architecturally rooted in the same set of design assumptions made earlier when the only audited traffic was small JSON request/response pairs:

1. **Request body and response body are never persisted** for SSE responses, even when the operator has enabled `payload_capture.storeRequestBody` and `payload_capture.storeResponseBody` in `system_metadata`. Two independent root causes:
   - `compliance-proxy/internal/proxy/sse.go:emitAudit` hard-codes `requestBody=nil, responseBody=nil` in every audit emit, so the `audCtx.requestBody` already buffered upstream is silently dropped on the SSE branch.
   - `compliance-proxy/internal/audit/event_message.go:73,76` strong-casts the body `[]byte` into `json.RawMessage`. SSE bodies are **not JSON** — they are a text stream of `event:` / `data:` lines per RFC. Any non-JSON byte (notably `\x1b` ANSI escape from a server log line bleed-through) makes `json.Marshal` of the audit message error out, and `mq_writer.go:233` `continue`s past the failure — silently dropping the event from the MQ stream entirely.
2. **Request hooks appear not to run** in `traffic_event.hooks_pipeline`. They actually do run (`forward_handler.go:244-256`), but their `HookResult` slice is captured into `reqHookResult` only to stamp a `x-nexus-cp-*` response header (`cpHookOutcomeFromResult`); it is never passed to the audit emitter. The SSE audit path's `emitAudit` accepts a single `result` argument and writes only the response pipeline's executions to the row.
3. **Response hooks "run on partial content" or "do not run at all"** depending on streaming mode and provider classification. `chatgpt.com` is registered with `interception_domain.adapter_id=openai-compat` but its outbound `/backend-api/f/conversation` body is JSON-Patch-shaped SSE, not OpenAI SSE — so the OpenAI adapter cannot extract content, the request meta classifier returns no provider, the `live` streaming pipeline takes the `pipelineExec == nil && acc == nil` fast-path, and response hooks never fire.
4. **Hook executions for the request side are absent from `traffic_event.hooks_pipeline` JSONB**. The schema has a single `hook_decision` + `hooks_pipeline` pair shared between stages, with the response writer overwriting whatever the request writer set. The dead `response_hook_decision` column reveals that someone planned the split but never finished it.
5. **Bodies for AI workloads exceed every existing limit**. The system was sized for sub-100 KiB chat messages: `payload_capture.MaxBodyBytes` defaults to 64 KiB; the proxy's max response buffer is 8 MiB; Postgres JSONB performance degrades sharply past ~1 MiB; NATS JetStream message limit is 1 MiB. Modern AI requests with 1M-token context windows are 5+ MiB raw, agentic loops can exceed 20 MiB, and any of these limits silently truncates or drops the row.
6. **Rule Pack runtime usage is fragile**. `rulepack.Enrich` is wired in `init.go:373` and reaches the engine, but `rule_pack_install.boundHookId` has no foreign-key constraint to `"HookConfig".id`, so deleting a hook leaves orphan installs that make `Enrich` (strict mode) fail the next config reload. A separate window of `cached plan must not change result type` (Postgres `SQLSTATE 0A000`) errors after schema migrations also took out reloads.

These defects are cross-cutting across `compliance-proxy`, `ai-gateway`, and `agent`, all three of which share `packages/shared/transport/streaming` and the same audit message wire format. Fixing them per-service produces drift; fixing them in `shared` once propagates uniformly.

The user has explicitly approved a full refactor (no phased compatibility path, no `@deprecated` shims, no data migration code for pre-GA traffic_event rows) consistent with the project's pre-GA "no backward compatibility, no defer" policy.

## 2. Scope

### Must (functional)

- **Capture both request and response bodies** for SSE and non-SSE traffic on all three data planes when payload capture is enabled at the resolved scope. A captured body must round-trip the entire pipeline (data plane → MQ → Hub → DB → admin readback) without loss for any byte sequence — JSON, SSE text, multipart, binary, gzip-compressed bytes are all valid.
- **Run both request- and response-stage hook pipelines** for every PROCESSed request, with both pipelines' `HookResult` slices recorded on the same `traffic_event` row. The single `hook_decision` column is replaced by `request_hook_decision` + `response_hook_decision`; the single `hooks_pipeline` column is replaced by `request_hooks_pipeline` + `response_hooks_pipeline`.
- **Three streaming modes** selectable per host (compliance-proxy, agent) or per provider (ai-gateway):
  - `passthrough` — relay only, no hook, no body capture. (Sub-AI-content-traffic fast path.)
  - `buffer_full_block` — assemble the full extracted content; run response hook once at stream end; HTTP-451 the client when hook rejects (`fail_close`) or forward upstream bytes when `fail_open` and the rejection is informational.
  - `chunked_async` — relay bytes to client in real time, but accumulate extracted content into chunks of `chunk_bytes`, run response hook on each chunk and once more at stream end. Hook executions per stream are capped at 64; oversized streams adapt `chunk_bytes` upward to honor the cap.
- **Per-provider SSE content extraction** for the four providers we ship in this round: `openai-api` (existing), `anthropic-messages` (existing), `google-gemini` (existing), `chatgpt-web` (new — JSON-Patch-shaped SSE). Each extractor reduces the SSE byte stream to a canonical `{prompt, completion}` text view; the hook pipeline only ever sees the canonical view, never raw SSE frames.
- **Two-tier body storage**: bodies < 256 KiB extracted go inline into `traffic_event_payload`; bodies ≥ 256 KiB are written via `SpillStore.Put` and the row stores only the spill reference. `SpillStore` is an interface with a built-in `localfs` backend; S3 / Azure Blob / GCS adapters can plug in without touching callers.
- **Per-host / per-provider override** of streaming mode + capture flags + spill enablement, with global default in `system_metadata['streaming_compliance.config']`. Per-host columns live on `interception_domain` (used by compliance-proxy + agent). Per-provider columns live on `Provider` (used by ai-gateway). NULL = inherit global.
- **fail_open / fail_close** behavior on hook errors, hook timeouts, oversized buffers, and SSE parse failures, applied uniformly across the three modes (no-op on `passthrough`).
- **NDJSON fallback** for audit messages that cannot reach Hub MQ — whether MQ is unreachable **or** a single message fails to marshal/enqueue. No event is silently dropped.
- **Admin UI** to edit the global default and per-resource overrides.
- **Rule pack runtime resilience**: orphan `rule_pack_install` rows must not break a config reload. `Enrich` becomes best-effort: a single failed install is logged + countered + skipped, not propagated into a reload-fail.

### Must (non-functional)

- **Performance**: ai-gateway hot-path overhead from streaming policy resolution + content extraction must not add more than 5% p99 latency vs the existing `live` mode. Hook regex execution on 1 MiB of extracted content must complete within `hook_timeout_ms`.
- **Memory**: per-stream in-memory buffer must not exceed 64 MiB on any data plane. The agent runs on customer desktops; same cap applies.
- **Storage cost**: `traffic_event_payload` inline rows must remain under 256 KiB to keep Postgres JSONB query performance acceptable. Spill backend retention defaults to 30 days + 50 GiB total cap, configurable per-deployment.
- **Wire compatibility**: MQ audit message must not exceed 512 KiB (NATS JetStream cap is 1 MiB; we leave headroom for future schema growth).
- **Observability**: per-mode hook latency, body-spill rate, NDJSON-fallback count, content-extractor parse-failure rate are exposed as Prometheus metrics on every data plane.
- **Security**: spilled bodies inherit the same access control as `traffic_event_payload`. The spill backend's `Get` path is reached only via Hub admin API, not directly by the data plane after `Put`.
- **Failure budget**: a chatgpt.com SSE conversation that exhausts `chunk_bytes × 64` cap or breaks the extractor must still leave the client connection intact; the audit row is marked `truncated=true` and the operator is alerted via standard alerting (E21).

### Should

- Smoke test `scripts/smoke-compliance-proxy-sse.sh` that drives a real `chatgpt.com` flow + an `ai-gateway` `/v1/chat/completions` flow + an `agent` SSE flow, asserts the audit row contains both pipelines' executions, both inline bodies (or spill refs), and matching extracted content.
- Local `localfs` spill rotation that respects retention policy automatically (no separate cron required for dev).
- Postgres `SQLSTATE 0A000` cached-plan recovery on the affected pgx pool.

### Won't (this epic)

- S3 / Azure Blob / GCS spill backend implementations — only the `SpillStore` interface and `localfs` adapter ship in this epic. Cloud backends are a follow-up plug-in.
- New AI providers beyond the four extractors named above. (Bedrock / Vertex / GLM / DeepSeek extractors stay on the existing OpenAI-compat path until called out separately.)
- Encryption-at-rest for spill files. The local filesystem inherits OS-level disk encryption when present; explicit envelope encryption is a separate compliance epic.
- Per-virtual-key streaming mode override on ai-gateway — provider-level granularity is the v1 ceiling.

## 3. Personas & Roles

- **Compliance officer (Carol)** — turns on body capture for `chatgpt.com`, sets streaming mode to `chunked_async` + `fail_close`, expects to query a redacted audit by content keyword.
- **Provider admin (Bob)** — adds a new in-house provider, configures `buffer_full_block` for it because the model is small and latency is acceptable.
- **Platform admin (Alice)** — manages the global default; needs visibility into spill disk usage; rolls out per-host overrides cautiously.
- **Operator on call (Diana)** — sees `hook_error_blocked` alerts; needs a fast pivot from alert to the underlying audit row + extracted content.
- **Developer (engineer running dev mode)** — runs the smoke test, expects deterministic results against the seeded chatgpt domain.

## 4. Constraints & Assumptions

- All three data planes share `packages/shared/transport/streaming` and `packages/shared/audit`. Schema changes to either propagate uniformly.
- The audit message wire format is a single struct in `packages/shared/transport/mq.TrafficEventMessage`; Hub's db-writer is the sole consumer. No backward-compatibility shim exists or is wanted.
- Postgres JSONB is the inline body store; performance characteristics are well understood (degrades past ~1 MiB).
- NATS JetStream is the production MQ; default per-message limit is 1 MiB. We hard-cap audit messages at 512 KiB.
- The `localfs` spill backend lives in each data-plane service's own filesystem (compliance-proxy / ai-gateway: server disk; agent: user home dir). Retention sweeps run in-process.
- Pre-GA: no backward-compatibility shims, no `@deprecated` markers, no data migrations for pre-existing `traffic_event` rows. Old rows simply have NULL in the new columns.
- The `interception_domain` table is shared by compliance-proxy and agent. The `Provider` table is owned by ai-gateway. Adding per-resource columns to both is acceptable since they hold the same Policy struct semantics — the resolver lives in `shared/streaming/policy/`.

## 5. Glossary

- **Extracted content** — canonical `{prompt, completion}` text view assembled from a provider-specific raw body shape (SSE, JSON, etc.). The hook pipeline operates only on this view.
- **Inline body** — extracted content stored directly in `traffic_event_payload.inline_request_body` / `inline_response_body` JSONB column. Gating: `< 256 KiB`.
- **Spill body** — extracted content written to a `SpillStore` backend; the row stores a `spill_ref` discriminator only. Gating: `≥ 256 KiB` or non-text body.
- **`StreamingPolicy`** — resolved settings (mode + chunk_bytes + hook_timeout_ms + fail_behavior + capture flags + spill enablement) that govern one stream's compliance handling. Computed by `shared/streaming/policy.Resolve(...)`.
- **`buffer_full_block`** — streaming mode that buffers the entire response before forwarding any byte to the client; supports response-stage hard reject.
- **`chunked_async`** — streaming mode that forwards bytes in real time and runs hooks asynchronously on accumulating chunks; cannot reject (audit-only).
- **`SpillStore`** — pluggable interface for storing large bodies out-of-band; v1 ships `localfs` only.
- **Dual pipeline** — request-stage and response-stage hook pipelines both recorded on the same audit row, in their own columns.

## 6. Priority

| Requirement | Priority |
|---|---|
| Capture both bodies for SSE + non-SSE on all three data planes | Must |
| Run + record dual hook pipeline on every audit row | Must |
| Three streaming modes with per-resource override | Must |
| Per-provider SSE content extraction (4 providers) | Must |
| Two-tier body storage (inline + spill) | Must |
| `SpillStore` interface + `localfs` backend | Must |
| `fail_open` / `fail_close` per scope | Must |
| NDJSON fallback on marshal/enqueue failure | Must |
| Admin UI for global + per-resource policy editing | Must |
| Rule pack runtime resilience (FK + best-effort Enrich) | Must |
| Smoke test script | Should |
| Postgres `SQLSTATE 0A000` recovery | Should |
| S3 / Azure / GCS spill backends | Won't (v1) |
| Encryption-at-rest for spill files | Won't (v1) |

## 7. Acceptance Signals

A subsequent SDD set (E33-S1 .. E33-S7) carries the per-story acceptance criteria. The epic-level signal is the smoke test:

```
scripts/smoke-compliance-proxy-sse.sh
  → starts compliance-proxy + ai-gateway + (mock) agent
  → drives 1× chatgpt.com SSE conversation through compliance-proxy
  → drives 1× /v1/chat/completions stream through ai-gateway with the seeded VK
  → drives 1× MITM SSE relay through agent
  → asserts each traffic_event row has:
      - request_hooks_pipeline length ≥ enabled-request-hook count for that scope
      - response_hooks_pipeline length ≥ enabled-response-hook count for that scope
      - request_hook_decision and response_hook_decision both set
      - traffic_event_payload row exists with non-empty inline_request_body
      - traffic_event_payload.inline_response_body OR response_spill_ref set
      - extracted content includes the user prompt and the model completion
      - mq_writer NDJSON fallback file is empty
  → exits 0 on all assertions passing
```
