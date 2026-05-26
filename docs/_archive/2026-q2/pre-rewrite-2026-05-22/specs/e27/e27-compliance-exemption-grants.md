# E27 — Compliance exemption grants (database source of truth)

**Status:** Active — 2026-04-22  
**Epic:** 27

## 1. Business goal

Temporary compliance-hook exemptions must be auditable, listable by lifecycle (effective, scheduled, expired), and safe to retain after use. The Hub `active_exemptions` shadow must remain a **materialized projection** of grants stored in PostgreSQL so the compliance proxy fleet receives a single, consistent snapshot.

## 2. User roles

| Role | Need |
|------|------|
| **Compliance officer** | Review pending requests, approve or reject, see history of expired grants. |
| **Platform admin** | Create direct grants, disable without deleting activated grants, remove only pre-activation mistakes. |

## 3. Functional requirements

| ID | Requirement | MoSCoW |
|----|-------------|--------|
| FR-1 | Persist every approved exemption as a row in `compliance_exemption_grant` with `source_ip`, `target_host`, `reason`, `duration_minutes`, `effective_from`, `expires_at`, `approved_by`, optional `exemption_request_id`. | Must |
| FR-2 | Approve flow: atomically mark `exemption_request` APPROVED and insert the grant; then push Hub shadow from DB projection. | Must |
| FR-3 | Hub shadow entries are only grants where `inactive = false`, `effective_from <= now`, `expires_at > now`. | Must |
| FR-4 | `activated_at` is set once when the grant first becomes eligible for the live snapshot (same predicate as FR-3). No packet-level proof is required. | Must |
| FR-5 | `DELETE` is allowed only when `activated_at` is null; otherwise use `inactive` or wait for natural expiry. | Must |
| FR-6 | Admin API lists grants by tab: `effective`, `oncoming` (`effective_from > now` and not yet expired), `expired` (`expires_at <= now`), with `limit`/`offset` and `total`. | Must |
| FR-7 | Pending queue remains `exemption_request` with paginated list. | Must |
| FR-8 | Background reconcile (periodic) advances `activated_at`, refreshes Hub when oncoming grants become effective without a separate admin write. | Should |

## 4. Non-functional requirements

| ID | Requirement |
|----|-------------|
| NFR-1 | All timestamps UTC (RFC3339 in JSON). |
| NFR-2 | Pre-GA: no backward compatibility with template-only authoring; UI uses grant APIs only. |

## 5. Glossary

| Term | Meaning |
|------|---------|
| **Grant** | Durable approved exemption row in `compliance_exemption_grant`. |
| **Snapshot** | JSON payload under Hub config key `active_exemptions` derived from grants. |
| **Oncoming** | Approved grant whose `effective_from` is in the future. |

## 6. Constraints

- Control Plane continues to call Hub `NotifyConfigChange`; Hub owns template UPSERT and WebSocket push.
- Compliance proxy continues to consume shadow JSON only.
