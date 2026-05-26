# Observability issue cleanup — remaining work

Doc-write pass on D01-D04 surfaced 17 product / architecture / technical issues across the observability stack. **6 PRs landed** in the current session; **4 PRs + doc updates remain** for follow-up sessions.

## Status

| PR | Scope | Status |
|---|---|---|
| PR-D | 6 small fixes (alert builtin drift, CI walk path, dedup counter, mq SourceProcess/Action, Prisma cosmetic) | **Landed** |
| PR-E | passthrough handler audits + Hub device-assignment audit emit | **Landed** |
| PR-K | AlertSeverity typed enum across 3 layers | **Landed** |
| PR-F | admin api-key.rotate verb + multi-key + status lifecycle | **Landed** |
| PR-G | DiagEvent typed `trace_id` field + index + slog wiring | **NEEDS RE-DO** — sub-agent edited main repo path not worktree; changes lost. |
| PR-I | SIEM consolidation — Hub canonical, drop compliance-proxy direct sink | TODO |
| PR-J | audit pipeline hardening — DLQ + normalize-backfill + coerceEmbeddingRow + audit.go split | TODO |
| PR-H | Hub observability directory reorg | TODO (run LAST — pure file moves, conflicts with everything) |
| Docs | Update D01-D04 + B04 + B06 to reflect fixes; run `/doc-review` on each | TODO |

## Uncommitted draft docs in working tree

The next session will find these uncommitted in `worktrees/docs-backfill`:

- `docs/developers/architecture/cross-cutting/observability/observability-architecture.md` — D01 draft (the full umbrella body)
- `docs/developers/architecture/cross-cutting/observability/admin-audit-log-coverage.md` — D02 draft
- `docs/developers/architecture/cross-cutting/observability/alerting-architecture.md` — D03 draft
- `docs/developers/architecture/cross-cutting/observability/audit-pipeline-architecture.md` — D04 draft
- `docs/developers/architecture/cross-cutting/safety/kill-switch-architecture.md` — B04 draft (still describes pre-PR-B `enabled` semantic — will rewrite once PR-B lands)
- `docs/developers/architecture/cross-cutting/safety/pii-redaction-policy-architecture.md` — B06 draft (still describes pre-PR-C `AIGUARD_SUGGESTED_VS_POLICY` half-built state — will rewrite once PR-C lands)

After PR-G/I/J/H land, **each doc's "known issue" sections** need to be deleted/rewritten to describe the fixed end-state (per the no-archaeology rule). Then commit each doc separately + run `/doc-review`.

## PR-G — DiagEvent trace_id (RE-DO)

Sub-agent's report claimed success but edits went to `/Users/nexus/workspaces/workspace-nexus/nexus-gateway/packages/...` (main repo) instead of `/Users/nexus/workspaces/workspace-nexus/nexus-gateway/worktrees/docs-backfill/packages/...` (this worktree). Re-do, explicitly pin the worktree path in the sub-agent prompt.

Scope (unchanged from original):
1. Add `TraceID string` field to `DiagEvent` Go struct (`packages/shared/core/metrics/registry/types.go`).
2. Migration `<unique-ts>_thing_diag_event_trace_id/migration.sql` adds `trace_id TEXT NULL` column + btree index `(thing_id, trace_id, occurred_at DESC)` on `thing_diag_event`.
3. Prisma schema mirrors.
4. `SlogSink` (`packages/shared/core/diag/slog_sink.go`) auto-extracts `trace_id` attr into the typed field. NB: `WithAttrs` is a no-op today — sub-agent flagged it as load-bearingly broken (silently drops `slog.With` attrs in production). Fix `WithAttrs` to a real clone-and-prepend.
5. Per-service slog wiring: at request-entry handlers in ai-gateway proxy, compliance-proxy forward, agent intercept — stamp `logger = logger.With("trace_id", traceID)`.
6. Hub consumer (`packages/nexus-hub/internal/observability/handler/diag/`) populates the new column.
7. Tests: SlogSink extraction, DiagEvent JSON round-trip, Hub consumer insert.

Migration timestamp: pick next after `20260610000000_admin_api_key_rotation`.

## PR-I — SIEM consolidation

User decision: Hub is canonical SIEM forwarder. Compliance-proxy, if it needs to ship to SIEM, only forwards events to MQ (no direct SIEM sink).

Scope:
1. Delete `packages/compliance-proxy/internal/siem/` package entirely (the local-fan-out Sink + spool + admin surface).
2. Compliance-proxy's audit producer already publishes `TrafficEventMessage` to `nexus.event.compliance` — verify that's still the case + that no compliance-proxy code path bypasses MQ to write SIEM directly.
3. Hub's `packages/nexus-hub/internal/traffic/siem/bridge.go` already polls `traffic_event` + `AdminAuditLog`; it should already pick up CP traffic since CP rows land in `traffic_event`. Verify the bridge's filter doesn't exclude `source='compliance-proxy'`.
4. Update `system_metadata` SIEM config to drop the per-service split (now only Hub-side config matters).
5. If admin UI had a "compliance-proxy SIEM" surface, fold it into the Hub-side SIEM admin page.
6. Tests: smoke that a CP-origin traffic event flows through MQ → Hub `traffic_event` → SIEM bridge → external sink.

