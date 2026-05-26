# E49 — Diag-Event Triage & Silence

**Status:** Shipped — 2026-05-14 (prod commit `0d228aa7` + follow-ups)
**Epic:** 49
**Depends on:** E33 dual-pipeline diag events (thing_diag_event table + agent drain), shared `observability` IAM resource

## 1. Business Goal

Operators looking at `/infrastructure/errors` want to answer **"is anything on fire right now?"** in under five seconds. The pre-E49 page surfaced two stacked tables (Top groups + Recent stream) with a five-filter bar above them. In practice prod data is dominated by known noise (e.g. `auto-updater disabled: no Ed25519 key` repeats ~30×/day on dev), and operators had no way to distinguish those from a new incident. Triage was reactive ("check after a customer complaint") rather than glanceable.

E49 reshapes the page into a noise-controlled triage view: hero tiles for at-a-glance health, an Issue list grouped by `(message_hash, max_level)` with inline sparklines and quick actions, and a silence registry that lets operators ack repeating noise so the list keeps showing only what's new or escalating.

## 2. Scope

### In scope

- New `diag_silence(message_hash, level, silenced_by, silenced_at, expires_at, reason)` table + 3 admin CRUD endpoints (`GET /api/admin/diag-silences`, `POST`, `DELETE /:id`). IAM gated under existing `observability` resource (read + write).
- `GET /api/admin/diag-events/groups` enriched with two fields per row: `buckets` (5-min `date_bin` sparkline series over the requested range) + `silenced` (EXISTS against `diag_silence` for `(messageHash, maxLevel)` with active-TTL check).
- Frontend rewrite of `InfraRecentErrorsPage`: 4 hero tiles (errors/hr + trend, active issues, top offender source, newest issue) + fleet sparkline + single Issue list with inline per-row sparkline + NEW badge for first-seen-in-last-hour + silenced visual + filter panel collapsed by default + Manage Silences popup.
- Detail UX: right-side `xl` drawer for the issue context (group meta + sparkline + Silence actions + inline paginated Affected-events table at 10/page with Load more). Clicking an event row opens a small centered Event-detail popup with attrs/stack/trace on top of the drawer.

### Out of scope

- Retention / cleanup job for expired silences. Lazy-expiry (filter at query time) chosen so the implementation stays at zero scheduled-job surface area. Tracked as a future enhancement.
- Push notification when a silence expires and the noise resumes.
- Silence dimensions beyond `(messageHash, level)` (e.g. per-node-id silence). The current granularity matches how operators ack groups in the Issues view.
- Lifecycle / startup events. The Issue list keeps showing whatever the diag store returns; classifying lifecycle out of "errors" is the responsibility of the `eventType` column added separately.

## 3. Functional Requirements

| ID | Requirement | Priority |
|---|---|---|
| FR-1 | The page MUST render hero tiles (errors/hr last 1h, active issues, top offender source, newest issue) computed client-side from the same `/groups` response — no extra endpoint. | Must |
| FR-2 | The Issue list MUST collapse events into `(messageHash, maxLevel)` groups, ordered by `totalOccurrences DESC, lastSeen DESC`, capped at 100 groups per request. | Must |
| FR-3 | Each Issue row MUST show an inline sparkline derived from the new `buckets` field (5-min granularity over the active time range). | Must |
| FR-4 | Issues whose `firstSeen` falls within the last 1h MUST be marked with a "NEW" badge. | Must |
| FR-5 | The operator MUST be able to silence a `(messageHash, level)` pair via 1h or 24h presets from the row's quick actions and from the drawer. | Must |
| FR-6 | Silence creation MUST refresh both `/groups` (to flip the row's `silenced` flag) and `/diag-silences` (so the header counter updates). | Must |
| FR-7 | Silenced rows MUST be hidden from the default view but discoverable via a "Show silenced" toggle in the filter panel. | Must |
| FR-8 | A Manage Silences popup MUST list every active silence with level, message-hash, silenced-at, expires-at (or "permanent"), reason, and a per-row Unsilence button. | Must |
| FR-9 | The issue detail surface MUST open as a right-side drawer (`xl` width). Inline within the drawer: group meta, sparkline, Silence actions, paginated Affected-events table (10/page). | Must |
| FR-10 | Clicking an Affected-events row MUST open a centered Event-detail popup on top of the drawer, NOT a second drawer or nested modal stack. | Must |
| FR-11 | Closing the Event-detail popup MUST return the operator to the drawer at the same scrolled position in the events list. | Must |
| FR-12 | Closing the drawer MUST reset both group and event state (no stale popup left open). | Must |
| FR-13 | TTL values MUST be server-clamped to `[1s, 30d]` for non-permanent silences. TTL = 0 means permanent. | Must |
| FR-14 | Active silences MUST be filtered at query time via `expires_at IS NULL OR expires_at > NOW()`. No background job sweeps expired rows. | Must |

## 4. Non-Functional Requirements

| ID | Requirement | Notes |
|---|---|---|
| NFR-1 | The `/groups` SQL MUST execute in O(buckets) per top-100 hash. Buckets fetched in a second query keyed by `message_hash = ANY($hashes)` to keep the inner `GROUP BY` cardinality small. | Verified on prod with 75 errors / 24h. |
| NFR-2 | The Issues page MUST stay below 2s time-to-interactive on the common 24h range. Hero tiles compute client-side from the same response — no second round-trip. | |
| NFR-3 | The diag-silences APIs MUST audit every create/delete via `Audit.LogObserved` with before/after state and `entityType = observability`. | Same pattern as diag-mode endpoints. |
| NFR-4 | New i18n keys MUST exist for all three locales (en/zh/es) and be mirrored from `src/i18n/locales/` to `public/locales/`. | Enforced via i18n-gap-check skill. |

## 5. User Roles

- **Super admin / Provider admin / Compliance officer** — same `observability:read` + `observability:write` actions as the existing diag-mode + diag-events surfaces. Silencing is comparable in blast-radius to changing what other operators see in dashboards.

## 6. Constraints & Assumptions

- `diag_silence` rows accumulate indefinitely (no cleanup job). Cardinality expected to stay below 1000 rows lifetime; partial index covers the active-set filter so reads stay cheap regardless.
- Expiry is **wall-clock based** — silences become inactive at the exact `expires_at` instant via the `NOW() > expires_at` predicate. No drift window, no eventually-consistent behaviour.
- Lifecycle events ride the same `thing_diag_event` table and surface in this Issues view unless the operator narrows by `eventType=error|crash`. Out-of-scope to silently filter them; the operator is in control.

## 7. Glossary

- **Issue** — a `(message_hash, max_level)` group of diag events. The unit of triage in the Issues list.
- **Silence / Snooze** — an admin ack that suppresses an issue's `silenced=true` flag for a bounded TTL (or permanently when TTL = 0).
- **Sparkline** — 5-minute bucket counts rendered inline per row + per fleet hero tile.
- **NEW** — visual badge on issues whose `firstSeen` is within the last hour.
- **Affected events** — the underlying diag events that compose an issue, paginated inside the drawer.

## 8. Acceptance Criteria

- Operator can identify a new spike in <5s glanceable. (Hero tiles + NEW badge satisfy.)
- Operator can ack a noisy issue and have it disappear from the default view within one full refresh. (Silence → groups.refetch.)
- Operator can see and undo every silence they (or another admin) created from a single popup. (Manage Silences.)
- A silence that has expired stops affecting the `silenced` flag immediately, with no job lag. (Verified via lazy-expiry predicate.)
- Existing diag-events list endpoint behaviour is unchanged. The page-level redesign and silences are additive.
