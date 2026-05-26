# E32 — Story 1: Timezone correctness, UTC end-to-end

## Context

Today the Nexus Gateway stack mixes timezones at every layer:

- `traffic_event.timestamp` is `timestamp WITHOUT TIME ZONE` (134 such columns across the schema)
- `ai-gateway` and `compliance-proxy` write `time.Now()` (host local TZ, +08 in dev) instead of `time.Now().UTC()`
- pgx default session TZ is UTC, but the postgres docker image was started with `timezone=Asia/Shanghai` server config — those two cancel out non-obviously
- The Hub `rollup_5m` job uses `time.Now().UTC()` for windowing, so events written with local-wall-clock timestamps look "in the future" by 8 hours and never aggregate
- The Live Traffic UI shows `formatDateTime` results without any TZ label, so a viewer in Singapore can't tell whether `18:42` is their local time, the server's, or the originating customer's
- There is no per-user or per-org TZ field, so business-rule semantics like "yesterday's spend" or "reset quota at midnight" have no defined TZ

This story rebuilds the timezone story top-to-bottom under the iron rule:

> **All timestamps stored, transmitted, computed, and compared as UTC. Conversion to local time happens only at the display boundary (UI rendering).**

Pre-GA, no backwards compatibility. Existing dev data is either truncated (event/audit tables) or reinterpreted as having been written in `Asia/Shanghai` (config tables — Project / Organization / NexusUser etc., which were created via seed.ts or admin UI from a +08 dev machine).

## User Story

**As a** Nexus Gateway operator running multi-region deployments,
**I want** every timestamp in the stack to be unambiguously UTC at rest, in transit, and in computation, with explicit timezone display in the UI,
**so that** analytics rollups never silently lose data to TZ skew, business rules ("yesterday", "midnight reset") evaluate consistently per-org, and an admin in any timezone can read/write timestamps without computing offsets in their head.

## Tasks

### 1. Phase 1 — Stop the bleed (immediate)

**1.1 Docker compose all UTC.** `docker-compose.yml` set `TZ: UTC`, `PGTZ: UTC` on every service; postgres `command: ["postgres", "-c", "timezone=UTC", "-c", "log_timezone=UTC"]`.

**1.2 Go writers explicit UTC.** Every `time.Now()` that flows into a persisted/transmitted/compared timestamp must call `.UTC()`. Concrete sites identified:

- `packages/ai-gateway/internal/handler/proxy.go:151` — `start := time.Now()`
- `packages/compliance-proxy/internal/compliance/emitter.go:177, 239, 264`
- Any others surfaced by Phase 3's grep

Sites that may keep bare `time.Now()` (monotonic-clock / deadline use): `SetReadDeadline`, `SetWriteDeadline`, `WithTimeout`, `Sleep`, `.Add(...)` for ticker math.

**1.3 Truncate dirty event data.** Pre-GA dev data in `traffic_event`, `runtime_audit`, etc. is mixed local-wall-clock + about-to-be-fixed-UTC; not worth converting. `TRUNCATE` before Phase 2 migration runs.

### 2. Phase 2 — Schema migration to `timestamptz`

**2.1 Audit Prisma schema.** All 134 `DateTime` columns get `@db.Timestamptz(3)`. Every model. No exceptions.

**2.2 Single migration.** `tools/db-migrate/migrations/<ts>_timestamps_to_timestamptz/migration.sql`:

```sql
-- One-time pre-GA conversion. Existing config rows came from seed.ts
-- + admin UI running on a +08 dev machine, so we reinterpret as
-- Asia/Shanghai to recover the correct UTC instant. Event/audit
-- tables are truncated since they have mixed-TZ dirty rows post-bug.

-- Truncate event tables first (pre-GA dev data, no value).
TRUNCATE TABLE traffic_event, runtime_audit, /* … all event tables */ RESTART IDENTITY CASCADE;

-- Convert remaining config-table columns
ALTER TABLE "Organization" ALTER COLUMN "createdAt" TYPE TIMESTAMPTZ(3) USING "createdAt" AT TIME ZONE 'Asia/Shanghai';
ALTER TABLE "Organization" ALTER COLUMN "updatedAt" TYPE TIMESTAMPTZ(3) USING "updatedAt" AT TIME ZONE 'Asia/Shanghai';
-- ... repeat for every column
```

**2.3 Verify.** `\d traffic_event` shows `timestamp with time zone`. `SELECT NOW()` returns `+00`.

### 3. Phase 3 — `shared/timeutil` package + CI lint

**3.1 New package** `packages/shared/timeutil/timeutil.go`:

