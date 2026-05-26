---
doc: agent-attestation-architecture
area: service
service: agent
tier: 1
updated: 2026-05-21
---

# Agent Attestation — Architecture

> **Tier 2 architecture doc.** Read this before touching the
> `x-nexus-attestation` header writer (agent side) or verifier (CP side),
> or before changing the agent identity-cert lifecycle as it pertains to
> attestation signing. The Compliance-Proxy MITM bypass path is a
> high-blast-radius surface — every change here lands together with the
> CLAUDE.md "macOS NE proxy must fail-open, never fail-closed" review.

**Last updated:** 2026-05-19 (E60 implementation complete + post-impl
architectural correction).
**Status:** Implementation shipped across E60-S2 through E60-S5 plus
a corrective refactor on the same day. Three implementation
decisions diverged from the literal S1 architecture text — captured
below in **§ 0.1**. The corrective refactor (§ 0.2) changed the
**detection topology** and the **header transport** to match a
principle the team articulated mid-implementation: *agent doesn't
know about CP; CP is the detector*.

## 0.1 Implementation deviations from the S1 freeze

The S1 architecture was approved on the basis of two claims that
turned out to be mutually inconsistent once implementation started:

- The freeze said *"Ed25519 signature"* (§ 3.1) AND *"reuses the
  existing agent identity cert"* (§ 3.2).
- The existing agent identity cert is **ECDSA P-256**
  (`packages/nexus-hub/internal/identity/agentca/ca.go:154`). You
  cannot sign Ed25519 with a P-256 key, so the two claims couldn't
  both be true.

The discovery surfaced during S2 implementation. The user picked the
**dual-cert option** from three brainstormed alternatives:

| Option | Key separation | mTLS regression risk | ECDSA-nonce footgun risk | Code blast radius |
|---|---|---|---|---|
| **A. Dual-cert (CHOSEN)** | ✅ mTLS key + attestation key isolated | none (mTLS path unchanged) | none (Ed25519 deterministic) | small (+30 LoC in agentca) |
| B. Switch existing cert to Ed25519 | ❌ same key for handshake + payload signing | high (TLS 1.3 compat needed on all OS keystores) | none | large (mTLS chain) |
| C. Use ECDSA P-256 for attestation | ❌ same as B | none | **HIGH** (ECDSA per-sig nonce reuse leaks key) | small |

Decision rationale: Option A wins both **safer** (proper key
separation per NIST SP 800-57; no ECDSA RNG footgun; mTLS
unaffected) AND **smaller blast radius** (one new `SignAttestationCSR`
helper in `agentca`, no mTLS chain changes). Implementation:
agent dual-CSR enrollment, Hub signs both via existing CA, agent
holds P-256 mTLS key + Ed25519 attestation key in separate disk
files. Captured in commit `92634cd4` (E60-S3).

## 0.2 Topology — agent unaware, CP detects

**Agent does not know whether CP is in the path** — that's a property of
the enterprise network (DNS / router / firewall), not something the agent
should be configured with. The agent's outbound traffic is just direct
TLS to the upstream provider; whether CP transparently intercepts at the
network layer is invisible to the agent.

That principle forces two design changes:

1. **Header rides on the inner HTTP request, not on CONNECT.**
   Agent emits a normal `TCP+TLS` direct-dial to
   `api.openai.com:443`. There's no CONNECT line for CP to peek
   (CONNECT is only emitted when an HTTP proxy is configured —
   and the agent has no HTTP proxy because it doesn't know
   about CP). CP, when transparently in path, bumps the TLS,
   sees the inner HTTP request, and finds the `X-Nexus-Attestation`
   header among the request headers.
2. **CP, on detection, becomes pure passthrough.** Once CP
   verifies the header, it forwards the request upstream and
   streams the response back without running its hook pipeline,
   without writing a `traffic_event` row, without capturing
   payload bytes. The agent's own audit row is the
   system-of-record for that flow; a CP-side row would be
   duplicate noise.

