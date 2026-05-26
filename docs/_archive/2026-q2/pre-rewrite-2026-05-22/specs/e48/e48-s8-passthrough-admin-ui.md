# E48-S8 — Emergency Passthrough Admin UI + IAM seed grants

## Context

E48 shipped the backend for the 3-tier emergency passthrough kill-switch across
S1–S7 (schema → runtime → bypass branches → audit fields → admin API + IAM
resource + Hub reconcile job). S7 explicitly deferred two pieces:

1. **The admin UI page** — operators had to use `curl` against
   `/api/admin/passthrough/*` to flip the kill-switch, which contradicts the
   "Incident responder" persona documented in
   `docs/developers/specs/e48/e48-emergency-passthrough.md:143`.
2. **The IAM seed grants** — the `passthrough` resource was added to the
   catalog and the handler wired through the verbs, but no managed policy
   actually contained the corresponding action grants. Only super-admin (via
   `admin:*` wildcard) could call any of the passthrough endpoints.

This story closes both gaps in one PR.

## Goals

- A dedicated admin page at `/ai-gateway/passthrough` that exposes all 3
  tiers (global / adapter / provider) plus the active-state banner +
  countdown timer.
- The seeded managed policies `NexusProviderAdminAccess`,
  `NexusSecurityAdminAccess`, and the **new** `NexusIncidentResponse`
  carry the appropriate passthrough grants so non-super-admin roles can
  actually operate the kill-switch as designed.
- Cross-toggle invariants and the 20-character reason requirement are
  enforced client-side **and** server-side; the server stays authoritative
  but the UI provides instant feedback.
- IAM impact review captured in this doc per the CLAUDE.md binding so a
  reviewer can trace policy intent without reading the diff backwards.

## Non-goals

- Per-VirtualKey passthrough tier. Future work, product call required.
- Slack / PagerDuty integration on enable. Out of scope for S8; existing
  AlertRule `passthrough-active` already fires when any tier is enabled.
- Bulk-disable affordance. Out of scope — operator deletes the rows
  individually or waits for auto-expiry.

## Tasks

### T1 — Backend: bulk snapshot read

`GET /api/admin/passthrough/snapshot` is added in
`packages/control-plane/internal/handler/admin_passthrough.go`. Returns
the same shape as the Hub shadow blob plus a `providerNames` lookup so the
page can render every tier without N round-trips. IAM-gated by
`admin:passthrough.read`. Acceptance: a single `curl` against this endpoint
returns the full state needed by the page.

### T2 — Backend: catalog-driven action constants

The three opaque string literals in `admin_passthrough.go` are replaced with
`iam.ResourcePassthrough.Action(iam.Verb*)` calls. The deferred-cleanup
note in the file header is removed. Acceptance: `TestPassthroughActionCatalogAlignment` pins the produced strings to the canonical IAM form so a rename of the verb constants in `shared/iam` fails this test instead of silently breaking the route gates.

### T3 — IAM seed: managed policy grants

`tools/db-migrate/seed/data/seed-baseline.sql` is updated to:

- Append two Statements to `NexusProviderAdminAccess`:
  `passthrough-read` (`admin:passthrough.read`) and `passthrough-write`
  (`admin:passthrough.write`), both scoped to
  `nrn:nexus:gateway:*:passthrough/*`.
- Append the same two Statements to `NexusSecurityAdminAccess`.
- Insert a new managed policy `NexusIncidentResponse` carrying
  `passthrough.emergency-enable` plus situational-awareness reads
  (`observability.read`, `alert.read+acknowledge`, `traffic-log.read`,
  `audit-log.read`, `node.read+force-resync`, `kill-switch.read+toggle`,
  `settings.read`).

`packages/control-plane/internal/iam/managed.go` adds
`admin:passthrough.read` to the `NexusViewer` fixture so the coverage
regression test in `iam_test.go` reflects the new resource.

A one-shot idempotent SQL script
`tools/db-migrate/manual-scripts/e48_iam_passthrough_policies_2026_05_13.sql`
mirrors these changes for the existing prod database. Safe to re-run.

### T4 — Frontend: PassthroughPage

`packages/control-plane-ui/src/pages/ai-gateway/passthrough/PassthroughPage.tsx`
hosts four panels under a single page:

| Panel | Purpose |
|-------|---------|
| Active banner | Red pulsing banner whenever any tier has `enabled=true`, listing each active tier with its bypass flags + countdown to `expiresAt`. Neutral grey when nothing is active. |
| Global panel | Edit-in-place + Save for the singleton global tier. Triggers a confirm modal when `enabled=true`. |
| Adapter overrides | Table of all enabled-or-disabled adapter overrides + "Add" → modal with adapter dropdown + tier editor. |
| Provider overrides | Same shape as adapter, with provider name + adapterType lookup in the dropdown. |

