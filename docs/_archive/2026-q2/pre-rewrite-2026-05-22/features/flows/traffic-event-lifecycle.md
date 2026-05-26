# Flow — Traffic event lifecycle

## What this flow accomplishes

A `/v1/*` request (or compliance-proxy-routed request, or agent-intercepted request) generates a `traffic_event` record that traverses MQ, lands in Postgres, joins to spillstore for bodies, and surfaces in the analytics UI.

## Actors

Emitter (AI Gateway · Compliance Proxy · Agent) · MQ (NATS JetStream) · Hub audit-sink · Postgres · Spillstore (S3) · CP analytics UI.

## Sequence

1. **Emitter** processes the request through its pipeline; on completion (success or failure) constructs the `traffic_event` payload.
2. **PII redaction** at emit time (per hook config; cross-ref `audit-pipeline-architecture.md` §6).
3. **Body size check** — if ≥ 256 KiB, write to spillstore; payload stores `{ "ref": "s3://...", "size": N, "hash": "..." }`.
4. **Publish** to `nexus.event.ai-traffic` JetStream subject (cross-ref `packages/shared/transport/mq/messages.go:11` + `streams.go:74` — all `nexus.event.*` subjects route to the single `NEXUS_EVENTS` stream); envelope carries `event_id` (UUID v7), `trace_id`, `external_request_id`, `thing_id`, `org_id`, `schema_version`.
5. **Fallback** — if MQ unreachable: HTTP POST to Hub `/api/hub/audit` (mTLS for agent). Agent additionally buffers in SQLCipher if HTTP also down.
6. **Hub audit-sink consumer** pulls from `nexus.event.ai-traffic` → validates → dedups (cross-ref `mq-architecture.md` §5) → resolves `org_id → org_ancestor_path` → extracts typed columns → bulk-inserts batch to Postgres.
7. **Postgres** stores the row in `traffic_event`. Actual indexes per `tools/db-migrate/schema.prisma` (model `traffic_event`): `(timestamp)`, `(source, timestamp)`, `(org_id, timestamp)`, `(entity_id, timestamp)`, `(provider_id, timestamp)`, `(provider_name, timestamp)`, `(target_host, timestamp)`, `(thing_id, timestamp)`, `(cache_status, timestamp)`, `(request_hook_decision, timestamp)`, `(response_hook_decision, timestamp)`. Per-request identification uses `trace_id` + `external_request_id` (no bare `request_id` column).
8. **CP analytics endpoints** (`/api/admin/analytics/*`) query `traffic_event`; bodies are fetched via spillstore presigned URLs on demand.
9. **Retention purge** (`retention.purge.traffic_event` Hub job) deletes rows past TTL; spillstore cleanup is a separate parallel job.

## Failure modes

- **MQ down** — HTTP fallback. Agent additionally uses SQLCipher local queue.
- **Hub audit-sink lag** — `mq_consumer_lag_high` alert; stream retention sized for 24h hot, so a few hours of lag is recoverable.
- **Postgres down** — JetStream buffers; resumes on Postgres return.
- **Spillstore down** — bodies are dropped (event still recorded with `body_dropped=true`).
- **`empty-string` mishandling** — agent emits `""` for unset string fields; Hub must stamp-unconditionally or strip-empty for CHECK-constrained columns or pipeline stalls silently. Binding fix in `audit-pipeline-architecture.md` §3.
- **Dedup miss** — bloom-filter probabilistic + Postgres unique index catches; if both somehow fail, duplicates are detected by analytics.

## Verification

```bash
# 1) Issue a request.
curl ... /v1/chat/completions

# 2) Confirm row arrived:
docker exec postgres psql -U postgres -d nexus_gateway \
  -c "SELECT trace_id, external_request_id, provider_name, error_code FROM traffic_event ORDER BY timestamp DESC LIMIT 1"

# 3) For a large request, confirm body went to spillstore. Columns per
#    `traffic_event_payload` (schema.prisma:1570): `inline_request_body`,
#    `inline_response_body`, `request_spill_ref` (JSONB pointer), `response_spill_ref`,
#    `request_size_bytes`, `response_size_bytes`, `request_truncated`, `response_truncated`.
docker exec postgres psql -U postgres -d nexus_gateway \
  -c "SELECT (request_spill_ref->>'ref') AS spillref, request_size_bytes FROM traffic_event_payload WHERE traffic_event_id='...'"
```

## References

- `docs/developers/architecture/cross-cutting/observability/audit-pipeline-architecture.md` — full pipeline + retention.
- `docs/developers/architecture/cross-cutting/foundation/mq-architecture.md` — JetStream + dedup.
- `docs/developers/architecture/cross-cutting/observability/trace-id-propagation-architecture.md` — `request_id` / `trace_id` carrier.
- `docs/developers/architecture/cross-cutting/foundation/multi-endpoint-coordination-architecture.md` §7 — flow diagram.
- `docs/users/features/cp-ui/overview.md` — analytics surfaces.
- `project_prod_s3_spillstore_done` (memory) — production cutover.