Implications for the wire format + perf claim:

- **Wire format unchanged**: the header bytes are still
  `v1;ts=<…>;nonce=<…>;hash=sha256:<64-hex>;agent_id=<UUID>;sig=<86b64url>`
  with the same canonical signing pre-image. Only the header's
  HTTP layer moves from CONNECT to inner request.
- **`hash` field now binds to real body bytes**, not the placeholder
  `sha256("")`. The agent's request injector
  (`packages/agent/internal/identity/attestation/signer.go` —
  `Signer.InjectInto`) reads the request body, computes the
  hash, and rewraps the body so the wire send sees identical
  bytes. Bodies above the 8 MiB injector cap fall back to the
  empty-body hash (documented and small in practice — AI
  traffic bodies are typically <1 MiB).
- **Perf claim revised**: the original "~30-50ms latency saved
  per attested request" came from skipping the second TLS
  handshake. Under the corrected design CP **still bumps TLS**
  (it has to, to see the header). The remaining savings:
  - Hook pipeline CPU (request + response stages)
  - Payload-capture body buffering + spill write
  - Audit emission (no CP-side `traffic_event` row written)
  - Normalize / extract / detect calls on bumped bodies

  Order-of-magnitude estimate: 5-15% CP CPU reduction at typical
  per-request hook-pipeline depth (1-3 hooks); higher on routes
  with heavy normalize codecs (e.g., Anthropic Messages with
  big system blocks).
- **Threat model unchanged** (every § 2 mitigation still holds —
  forgery still requires the agent's private Ed25519 key, replay
  still requires capturing within the 5-min window, etc.).

Implementation surface for § 0.2:
- Agent: `Signer.InjectInto(*http.Request)` adapter +
  `UpstreamOptions.RequestInjector` callback on
  `tlsbump.UpstreamTransport.ForwardRequest`. Agent main wires
  the closure when `attestationEnabled` is true and an Ed25519
  cert is on disk.
- CP: `tlsbump.WithAttestationVerifier(fn)` BumpOption +
  per-request peek in `tlsbump.buildForwardHandler` BEFORE the
  compliance pipeline build. Valid → `attestationPassthrough`
  helper (just `upstream.ForwardRequest` + `copyResponse`, no
  hooks, no audit). Invalid/missing → existing MITM path
  unchanged.
- Removed: the CONNECT-line peek in
  `compliance-proxy/internal/proxy/server/server.go` ServeHTTP
  + the `EmitAttestationPassthrough` audit emitter (dead code
  after the "no CP audit row" decision).

`AuditEvent.AttestationVerified` / `AuditEvent.AttestationAgentID`
+ `mq.TrafficEventMessage.Attestation*` + the corresponding
`traffic_event` DB columns stay in place as **reserved schema**.
v1 CP never writes them; a future strict_mode CP that wants to
record a minimal "I saw an attested flow" row (for security audit
purposes, distinct from the policy-decision row) can populate them
without a schema migration.

---

## 1. Why this exists (PM lens)

When a request flows **agent → (transparent CP in path) → upstream**, both Nexus
services would otherwise run independent compliance pipelines on the same
payload. After agent has already terminated TLS, inspected the payload, run
its hook pipeline, and applied policy decisions, the compliance-proxy in the
transparent path would re-do everything:

1. Bump the TLS handshake to inspect inner bytes
2. Re-parse the request body
3. Re-run the hook pipeline
4. Re-classify against domain rules
5. Buffer + spill payload bytes
6. Emit its own `traffic_event` row

CP still has to bump TLS to see the attestation header (per §0.2), so the
TLS handshake cost is **not** removed. What attestation removes is the
duplicated *content* work: hook execution, payload capture, normalize/extract,
and the duplicate audit row. The agent's audit row remains the system-of-record
for the flow.

### Customer value (corrected — see §0.2)