The shared `TierEditor` component renders the 3 toggles, datetime-local
expires-at picker (capped at NOW + 8h), reason textarea with a live char
counter, and surfaces the `validation.*` error code as a translated
message before the Save button can fire.

Cross-toggle: enabling `bypassNormalize` auto-enables `bypassCache` and
disables the `bypassCache` switch — the UI mirrors the server constraint
that the cache key derives from the normalized payload.

### T5 — Frontend: API service + permission keys

`packages/control-plane-ui/src/api/services/passthrough.ts` exposes the 9
typed endpoints plus `validatePassthroughPayload` (a one-for-one mirror
of the server validator that returns stable error codes for i18n lookup).

`packages/control-plane-ui/src/hooks/usePermission.ts` adds
`passthrough:read`, `passthrough:write`, and `passthrough:emergencyEnable`
to the `ACTION_MAP` so the page can guard the Save button on the strongest
required verb (`passthrough:emergencyEnable`) and the per-row Delete on
`passthrough:write`.

### T6 — Frontend: route + nav + sidebar icon

`packages/control-plane-ui/src/routes/shellRouteConfig.tsx` registers
`/ai-gateway/passthrough` with `allowedActions: ['admin:passthrough.read']`
and `nav.labelKey: 'passthrough'` under the `aiGateway` section,
ordered 8 (after Prompt Cache).

`packages/control-plane-ui/src/components/ui/Sidebar/Sidebar.tsx` gains a
warning-triangle icon case for `/ai-gateway/passthrough`.

### T7 — i18n × 3 locales

`pages.passthrough.*` namespace added in `en/zh/es` covering all
labels, hints, banner copy, validation messages, confirm modal, toasts.
`nav.passthrough` added too. Key counts are kept in lockstep across
locales (verified via `python3` counter).

### T8 — Tests

- `packages/control-plane/internal/handler/admin_passthrough_test.go`
  table-driven covers all six payload-validation invariants plus the
  IAM action alignment and the magic-number constants
  (`passthroughMaxExpiry`, `passthroughMinReasonLen`,
  `passthroughShadowKey`).
- Existing UI Vitest suite continues to pass; no new Vitest tests
  added for the page in this PR (the value of testing dialog wiring
  is low compared to manual prod smoke).

## Acceptance criteria

1. A user holding `NexusViewerAccess` can open the page and see the
   current state but cannot enable / disable any tier or delete rows
   (Save buttons disabled, "no permission" hint visible).
2. A user holding `NexusIncidentResponse` can open the page, see the
   state, AND enable / disable any tier. The Save button triggers the
   confirm modal when `enabled=true`. The Delete button on any row is
   enabled (write verb is part of the policy).
3. A user holding `NexusProviderAdminAccess` can read state and delete
   rows but cannot enable (gating on `emergency-enable`). The Save
   button on the Global panel + the editor modals is disabled with the
   "no permission" hint visible.
4. A user with only `agent-device.read` cannot reach the page (route
   guard hides the nav entry).
5. Saving with `enabled=true` requires a reason ≥ 20 characters, an
   `expiresAt` in the future and ≤ NOW + 8h, and at least one bypass
   flag set. All three failure modes surface a translated error message
   inline before the Save button fires.
6. Enabling `bypassNormalize` automatically enables `bypassCache` and
   disables editing of the `bypassCache` switch.
7. When any tier is enabled, the banner at the top of the page goes
   red and lists each active tier with a live countdown to
   `expiresAt`.
8. After a save, the page re-fetches the snapshot and the new state is
   reflected immediately.

## IAM impact review (CLAUDE.md binding)

| Resource | Verbs | NexusViewer | NexusProviderAdmin | NexusSecurityAdmin | NexusIncidentResponse | Super-admin |
|----------|-------|-------------|--------------------|--------------------|----------------------|-------------|
| `passthrough` | `read` | ✓ (added in `managed.go` + via `admin:*.read` wildcard in seed) | ✓ (added in seed) | ✓ (added in seed) | ✓ (new policy in seed) | ✓ (via `admin:*` wildcard) |
| `passthrough` | `write` | — | ✓ (added in seed) | ✓ (added in seed) | ✓ (new policy in seed) | ✓ |
| `passthrough` | `emergency-enable` | — | — | — | ✓ (new policy in seed) | ✓ |

The handler's `iamMW(...)` action constants now derive from
`iam.ResourcePassthrough.Action(...)`, so a future verb rename produces
a compile-time string change in tests rather than a silent 403.
