# E27-S1 — Compliance exemption grants and Hub projection

## User story

As a compliance officer, I need exemptions stored in the database with clear lifecycle tabs and a projected runtime config so proxies stay in sync without hand-editing Hub templates.

## Tasks

1. Add `compliance_exemption_grant` table (Prisma + SQL migration) with indexes supporting tab queries and shadow projection.
2. Control Plane: grant CRUD store layer; `FlushGrantActivatedAt`; list-by-tab with pagination; transactional approve + insert grant.
3. Replace legacy `GET/POST/PATCH/DELETE /api/admin/compliance/exemptions` (template-centric) with `/api/admin/compliance/exemption-grants` for grant lifecycle; keep `POST .../compliance/exemptions/:id/(approve|reject)` where `:id` is the **exemption request** id.
4. Implement `ProjectComplianceExemptionGrantsToHub` (read grants → build `ActiveExemptions` → `NotifyConfigChange`).
5. Start periodic materializer when DB and Hub are configured.
6. Extend `ActiveExemption` with optional `effectiveFrom`; compliance-proxy `Rebuild` skips entries whose `effectiveFrom` is in the future (defense in depth).
7. Control Plane UI: single list with a Status filter (default `all`; values `all` / `effective` / `oncoming` / `pending` / `expired`); server-side pagination; delete only when API allows (pre-activation). Approve/reject actions render inline on rows whose `kind=pending`.
8. OpenAPI: `docs/users/api/openapi/admin/e27-s1-compliance-exemption-grants.yaml`.
9. Tests: Go (handlers + store projection + delete guard + unified list), Vitest (API client + page smoke if present).
10. Unified list endpoint: `GET /api/admin/compliance/exemption-grants?tab=all|effective|oncoming|expired|pending` returns `{ rows: UnifiedExemptionRow[], total }` where each row carries a `kind` discriminator (`grant` | `pending`) and per-kind nullable fields. The legacy `GET /api/admin/exemption-requests` listing endpoint is removed; admin review of PENDING requests now goes through the unified list with `tab=pending`. `POST /api/admin/exemption-requests` (employee-side submit) stays.

## Acceptance criteria

- [ ] Approving a pending request creates a grant row and updates Hub shadow within one successful request.
- [ ] `GET /api/admin/compliance/exemption-grants` (no tab) defaults to `tab=all` and returns `{ rows, total }` containing both `kind=grant` and `kind=pending` rows ordered by `createdAt` DESC.
- [ ] `GET /api/admin/compliance/exemption-grants?tab=effective|oncoming|expired|pending` filters to that lifecycle bucket; `tab=bogus` returns 400.
- [ ] `DELETE /api/admin/compliance/exemption-grants/{id}` returns 403 or 409 when `activated_at` is set.
- [ ] `PATCH` toggles `inactive` and shadow updates.
- [ ] Materializer sets `activated_at` for grants that cross into the effective window without manual admin action (within the tick interval).
- [ ] Proxy ignores disabled entries and future `effectiveFrom` entries if present in payload.