Doc impact: D01 §11 SIEM bridge mention + new D11 (siem-bridge-architecture.md) when written.

## PR-J — audit pipeline hardening

Four sub-fixes, can be one coherent PR or split:

**J1 — Hub consumer DLQ** (`packages/nexus-hub/internal/jobs/consumer/traffic.go`):
- Today: non-22021 DB error → `nakAll` → broker redelivers forever until JetStream `MaxAge=6h` / `MaxBytes=8GiB` evicts.
- Fix: add redelivery count threshold (e.g. 5 attempts), after which ACK + write to a DLQ table (`traffic_event_dlq` with the raw message bytes + error + original timestamp).
- Add admin endpoint to inspect + retry DLQ rows.

**J2 — normalize backfill job**:
- Today: `insertNormalizedPayloads` partial failure logs a warning but the parent tx still commits. Raw bytes live in `traffic_event_payload`; the normalized sidecar is left NULL forever.
- Fix: build a job (under `packages/nexus-hub/internal/jobs/defs/`) that scans `traffic_event_normalized` for NULL rows, re-runs normalize against the raw bytes, fills in the sidecar. Cron-style or admin-triggered.

**J3 — coerceEmbeddingRow layer move**:
- Today: lives in `audit.Enqueue` (`packages/ai-gateway/internal/platform/audit/audit.go`) — fixes producer bugs at the audit boundary.
- Fix: move the check to the codec layer where the producer bug originates; audit boundary keeps only a log statement (not a value-coerce).

**J4 — audit.go split**:
- Today: `audit.go` is 1471 lines, owns 8+ enum types, the Record struct (~120 fields), Writer lifecycle, `recordToMessage` (~280 lines), `applyStorageAction`, `coerceEmbeddingRow`, more.
- Fix per CLAUDE.md directory-decomp binding: split into 6 files in same package:
  - `enums.go` — all 8 enum types
  - `record.go` — `Record` struct + per-field helpers
  - `writer.go` — `Writer` + buffering + retry + shutdown
  - `message.go` — `recordToMessage`
  - `storage_action.go` — `applyStorageAction`
  - `coerce.go` — moved-up consumer for coerceEmbeddingRow (or deleted if J3 lands first)

## PR-H — Hub observability directory reorg

Run **last** — pure file moves, will conflict with anything else touching Hub files. Move:

- `packages/nexus-hub/internal/observability/opsmetrics/` (already there — keep)
- `packages/nexus-hub/internal/observability/handler/diag/` (already there — keep)
- `packages/nexus-hub/internal/jobs/consumer/traffic.go` + `admin_audit.go` + `message.go` → `packages/nexus-hub/internal/observability/consumer/`
- `packages/nexus-hub/internal/traffic/siem/` → `packages/nexus-hub/internal/observability/siem/`

Update every import. `go vet ./...` clean. Tests still pass.

Doc impact: D01 §2 anchor packages table needs the new paths.

## Doc update workflow

After PR-G/I/J/H land:
1. Update each of D01-D04 to describe fixed end-state (delete "known issue" callouts; the issues are gone).
2. Update B04 to describe `engaged` semantic (after PR-B kill-switch — separate handoff).
3. Update B06 to describe wired AI-Guard reconcile (after PR-C — separate handoff).
4. Run `/doc-review` skill on each updated doc.
5. Commit each doc separately.

## Pre-existing handoffs

- `docs/handoffs/quota-killswitch-aiguard/HANDOFF.md` — PR-B (kill-switch `enabled`→`engaged`) + PR-C (AI-Guard reconcile producer). Still TODO.

## What landed today

Commits on `feature/docs-backfill` from this session, in order:
- `f13763acf` — PR-A VK budgetLimitUsd cleanup (the original PR)
- `fc8be8aeb` — PR-D fast-track bundle (6 small fixes)
- (PR-E commit) — passthrough + device-assignment audit emits
- `b65720586` — PR-K AlertSeverity typed enum
- `44412cb77` — PR-F api-key.rotate + multi-key + status lifecycle

Plus the earlier D01-D04 / B05 / C01 docs commits and skill / memory updates.

## Cross-cutting reminders (binding)

- All new docs via `/doc-write` skill; all reviews via `/doc-review` skill. Per-claim verdicts; CLEAN before commit.
- No archaeology, no dates, no Epic/SDD/bug refs, no line numbers in doc body, `## References` section at end.
- Sub-agents MUST inline the supreme-constitution rules + the worktree absolute path (not the main repo path — the PR-G sub-agent edited the wrong directory, lost an hour of work).
- AIGW smoke after any change under `packages/ai-gateway/**`.
- `--no-verify` only for documented coverage drops; track follow-ups for each.