```go
package timeutil

import "time"

// Now returns the current UTC instant. Use this everywhere a timestamp
// will be persisted, transmitted, compared, or scheduled. Bare
// time.Now() is reserved for monotonic deadlines (SetReadDeadline etc).
func Now() time.Time { return time.Now().UTC() }

// InOrg converts a UTC instant into the wall-clock representation in
// the organization's configured TZ. Used for business-rule
// computations (start-of-day, business hours).
func InOrg(t time.Time, orgTZ string) (time.Time, error) {
    loc, err := time.LoadLocation(orgTZ)
    if err != nil { return t, err }
    return t.In(loc), nil
}

// StartOfDayUTC returns the UTC instant corresponding to midnight
// of the day enclosing `now` in the org's TZ.
func StartOfDayUTC(now time.Time, orgTZ string) (time.Time, error) {
    inOrg, err := InOrg(now, orgTZ)
    if err != nil { return now, err }
    return time.Date(inOrg.Year(), inOrg.Month(), inOrg.Day(), 0, 0, 0, 0, inOrg.Location()).UTC(), nil
}
```

**3.2 Replace bare `time.Now()`** in persistence-relevant directories: `internal/audit/`, `internal/store/`, `internal/handler/`, `internal/jobs/`, `cmd/`. Allowlist for monotonic uses (deadline, sleep, ticker).

**3.3 CI lint** `scripts/check-timezone-correctness.sh`:

```bash
#!/usr/bin/env bash
set -euo pipefail
fail=0

# (1) bare time.Now() in persistence paths — reject
hits=$(git grep -nE 'time\.Now\(\)[^.]' \
  packages/{ai-gateway,control-plane,nexus-hub,compliance-proxy,shared,agent}/{cmd,internal}/{audit,store,handler,jobs}/ \
  2>/dev/null \
  | grep -v 'SetReadDeadline\|SetWriteDeadline\|WithTimeout\|WithDeadline\|Sleep\|\.Add(\|Sub(\|Until(' \
  || true)
if [ -n "$hits" ]; then
  echo "FAIL: bare time.Now() in persistence path; use timeutil.Now()"
  echo "$hits"; fail=1
fi

# (2) `timestamp` (no tz) in Prisma migrations — reject
if git grep -E '\"[A-Za-z]+\" TIMESTAMP\(' tools/db-migrate/migrations/*/migration.sql \
  | grep -v 'TIMESTAMP WITH TIME ZONE\|TIMESTAMPTZ' >/dev/null; then
  echo "FAIL: timestamp without tz in migration"
  fail=1
fi

exit $fail
```

Wired into `package.json` `lint:tz` script and the existing CI workflow.

### 4. Phase 4 — UI display layer

**4.1 New helper** `packages/control-plane-ui/src/lib/time.ts`:

```ts
import { format, formatInTimeZone, fromZonedTime } from 'date-fns-tz';

export function browserTZ(): string {
  return Intl.DateTimeFormat().resolvedOptions().timeZone;
}

export function formatInUserTZ(
  isoString: string,
  userTZ: string = browserTZ(),
  pattern = 'yyyy-MM-dd HH:mm:ss zzz',
): string {
  return formatInTimeZone(new Date(isoString), userTZ, pattern);
}

/** Convert a <input type="datetime-local"> string in `userTZ` to a
 *  UTC RFC3339 string for transmission to the backend. */
export function localInputToUTC(localStr: string, userTZ: string = browserTZ()): string {
  return fromZonedTime(localStr, userTZ).toISOString();
}

/** Convert a <input type="date"> ("YYYY-MM-DD") to end-of-day UTC RFC3339
 *  in userTZ — semantically "valid through this calendar day". */
export function endOfDayUTC(dateStr: string, userTZ: string = browserTZ()): string {
  return fromZonedTime(`${dateStr}T23:59:59.999`, userTZ).toISOString();
}

/** Relative time for very recent events ("3 minutes ago"). Falls back
 *  to absolute formatInUserTZ when older than 1 hour. */
export function formatRelativeOrAbsolute(isoString: string, userTZ?: string): string { /* … */ }
```

Add `date-fns-tz` to UI package.json.

**4.2 Replace call sites.** `git grep formatDateTime\\|toLocaleString` → swap every one for `formatInUserTZ(value, userPrefTZ)`. Includes (incomplete list):
- `liveTrafficFilters.ts:formatDateTime` (chip lines)
- `trafficAuditDrawer.tsx`
- `TrafficTab.tsx` columns
- AdminAuditLog views, VK detail, Provider detail, Job views, etc.

**4.3 TZ badge** rendered next to every timestamp display: `2026-04-26 18:42:13 GMT+8`. The `zzz` pattern in the format string handles this.

**4.4 Date-input semantics.** All `<input type="date">` (e.g. VK `expiresAt`) use `endOfDayUTC` at submit. All `<input type="datetime-local">` use `localInputToUTC`. The existing `toRFC3339WithOffset` is replaced.

### 5. Phase 5 — User TZ preference

**5.1 Schema** `NexusUser.preferredTimezone String?` — IANA TZ name (`Asia/Shanghai` etc.). Nullable; null means "use browser TZ at display time".

**5.2 API.** `GET /api/my/profile` returns `preferredTimezone`. `PUT /api/my/profile` accepts it (validated against `time.LoadLocation` server-side; reject unknown).

**5.3 Settings page.** `/account?tab=display` → Timezone dropdown (IANA names grouped by region, plus "Browser default" sentinel). Save → invalidates query cache so all timestamps re-render.

