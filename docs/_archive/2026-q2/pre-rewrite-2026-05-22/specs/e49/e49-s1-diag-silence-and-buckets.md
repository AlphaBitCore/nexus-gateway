# E49 S1 — Diag-Event Silence Registry + Group Buckets

**Status:** Shipped — 2026-05-14 (prod commit `0d228aa7` + UX iterations)
**Story:** s1 of E49

## User Story

As a **Nexus admin operator** triaging `/infrastructure/errors`, I want to **ack known-noise issues with a TTL** so my Issues list stays focused on what's new or escalating, instead of being dominated by the same handful of warnings that repeat dozens of times a day.

## Architectural Snapshot

Two surfaces, both backed by the existing CP-direct `/api/admin/diag-events/*` path (CP queries `thing_diag_event` directly via pgx; no Hub round-trip):

```
 Browser
 ┌─────────────────────────────────────────────┐
 │  InfraRecentErrorsPage                      │
 │    ▷ hero tiles (computed from /groups)     │
 │    ▷ Issue list with inline sparklines      │
 │    ▷ right-side drawer (group detail)       │
 │    ▷ event popup (atop drawer)              │
 │    ▷ Manage Silences popup                  │
 └─────────────┬─────────────────┬─────────────┘
               │                 │
        GET /groups        GET/POST/DELETE
        (buckets+silenced)  /diag-silences
               │                 │
 ┌─────────────▼─────────────────▼─────────────┐
 │  CP — internal/handler/                     │
 │    diagevents.go: ListDiagGroups            │
 │    diag_silences.go: list / create / delete │
 └─────────────┬─────────────────┬─────────────┘
               │ pgx             │ pgx
 ┌─────────────▼─────────────────▼─────────────┐
 │  Postgres (nexus_gateway)                   │
 │    thing_diag_event (existing, E33)         │
 │    diag_silence (E49 new)                   │
 └─────────────────────────────────────────────┘
```

No Hub changes, no MQ changes. CP gains one table + one handler file; the existing diag-groups query gains two JOINs.

## Tasks

### T1 — Migration: `diag_silence` table

**Files:** `tools/db-migrate/migrations/20260519000000_e49_diag_silence/migration.sql`, `tools/db-migrate/schema.prisma`.

Schema:

```sql
CREATE TABLE "diag_silence" (
  "id"           UUID PRIMARY KEY,
  "message_hash" TEXT NOT NULL,
  "level"        TEXT NOT NULL,
  "silenced_by"  TEXT NOT NULL,
  "silenced_at"  TIMESTAMPTZ(3) NOT NULL DEFAULT CURRENT_TIMESTAMP,
  "expires_at"   TIMESTAMPTZ(3),
  "reason"       TEXT
);
CREATE INDEX "diag_silence_lookup_idx"
  ON "diag_silence" ("message_hash", "level");
CREATE INDEX "diag_silence_active_idx"
  ON "diag_silence" ("expires_at")
  WHERE "expires_at" IS NULL OR "expires_at" > '2000-01-01'::timestamptz;
```

The partial index keeps active-silence scans proportional to live rows, not the historic pile.

### T2 — Silences CRUD store + handler

**Files:** `packages/control-plane/internal/store/diag_silence_store.go`, `packages/control-plane/internal/handler/diag_silences.go`.

- `CreateDiagSilence(p)` — INSERT with `actor.UserID` as `silenced_by`. TTL=0 → `expires_at` NULL.
- `ListActiveDiagSilences()` — `WHERE expires_at IS NULL OR expires_at > NOW()`, ordered `silenced_at DESC`, capped at 500.
- `GetDiagSilence(id)` — sentinel `ErrSilenceNotFound` on miss; used for audit before-state.
- `DeleteDiagSilence(id)` — `DELETE`; sentinel-not-found semantics.
- Routes registered in `RegisterDiagSilencesRoutes(g, iamMW)`:
  - `GET /diag-silences` → `observability:read`
  - `POST /diag-silences` → `observability:write`
  - `DELETE /diag-silences/:id` → `observability:write`