- **Cost: 5-15% CP CPU reduction** at typical per-request hook-pipeline depth
  (1-3 hooks); higher on routes with heavy normalize codecs (e.g., Anthropic
  Messages with big system blocks). Order-of-magnitude estimate, dominated
  by hook-pipeline CPU + payload capture/spill work that CP no longer does
  on an attested flow.
- **Audit cleanliness**: one row per logical request (the agent's), instead
  of two duplicate rows. Compliance teams can still filter "all attested
  traffic from agent X" via the reserved `attestation_verified` /
  `attestation_agent_id` columns the v1 schema carries (see §5; v1 CP does
  not populate them today — reserved for future strict_mode).

The original perf claim ("typical chat completion saves 30-50ms wall-clock"
plus "~50% CP CPU reduction") came from the pre-0.2 design where CP could
skip the second TLS handshake. Under the corrected topology CP still bumps,
so the latency claim is dropped and the CPU figure is revised down.

### Non-goals

- This is **not** an authorization mechanism — attestation says "this
  request was inspected", not "this request is allowed". Policy / hook
  decisions remain enforced by whichever service ran them.
- Attestation does **not** bypass `x-nexus-via` or `x-nexus-request-id`
  observability — those still flow.
- Not in scope: signed responses (only requests carry attestation).

---

## 2. Threat model

| Threat | Severity | Mitigation |
|---|---|---|
| **Forgery** — attacker sets `x-nexus-attestation: anything` to bypass CP | Critical | Ed25519 signature over (timestamp, nonce, body_hash, agent_id). CP verifies via agent public cert. Fail-closed: invalid sig → revert to normal MITM (do not reject the request — see Downgrade below). |
| **Replay** — attacker captures a valid attestation, sends with different body | Critical | Header includes `hash=sha256(body)` bound to the same signature. CP recomputes hash after reading body; mismatch → invalid. |
| **Replay (same body)** — attacker re-sends the exact captured packet | High | Header `ts` (Unix seconds) plus CP-side replay-window check (default ±5 minutes); plus `nonce` (16 random bytes) tracked in a sliding-window LRU on CP for the same 5-min window. |
| **Key compromise** (agent private key stolen) | High | Per-agent identity certs already rotate via Hub (E48 PKI); attestation reuses the same cert lifecycle. Compromised agent's cert can be revoked via Hub admin UI; CP queries an in-process cert-validity cache (60s TTL); revocation propagates within one TTL window. |
| **Downgrade** (attacker strips the header) | Medium | Default CP behavior is to MITM when no attestation present. Attacker-stripped header lands in the same path as "agent didn't send one" — no security loss, only the perf benefit is lost. |
| **DoS via invalid attestations** | Low | Verification is per-request CPU. Bad signatures fall through to normal MITM (no extra cost vs the un-attested case). Add `nexus_cp_attestation_total{outcome="invalid_sig"}` counter — operator alert on sustained rate. |
| **Cross-agent replay** (Agent A's attestation reused for Agent B's traffic) | Low | Header includes `agent_id`; CP verifies signature with Agent A's public cert. If Agent B's traffic carries Agent A's signed token, the body_hash check fails (or the agent_id mismatches the connection's source identity). |
| **Time skew** | Low | CP and agents NTP-synced; ±5-min window covers typical drift. Outside the window: header rejected as expired (counted as `outcome="expired"`). |
| **Body modification between agent signing and CP verification** | Critical (correctness) | `hash=sha256(body)` is over the **exact bytes** agent signed. Any intermediate hop modifying the body invalidates the attestation. By design — agent's policy decision applies only to the body it inspected. |

**Failure mode contract**: an invalid / expired / missing attestation
header NEVER blocks the request. It only changes the CP code path:

- valid → CP tunnels transparently + audits `passthroughReason=agent-attested`
- invalid → CP runs normal MITM + emits `attestation_invalid_sig` metric + structured warn log
- missing → CP runs normal MITM (default, no logging anomaly)

This matches the CLAUDE.md "fail-open, never fail-closed" pattern that
governs the macOS NE proxy. Attestation is a perf optimization, not a
gate.

