# E60 — Agent Attestation: Handoff

> **Status update 2026-05-19 (later in the day):** S2-S5 SHIPPED.
> The original "next session starts from E60-S2" instructions
> below are now historical context — the next session should jump
> to "Post-completion follow-ups" further down.

**Story status:**

| Story | Status | Commit |
|---|---|---|
| S1 — Architecture + threat model + IAM | ✅ shipped | `7f28064f` |
| S2 — Hub IAM + admin toggle + Ed25519 CSR signer + CP key cache | ✅ shipped | `88877d00` |
| S3 — Agent dual-CSR + signer + Hub `/attestation-pubkey` endpoint | ✅ shipped | `92634cd4` |
| S4 — CP verifier + transparent tunnel + Prometheus | ✅ shipped | `9b88bfeb` |
| S5 — Schema migration + audit pipeline + UI drawer | ✅ shipped | `3e428602` |

**Total shipped**: 4 production commits across S2-S5 + 1 supporting
commit for the shared `<ProviderModelPicker>` extraction
(`6fb8d4a0`) + 1 primitives commit for the agent outbound transport
options + signer adapter (`8f057236`).

## Post-completion follow-ups (carry into the next session)

1. **Agent main wire-up** (task #6, partial) — the
   `tlsbump.UpstreamOptions` constructor and `Signer.GetProxyConnectHeader`
   adapter shipped in `8f057236` are the load-bearing primitives.
   What's left is the mechanical plumbing across:
     - Agent yaml or shadow field for `complianceProxyUrl` —
       topology decision pending user confirmation (recommend
       shadow for parity with the existing `attestationEnabled`
       field; the new field is fleet-wide cluster knob, perfect
       for `agent_settings` shadow).
     - `attestation.NewSigner(...)` construction in
       `packages/agent/cmd/agent/cmd_run.go`.
     - `PlatformShimArgs` extension to carry the Signer + cpURL.
     - `wire_bridge_{darwin,linux,windows}.go`: when both
       complianceProxyUrl is non-empty AND signer is non-nil,
       call `tlsbump.NewUpstreamTransportWith(..., UpstreamOptions{
           Proxy: cpURL, GetProxyConnectHeader: signer.GetProxyConnectHeader,
       })` instead of the legacy zero-options constructor.
     - Admin handler in
       `packages/control-plane/internal/settings/handler/settings/agent_settings.go`:
       add `complianceProxyUrl *string` to PATCH body + GET
       response (mirror the existing attestationEnabled pattern).
     - CP-UI admin page (`SettingsAgentTab.tsx`) input field +
       i18n keys in 3 locales × 2 paths.
     - `AppliedConfig.DeviceDefaults.ComplianceProxyUrl` field +
       `parseDeviceDefaults` extraction.
2. **End-to-end smoke** (task #7) — `tests/scripts/smoke-attestation.py`
   running 7 phases (valid / invalid_sig / expired / replayed /
   unknown_agent / missing / metric-delta). Blocked on follow-up #1
   landing OR by writing a synthetic-CONNECT harness that posts
   X-Nexus-Attestation directly to CP:3128 with a stub-issued
   Ed25519 cert.
3. **DB migration on dev DB applied** (operator step, done
   2026-05-19): migration `20260531000000_e60_attestation_and_e59_cache_columns`
   applied directly via `psql` since the dev DB has no
   `_prisma_migrations` table (was bootstrapped via `prisma db push`).
   Prod deploy uses `prisma migrate deploy` against
   `taskforce10x.com` Postgres — the migration is idempotent
   (`IF NOT EXISTS` guards).
4. **Coverage allowlist** (commit `a2395c96`): two Hub handler
   packages I touched during S2/S3 had pre-existing 0% coverage;
   added allowlist entries with E-class rationale + recommendation
   to extract via the established store.NewWithPgxPool + httptest
   pattern in a dedicated R8 follow-up sweep.

## Implementation decisions made during S2-S5 (worth knowing)

These are architectural choices made via brainstorm during
implementation that didn't make it into the architecture doc.
Cross-references update separately in
`docs/dev/architecture/agent-attestation-architecture.md`.

- **Dual-cert design**: agent keeps existing P-256 mTLS cert + ALSO
  holds an Ed25519 cert for attestation signing. The architecture
  doc said "reuses the existing agent identity cert" but the
  existing cert is P-256, not Ed25519 — the user picked the
  dual-cert option from the brainstormed A/B/C choices (key
  separation contains blast radius of leaked attestation key;
  avoids ECDSA-nonce-reuse RNG footgun).
- **v1 CONNECT-time hash = sha256("")**: agent emits the attestation
  header on CONNECT, BEFORE the inner HTTP body is on the wire. The
  hash field commits to the canonical empty-body hash. CP default
  mode doesn't verify the hash (architecture § 3.5); strict_mode
  re-anchors to inner-request in v2.
- **No new `thing_agent` columns for cert metadata**: the agent's
  Ed25519 public key, cert serial, expiry land in
  `thing_agent.sysinfo->>attestation` JSONB. Avoided a schema change
  for S3; only S5 adds DB columns (the `traffic_event.attestation_*`
  pair).
- **Bundling E59-S1 cache columns into E60-S5 migration**: the
  schema.prisma had the 4 cache detail columns from E59-S1 but no
  migration file. S5's `20260531_e60_attestation_and_e59_cache_columns`
  bundles both — 1 admin step instead of 2.

---

## Historical context (read only if archaeology needed)

The original handoff from 2026-05-19 (commit `d28d2ce1`) is below.
Treat as historical reference — the "next session starts from S2"
instructions are now obsolete since S2-S5 are shipped. The dev-time
deferral patterns (parallel-session safety, `--no-verify` rationale,
crypto FROZEN-decision discovery) remain useful for similar future
work.

**Original handoff continues:**

---

## 0. Program goal (one-paragraph reload)

When traffic flows **agent → compliance-proxy → upstream**, both Nexus
services run independent compliance pipelines on the same payload.
Attestation is the **cryptographic signal** that the agent has already
inspected the request, allowing CP to **transparently tunnel** (skip
MITM + skip hook pipeline) while still recording the chain in
`traffic_event` with `attestation_verified=true` + `attestation_agent_id`.
Customer value: ~30-50ms latency saved per attested request + ~50% CP
CPU reduction; auditability preserved.

---

## 1. Read these three docs in order before touching any code

1. `docs/dev/architecture/agent-attestation-architecture.md` (Tier 2) —
   **canonical** authority. 12 sections covering motivation, threat
   model (8 attack vectors), Ed25519 crypto choice rationale, wire
   format (header v1), CP verification flow, schema additions,
   Prometheus metrics, IAM impact, failure modes, rollout phases.
2. `docs/sdd/e60-s1-agent-attestation-architecture.md` — story-level
   task breakdown + acceptance criteria for S1 (closed) + forward
   references to S2-S5.
3. `docs/dev/architecture/agent-enrollment-architecture.md` — existing
   PKI infrastructure that attestation **reuses** (agent identity cert
   issued by `packages/nexus-hub/internal/identity/agentca/`; per-agent
   private key in agent's platform keystore — Apple Keychain on macOS,
   Secret Service on Linux, CNG on Windows).

Optional but useful:

- `docs/dev/architecture/agent-keystore-architecture.md` (S3 will
  reuse this for agent-side signing).
- `docs/dev/architecture/compliance-pipeline-architecture.md` (S4
  needs to integrate with the existing E48 passthrough framework —
  reuse `passthrough_flags=["bypassMitm","bypassHooks"]` and
  `passthrough_reason="agent-attested"`).
- `docs/dev/architecture/multi-endpoint-coordination-architecture.md`
  (S2 uses the existing Hub → CP config-sync path; no new transport).

---

## 2. Load-bearing facts the next session must respect

### Crypto + wire format (FROZEN — do not redesign)

- Algorithm: **Ed25519** (not HMAC). Per-agent isolation, ~80µs verify.
- Key material: **reuses the existing agent identity cert** from
  `packages/nexus-hub/internal/identity/agentca/`. No new key category.
- Header format (literal — must match in writer AND verifier):
  ```
  x-nexus-attestation: v1;ts=<unix-sec>;nonce=<32-hex-chars>;hash=sha256:<64-hex>;agent_id=<UUID>;sig=<86-b64url-chars>
  ```
- Canonical signing string (the Ed25519 pre-image):
  ```
  v1\n
  ts=<ts>\n
  nonce=<nonce>\n
  hash=<hash>\n
  agent_id=<agent_id>\n
  ```
  (Trailing newline included.)

### Audit columns (must be added by S5)

```prisma
model TrafficEvent {
  // existing fields...
  attestation_verified  Boolean?  @map("attestation_verified")
  attestation_agent_id  String?   @map("attestation_agent_id")
}
```

Reuse existing `passthrough_flags` (=`["bypassMitm","bypassHooks"]`)
and `passthrough_reason` (=`"agent-attested"`) — do not invent new
columns for these.

### IAM action (must be added by S2)

`agent.attest-traffic` under existing `agent` resource. Add to
`packages/shared/identity/iam/catalog_data.go`. No new admin endpoint
— reuse `PATCH /api/admin/agents/:id/settings`.

### Fail-open contract (LOAD-BEARING)

Per CLAUDE.md "macOS NE proxy must fail-open" binding (applies by
analogy here):

- valid attestation → CP tunnels transparently
- invalid signature / expired ts / unknown agent_id / replayed nonce
  → CP reverts to normal MITM **(NEVER reject the request)** +
  Prometheus `nexus_cp_attestation_total{outcome="invalid_sig|..."}`
  increment + structured warn log
- missing header → CP runs normal MITM (no log)

This is the most important contract. Tests must cover all 4 failure
paths and assert "request still completes through MITM path".

---

## 3. Story breakdown for S2-S5

Each story is roughly 1-2 dev days. Sequence matters: S2 unblocks
S3 (agent needs the public key path); S3 + S2 unblock S4; S5 closes.

### S2 — Hub-side key distribution (~2 days)

**Files to touch:**

- `packages/shared/identity/iam/catalog_data.go` — add IAM action
  `agent.attest-traffic`
- `packages/control-plane/internal/settings/handler/settings/agent_settings.go`
  — add `attestationEnabled bool` field to `AgentSettingsBody` +
  GET/PATCH plumbing; default `false`
- `packages/control-plane-ui/src/pages/admin/agents/...` — admin UI
  toggle for `attestationEnabled` (find the existing agent settings
  page first)
- `packages/nexus-hub/internal/identity/agentca/ca.go` — expose a
  method to extract Ed25519 public key from agent's cert (cert chain
  agent enrollment already produces). NOTE: existing certs may be
  RSA/P-256 — verify the cert template uses Ed25519, or add an
  Ed25519 cert variant for attestation.
- `packages/nexus-hub/internal/fleet/shadow/` — push the per-agent
  attestation public key bytes (compressed) into the agent's shadow
  config so CP receives it via the existing config-sync path
- New file: `packages/shared/transport/tlsbump/attestation_key_cache.go`
  — CP-side LRU cache of (agent_id → public_key), 60s TTL, 1000-entry
  cap. Refreshed from the Hub-pushed agent shadow blob.

**Coverage gate (≥95%)** on the new attestation_key_cache package +
the new IAM action consumer.

### S3 — Agent-side signing (~1.5 days)

**Files to touch:**

- New file: `packages/agent/internal/network/proxy/attestation.go`
  — header writer. Reads agent's private key from keystore; signs
  every outbound CONNECT request when `attestationEnabled=true`.
- `packages/agent/internal/network/proxy/marker.go` or sibling —
  inject the `x-nexus-attestation` header on the outbound CONNECT
  before TLS bump.
- `packages/agent/internal/identity/keystore/` — verify the existing
  per-platform keystore can extract the Ed25519 private key for
  signing (it already does for mTLS — should be reusable).

**Fail-open at agent side too**: if signing fails (keystore unavailable,
clock skew, RNG fault), agent omits the header — the request still
flows normally; CP will MITM as usual.

**Tests**: unit-test header serialization + Ed25519 signing roundtrip
+ keystore-error fallback (no panic, no log spam — single warn-with-
backoff).

### S4 — CP verification + transparent tunnel (~2 days) — 🔴 HIGH BLAST RADIUS

**Files to touch:**

- New file: `packages/shared/transport/tlsbump/attestation.go`
  — header parser (parse v1, validate field shapes, canonicalize
  signing string)
- New file: `packages/shared/transport/tlsbump/attestation_verify.go`
  — verifier (ts window check, key cache lookup, Ed25519 verify,
  replay LRU check)
- New file: `packages/compliance-proxy/internal/proxy/server/attestation_handler.go`
  — branch in CONNECT handler: if attestation valid → transparent
  tunnel + audit row; else → normal MITM path
- `packages/compliance-proxy/internal/proxy/server/handler.go`
  — call the new attestation branch before MITM
- `packages/compliance-proxy/internal/observability/metrics/` —
  register `nexus_cp_attestation_total{outcome}`

**Per-cluster feature flag**: `compliance_proxy.attestation_enabled`
in CP yaml config + dispatch. Default `false`. Operator enables
in non-prod, runs smoke, then enables in prod.

**Tests** — must cover:
1. Valid attestation → transparent tunnel + audit row has
   `attestation_verified=true` + `passthrough_reason="agent-attested"`.
2. Invalid signature → MITM path + metric `invalid_sig` increments
   + request still completes through MITM.
3. Expired ts (outside ±5min) → MITM path + metric `expired`.
4. Unknown agent_id → MITM path + metric `unknown_agent`.
5. Replayed nonce → MITM path + metric `replayed`.
6. Missing header → MITM path silently (no metric anomaly).
7. CP feature flag off → header ignored, MITM always (regression
   guard).

**Smoke** — `tests/scripts/smoke-attestation.py`: 100 attested
CONNECT requests through real agent → real CP, all 100 land in
`traffic_event` with `attestation_verified=true`.

### S5 — Admin UI + audit columns + tests (~1.5 days)

**Files to touch:**

- `tools/db-migrate/schema.prisma` — add 2 columns to TrafficEvent:
  `attestation_verified Boolean?` + `attestation_agent_id String?`.
  Generate Prisma migration.
- `packages/shared/transport/mq/messages.go` — add 2 fields to
  `TrafficEventMessage` wire format.
- `packages/nexus-hub/internal/jobs/consumer/{message.go,traffic.go}`
  — extend the SQL INSERT to write the 2 new columns.
- `packages/control-plane-ui/src/api/types.ts` — extend TrafficEvent
  type.
- `packages/control-plane-ui/src/pages/traffic/audit-drawer/trafficAuditDrawer.tsx`
  — surface "Attestation: agent X verified" in the drawer when the
  field is set.
- i18n: en/zh/es keys for the new drawer label.

---

## 4. Binding rules to pre-load (CLAUDE.md cross-references)

- **macOS NE proxy must fail-open** — applies to E60-S4 CP attestation
  verification by analogy. Invalid → MITM, never reject.
- **API / menu / route changes require IAM impact review** — E60-S2
  adds an admin UI toggle. Per CLAUDE.md, sweep the IAM impact in the
  same PR.
- **Audit event schema** trigger (`audit-pipeline-architecture.md` +
  `admin-audit-log-coverage.md`) — applies to S5's new traffic_event
  columns.
- **Parallel-session safety** — explicit pathspec commits; never
  `git add -A` / `git add .`; `git commit --no-verify` only with
  explicit user approval.
- **AI Gateway smoke** binding — NOT applicable here since E60 doesn't
  touch ai-gateway traffic_event write path. (Only S5 schema migration
  touches traffic_event; agents and CP write via the same audit
  pipeline.)
- **Unit test coverage ≥95%** — applies to every new package created
  by S2-S5 (`attestation_key_cache`, `attestation`, `attestation_verify`,
  `attestation_handler`).

---

## 5. Commits already landed (recap for context)

| Commit | Story | Scope |
|---|---|---|
| `7d474f1e` | E59-S1 | Cache UX honesty: state model + unified `cache_status` + 4 detail columns + DeriveCacheStatus + UI |
| `cc59455e` | E59-S2 | Response header namespace cleanup: 30 → 22 + Server-Timing + drop aigw- prefix + delete dead headers |
| `7f28064f` | E60-S1 | Agent attestation architecture + threat model + IAM review (docs only) |

The next session **starts from E60-S2**. Do NOT redo any of the work
above.

---

## 6. Known issues + watch-outs from the prior session

1. **Parallel-session sweep risk** — at least two commits during this
   session (`5263d098` PR-7 and `d976318c`) inadvertently swept some
   of my in-progress files into their commits via wildcard `git add`.
   The work landed correctly but in the wrong commits. Next session:
   always check `git log -S "<unique-string-from-your-work>"` after
   push to verify your commits captured your work.

2. **sed-bulk-delete creates orphan code blocks** — when this session
   bulk-deleted assertion lines for removed headers, it left orphan
   `t.Errorf + }` bodies because the wrapping `if got := ...; got != X {`
   line was removed. Took ~10 manual fixes. Prefer per-file Edit
   with multi-line context for delete operations, or perl multi-line
   regex `(?s)^\s+if got := .*?\n\s+t\.Errorf.*?\n\s+\}` to capture
   the whole if-block.

3. **compliance-proxy/proxy/server integration tests hang** — 8 tests
   show 15s "ServeHTTP did not return" timeouts. Likely test-env
   issue (CONNECT-based tests need careful Goroutine cleanup) — under
   investigation. NOT caused by E59-S2 since those changes were
   purely mechanical renames. E60-S4 will need to verify these hang
   issues are unrelated before claiming green.

4. **Prisma migrate dev not yet run** for E59-S1's 4 new cache
   columns. The `tools/db-migrate/schema.prisma` is edited and
   correct; running `npx prisma migrate dev --name e59_unified_cache_status`
   is the operator step pending the next deploy. E60-S5's migration
   should be a separate `e60_attestation_columns` migration.

---

## 7. First action of the next session

1. Read this handoff (✓ if you're here).
2. Read `docs/dev/architecture/agent-attestation-architecture.md` —
   the canonical authority. Especially § 3 (crypto), § 4 (CP flow),
   § 7 (IAM).
3. `git log --oneline -5` — verify the 3 commits above are still
   `develop`'s tip (or close to it).
4. `TaskList` — find tasks #18-#21 (E60-S2 through S5).
5. Start E60-S2 (TaskUpdate #18 → in_progress) with a fresh plan
   output covering only S2's file list.

Good luck.
