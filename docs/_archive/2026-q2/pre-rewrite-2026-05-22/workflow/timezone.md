# Timezone policy

> **Iron rule.** All timestamps in the Nexus Gateway stack are stored,
> transmitted, computed, and compared as UTC instants. Conversion to a
> human-readable wall-clock happens **only at the display boundary
> (UI rendering)**. There are no exceptions.

## Why this exists

Before E32-S1 the stack mixed timezones at every layer:
`traffic_event.timestamp` was tz-less, `time.Now()` ran on the host
TZ (+08 in dev), pgx's session was UTC, the postgres container was
configured `Asia/Shanghai`, and the rollup job compared events
written in local wall-clock against `NOW()` in UTC. Net effect: 8
hours of fresh traffic looked "in the future" to the rollup and never
aggregated. The Live Traffic UI showed `18:42` with no TZ designator,
so a viewer in Singapore couldn't tell whose clock that was.

This document is the project policy that prevents it from happening
again.

## Layered design

```
┌───────────────────────────────────────────────────────────┐
│  USER (browser)                                           │
│    Settings → Display TZ:    Asia/Shanghai (or UTC, …)   │
│    Display:                  "2026-04-26 18:42 GMT+8"    │
└─────────────────────┬─────────────────────────────────────┘
                      │  RFC3339 over HTTPS — always UTC
                      │  e.g. "2026-04-26T10:42:13.477Z"
┌─────────────────────▼─────────────────────────────────────┐
│  GO SERVICES (containers TZ=UTC)                          │
│    time.Now().UTC()          → UTC time.Time              │
│    Cross-service compare     → UTC equality               │
│    Business-rule "yesterday" → orgTZ-aware AT TIME ZONE   │
│                                in SQL, or LoadLocation    │
│                                + In() at the boundary     │
└─────────────────────┬─────────────────────────────────────┘
                      │
┌─────────────────────▼─────────────────────────────────────┐
│  POSTGRES (container TZ=UTC, server timezone=UTC)         │
│    Every time column → timestamptz (= UTC instant)        │
│    Indexes / comparisons are tz-correct by construction   │
└───────────────────────────────────────────────────────────┘

ORG layer:   Organization.timezone  (IANA name, default "UTC")
             — drives "midnight reset" / "yesterday" / "business hours"
USER layer:  NexusUser.preferredTimezone  (IANA name, nullable)
             — drives display formatting; falls back to browser TZ
```

## Concrete rules

### 1. Postgres
- All timestamp columns: `DateTime @db.Timestamptz(3)` in
  `tools/db-migrate/schema.prisma`. Plain `DateTime` (without the
  `@db.` attribute) is rejected by `npm run check:tz`.
- `docker-compose.yml` runs postgres with `TZ=UTC`,
  `PGTZ=UTC`, and `command: ["postgres", "-c", "timezone=UTC"]` —
  cosmetic for psql output, since `timestamptz` storage is UTC
  regardless of session TZ.

### 2. Go services
- **Always use `time.Now().UTC()`** for any timestamp that will be
  persisted, transmitted, compared across services, audited, or
  emitted to a metric/event. The `.UTC()` call strips the local
  monotonic-clock location wrapper but preserves the underlying
  monotonic reading, so duration arithmetic (`.Sub`, `time.Since`)
  still works correctly.
- **Bare `time.Now()` is allowed only** for monotonic-clock APIs
  whose result never escapes the function: `SetReadDeadline`,
  `SetWriteDeadline`, `WithTimeout`, `WithDeadline`, `time.Sleep`,
  `time.Since`, `time.Until`, `.Add(...)`, `.Sub(...)`,
  `.AddDate(...)`. These are exempt because the location attached
  to the returned `time.Time` is irrelevant to the operation.
- **Business semantics in an org's calendar** (e.g. "start of today's
  quota window", "yesterday in this org's timezone") are computed at
  the boundary with `time.LoadLocation(org.Timezone)` followed by
  `now.In(loc)`, or — preferred for aggregates — directly in SQL
  via `(traffic_event.timestamp AT TIME ZONE 'Asia/Shanghai')::date`.
  There is no central helper package; the operation is a one-liner
  and lives next to the business rule that needs it.

```go
import "time"

// Persist — every audit / emit / write site looks like this
rec.Timestamp = time.Now().UTC()

// Daily quota window in an org's calendar
loc, _ := time.LoadLocation(org.Timezone)         // IANA name
nowLocal := time.Now().In(loc)
startLocal := time.Date(nowLocal.Year(), nowLocal.Month(), nowLocal.Day(),
    0, 0, 0, 0, loc)
startUTC := startLocal.UTC()                       // store/compare as UTC
```

### 3. JSON / MQ envelopes
Standard library `json.Marshal` of a UTC `time.Time` emits RFC3339
with the `Z` designator. Receivers (`time.Parse(time.RFC3339, …)` and
JS `new Date(s)`) accept this without any custom parsing. Don't
invent another wire format.

### 4. Frontend (control-plane-ui)
- Every timestamp displayed to the user goes through
  `formatDate` / `formatDateTime` / `formatTime` from
  `@/lib/format`. These render in the user's display TZ
  (`getDisplayTZ()`) and **always include a TZ designator**
  (`GMT+8`).
- Date pickers (`<input type="date">`, `<input type="datetime-local">`)
  serialize via `endOfDayUTC(date)` or `localInputToUTC(local)`.
  Never send a tz-less string to the backend.