---

## 3. Cryptographic design

### 3.1 Why Ed25519 (not HMAC)

| Property | Ed25519 ✓ | HMAC |
|---|---|---|
| Per-agent isolation | Each agent has its own key pair; compromise of one agent doesn't affect others | Shared secret across all agents — single key compromise = full bypass |
| Rotation | PKI infra already in place (`packages/nexus-hub/internal/identity/agentca`) | New shared-secret distribution system would be needed |
| CPU cost | ~30µs sign, ~80µs verify (per request, x86_64) | ~1µs (much cheaper) |
| Crypto reuse | Reuses the agent identity cert (E48 enrollment lifecycle) | Net-new secret category |
| Audit | `agent_id` in header is verifiable from cert | `agent_id` in header is just a claim, no signature binding |

The HMAC option saves ~70µs/request but introduces a shared-secret
attack surface that's catastrophic to operate (one compromised agent
machine = system-wide bypass). Ed25519's per-agent isolation is the
right trade-off; ~80µs verify is negligible vs the 30-50ms MITM cost
saved.

### 3.2 Key material

Reuses the **agent identity cert** issued by Nexus Hub during enrollment
(see `agent-enrollment-architecture.md`). The cert's private key is
already used for the agent → Hub mTLS handshake; no new keystore entry
or distribution path is needed.

- Agent: signs attestation tokens with the same private key that
  authenticates the mTLS connection to Hub.
- Nexus Hub: already has the agent's public cert. Pushes a per-agent
  attestation-enabled flag + public key fingerprint to CP via the
  existing config-sync path (Thing shadow).
- CP: receives the per-agent public key (extracted from the cert chain
  Hub already distributes), caches in-process (60s TTL, LRU bound).

**Cert renewal** triggers an automatic key rotation: the old key
remains valid until the cert's `NotAfter` (typically 90 days), so
in-flight attestations signed by the previous key still verify until
the cert expires.

### 3.3 Header format (v1)

```
x-nexus-attestation: v1;ts=1716100000;nonce=ab12cd34...;hash=sha256:abc123...;agent_id=550e8400-...;sig=base64url(Ed25519-sig)
```

Fields are semicolon-separated key=value pairs:

| Field | Format | Length | Notes |
|---|---|---|---|
| `v1` | literal | 2 chars | version prefix; v2 onwards adds new fields rather than changing existing ones |
| `ts` | Unix seconds, base 10 | 10 chars (until year 2286) | replay-window anchor |
| `nonce` | hex | 32 chars (16 bytes random) | replay protection; CP tracks (ts, nonce) tuples in a 5-min LRU |
| `hash` | `sha256:` + hex | 71 chars | SHA-256 over the exact request body bytes that agent signed |
| `agent_id` | UUID | 36 chars | agent's identity cert CN / SAN URI |
| `sig` | base64url(64 bytes) | 86 chars | Ed25519 signature over the canonical signing string |

Total header value length: ~260 bytes. Well under typical 4-8KB header
budget.

### 3.4 Signing string (canonical pre-image)

The signature is computed over a single newline-separated string:

```
v1\n
ts=<ts>\n
nonce=<nonce>\n
hash=<hash>\n
agent_id=<agent_id>\n
```

(Trailing newline included.) This canonical form means a future v2
extension can add fields without affecting v1 verification semantics.

**Why not JWT** (JOSE)? JWS would add ~120 bytes of base64-encoded
header + protected-claims overhead, and the JOSE format invites
mistakes (alg confusion attacks, etc.). The hand-rolled format is 2-3×
smaller and locks down a single signature algorithm. CP-side
implementation is ~80 lines of Go (parse → verify) vs the JOSE library
dependency.

### 3.5 Body hashing

`Signer.SignForBody(body []byte)` (and `Signer.InjectInto(*http.Request)` which calls it) hash the **real request body bytes** the agent is about to send. The `hash` field in the wire header is `sha256:<hex>` over those bytes. This is the v1 production behaviour — there is no "trust-without-hash-verify" default mode; the agent always commits to real bytes when it has them.

