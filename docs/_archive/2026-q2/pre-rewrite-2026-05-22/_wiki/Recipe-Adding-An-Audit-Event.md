# Recipe Adding An Audit Event

*Audience: contributors adding a new event type to the Nexus Gateway audit pipeline.*

The audit pipeline records every traffic-affecting event into `traffic_event` (data-plane requests) and every admin mutation into `AdminAuditLog`. Adding a new audit event means extending the `AuditEvent` struct, updating the Postgres schema, wiring the emitter, and confirming the event appears in the CP UI. The canonical architecture reference is [`audit-pipeline-architecture.md`](https://github.com/AlphaBitCore/nexus-gateway/blob/main/docs/developers/architecture/cross-cutting/observability/audit-pipeline-architecture.md).

---

## Which table to use

Two audit tables serve different purposes:

| Table | What goes here | Who writes it |
|---|---|---|
| `traffic_event` | Every data-plane request (AI Gateway, Compliance Proxy, Agent) — provider, model, tokens, cost, hook decisions, latency phases | AI Gateway / Compliance Proxy / Agent via the `Writer` interface + NATS `nexus.event.ai-traffic` |
| `AdminAuditLog` | Every admin API mutation and sensitive read (policy changes, credential rotation, kill-switch toggle) | Control Plane handlers, in-transaction |

New traffic signals (new token fields, new hook verdicts, new provider metadata) extend `traffic_event`. New admin actions extend `AdminAuditLog`. Do not mix the two — a traffic event is not an admin action.

---

## Adding a traffic_event field

### Step 1 — Extend the AuditEvent struct

Add the new field to `packages/shared/audit/event_types.go`:

```go
type AuditEvent struct {
    // ... existing fields ...
    YourNewField string `json:"your_new_field,omitempty"`
}
```

If the field is a usage/token count, also add it to the `Usage` struct in `packages/ai-gateway/internal/providers/core/types.go`.

### Step 2 — Add the Postgres column

In `tools/db-migrate/schema.prisma`, add the column to the `TrafficEvent` model:

```prisma
model TrafficEvent {
  // ...
  yourNewField  String?  // nullable until backfilled
}
```

Generate the migration:

```bash
cd tools/db-migrate
npx prisma migrate dev --name add_traffic_event_your_new_field
npm run check:migration-timestamps   # timestamp must be unique
```

### Step 3 — Stamp the field at all five proxy sites (binding)

If the new field derives from the provider response (token count, cost, cache classification), stamp it at all five sites in `packages/ai-gateway/internal/ingress/proxy/`:

1. `proxy.go:handleNonStream` — live non-cached response.
2. `proxy_cache.go:handleStreamHit` — streaming response replayed from cache.
3. `proxy_cache.go:handleNonStreamHit` — non-streaming response replayed from cache.
4. `proxy_cache.go:handleStreamWithSubscription` — streaming response on cache miss (fills cache while serving).
5. `proxy_cache.go:handleNonStreamWithSubscription` — non-streaming response on cache miss.

Missing the four cache sites means all cache-hit rows show NULL for the new field — the most common post-launch discovery.

### Step 4 — Stamp in the emitter

In the code path that creates the `AuditEvent`, populate the new field:

```go
event.YourNewField = extractYourFieldFromResponse(resp)
```

For Compliance Proxy and Agent paths, check that the same field is populated in their respective emitter code (`packages/compliance-proxy/` and `packages/agent/`).

### Step 5 — Update the Hub ingest path

The Hub audit sink in `packages/nexus-hub/internal/traffic/ingest/audit/` reads `AuditEvent` fields and maps them to Postgres columns. If the new field maps to a new column (not an existing nullable column), add the mapping in the ingest helper.

**Empty-string invariant (binding)**: the Hub ingest path must handle `""` values from the Agent's `auditEventToMap`. For any CHECK-constrained column, either accept `""` and write NULL, or reject `""` with a clear error. Inconsistent handling causes silent pipeline stalls.

### Step 6 — Verify the row

```bash
# Issue a test request:
curl -H "Authorization: Bearer <VIRTUAL_KEY>" \
     http://localhost:3050/v1/chat/completions \
     -d '{"model":"<model>","messages":[{"role":"user","content":"audit test"}]}'

# Confirm the new field is populated:
docker exec postgres psql -U postgres -d nexus_gateway \
  -c "SELECT your_new_field FROM traffic_event ORDER BY emitted_at DESC LIMIT 1;"
```

---

## Adding an AdminAuditLog entry

### Step 1 — Add the event in the handler

`AdminAuditLog` rows are inserted in-transaction by CP admin handlers. The canonical insert lives in the handler alongside the data mutation:

```go
// Inside the handler, within the same transaction:
if err := auditLog.Record(ctx, tx, AdminAuditLogEntry{
    Actor:        principal.ID,
    Action:       "your-resource.your-verb",
    ResourceType: "your-resource",
    ResourceID:   resourceID,
    Detail:       jsonDetail,
}); err != nil {
    return err  // rolls back the mutation too
}
```

The `AdminAuditLog` table is hash-chained (`previousHash` → `integrityHash`). The insert helper computes the chain automatically; do not compute the hash manually.

### Step 2 — Add the action to the coverage matrix

`docs/developers/architecture/cross-cutting/observability/admin-audit-log-coverage.md` is the companion doc that catalogs every admin action covered by `AdminAuditLog`. Add a row for the new action:

```markdown
| `your-resource.your-verb` | Handler in `packages/control-plane/internal/<domain>/handler/` | What detail JSON contains |
```

Without this, the coverage matrix drifts from reality and compliance audits produce false gaps.

### Step 3 — Confirm CP UI surface

`AdminAuditLog` entries appear in the CP UI Audit Log section (`/audit-log`). Verify the new action renders correctly — the drawer shows the `action`, `resourceType`, `resourceID`, and `detail` JSON. If the `detail` JSON contains sensitive fields (credential values, policy document contents), confirm the redaction step in the handler strips them before writing.

---

## What links break if you skip this

- **Skipping the five-site stamp sweep**: cache-hit rows show NULL for the new field, making analytics queries that filter by the new field silently return incomplete results. This is not visible in non-cache traffic.
- **Skipping the Hub ingest mapping**: the field is stamped in `AuditEvent` and sent over NATS, but the Hub sink never writes it to Postgres — all rows show NULL regardless of whether caching is involved.
- **Skipping the AdminAuditLog coverage matrix update**: a future compliance audit finds an undocumented action gap and incorrectly flags it as a compliance failure.
- **Ignoring the empty-string invariant**: Agent-originated events with empty string fields cause CHECK constraint violations at the Hub ingest path, silently halting the audit pipeline for all Agent traffic until the queue drains or the Hub restarts.

---

## Canonical docs

- [`audit-pipeline-architecture.md`](https://github.com/AlphaBitCore/nexus-gateway/blob/main/docs/developers/architecture/cross-cutting/observability/audit-pipeline-architecture.md) — two audit tables, emission flow, body storage tiering, failure modes, five-site stamp binding
- [`admin-audit-log-coverage.md`](https://github.com/AlphaBitCore/nexus-gateway/blob/main/docs/developers/architecture/cross-cutting/observability/admin-audit-log-coverage.md) — per-action coverage matrix for AdminAuditLog

**Adjacent wiki pages**: [Control Plane Audit Log](Control-Plane-Audit-Log) · [Security Audit Forensics](Security-Audit-Forensics) · [Observability Stack](Observability-Stack) · [Recipe Adding An IAM Action](Recipe-Adding-An-IAM-Action) · [Recipe Index](Recipe-Index)
