# E34 — Per-Thing Config Override + Force-Sync

Status: In progress
Epic owner: Platform / Control Plane
Related: E3 (Thing Model), E31-S7 (Runtime Introspection), Thing Model SDD §5/§6/§10.

## 1. Background

The Nexus platform manages a fleet of heterogeneous nodes (ai-gateway,
compliance-proxy, control-plane, nexus-hub, agent) via the Thing Model
(`docs/developers/architecture/cross-cutting/foundation/thing-model.md`). Today the only configuration tier is the
per-`(type, config_key)` template in `thing_config_template`: when an admin
edits a template, every Thing of that type receives the same payload.

Five recurring enterprise patterns require **per-Thing differentiation** that
the current single-tier model cannot express:

| Code | Pattern | Why per-Thing matters |
|------|---------|------------------------|
| **A** | Canary / staged rollout | Try new routing rules / hook chain on one node before fleet-wide |
| **B** | Region / regulatory difference | EU node has stricter PII redaction; APAC node has different provider list |
| **C** | Capacity differentiation | Larger host carries higher rate-limit / cache size |
| **D** | Incident / break-glass | One node temporarily relaxes a hook or extends an exemption while ops investigates |
| **F** | Diagnostics | One node runs verbose logging / sample-rate=1.0 while others stay normal |

Use case **E** (per-tenant node pinning) is explicitly **out of scope** —
Nexus is single-tenant on-prem with no confirmed customer requirement for
tenant-dedicated nodes.

In addition, current force-sync is gated on `reported_ver < desired_ver`. The
admin UI has no way to make a Thing re-apply config when the Thing claims to
already be in sync — operators routinely need this for cache invalidation,
suspected silent drift, or post-restart sanity checks.

This epic delivers per-Thing override CRUD + an explicit force-sync that
bypasses the version-equality short-circuit.

## 2. Functional Requirements

### 2.1 Override CRUD (Must)

- **FR-1.1** Admin can set / update a per-Thing override for any overridable
  `config_key`. Override payload is a JSON object that fully replaces the
  template state for that key on that Thing only (whole-key replacement, no
  deep merge).
- **FR-1.2** Admin can clear an override; the key reverts to the template
  default and the Thing receives the new state via WebSocket push.
- **FR-1.3** Each override stores: `state`, `set_by`, `set_at`,
  `template_ver_at_set`, `reason` (≤ 500 chars, optional), `expires_at`
  (optional, range [NOW + 5 m, NOW + 30 d]), `emergency_override` flag.
- **FR-1.4** A configurable blacklist forbids overrides on
  `{credentials, virtual_keys}`. Server returns 400 BadRequest on attempts.
  Blacklist is hard-coded in Go for compile-time visibility.
- **FR-1.5** Setting the override on `killswitch`, or with a `reason`
  starting with `break-glass:`, automatically marks `emergency_override = true`.

### 2.2 Force-sync (Must)

- **FR-2.1** Admin can force-sync a single key on a Thing regardless of
  whether the Thing reports as already synced.
- **FR-2.2** Admin can force-sync **all** keys on a Thing in one call.
- **FR-2.3** The Thing re-runs `OnConfigChanged` and emits a fresh
  shadow_report on every force-sync (Force=true on the wire).
- **FR-2.4** Force-sync writes `admin_audit_log` (`thing_force_resync` /
  `thing_force_resync_all`) but **not** `config_change_event` — it is
  redelivery, not a config change.

### 2.3 TTL auto-expiry (Must)

- **FR-3.1** A Hub-side scheduled job runs every 60 s and clears any
  override whose `expires_at < NOW()`.
- **FR-3.2** Auto-expiry uses the same path as admin clear (recompute
  `thing.desired`, bump `desired_ver`, push, audit) with
  `actor = "system:override-expiry-job"`. The audit row reuses action
  `thing_override_cleared` (same shape as admin clears); the actor is the
  discriminator that distinguishes auto-expiry from admin-initiated clears.

### 2.4 Stale detection (Must)

- **FR-4.1** A row is considered **stale** when the current
  `thing_config_template.version` for `(thing.type, override.config_key)`
  exceeds `override.template_ver_at_set`.
- **FR-4.2** Stale flag is computed at read time via JOIN; no separate
  background job and no separate column.
- **FR-4.3** Stale is informational only — never blocks the override.

### 2.5 UI surfaces (Must)

- **FR-5.1** `/infrastructure/nodes` list page shows an `Overrides` column
  with per-row count + ⚠ stale chip. Includes a `Has overrides` filter.