Body buffering rules in `Signer.InjectInto`:

- The injector reads `req.Body` fully into memory (capped at `maxBodyForHash = 8 MiB`), hashes it, and rewraps the request with a `bytes.Reader` so the downstream wire send sees identical bytes. `req.GetBody` is reinstated to return a fresh reader for any retry path.
- **Bodies up to 8 MiB**: hashed + committed. Normal AI traffic bodies sit well under this cap (typically <1 MiB), so this is the universal path.
- **Bodies above 8 MiB**: fall back to the empty-body hash (`sha256("")`) and rewrap the buffered prefix + remaining stream so the wire send still gets the full body. The cap exists to bound injector memory on a runaway client; tagged for follow-up to a streaming-hash `io.TeeReader` if real traffic hits the limit.
- **Streaming/unbounded bodies**: signed with the empty-body hash because the injector cannot consume them without breaking the flow. Future strict_mode v2 will re-anchor; v1 CP default mode tolerates this.

`Sign()` (no body) is the same code path with `body == nil` and is used in the rare cases where the caller hasn't buffered the request bytes.

Fail-open contract (load-bearing): ANY error in `InjectInto` (read failure, sign failure, disabled, missing key) returns `nil`. The request still forwards, just without the attestation header; CP that receives an unattested request runs its normal MITM pipeline.

---

## 4. CP verification flow

The agent emits a direct TLS dial to the upstream provider (no CONNECT — there's no HTTP proxy configured because the agent doesn't know about CP). CP, transparently in path, terminates the TLS handshake, reads the inner HTTP request, and finds the header among the request headers.

```
TCP+TLS to api.openai.com:443   (no CONNECT line)
   ↓
CP bumps TLS, reads inner request:

  POST /v1/chat/completions HTTP/1.1
  Host: api.openai.com
  X-Nexus-Attestation: v1;ts=...;nonce=...;hash=...;agent_id=...;sig=...

  ↓
CP handler peek (packages/compliance-proxy/internal/proxy/server/server.go
  + tlsbump.WithAttestationVerifier callback wired into
  tlsbump.buildForwardHandler BEFORE compliance pipeline build)
  ↓
1. Parse header (parseAttestationV1)
   - Missing → MITM path (default, no log)
   - Malformed → invalid_sig metric + warn log + MITM
2. Check ts within ±5min of CP wall-clock
   - Out of window → expired metric + warn log + MITM
3. Look up agent_id → public key (in-process LRU, 60s positive TTL / 5s
   negative TTL, cap 1000 entries; Hub-published. Defaults wired in
   `packages/shared/transport/tlsbump/attestation_key_cache.go:25-27`)
   - Unknown agent_id → invalid_sig metric + warn log + MITM
4. Verify Ed25519(canonical_signing_string, public_key, sig)
   - Sig invalid → invalid_sig metric + warn log + MITM
5. Check (ts, nonce) not in CP's 5-min replay LRU (default window 5min,
   cap 200_000 entries —
   `packages/shared/transport/tlsbump/attestation_replay_cache.go:40-41`)
   - Duplicate → replayed metric + warn log + MITM
   - Fresh → insert into LRU
  ↓
6. All checks pass → CP becomes pure passthrough:
   - attestationPassthrough helper: upstream.ForwardRequest + copyResponse
   - NO compliance pipeline build, NO hook execution
   - NO traffic_event row written, NO payload capture, NO spill write
   - The agent's own audit row is the system-of-record for this flow
```

**No CP audit row on attested flows.** The original §4 emitted a `passthrough` row from `audit.Stamp(...)` with `attestation_verified=true`. After the 2026-05-19 correction this is removed (`EmitAttestationPassthrough` is dead code), because a CP-side row would just be duplicate noise relative to the agent's audit row. The schema columns (`attestation_verified`, `attestation_agent_id`, `mq.TrafficEventMessage.Attestation*`) remain in place as **reserved** — a future strict_mode CP that wants a minimal "I saw an attested flow" security-audit row can populate them without a schema migration.