- Server-side validation: `level ∈ {debug, info, warn, error, fatal}`, `messageHash` non-empty, `ttlSeconds ∈ [0, 2592000]` (0 = permanent, max 30d), `reason ≤ 500 chars`.
- Audit via `audit.EntryFor(c, iam.ResourceObservability, iam.VerbWrite)` with before/after state.

### T3 — `/groups` endpoint enriched with `buckets` + `silenced`

**Files:** `packages/control-plane/internal/store/opsmetrics_store.go` (`DiagGroup` + `ListDiagGroups`).

Two queries, merged in Go:

1. **Group facts** (groupby `message_hash`, top 100): existing aggregates + an EXISTS subquery that resolves `silenced = EXISTS (… diag_silence WHERE message_hash = e.message_hash AND level = MAX(e.level) AND active)`.
2. **Bucket counts** (per-hash, scoped to the same hash set via `message_hash = ANY($4)`): `date_bin('5 minutes', occurred_at, '2000-01-01')` → `(message_hash, bucket_ts, count)`. Merged onto `out[i].Buckets` after the first query closes.

PG14+ `date_bin` chosen so the bucket boundary is deterministic and aligned across all groups (no jitter from the per-group MIN-time anchor that `date_trunc` would imply).

### T4 — Frontend `InfraRecentErrorsPage` rewrite

**Files:** `packages/control-plane-ui/src/pages/infrastructure/InfraRecentErrorsPage.tsx`, `.module.css`, `.test.tsx`, `src/api/services/diagevents.ts`, and i18n locale files (en/zh/es × src + public).

UX state machine for the detail surface:

```
                   click Issue row
                          │
                          ▼
  ┌──────────────────────────────────────────────┐
  │ Drawer (xl, right) — Group context           │
  │   group meta · sparkline · Silence actions   │
  │   Affected events table (10/page + Load more)│
  └──────────┬────────────────────┬──────────────┘
             │ click event row    │ close drawer
             ▼                    ▼
  ┌──────────────────────┐    (drawer + event reset)
  │ Centered popup —     │
  │ Event detail         │
  │   meta · attrs · stack│
  └──────────┬───────────┘
             │ close popup
             ▼
  (drawer stays at the same scrolled position)
```

`useEffect` re-syncs `detailGroup` against `groups.data` whenever the latter refetches, so silencing inside the drawer flips the `silenced` flag without a full close/reopen cycle.

Page sections in order: PageHeader → hero tiles row → fleet sparkline card → Filter panel (collapsed) → Issues list.

### T5 — Visibility affordances

- "🔕 Silences (N)" pill in the Issues header (warning-tone bg, `flex-shrink: 0; white-space: nowrap` so it never wraps). Only renders when `N > 0`.
- Silence buttons (`Silence 1h` / `Silence 24h`) use `variant="secondary"` + 🔕 prefix to differentiate from the lower-prominence Unsilence action.
- Affected-events table columns: Time · Level (badge) · Source · Node · Event type · Repeat ×N (only when >1) · drill arrow.

## Acceptance Criteria

1. Creating a silence on a known-noise issue makes its row disappear from the default list within one `groups.refetch` cycle.
2. The "🔕 Silences (N)" counter in the Issues header reflects every active silence and stays in sync after create/delete.
3. Opening the Manage Silences popup shows every active silence with operator (`silencedBy`), expires-at (or "permanent"), and reason.
4. An expired silence stops affecting the `silenced` flag at the exact `expires_at` instant — verified by setting a 60s TTL and observing the row reappear after the next refetch past expiry.
5. Clicking an event row inside the drawer opens a centered popup on top, NOT a stacked second drawer.
6. Closing the event popup returns the user to the drawer at the same scrolled position.
7. All three locales (en/zh/es) parse without missing-key fallback to bare strings.

## Notes on Deferred Work

- **Retention / cleanup of expired silences.** Not implemented. Lazy-expiry at query time is sufficient for expected cardinality (<1000 lifetime). Future epic can add a daily `DELETE FROM diag_silence WHERE expires_at < NOW() - INTERVAL '90 days'`.
- **Expiry push notification.** Not implemented. Operator currently rediscovers the noise via the next page refresh after expiry.
- **Per-node silence dimension.** Not implemented. Silences are `(messageHash, level)` only; per-node was rejected as over-granular for the current operator workflow.
