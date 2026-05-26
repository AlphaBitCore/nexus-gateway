---
doc: admin-audit-log-coverage
area: cross-cutting
service: observability
tier: 1
updated: 2026-05-20
---

# Admin Audit Log Coverage

> **Tier 1 architecture doc.** Companion to `audit-pipeline-architecture.md`. The pipeline doc covers the **how**; this doc covers **what is audited**. Read this when adding a new admin endpoint and you need to know "should this emit an audit event?".

Every admin mutation and sensitive admin read must emit an `AdminAuditLog` row. The coverage matrix below is the binding reference.

---

## 1. The `AdminAuditLog` row

Real columns from `tools/db-migrate/schema.prisma` (model `AdminAuditLog`, line ~247):

```
id              -- uuid PK
sequenceNumber  -- monotonic per-row (autoincrement); pairs with hash chain
timestamp       -- DB-side insert clock
actorId         -- the operator (NexusUser.id, or service principal id)
actorLabel      -- human-readable label (email / API-key name) snapshotted at emit time
actorRole       -- which role the operator used (nullable)
sourceIp        -- best-effort source IP (nullable)
action          -- "create" | "update" | "delete" | "flush" | "export" | "rollback" | "simulate" | "reset"
entityType      -- "policy" | "hook" | "provider" | "credential" | "virtualKey" | "routingRule" | "config" | "apiKey" …
entityId        -- per-entity natural id (nullable)
beforeState     -- JSONB snapshot before mutation (sensitive fields stripped)
afterState      -- JSONB snapshot after mutation (sensitive fields stripped)
nexusRequestId  -- Hub-stamped request id (matches access logs / x-nexus-request-id response)
clientRequestId -- not trusted for new writes (kept for legacy reads only)
clientUserId    -- mirrors actorId for admin API keys
clientSessionId -- session id (nullable)
previousHash    -- prior row's integrityHash; NULL on genesis row
integrityHash   -- SHA-256 of hashInput (tamper-evident chain)
hashInput       -- exact canonical bytes hashed; source of truth for VerifyChain
```

`beforeState` / `afterState` are intentionally **subset** snapshots — sensitive fields (provider credentials, raw VK secrets, agent device private keys) are NEVER included. The hash chain is computed under a `pg_advisory_xact_lock` (`packages/nexus-hub/internal/traffic/chain/chain.go`) so concurrent inserts cannot fork the chain.

## 2. Coverage matrix — mutations (all emit `AdminAuditLog`)

| Resource | Actions | Notes |
|---|---|---|
| Provider | Create / Update / Delete | `after.encrypted_blob` is omitted; only metadata. |
| Credential | Create / Update / Disable / Revoke / Rotate | Never log plaintext. |
| Virtual Key | Create / Update / Revoke | Hashed secret only. |
| Routing Rule | Create / Update / Delete | Full rule JSON in `after` (no secrets). |
| Hook Config | Create / Update / Delete / Enable / Disable | Full config including `onMatch`. |
| Quota Policy | Create / Update / Delete | |
| Organization | Create / Update / Delete / Move | Move emits both `before.parent` and `after.parent`. |
| Project | Create / Update / Delete | |
| User (local) | Create / Update / Delete / Reset Password | `password_hash` never logged. |
| Membership | Create / Update / Delete | |
| Role | Create / Update / Delete | Includes policy bindings. |
| Policy | Create / Update / Delete | Full policy document. |
| IdP Config | Create / Update / Delete | Secrets masked. |
| Alert Rule | Create / Update / Delete / Mute (all rolled into IAM `admin:alert.update`) | The IAM catalog has no separate Mute/Unmute action; the audit row's `action` field is `update` regardless. |
| Alert Channel | Create / Update / Delete / Test (all under IAM `admin:alert.update`) | Test-connection results audited as `update` rows. |
| Kill Switch | Activate / Deactivate / Auto-Revert | Per E48; both manual and auto emit. |
| Emergency Passthrough | Activate / Reset / Reconcile | Per E48; non-optional audit trail. |
| Interception Domain | Create / Update / Delete | Compliance Proxy + Agent. |
| Exemption | Create / Update / Delete / Auto-Apply | Auto-apply from pinning detection still audited. |
| Enrollment Token | Issue / Revoke | Token plaintext returned once, never re-logged. |
| Device (Agent) | Revoke / Rename | Enrollment itself emits a Hub-side diag/lifecycle event (`diag-event-triage-architecture.md`), not an `AdminAuditLog` row. |
| SSO User (JIT) | Provision / Update | JIT provision is auto-emitted on first federation. |
| Settings | Update | Any global setting change. |
| Backups / Exports | Trigger / Download | Includes export-scope and result. |
| Retention Policy | Update | |

## 3. Coverage — sensitive reads