**Caching the (ts, nonce) LRU**: in-process map, capped at 200K entries, TTL 5 minutes. Memory bound: ~32 bytes/entry → ~6.4MB. At 1000 req/s the LRU holds ~300K entries — bump the cap if 1000 req/s sustained.

---

## 5. Schema additions to `traffic_event`

```prisma
model TrafficEvent {
  // existing fields ...

  // E60: agent attestation
  attestation_verified  Boolean?  @map("attestation_verified")
  attestation_agent_id  String?   @map("attestation_agent_id")
}
```

- `attestation_verified` is `true` when CP transparently tunneled
  because of a valid attestation; `false` when an attestation header
  was present but failed verification; `null` when no header was sent
  (the dominant case for non-attested agents).
- `attestation_agent_id` is set when `attestation_verified = true`;
  links the row to the agent's identity cert (queryable in admin UI as
  "all events attested by this agent").
- Existing `passthrough_flags` / `passthrough_reason` columns reused:
  flags = `["bypassMitm","bypassHooks"]`, reason = `"agent-attested"`.

---

## 6. Prometheus metrics

```
nexus_cp_attestation_total{outcome="valid|invalid_sig|expired|replayed|unknown_agent|missing"}
```

- `valid` increments when verification succeeds + CP tunnels
- `invalid_sig` / `expired` / `replayed` / `unknown_agent` each trip
  CP-side warn logs and increment the counter
- `missing` (no header present) increments quietly; allows operators to
  measure attestation coverage (`valid / (valid + missing)`)

Alert: ratio `invalid_sig + expired + replayed + unknown_agent` over
total non-missing requests > 1% sustained for 5 minutes → page CP
on-call (indicates either a bad agent rollout or a forgery attempt).

---

## 7. IAM impact review

**New IAM action**: `agent.attest-traffic`

- Scope: per-agent capability flag in `agent_settings` Thing shadow
- Stored as: boolean `attestationEnabled` in the agent settings JSON
- Admin UI: toggle in "Agent Settings → Compliance" — defaults to
  `false` until the operator explicitly opts the agent into attestation
- Granted by: only users with `admin.agents.write` IAM action (existing)
- Per CLAUDE.md "API / menu / route changes require IAM impact review":
  this story adds a new agent-side capability flag + an admin UI toggle
  but does not add a new admin API endpoint — uses the existing
  `PATCH /api/admin/agents/:id/settings` flow.

**No new IAM resource** — `agent.attest-traffic` is a capability under
the existing `agent` resource.

**Audit considerations** (per CLAUDE.md mandatory audit-pipeline arch
doc trigger):
- Enabling/disabling attestation per agent generates an
  `AdminAuditLog` entry (existing — uses the standard settings-update
  audit path).
- Every `attestation_verified` event lands in `traffic_event` with the
  source attribution + agent_id, supporting "list every attested
  request by agent X" SQL queries.

---

## 8. Failure modes

