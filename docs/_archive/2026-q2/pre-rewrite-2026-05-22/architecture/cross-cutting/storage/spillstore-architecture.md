---
doc: spillstore-architecture
area: cross-cutting
service: storage
tier: 1
updated: 2026-05-20
---

# Spillstore Architecture (S3)

> **Tier 2 architecture doc.** Read when touching `packages/shared/storage/spillstore/`, the S3 driver, or any code that overflows large bodies out of inline JSONB. Production cutover landed 2026-05-14 (memory `project_prod_s3_spillstore_done`). The audit pipeline sub-doc is `audit-pipeline-architecture.md` §7.

Audit rows have a hot path (Postgres JSONB) and a cold path (object store). Bodies ≥ 256 KiB overflow to spillstore; the audit row stores a reference. Presigned URLs deliver bodies to admins on demand.

---

## 1. The threshold

Inline JSONB up to **256 KiB** per body (request or response). Above that, overflow to spillstore. The threshold is one of the few tuned-by-pain numbers in the system: smaller threshold = more S3 calls + slower admin UI; larger threshold = bloated Postgres rows + slow JSONB scans.

## 2. Storage layout

Production: `s3://nexus-payload-capture-bucket/<prefix>/<YYYY-MM-DD>/<event-id>-<direction>.bin` where `<prefix>` is the store's configured prefix (e.g. `prod/`) and `<direction>` is `request` or `response`. Implemented in `Store.KeyFor` in `packages/shared/storage/spillstore/s3/s3.go`.

The body's SHA-256 is computed at upload time and persisted as an S3 object-metadata header (`x-amz-meta-sha256`) — not as part of the object key — so the audit row can verify integrity on download. The localfs driver (dev mode) uses the same date-prefixed key shape so retention sweeps are identical across drivers.

Per-day partitioning makes retention by date cheap (lifecycle rule on a prefix).

## 3. Reference stored in audit row

The `traffic_event_payload` row carries two parallel sets of columns — inline bodies for small payloads and spill refs for overflows. The spill ref is a JSONB blob:

```
traffic_event_payload {
  traffic_event_id      -- PK + FK to traffic_event.id
  inline_request_body   JSONB?   -- present when body is below the 256 KiB threshold
  inline_response_body  JSONB?
  request_spill_ref     JSONB?   -- present on overflow; shape: { backend, key, size, sha256, content_type } per `tools/db-migrate/schema.prisma:1571`
  response_spill_ref    JSONB?
  request_size_bytes    BIGINT?
  response_size_bytes   BIGINT?
  request_truncated     BOOLEAN  -- size cap hit
  response_truncated    BOOLEAN
  request_content_type  TEXT?
  response_content_type TEXT?
  created_at            TIMESTAMPTZ(3)
}
```

When the admin UI fetches a row, it reads from `traffic_event_payload`: if an inline body is present, render directly; if a spill ref is present, request a presigned GET URL from the Hub and fetch the body client-side. The two ref shapes (request/response) and the two inline shapes are independent — request can spill while response stays inline, or vice versa.

## 4. Three upload paths

### Server-side direct (AI Gateway / Compliance Proxy / Hub)

Direct S3 write using the service's IAM role / credentials. Fast path; happens inline with the audit emit.

### Agent presigned URL

Agents don't have S3 credentials. Instead:

1. Agent emits audit event with body in a local SQLCipher staging table.
2. Hub receives the audit metadata and issues a presigned PUT URL.
3. Agent uploads the body directly to S3 via the presigned URL.
4. Agent confirms completion to Hub; Hub finalises the audit row.

Memory `project_prod_s3_spillstore_done` records the prod cutover.

### Legacy local filesystem (dev only)

`SpillStore` interface has a local-FS driver for dev environments without S3 access. The same code paths work, just slower and not cross-instance.

## 5. Presigned URL TTL

`spillupload.MaxTTL = 5 minutes` (`packages/shared/storage/spillupload/token.go:29`); both the Hub presigner and the `PresignPut` default in the S3 driver share this bound. The admin UI generates a fresh URL per click — never long-lived. This bounds the leak radius if a URL is accidentally copied into a chat / email.

