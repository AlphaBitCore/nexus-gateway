# E20-S9 — Agent exemptions approval workflow (SDD)

## User story

As a compliance administrator, I need agent TLS bump exemption requests to start in a pending state so that another administrator can review and approve or reject them before they affect fleet policy.

## Tasks

1. Persist admin submissions with `status=pending` (same as auto-detected rows).
2. Expose `GET /api/admin/agent-exemptions/:id` for detail views.
3. Guard `POST .../approve` and `POST .../reject` so only `pending` rows transition.
4. Implement approve semantics: `AUTO` → `ADMIN` + 30-day expiry; `ADMIN` pending → `approved` without overwriting expiry.
5. Update Control Plane UI: list filters, detail route, create route, shared form components styling.
6. Update developer seed data with representative statuses.

## Acceptance criteria

- [ ] Creating an exemption via `POST /api/admin/agent-exemptions` returns `status: "pending"`.
- [ ] Approving an `ADMIN` pending row preserves `expiresAt` from the create payload when set.
- [ ] Approving an `AUTO` pending row sets `source` to `ADMIN`, `status` to `approved`, and refreshes expiry to roughly 30 days from approval time.
- [ ] Approving or rejecting a non-pending row returns HTTP 409.
- [ ] `GET /api/admin/agent-exemptions/:id` returns 404 for unknown ids.
- [ ] UI create flow lands on the detail page and shows Pending until approval.