| Failure | Behavior | Recovery |
|---|---|---|
| Agent's identity cert expired | Agent re-enrolls via existing flow; old attestations expire naturally at the cert's `NotAfter` | Auto — no operator action |
| CP's cached public key stale (cert rotated) | Verification fails for the new key's signatures until cache TTL expires; falls back to MITM | Within 60s the cache refreshes from Hub |
| Hub down (CP can't fetch new agent keys) | CP keeps using its in-process cache; misses any keys issued during the outage; agents with new certs fall back to MITM | When Hub recovers, cache refreshes naturally |
| Clock skew > 5min on agent | Attestation rejected as expired; MITM path engaged | Operator alert via metric; agent reboots NTP sync |
| Same nonce reused (broken agent RNG) | Second use rejected as replayed | Operator alert; agent restart should fix; if persistent, investigate agent platform crypto |
| 1000 req/s sustained → LRU eviction | Genuine old (ts, nonce) tuples evicted before 5min TTL; could allow narrow replay window | Increase LRU cap; under normal load not reachable |

---

## 9. Test plan (cross-references to E60-S5)

1. **Unit tests (Go)** — `packages/shared/transport/tlsbump/attestation_test.go`:
   - parse v1 header (happy + 6 malformed variants)
   - verify-passes for valid Ed25519 signature
   - verify-rejects (8 cases: bad sig / bad hash / expired ts / future ts / unknown agent_id / wrong key / mangled b64 / wrong version)
   - replay LRU: insert + reject-on-duplicate + TTL eviction
2. **Integration test** — CP end-to-end with a stubbed agent identity:
   - valid attestation → CONNECT → transparent tunnel → audit row has `attestation_verified=true`
   - invalid attestation → CONNECT → normal MITM → audit row has `attestation_verified=false`
   - missing → CONNECT → normal MITM → audit row has `attestation_verified=null`
3. **Smoke** — `tests/scripts/smoke-attestation.py`:
   - Send 100 attested CONNECT requests through a real agent → real CP, assert all 100 have `passthroughReason=agent-attested` in traffic_event
4. **Negative smoke** — forge a fake attestation header from a non-agent client; assert CP rejects with `invalid_sig` metric increment.

---

## 10. Rollout phases (cross-references to E60-S2..S5)

1. **S2 — Hub-side**: agent attestation key distribution via existing
   shadow path. Per-agent `attestationEnabled` flag in
   `agent_settings`. New admin API endpoint? **No** — reuse
   `/api/admin/agents/:id/settings`.
2. **S3 — Agent-side**: signing of every outbound request when
   `attestationEnabled=true`. Fail-open if signing fails (don't block
   the request — agent's hooks still ran).
3. **S4 — CP-side**: verification + transparent tunnel. **Highest
   blast radius** — gate behind a CP-side feature flag
   (`compliance_proxy.attestation_enabled`) defaulting to `false`.
   Operator can enable in non-prod, run smoke, then enable in prod.
4. **S5 — Admin UI + audit fields + Prometheus**: surface
   `attestation_verified` in admin traffic drawer; add the Prometheus
   counter; document for compliance teams.

**Rollback**: per-agent toggle off; per-cluster feature flag off. No
schema rollback needed — `attestation_verified` is nullable.

---

## 11. Open questions tracked here

- **Streaming requests** — current design covers single-shot HTTPS
  requests (POST /v1/chat/completions, etc.). For SSE / streaming
  CONNECTs, the body hash is over the request body only (response is
  not signed). This matches the "attestation is about agent's
  inspection of the request" semantics. Out of scope for v1: signed
  responses.
- **Custom-bundle agents** (E62 future) — if Nexus ships agent
  variants without a Hub-issued identity cert (e.g., open-source
  community agents), they cannot attest. Documented limitation; v2
  could add an external-CA mode.
- **HSM-backed agent keys** — current PKI uses agent platform
  keystore (Apple Keychain, Linux Secret Service, Windows CNG). If
  enterprises demand HSM-backed agent keys, the signing path is
  unchanged — only the key-access wrapper changes. Out of scope for v1.

When any of these become real requirements, file an architecture-doc
PR before changing crypto / wire formats.

---

## 12. Where to read next

- `agent-enrollment-architecture.md` — the existing PKI infrastructure
  attestation reuses (cert issuance, agent identity, key storage,
  Hub-side validation).
- `agent-keystore-architecture.md` — per-platform private-key storage
  (Keychain / Secret Service / CNG).
- `compliance-pipeline-architecture.md` — the CP MITM + hook pipeline
  that attestation bypasses.
- `multi-endpoint-coordination-architecture.md` — Hub → CP config-sync
  path that distributes per-agent attestation enable flags.
- `docs/developers/specs/e60/e60-s1-agent-attestation-architecture.md` — story-level
  task breakdown (companion to this doc).
