# Timezone

One invariant runs through the whole stack: **timestamps are stored and
transmitted as absolute UTC instants, and converted to a human-local timezone
only at the display edge.** There is exactly one conversion boundary — the UI —
so every value in the database, on the wire, and in Go memory is unambiguous
without carrying a zone alongside it.

## 1. Database

Every `DateTime` column across `tools/db-migrate/schema/` carries
`@db.Timestamptz(3)` — a millisecond-precision, timezone-aware PostgreSQL
`timestamptz`. The two ubiquitous audit columns are stamped by the database
itself rather than by application code:

```prisma
createdAt DateTime @default(now())  @db.Timestamptz(3)
updatedAt DateTime @updatedAt        @db.Timestamptz(3)
```

`@default(now())` lets PostgreSQL fill the create time (its `now()` is
timezone-aware), and `@updatedAt` lets Prisma maintain the modify time. A new
`DateTime` field that omits `@db.Timestamptz(3)` is a build failure (see §4).

## 2. Go services

Anywhere a service computes a "now" that will be persisted, transmitted, or put
into an audit record, it uses `time.Now().UTC()` — never a bare `time.Now()`,
which carries the host's local zone and a monotonic-clock reading. The `.UTC()`
call normalizes the wall time to UTC, so the value that lands in the database
matches the column's `timestamptz` semantics.

Bare `time.Now()` remains correct — and is intentionally allowed — when the
value is consumed by a monotonic-clock API and never stored: connection
deadlines (`SetReadDeadline`, `WithTimeout`, `WithDeadline`), sleeps
(`time.Sleep`), and duration math (`.Add`, `.Sub`, `.Since`, `.Until`,
`.AddDate`). Those call-sites want the monotonic reading and never leak a local
wall time anywhere durable.

On the wire, services emit and parse RFC 3339. Outbound `time.Time` values
marshal to RFC 3339 strings, and because they are UTC they carry the `Z`
designator. Inbound timestamp query parameters are parsed by
`parseRFC3339Flexible` in `packages/control-plane/internal/handler/helpers.go`,
which accepts both `RFC3339Nano` and `RFC3339`.

## 3. The display edge

`packages/control-plane-ui/src/lib/format.ts` is the **only** place local
conversion happens — the backend hands the UI absolute UTC instants, and this
layer turns them into a viewer's local clock.

The display timezone is the user's preferred IANA zone, resolved by
`getDisplayTZ()`. It defaults to the browser's zone (`browserTZ()`, from
`Intl.DateTimeFormat().resolvedOptions().timeZone`) and is overridden by
`setDisplayTZ()` when the user-profile bootstrap supplies a preference; an empty
preference falls back to the browser zone.

The render helpers — `formatDate`, `formatDateTime`, `formatTime` — format in
the display zone (or an explicit `userTZ` argument) and always include a zone
designator (`timeZoneName: 'short'`, e.g. `Apr 26, 2026, 2:30 PM GMT+8`) so a
viewer is never left guessing whose clock a value belongs to.
`formatRelativeTime` (`3m ago`, `2h ago`, `yesterday`) is timezone-free by
definition and falls back to `formatDate` for older instants.

User-entered times round-trip through the same boundary, using `date-fns-tz`:

- `localInputToUTC` — a `<input type="datetime-local">` string, interpreted in
  the display zone, becomes a UTC RFC 3339 string for the backend.
- `endOfDayUTC` — a `<input type="date">` value becomes the UTC instant one
  millisecond before the next calendar day in the display zone ("valid through
  this calendar day").
- `utcToLocalInput` — the inverse of `localInputToUTC`, rendering a UTC instant
  as a `datetime-local` value for editing.
- `utcToDateInput` — the inverse of `endOfDayUTC`, rendering a UTC instant as
  the `YYYY-MM-DD` calendar day it falls on in the display zone, for a
  `<input type="date">` value.
- `dateInputFromToday` — the calendar day a whole-day / whole-month offset from
  today in the display zone (`{ days: 1 }` is tomorrow on the viewer's
  calendar), for a `<input type="date">` `min` or default value.

A `<input type="date">` carries no timezone: it exchanges a bare calendar day,
and the user reads its calendar against their own clock. So a date control is
**anchored to the display zone on every leg** — seed with `utcToDateInput`,
bound with `dateInputFromToday`, submit with `endOfDayUTC`. Two shortcuts look
equivalent and are not, because each silently anchors one leg to UTC while the
rest of the page renders local:

- `instant.toISOString().slice(0, 10)` to seed a picker yields the **UTC** day,
  which disagrees with the `formatDate` beside it whenever the instant's UTC day
  differs from the viewer's. Use `utcToDateInput`.
- `new Date()` + `setDate`/`setMonth` (local fields) then `.toISOString()`
  (UTC) mixes both anchors in one expression, so the result is off by a day for
  part of every day — a zone-dependent window, not an edge case. Use
  `dateInputFromToday`, whose offsets are civil arithmetic and so are also
  immune to the 23- and 25-hour local days at a DST transition.

Note that `check:tz` (§4) scans Go only, so neither shortcut is caught by CI;
the date-control helpers above are the guard.

## 4. The CI guard

`scripts/check-timezone-correctness.sh` (npm script `check:tz`, part of
`check:all`) keeps both ends of the invariant from regressing. It fails the
build on either of two patterns:

1. **Bare `time.Now()` in a persistence path.** The script scans a fixed set of
   persistence-relevant directories (service `cmd`, `audit`, `store`,
   `handler`, `jobs`, quota, compliance, and the shared audit/store packages)
   for `time.Now()` that is not chained with `.` — `time.Now().UTC()` is
   followed by a dot and so passes. Monotonic-clock call-sites are exempted by
   an allowlist (the deadline / sleep / duration-math APIs from §2), `_test.go`
   files are skipped, and a single line can opt out with a `// tz-skip` comment
   for a deliberate, reviewed exception.
2. **A tz-less `DateTime` in `tools/db-migrate/schema/`.** Any `DateTime` field
   line that lacks `@db.Timestamptz` fails the check. The `tools/db-migrate/schema/`
   folder is the schema source of truth; the check scans that folder.

## References

- `tools/db-migrate/schema/` — `@db.Timestamptz(3)` on every `DateTime`
- `packages/control-plane/internal/handler/helpers.go` — `parseRFC3339Flexible`
- `packages/control-plane-ui/src/lib/format.ts` — the display-edge conversion layer
- `scripts/check-timezone-correctness.sh` — the `check:tz` guard