- The user's display TZ is loaded from the
  `/api/admin/me.preferredTimezone` field at session bootstrap and
  applied via `setDisplayTZ(...)`. Falls back to
  `Intl.DateTimeFormat().resolvedOptions().timeZone` (browser).

### 5. Org TZ vs User TZ — keep them separate
- `NexusUser.preferredTimezone` controls **display only**. Alice in
  Shanghai can choose to "see things in UTC" without affecting
  anyone else.
- `Organization.timezone` controls **business semantics**: what
  counts as "midnight" for daily quota reset, "yesterday" for the
  cost dashboard, "business hours" for tiered rate limits. Default
  `UTC` = no special handling.

### 6. Scheduled jobs
Audited at E32-S1: the Hub scheduler is interval-based and there is
zero cron-expression usage anywhere in `packages/`. If/when cron is
introduced (e.g. for tenant-facing "weekly cost report at 09:00 in
my timezone"), it MUST go through `cron.WithLocation(orgTZ)` with an
IANA TZ name (NOT a fixed offset like `+08:00`) so DST transitions
are handled automatically. The recommended library is
`github.com/robfig/cron/v3`.

## Local dev convenience

The DB session is UTC by design, so `SELECT NOW()` returns
`2026-04-26 02:46+00`. If you'd rather see your local wall-clock at
the psql prompt, set the **client** TZ — this is purely a display
override, never affects storage or comparisons:

```bash
# zsh / bash
alias pg='PGTZ=Asia/Shanghai psql ...'

# or per-session
PGTZ=Asia/Shanghai psql -h localhost -p 55532 -U postgres -d nexus_gateway
```

For ad-hoc queries that should display in a specific TZ:
```sql
SELECT timestamp AT TIME ZONE 'Asia/Shanghai' AS local_ts
FROM traffic_event ORDER BY timestamp DESC LIMIT 5;
```

## CI lint (`npm run check:tz`)

`scripts/check-timezone-correctness.sh` enforces the policy at
build time:

1. **Bare `time.Now()` in persistence paths** —
   `internal/audit`, `internal/store`, `internal/handler`,
   `internal/jobs`, etc. — fails the build. Required fix:
   replace with `time.Now().UTC()`. Allowlist (per
   `scripts/check-timezone-correctness.sh`): monotonic-clock callers
   matched by the regex
   `SetReadDeadline|SetWriteDeadline|WithTimeout|WithDeadline|time.Sleep|.Add(|.Sub(|time.Until|.AddDate(|time.Since(`,
   `_test.go` files, and any line carrying the
   `// timeutil-skip` trailing marker.
2. **Tz-less `DateTime` in `schema.prisma`** — every `DateTime`
   field must carry `@db.Timestamptz(3)`.

Run locally before opening a PR:
```bash
npm run check:tz
```

## DST gotcha — don't store offsets, store IANA names

DST transitions move the wall-clock by ±1 hour twice a year. A cron
expressed as `+02:00` silently drifts every spring/fall; a cron
expressed as `Europe/Berlin` follows the local rules. Same applies
to `Organization.timezone` and `NexusUser.preferredTimezone`: these
fields hold IANA names like `Europe/Berlin`, never offsets.

The Go standard library's `time.LoadLocation` resolves IANA names via
the embedded `tzdata`. The Postgres `timestamptz AT TIME ZONE 'X'`
construct accepts the same names.

## Common pitfalls (and how to avoid them)

| Symptom | Likely cause | Fix |
|---------|--------------|-----|
| `metric_rollup_5m` not advancing past a recent bucket | Writer dropping local wall-clock into a tz-less column | Use `time.Now().UTC()` (E32-S1 fixed this) |
| UI shows `18:42` with no TZ — viewer can't tell whose clock | Direct `Date.toLocaleString()` instead of `formatDateTime` | Route through `@/lib/format` |
| `expiresAt` validation fails on `2026-05-02` body | Date-only string sent to a `time.Time` field | Pass through `endOfDayUTC(dateStr)` before submit |
| Daily quota resets at the "wrong" time for an APAC tenant | Quota bucket keyed off UTC midnight | Org TZ wiring (E32-S1 Phase 6 follow-up) |
| Cron job runs at 8AM Berlin local in winter, 9AM in summer | Offset-based cron, not TZ-aware | Use `cron.WithLocation(loadLocation("Europe/Berlin"))` |

## Migration notes (one-time, pre-GA)

The `timestamps_to_timestamptz` conversion (now folded into the
`00000000000000_baseline_2026_05_13` baseline; the original
`20260426025955_timestamps_to_timestamptz` folder was collapsed in the
2026-05-13 baseline reset) converted all 134 `DateTime` columns from
`timestamp` to `timestamptz`. Pre-GA
event tables (`traffic_event`, `metric_rollup_*`, `AdminAuditLog`,
`config_change_event`, `rollup_watermark`) were truncated; rollups
regenerate themselves on the next scheduler tick. Config tables
(`Organization`, `NexusUser`, `Project`, `Provider`, `Model`, etc.)
kept their existing rows; their `createdAt` / `updatedAt` displays
may be 8 hours off the original wall-clock (read as UTC instead of
Asia/Shanghai), which is acceptable since those columns are
display-only metadata.

Production deployments born after E32 ship are fully UTC-clean from
day zero.