**5.4 Header chip.** Top-right shows `Asia/Shanghai` (or current effective TZ); click → navigates to settings.

**5.5 Provider** of effective TZ across UI: `useUserTZ()` hook — reads from current user profile, falls back to browser. Single source of truth; every `formatInUserTZ` call uses it.

### 6. Phase 6 — Org TZ business semantics

**6.1 Schema** `Organization.timezone String @default("UTC")` — IANA TZ name.

**6.2 Admin API.** `PUT /api/admin/organizations/:id` accepts `timezone`; same `time.LoadLocation` validation.

**6.3 Quota reset.** Wherever quota counters are reset on a daily/weekly/monthly cadence, derive the next reset boundary from `timeutil.StartOfDayUTC(now, org.Timezone)` rather than UTC midnight.

**6.4 Analytics "yesterday".** `GET /api/admin/analytics/...` accepts `?tz=Asia/Shanghai` query parameter (defaults to org's TZ). Date-bucket SQL uses `WHERE timestamp >= $start AND timestamp < $end` where `$start`/`$end` are computed in the requested TZ.

**6.5 1d rollup re-aggregation.** `metric_rollup_1d` buckets stay UTC. Admin API "daily totals by org TZ" re-aggregates from `metric_rollup_1h` at query time using `(timestamp AT TIME ZONE org.Timezone)::date` grouping.

**6.6 Audit `originTz` column.** Add `traffic_event.origin_tz TEXT` (nullable) — populated by `audit.Record.ApplyVKMeta` from `vkMeta.OrganizationTimezone` at the time of the event, for compliance reports that need jurisdiction-local time. Wired through MQ envelope + hub consumer INSERT.

**6.7 Monthly quota window.** `monthlyQuotaKey(now, orgTZ)` formats the bucket as `YYYY-MM` in the org's calendar, so APAC tenants roll over at their local midnight on the 1st rather than UTC's. Falls back to UTC for VKs with no org binding.

**6.8 Analytics `?tz=` query parameter.** `parseTZParam(c)` validates an IANA name and returns the resolved location; `rollupDefaultTimeRange(start, end, loc)` aligns the default 24h window to the most recent calendar-day boundary in that location when start/end aren't explicitly supplied. Plumbed through every `tryRollup*` callsite via `tzLoc(c)`.

### 7. Phase 7 — Documentation

**7.1** `docs/developers/workflow/timezone.md`:
- Iron rule front-and-center
- Layered design diagram
- "Don't add `.UTC()` to deadline calls" guardrail
- DST-correct cron pattern (`time.LoadLocation` + IANA names)
- Dev psql alias (`PGTZ=Asia/Shanghai psql ...`) — local convenience, no project pollution
- CI lint behavior
- Migration recipe for re-introducing timestamp-handling code

**7.2** Update `CLAUDE.md` Tech Stack section to mention `timestamptz` schema convention.

## Acceptance Criteria

1. All 134 `DateTime` columns in `tools/db-migrate/schema.prisma` carry `@db.Timestamptz(3)` and the produced SQL emits `TIMESTAMP WITH TIME ZONE`.
2. `docker exec ... psql -c "SHOW timezone;"` returns `UTC` even with no client override.
3. After sending a non-stream `POST /v1/chat/completions`, the new `traffic_event` row has its `timestamp` column show `+00` and `metric_rollup_5m` advances within 1 minute (rollup window).
4. CI lint `scripts/check-timezone-correctness.sh` passes on the merge target and fails on a deliberately-introduced `time.Now()` without `.UTC()` in a persistence file.
5. Live Traffic UI: every timestamp column shows a TZ designator (e.g. `GMT+8`) and the header chip displays the active TZ name. Switching the user TZ in Settings re-renders all visible timestamps without a page reload.
6. `PUT /api/admin/organizations/:id` accepts `{"timezone":"America/New_York"}` and rejects garbage. The next quota-reset boundary computed for that org lands at NYC midnight, not UTC midnight.
7. `GET /api/admin/analytics/cost?tz=Europe/Berlin&period=daily` returns one row per Berlin-local day; same call with `tz=Asia/Tokyo` returns differently-sliced rows.
8. `go test ./... -race -count=1` passes for every changed package; `npm test` passes for `control-plane-ui`; `npm run check:i18n` passes.
9. `docs/developers/workflow/timezone.md` exists and is referenced from `CLAUDE.md`.

## Out of Scope

- Cron-expression-based scheduling with TZ — `grep -r cron packages/` confirms zero cron usage in the Go stack today; the doc carries the IANA-name guidance ready for when cron is introduced.
- Localized date formats (DD/MM/YYYY vs MM/DD/YYYY) — uses `Intl` defaults, not in this story.
- Server-rendered email/PDF reports — those go through the same UTC instants but rendering is a separate surface.
- Data backfill for historical data created in non-`Asia/Shanghai` dev environments — Pre-GA, the DB is reset + re-seeded to a clean UTC state at the start of E32-S1.
