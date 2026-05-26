# Control Plane UI — Compliance: records and reporting

This document covers the records part of the COMPLIANCE sidebar section: **Operation Logs**, **Data Subject Requests**, and **Compliance Report**. The hooks-and-policy part is in [compliance-hooks.md](./compliance-hooks.md), and the interception part is in [compliance-network.md](./compliance-network.md). Sidebar labels and routes are defined in `packages/control-plane-ui/src/routes/shellRouteConfig.tsx`.

## Operation Logs

**Purpose.** The admin audit trail — who changed which configuration object, and what the object looked like before and after.

**List page.** Columns: user, entity type, entity id, action, and time (relative). The filter toolbar has a user combobox, an action select, an entity-type select, start and end datetime pickers, and a clear-filters control. The list paginates. An export button (gated on the `audit:export` action) downloads the filtered logs as JSON.

**Detail.** Clicking a row opens a drawer showing the user, entity, action, and time, plus the before-state and after-state as JSON blocks with copy-to-clipboard. The record is the configuration diff itself — there is a before/after pair rather than a pass/fail result.

**Key concepts.** The action is `create`, `update`, `delete`, `reset`, or `simulate`. The entity type is `routingRule`, `credential`, `virtualKey`, `quota`, `model`, `provider`, or `iamRole`.

**Where the data comes from.** `systemApi` — `listAdminAuditLogs`, `exportAdminAuditLogs` (the user combobox uses `iamApi.listUsers`).

## Data Subject Requests

**Purpose.** File, triage, and fulfill data-subject access and erasure requests against the traffic records.

**List page.** A single "Request Queue" card lists requests with columns: subject, type, status, filed (created-at), completed (completed-at), notes, and actions. The toolbar has a status filter, a refresh control, and a "File request" button.

**Create and fulfillment.** The create dialog collects the data subject (chosen from the user list, stored as a subject id), an optional contact, the request type, and notes. Row actions are status-driven: a pending request can be started or rejected; an in-progress request can be fulfilled through a confirmation dialog — an access request exports the subject's virtual-key and proxy rows as downloadable JSON, and an erasure request anonymizes those rows; completed and rejected requests are terminal.

**Key concepts.** The request type is `ACCESS` or `ERASURE`. The status is `PENDING`, `IN_PROGRESS`, `COMPLETED`, or `REJECTED`. A fulfillment reports the anonymized row counts (for erasure) or the exported row sets (for access), split by virtual-key and proxy traffic.

**Where the data comes from.** `dsarApi` — `list`, `get`, `create`, `update`, `fulfill` (the subject picker uses `iamApi.listUsers`).

## Compliance Report

**Purpose.** Generate a print-friendly compliance summary over a chosen date range. The report is reached by direct URL (`/compliance/compliance-report`) and renders its full content there.

**What you see.** A controls card (excluded from print) with a preset select (24h, 7d, 30d, custom), start and end datetime pickers, a Generate action, and — once generated — a Print / Save-as-PDF action. The report body has four sections, each a table:

- **Coverage** — total events, coverage percentage, and the status breakdown.
- **Hook Health** — total decisions, the allow / deny / error / unknown split, and the top deny reason codes.
- **Reject Stats** — total rejects and the top target hosts.
- **DSAR Queue** — the pending, in-progress, completed, and rejected counts, plus the number completed within the period.

**Controls.** Export is the browser's print-to-PDF (Save as PDF); the report window is capped at 366 days.

**Key concepts.** Hook decision keys are allow, deny, error, and unknown. The range presets are 24h, 7d, 30d, and custom. The DSAR counts reuse the data-subject-request lifecycle statuses.

**Where the data comes from.** `complianceReportApi.get` (start and end time).

## References

- `packages/control-plane-ui/src/routes/shellRouteConfig.tsx` — route registry and `nav: { sectionKey: 'compliance', ... }` blocks
- `packages/control-plane-ui/src/i18n/locales/en/nav.json` — sidebar labels
- `packages/control-plane-ui/src/pages/governance/AuditLogPage.tsx` — Operation Logs
- `packages/control-plane-ui/src/pages/governance/adminAuditLogShared.tsx` — shared audit-log table, entry drawer, and column builders
- `packages/control-plane-ui/src/pages/governance/DSARPage.tsx` — Data Subject Requests
- `packages/control-plane-ui/src/pages/governance/ComplianceReportPage.tsx` — Compliance Report
- `packages/control-plane-ui/src/api/` — `systemApi`, `dsarApi`, `complianceReportApi`, `iamApi`
- `tools/db-migrate/schema.prisma` — `AdminAuditLog`, `DSARRequest` models