Most admin reads do NOT emit audit (would flood the table). Bulk audit export is the one read explicitly admin-gated and audited via its own IAM action — `admin:audit-log.export` (see `packages/control-plane-ui/src/test/msw-handlers.ts:413` for the seeded super-admin action set, which is the canonical action-code list at write time).

Other sensitive reads (returning provider credential plaintext, opening a stored body from spillstore, etc.) are not split into their own IAM verbs in the current catalog — they ride on the regular `admin:<resource>.read` action, which already requires super-admin or an explicit per-tenant policy grant. Adding a new sensitive-read verb requires updating the IAM action catalog (cross-ref `iam-identity-architecture.md`).

## 4. Coverage — sensitive non-mutation actions

Cascading or fleet-scope actions that aren't a straightforward CRUD on a single row still emit `AdminAuditLog`. Examples include device revocation (cert-serial captured in `afterState`), user suspension (membership snapshot captured), and SIEM replay (window range captured). All ride existing `admin:<resource>.update` / `.delete` IAM verbs — there is no `admin:siem.replay` action code in the catalog as of this writing.

## 5. Coverage — automated / system events

The audit pipeline also captures **non-admin** automated actions when they affect state — retention purges, kill-switch auto-reverts, device-cert mints, credential-health threshold crossings. These carry `actorId="system"` (synthetic actor; the Hub raiser path stamps `system` for non-user-originated rows — see `packages/nexus-hub/internal/alerts/engine/raiser.go` and the `actorFromHeader` helper) and `actorLabel` set to the originating job name. The set of qualifying system events grows with `jobs-architecture.md` — each job that mutates an audited resource is responsible for the corresponding `AdminAuditLog` emit.

## 6. Coverage — denials

`AdminAuditLog` does not have a `denied_reason` column. IAM denials do not insert a row in this table — they are surfaced via:

- HTTP 403 response with the action / required-action in the error body.
- A structured `slog` warn record on the CP service log (picked up by the diag pipeline — cross-ref `diag-event-triage-architecture.md`).
- Prometheus counters per IAM action / per actor.

The caveat: a denial caused by a missing `iamMW` wrapper (i.e., the endpoint forgot to wrap) does not produce any of the above. That's why the "API / menu / route changes require IAM impact review" rule in CLAUDE.md exists.

## 7. Required fields per emission

When you add a new admin endpoint that mutates state, populate the canonical columns and let the chain helper (`packages/nexus-hub/internal/traffic/chain/chain.go::NextHash`) stamp the hash trio:

- `actorId`, `actorLabel`, `actorRole` from the authenticated principal.
- `sourceIp` from the inbound request.
- `action` ∈ {`create`, `update`, `delete`, `flush`, `export`, `rollback`, `simulate`, `reset`} — pick the verb that best matches the mutation. IAM action codes (`admin:<resource>.<verb>`) live alongside `action` in handler middleware and drive the IAM gate, not the audit column.
- `entityType`, `entityId` for the target resource.
- `beforeState`, `afterState` — subset snapshots; sensitive fields stripped.
- `nexusRequestId` from `c.Get("nexusRequestId")` (set by Hub middleware).

Checklist when adding the audit emit:

- [ ] `action` is one of the canonical verbs.
- [ ] `entityType` matches an existing convention (do not invent a new value silently).
- [ ] `beforeState` / `afterState` exclude all sensitive fields.
- [ ] Emit happens **in the same transaction** as the mutation; the integrity-hash advisory lock guarantees fork-free chaining.
- [ ] No `denied_reason` column — denial signal lives in slog + Prometheus instead.
- [ ] Test case: positive + error path both produce correctly chained rows (`previousHash` = prior `integrityHash`).

## 8. What is NOT audited

By design, the following are NOT in `AdminAuditLog`:

- Read-only admin API calls (list / detail) — too high volume.
- `/v1/*` traffic — that's `traffic_event`.
- Agent operational events (heartbeat, drift, status) — those flow through the diag pipeline (`diag-event-triage-architecture.md`).
- Failed authentication attempts at the OAuth+PKCE layer — those land in slog + Prometheus and are surfaced via the alerting rule catalog (cross-ref `alerting-architecture.md`).

## 9. Sources

- `packages/control-plane/internal/handler/*` — admin endpoints that emit.
- `packages/control-plane/internal/identity/iam/` — IAM middleware + catalog.
- `packages/nexus-hub/internal/traffic/chain/chain.go` — `NextHash` integrity-hash chain helper.
- `tools/db-migrate/schema.prisma` — `AdminAuditLog` model.

## 10. Cross-references

- `audit-pipeline-architecture.md` — how these events flow.
- `iam-identity-architecture.md` — IAM action catalog these events reference.
- `tenancy-architecture.md` — `org_ancestor_path` attribution.
- `idp-sso-architecture.md` — login + JIT provisioning emits `AdminAuditLog`.
