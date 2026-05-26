# E60-S1 — Agent Attestation: Architecture + Threat Model + IAM Review

> Story: e60-s1
> Epic: 60 (Agent Attestation Trust Bypass)
> Status: Draft
> Architecture: `docs/developers/architecture/services/agent/agent-attestation-architecture.md` (canonical)
> Blocked by: none
> Blocks: E60-S2 (Hub key distribution), E60-S3 (agent signing), E60-S4 (CP verification), E60-S5 (admin UI + audit fields)

## User Story

As a Nexus operator running both Agent and Compliance-Proxy on the same
traffic path, I want a cryptographically-signed way for the agent to
declare "I already inspected this request" so the compliance-proxy can
**transparently tunnel** (skip MITM + skip hook pipeline) without
losing the audit trail that proves which agent attested it.

Currently every agent-CP traffic hop pays the full MITM + hook cost
twice — once at the agent, once at CP. Attestation makes the chain
"trust but verify": the agent signs, CP verifies, CP skips the
redundant work, both services still emit the same `traffic_event` row
correlated by `attestation_agent_id`.

## Tasks

### T1 — Architecture document (THIS STORY's core deliverable)

- T1.1 ✅ Create `docs/developers/architecture/services/agent/agent-attestation-architecture.md`
  covering: motivation (PM lens), threat model (8 attack vectors +
  mitigations), crypto design (Ed25519 with rationale vs HMAC),
  wire format (header v1), CP verification flow (fail-closed semantics),
  schema additions (`traffic_event.attestation_*`), Prometheus metrics,
  IAM impact, failure modes, rollout phases, open questions.
- T1.2 ✅ Add row to `docs/developers/architecture/README.md` per
  CLAUDE.md "every new architecture doc requires a trigger row".

### T2 — IAM impact review (per CLAUDE.md mandatory IAM review on
capability additions)

- T2.1 New IAM action `agent.attest-traffic` under existing `agent`
  resource. Scope: per-agent capability flag in `agent_settings` Thing
  shadow.
- T2.2 No new admin API endpoint — reuse existing
  `PATCH /api/admin/agents/:id/settings` flow.
- T2.3 Document the action in `packages/shared/identity/iam/catalog_data.go`
  (E60-S2 task — captured here as a forward dependency).

### T3 — Audit-pipeline impact review (per CLAUDE.md "Audit event schema"
trigger)

- T3.1 Two new columns on `traffic_event`:
    - `attestation_verified BOOLEAN NULL`
    - `attestation_agent_id TEXT NULL`
- T3.2 Reuse existing `passthrough_flags` / `passthrough_reason`
  columns with values `["bypassMitm","bypassHooks"]` and
  `"agent-attested"` respectively.
- T3.3 The schema migration is bundled into E60-S5 along with the
  admin UI surface.

### T4 — Crypto choice review

- T4.1 ✅ Ed25519 selected (rationale in architecture doc § 3.1).
  Per-agent isolation outweighs the ~70µs verify cost vs HMAC.
- T4.2 Reuses existing agent identity cert from
  `packages/nexus-hub/internal/identity/agentca/` — no new key
  category, no new distribution path, no new rotation policy.

### T5 — Threat-model sign-off

- T5.1 ✅ Architecture doc § 2 enumerates 8 attack vectors:
  forgery, replay (different body), replay (same body), key
  compromise, downgrade, DoS via invalid attestations, cross-agent
  replay, time skew, body modification mid-flight. Each has an
  explicit mitigation.
- T5.2 Fail-open contract (per CLAUDE.md "NE proxy must fail-open"
  binding) explicitly enforced: invalid attestation → revert to MITM,
  never reject the request. Metric + warn log on the anomaly.

### T6 — Rollback plan

- T6.1 Per-cluster feature flag `compliance_proxy.attestation_enabled`
  (default `false`). Operator can disable cluster-wide instantly.
- T6.2 Per-agent toggle `attestationEnabled` (default `false`).
  Operator can disable per-agent.
- T6.3 Schema-rollback: `attestation_verified` is nullable — keeping
  the column adds no operational risk; explicit DROP is the rollback.

## Acceptance Criteria

| ID | Acceptance |
|---|---|
| AC-1 | Architecture doc exists at `docs/developers/architecture/services/agent/agent-attestation-architecture.md` covering all sections enumerated in T1.1. |
| AC-2 | `docs/developers/architecture/README.md` has a row pointing at the new arch doc per CLAUDE.md lockstep rule. |
| AC-3 | Threat model enumerates the 8 attack vectors with explicit mitigations + a "fail-open never blocks the request" guarantee. |
| AC-4 | Crypto choice (Ed25519 over agent identity cert) is documented with rationale; reuses existing PKI (no new key category). |
| AC-5 | IAM impact: new action `agent.attest-traffic` is documented; no new admin endpoint required. |
| AC-6 | Audit-pipeline impact: 2 new `traffic_event` columns + reuse of `passthrough_flags`/`reason` are documented. |
| AC-7 | Rollout plan covers per-cluster + per-agent feature flags + per-cert key-rotation behavior. |

## Testing strategy

This story is documentation-only — no code; no tests added in S1.
S2-S5 ship the implementation + tests.

## Rollback plan

Documentation revert is `git revert` of this story's commit. No code,
no schema, no infrastructure changes in S1.

## Out of scope

- Signed **responses** (only requests carry attestation in v1).
- Custom-bundle / open-source agents without Hub-issued identity certs
  (deferred to v2).
- HSM-backed agent keys (deferred to v2 — wire format is unchanged).
- Streaming-response signing (out of scope for v1; agent's hooks ran
  on the request only).

## Next stories

- **E60-S2** — Hub-side key management: per-agent `attestationEnabled`
  flag, public-key distribution via existing shadow path, admin
  toggle.
- **E60-S3** — Agent-side per-request signing.
- **E60-S4** — CP verification + transparent tunnel + Prometheus metric.
- **E60-S5** — Admin UI traffic-drawer surfacing + audit columns
  schema migration + smoke test.