## 6. Encryption

In transit: HTTPS only. Presigned URLs are HTTPS-signed. Server-side encryption at rest is bucket-policy responsibility (operator-configured at the S3 bucket level); the Nexus S3 driver does not explicitly set encryption headers on uploads.

## 7. Retention

S3 lifecycle policies are applied at the bucket level by the operator. The shared S3 driver carries a default 30-day retention hint (`DefaultRetention = 30 * 24 * time.Hour` — `packages/shared/storage/spillstore/s3/s3.go:42`) consumed by callers that compute prefixes; the actual delete-by-date behaviour comes from the bucket's Lifecycle configuration. There is no Hub-side `retention.purge.spillstore` job in the current codebase — the data-retention job purges Postgres rows only, and orphan S3 objects rely on the bucket's Lifecycle rule.

## 8. Failure modes

| Failure | Behaviour |
|---|---|
| S3 unreachable at write time | Audit event still emits with `body_dropped=true` and `body_dropped_reason="s3_unreachable"`. Alert fires. |
| Presigned URL fails to issue (Hub down) | Admin UI shows "Body unavailable — Hub unreachable". Audit row is still readable; the body fetch is the failure surface. |
| Body checksum mismatch on download | Surface as "Body corrupt" with the stored SHA-256 vs computed SHA-256. Investigate the upload path. |
| Bucket misconfigured (4xx on PUT) | Same as unreachable; alert. |
| Lifecycle rule deletes body before audit row | Admin UI shows "Body expired" with the original timestamp. |

## 9. Agent SQLCipher staging

The agent local SQLCipher queue holds bodies until uploaded:

- Encrypted at rest with the platform keystore key.
- Drained as connectivity allows.
- On rotation (queue full), oldest non-blocked events drop with `body_dropped=true`.

This makes the agent resilient to extended offline windows. The audit metadata (without body) still emits when connectivity returns.

## 10. The `prod_s3_spillstore_done` retrospective

Pre-cutover, bodies lived inline in JSONB regardless of size. Some prod rows reached megabytes; admin queries timed out. Cutover compressed median row size by ~95% and made admin UI body fetch its own operation (which can fail gracefully).

The cutover required:

- Bucket setup (KMS + lifecycle + bucket policy).
- Hub presigned URL handler.
- Agent uploader.
- Service IAM roles.
- Migration of historical bodies (deferred — older rows stay inline).

Captured in memory `project_prod_s3_spillstore_done` (DONE 2026-05-14).

<!-- 💡 harvest: nothing new — the spillstore is structurally bounded. The "body_dropped=true" pattern is worth surfacing in the audit-pipeline doc, which it already is. -->

## 11. Sources

- `packages/shared/storage/spillstore/` — interface + drivers (subdirs: `s3/`, `localfs/`, `spillfactory/`).
- `packages/shared/storage/spillstore/s3/` — S3 driver (`Store.KeyFor`, `Put`, `PresignGet`, `PresignPut`).
- `packages/shared/storage/spillstore/localfs/` — local-FS driver used for dev mode (same key shape as S3).
- `packages/shared/storage/spillstore/spillfactory/` — driver-selection factory.
- `packages/nexus-hub/internal/traffic/ingest/spill/spill_uploads.go` — Hub presigned-URL endpoint (mints PUT URLs for agent uploaders; cross-ref `docs/users/api/openapi/admin/e37-s2-agent-presigned-spill.yaml`).
- `packages/agent/internal/observability/spilluploader/` — agent uploader (consumes the Hub-issued presigned URLs).
- `docs/developers/specs/e37/e37-payload-capture-unification.md` — original requirements.

## 12. Cross-references

- `audit-pipeline-architecture.md` §7 — parent body-storage tiering.
- `agent-forwarder-architecture.md` §4 — local SQLCipher staging.
- `data-retention-purge-architecture.md` — Postgres retention sweep (no S3 reaper today).
- `cache-multi-tier-architecture.md` — sibling cache catalogue.
