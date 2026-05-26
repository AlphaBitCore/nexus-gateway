# E31 S6 — Hub self-registration self-heal

**Epic:** 31
**Story:** 6
**Status:** Draft — 2026-04-27
**Requirements:** inline (operator resilience; no separate requirements doc)

## User Story

As an operator running Nexus Hub in development or low-touch production, I want the Hub's own row in the `thing` table to be reconstituted automatically when it disappears mid-run, so dependent foreign keys (notably `metric_ops_raw.thing_id_fkey`) keep working without manual intervention.

## Background — observed failure

Hub's `selfreg.SelfRegistrar` upserts a `thing` row at startup (id = `cfg.Hub.ID`, type = `nexus-hub`). Once running, a 15s heartbeat calls `store.UpdateLastSeen` to keep `last_seen_at` fresh.

If the row is removed mid-run — by `prisma migrate reset`, manual SQL, or any pruning job — `UpdateLastSeen` returns `store.ErrNotFound`. The current heartbeat just logs a `WARN` and keeps going. The row is never re-inserted, so:

1. Every heartbeat tick (15s) emits `level=WARN msg="self-heartbeat failed" id=hub-dev error="not found"`.
2. The Hub's own metrics-sample tick (separate goroutine, 15s) keeps enqueuing samples for `thing_id=hub-dev`. The opsmetrics writer's `COPY` into `metric_ops_raw` fails with `metric_ops_raw_thing_id_fkey` (SQLSTATE 23503), and the entire COPY batch is dropped.
3. Hub-side metrics never land in `metric_ops_raw`. Other Things' metrics are also dropped if they happen to share a batch with a hub sample.

## Scope

In:
- `selfreg`: extract a private `doUpsert(ctx)` shared between `Register` and the heartbeat fallback path; on `errors.Is(err, store.ErrNotFound)` the heartbeat re-runs `doUpsert`. INFO log on successful self-heal; WARN if the upsert itself fails.
- `selfreg`: promote the package-private `thingType` constant to an exported `ThingType` so the metrics-tick callsite in `cmd/nexus-hub/main.go` can use the same source of truth.
- `cmd/nexus-hub/main.go`: replace the literal `"service"` passed to `opsWriter.Enqueue` with `selfreg.ThingType` (= `"nexus-hub"`). The previous literal was a data-quality bug — `metric_ops_raw.thing_type` rows for the Hub disagreed with `thing.type`.
- One-shot data cleanup: `UPDATE metric_ops_raw SET thing_type='nexus-hub' WHERE thing_id='hub-dev' AND thing_type='service'` so historical rows match the new convention. Documented here; executed manually as part of verify.

Out:
- Investigation of how/why the `thing` row was deleted in the first place. Treated as a normal dev-loop possibility (seed reset / migration); the fix makes Hub resilient regardless of the cause.
- Schema changes to `metric_ops_raw` (e.g. dropping the redundant `thing_type` column). Independent decision.
- Heartbeat semantics for non-Hub Things — those are managed by Hub via WS, not selfreg.

## Tasks

### T1. Constant promotion

- `internal/selfreg/selfreg.go`: rename `thingType` → exported `ThingType`. Both `Register` and external callers can import it.

### T2. Self-heal heartbeat

- Extract `func (s *SelfRegistrar) doUpsert(ctx context.Context) error` containing the existing `store.UpsertThingEnrollment` call.
- `Register` keeps its current behavior (calls `doUpsert`; on success starts the heartbeat).
- `heartbeatLoop`: when `UpdateLastSeen` returns an error matching `store.ErrNotFound`, call `doUpsert(ctx)`:
  - Success → `s.logger.Info("hub re-registered after thing row was missing", ...)`. Continue to next tick.
  - Failure → `s.logger.Warn("hub re-registration failed", ...)`. Continue to next tick (next heartbeat will retry).
- Other heartbeat errors stay at WARN with the original message.

### T3. Writer callsite

- `cmd/nexus-hub/main.go:271`: `opsWriter.Enqueue(ctx, hubThingID, "service", batch)` → `opsWriter.Enqueue(ctx, hubThingID, selfreg.ThingType, batch)`.

### T4. Tests

- `internal/selfreg/selfreg_test.go`: stub store implementing the small surface used by selfreg (`UpsertThingEnrollment`, `UpdateLastSeen`, `UpdateThingStatus`).
  - `Register` calls `UpsertThingEnrollment` once.
  - When `UpdateLastSeen` returns `store.ErrNotFound`, the next heartbeat triggers exactly one additional `UpsertThingEnrollment` (re-register). Subsequent successful heartbeats do NOT re-upsert.
  - When `UpdateLastSeen` returns a generic error, no re-register is triggered.

### T5. Verify

- `go test -race -count=1 ./packages/nexus-hub/internal/self/reg/... ./packages/nexus-hub/internal/storage/store/...` passes.
- Manual: restart Hub, then `DELETE FROM thing WHERE id='hub-dev';`, wait 15s, observe:
  1. Log: `INFO msg="hub re-registered after thing row was missing"`.
  2. `SELECT id FROM thing WHERE id='hub-dev'` returns one row.
  3. `metric_ops_raw_thing_id_fkey` errors stop appearing in the Hub log.
- One-shot SQL cleanup `UPDATE metric_ops_raw SET thing_type='nexus-hub' WHERE thing_id='hub-dev' AND thing_type='service'`; record affected row count in the verify notes.

## Acceptance Criteria

1. After deleting the Hub's row from `thing` while Hub is running, the row is restored automatically within one heartbeat interval (≤ 15s).
2. After the row is restored, `metric_ops_raw` COPY no longer fails with `thing_id_fkey`; subsequent samples persist successfully.
3. Self-heal events log at INFO with structured fields (`id`, `role`, `address`); failures log at WARN with the underlying error.
4. The metrics-sample callsite uses `selfreg.ThingType` (`"nexus-hub"`); no callsite references the legacy literal `"service"` for Hub samples.
5. `metric_ops_raw` rows with `thing_id='hub-dev'` and `thing_type='service'` are updated to `thing_type='nexus-hub'` so historical and new rows agree.
6. `go test -race -count=1` passes for selfreg and store.

## Risks

- **Auto-recreation could fight a deliberate operator delete.** Acceptable: if the Hub process is running, it owns its row in the registry; soft-deleting belongs to `Deregister`, not direct SQL. If an admin really wants the row gone, they should stop the Hub.
- **`doUpsert` overwrites `desired = '{}'` via the existing UpsertThingEnrollment SQL.** This only runs when the row was already gone, so any prior `desired` value is already lost; resetting to `{}` is the correct floor and avoids divergence.