- **FR-5.2** `/infrastructure/nodes/:id` detail page consolidates the
  prior `Config Sync` + `Applied Config` tabs into a single `Configuration`
  tab (5 total tabs) with a 4-column table:
  Key / Template default / Override / Applied.
- **FR-5.3** Override editor opens as a right-side drawer with a read-only
  template pane and an editable JSON pane, plus TTL preset picker and
  reason field.
- **FR-5.4** A new `/infrastructure/overrides` page lists every active
  override across the fleet with filters (type, has-TTL, stale, recent)
  and per-row actions (View / Force resync / Clear / Extend). v1 has
  no bulk mutation.
- **FR-5.5** Force-sync buttons (per-key and whole-Thing) are visible at
  all times — the "in-sync" state does not hide them.
- **FR-5.6** When a Thing has an active `killswitch` override **and** the
  global killswitch is engaged, the detail page renders a red bypass
  banner; the list page row gets red styling; the global page row gets a
  red break-glass badge.

### 2.6 Cascade & wire format (Must)

- **FR-6.1** Override values, when set, fully replace the template state
  for that `(thing, key)` pair in `thing.desired`.
- **FR-6.2** Existing `thing.desired` JSONB remains the wire-format cache.
  WebSocket push and `BulkConfigPull` paths require **no client changes**.
- **FR-6.3** Override write/clear is a single transaction:
  1. UPSERT/DELETE `thing_config_override`
  2. Recompute `thing.desired = template ∪ override` for the affected Thing
  3. Bump `thing.desired_ver`
  4. Insert `admin_audit_log`
  5. Post-commit: Hub force-pushes the affected key.

## 3. Non-Functional Requirements

- **NFR-1** Override write end-to-end (admin save → client `OnConfigChanged`
  callback) ≤ 2 s in the local development tree.
- **NFR-2** TTL job latency: an override with `expires_at = T` is cleared
  by the next scheduler tick after `T`; worst-case 60 s past expiry.
- **NFR-3** Every set / clear / auto-expire / force-sync writes exactly one
  `admin_audit_log` row.
- **NFR-4** Existing client (`thingclient`) requires no schema change to
  support overrides — wire format and merge semantics are unchanged.
- **NFR-5** All UI strings localized (en / zh / es).
- **NFR-6** RBAC enforced server-side: `provider_admin` cannot override
  agent Things; `compliance_officer` cannot override service Things.

## 4. User Roles & Personas

| Role | Can read overrides | Can write overrides | Can force-sync |
|------|:------------------:|:-------------------:|:--------------:|
| `super_admin`        | All types       | All types       | All types       |
| `provider_admin`     | All (read-only) | Service types   | Service types   |
| `compliance_officer` | All (read-only) | Agent type only | Agent type only |
| `viewer`             | All (read-only) | —               | —               |

Type-scope filtering happens in the CP handler layer (consistent with
`thing-model.md` §11). No DB-level row security.

## 5. Constraints & Assumptions

- **Pre-GA, no backward compatibility shims**: schema additions only;
  existing `thing.desired` semantics clarified, not changed.
- **No phased rollout / no feature flag**: rollback is `git revert`.
- **No data migration**: new table starts empty.
- **`reason` ≤ 500 chars**: enforced by DB CHECK and handler validation.
- **TTL range [5 m, 30 d]**: enforced server-side. Outside range → 400.
- **`state` must be a JSON object at top level** — array / scalar / null
  rejected with 400.
- **Override on a key that has no template**: rejected with 400 (cannot
  override what was never templated).

## 6. Glossary

| Term | Definition |
|------|------------|
| **Override** | A per-Thing JSON payload that fully replaces the template state for one `config_key` on one Thing. |
| **Template** | The per-`(type, config_key)` default state stored in `thing_config_template`. |
| **Stale (override)** | An override whose `template_ver_at_set` is lower than the current `thing_config_template.version` for the same `(type, key)`. |
| **TTL** | Optional `expires_at` timestamp on an override; auto-cleared by the Hub scheduler. |
| **Break-glass** | An emergency override, identified by `configKey == "killswitch"` or `reason` starting with `break-glass:`. Stamps `emergency_override = true` in audit. |
| **Force-sync** | Hub-initiated redelivery of `desired` state to a Thing with `Force = true` on the wire, causing the Thing to re-run `OnConfigChanged` and emit a fresh shadow_report regardless of version equality. |

## 7. Priority

All requirements are **Must** for E34-S1 (single-spec scope). Nothing is
deferred — the design is greenfield per the project's pre-GA "no backcompat,
no defer" policy.
