# E20 — Agent TLS bump exemptions (requirements)

## Functional requirements

| ID | Requirement | MoSCoW |
|----|-------------|--------|
| FR-1 | Administrators can submit a new exemption (host, reason, scope, optional device/group, duration, denylist flag) through the Control Plane API and UI. | Must |
| FR-2 | Every new row is persisted with `status=pending` until explicitly approved or rejected. | Must |
| FR-3 | Users with approve permission can approve a pending row; `AUTO` rows become `ADMIN` with a 30-day expiry; `ADMIN` submissions become `approved` while retaining configured expiry. | Must |
| FR-4 | Users with approve permission can reject a pending row; the row is marked rejected and denylisted per product rules. | Must |
| FR-5 | Operators can read a single exemption by id (detail API + UI). | Must |
| FR-6 | The list UI supports filtering by `status` and `source`, links to a detail page, and a dedicated create page consistent with other admin forms. | Should |

## Non-functional requirements

| ID | Requirement | MoSCoW |
|----|-------------|--------|
| NFR-1 | Double approve/reject returns HTTP 409 with a clear error code (`NOT_PENDING`). | Must |
| NFR-2 | All user-visible UI strings use `react-i18next` keys in `en`, `zh`, and `es` locales. | Must |

## Glossary

- **Pending** — Awaiting compliance review.
- **Approved** — Review accepted; eligible for agent materialization when the config pipeline includes the row.
- **Rejected** — Review declined; host is denylisted per reject semantics.

## Constraints

- English-only prose in this document; localized UI copy lives in locale JSON files.
