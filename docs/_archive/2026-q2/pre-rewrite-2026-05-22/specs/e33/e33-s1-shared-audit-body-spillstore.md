# E33 S1 — Shared Audit Body Container + SpillStore + Schema Migration

## Story

As a developer working on any of the three data planes I need a single audit-message wire format that round-trips arbitrary body bytes (JSON, SSE text, multipart, binary) without `json.Marshal` failing, and a shared body-spill abstraction so large bodies (1M-token AI requests) do not poison the inline audit row, so that the compliance pipeline can capture full request/response content end-to-end.

## Scope

- `packages/shared/audit/types.go` — replace `RequestBody []byte` / `ResponseBody []byte` with a `Body` struct: `{Kind: "absent"|"inline"|"spill", InlineBytes []byte, SpillRef *SpillRef, SizeBytes int64, Truncated bool, ContentType string}`. Drop the `json:"-"` tags so the body is part of the wire format.
- `packages/shared/audit/body.go` (new) — helpers `NewInlineBody`, `NewSpillBody`, `EmptyBody`; the `MarshalJSON`/`UnmarshalJSON` for `Body` so non-JSON inline bytes round-trip via base64 (`encoding=raw|base64`).
- `packages/shared/storage/spillstore/spillstore.go` (new) — `SpillStore` interface (`Put` / `Get` / `Delete` / `Sweep`), `Ref` struct (`Backend, Key, Size, SHA256, ContentType`), `Metadata` struct.
- `packages/shared/storage/spillstore/localfs/localfs.go` (new) — filesystem backend, layout `<root>/<yyyy-mm-dd>/<event-id>-{req|resp}.bin`, retention sweep goroutine, total-size cap.
- `packages/shared/storage/spillstore/registry.go` (new) — backend factory keyed by `system_metadata['spill_store.config'].backend`.
- `packages/shared/transport/mq/types.go` — `TrafficEventMessage` adopts `RequestBody Body` / `ResponseBody Body` (replacing the old `json.RawMessage` fields), adds `RequestHooksPipeline []HookExecution` / `ResponseHooksPipeline []HookExecution` / `RequestHookDecision string` / `ResponseHookDecision string`. Drop `HookDecision` and `HooksPipeline` (single-stage legacy).
- `packages/compliance-proxy/internal/audit/types.go` — `AuditEvent.RequestBody/ResponseBody` change to `audit.Body`. Drop the `HookDecision` single field; replace with `RequestHookDecision`, `ResponseHookDecision`, `RequestHooksPipeline`, `ResponseHooksPipeline`.
- `packages/compliance-proxy/internal/audit/event_message.go` — rewrite `toMessage` to populate the new dual fields and to copy `Body` directly (no `json.RawMessage` cast).
- `packages/compliance-proxy/internal/audit/mq_writer.go:218-241` — on `json.Marshal` failure **OR** `producer.Enqueue` failure, fall back to NDJSON via `fallbackToNDJSON` instead of `continue`. Add Prom counter `nexus_compliance_audit_drop_total{reason}` to count fallback paths.
- `packages/compliance-proxy/internal/audit/ndjson.go` — accept `audit.Body` cleanly; spill-form events serialize the `SpillRef` only.
- `packages/nexus-hub/internal/dbwriter/traffic_event.go` — INSERT path adopts the dual-pipeline columns + the body kind discriminator. When `body.Kind == "inline"` write `inline_request_body` / `inline_response_body`; when `body.Kind == "spill"` write `request_spill_ref` / `response_spill_ref` JSONB. Drop the `hook_decision` / `hooks_pipeline` writes.
- `tools/db-migrate/schema.prisma` — `traffic_event` drops `hook_decision`, `hooks_pipeline`. Adds `request_hook_decision`, `response_hook_decision`, `request_hooks_pipeline Json?`, `response_hooks_pipeline Json?`. `traffic_event_payload` drops `request_body`, `response_body`. Adds `inline_request_body Json?`, `inline_response_body Json?`, `request_spill_ref Json?`, `response_spill_ref Json?`, `request_size_bytes BigInt?`, `response_size_bytes BigInt?`, `request_truncated Boolean default(false)`, `response_truncated Boolean default(false)`.
- `tools/db-migrate/migrations/<ts>_e33_dual_pipeline_body_capture/migration.sql` — generated migration.
- `tools/db-migrate/codegen-go-models.json` — re-emit `traffic_event` + `traffic_event_payload` Go structs.
- Tests: `packages/shared/audit/body_test.go` (round-trip JSON/SSE/binary). `packages/shared/storage/spillstore/localfs/localfs_test.go` (Put/Get/Delete/Sweep/retention cap). `packages/compliance-proxy/internal/audit/mq_writer_test.go` (marshal failure ⇒ NDJSON path; enqueue failure ⇒ NDJSON path).

## Tasks

1. Define `audit.Body` + `SpillRef` types and tests for marshal of every kind permutation (absent / inline-raw / inline-base64 / spill).
2. Define `spillstore.SpillStore` interface and `localfs` implementation with rotation sweep goroutine. Configurable root, retention days, total-size cap.
3. Wire `spillstore.NewFromConfig` to read `system_metadata['spill_store.config']`. Default backend `localfs`; data-plane services pass their own root path.
4. Migrate Prisma schema; run codegen; commit migration SQL.
5. Adapt `TrafficEventMessage` + `AuditEvent` to dual pipeline + `Body`. Update toMessage. Update Hub db-writer INSERT.
6. Adapt `MQBatchWriter.flushBatch` to fall back to NDJSON on marshal **or** enqueue failure. Add `nexus_compliance_audit_drop_total{reason}` counter.
7. Verify pre-existing rows are unaffected (NULL in new columns is acceptable).

## Acceptance criteria

- `go test ./packages/shared/audit/... ./packages/shared/storage/spillstore/...` green with race + count=1.
- `audit.Body` JSON round-trip preserves bytes for: a 1 KiB JSON request, a 200 KiB SSE text response, a 100-byte gzip-compressed body containing `\x1b`. None error on `json.Marshal(msg)`.
- `localfs.Put → Get` returns identical bytes; `Sweep(olderThan=now)` deletes all rows; total-size cap evicts oldest first.
- `MQBatchWriter` marshal-fail test injects a synthetic body with `\x1b` and asserts the NDJSON fallback file received the row + `nexus_compliance_audit_drop_total{reason="marshal_failed"}` incremented.
- After migration, `\d traffic_event` shows the new columns and the dropped legacy columns are gone.
