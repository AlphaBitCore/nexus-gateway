# Adversarial Security Audit — Program Tracker

> **Status:** PHASES 0–5 COMPLETE + **G6 verify round COMPLETE (2026-06-10)**. **41 confirmed** (2 CRIT, 17 HIGH, 14 MED, 8 LOW) + 19 kill-chains + threat-model + ranked roadmap; **41/41 findings have a landed fix** (35 CLOSED, 3 MORPHED with residuals fixed or risk-accepted, SEC-W3-01 re-fixed under G6, 2 platform-pending). Remaining: 2 platform-pending on-host verifications (SEC-M4-02 keystore, SEC-M8-02 NE watchdog, plus SEC-M8-01's live UDP test) — maintainer-owned via `Skill('build-agent')`. Layer A/C SHIPPED; Layer B CANCELLED (risk-accepted). Next: the closing arch+product two-round review, then program close.
> **Worktree:** `.claude/worktrees/security-audit` / branch `worktree-security-audit`
> **This file is the cross-session source of truth.** Every phase updates the status table + findings index here.
>
> **Resume note (2026-06-08):** arch-audit's read-only audit (Phases 1–7) is COMPLETE and its fix
> program has merged the S2 batches into `worktree-arch-audit` (through `4fb126066`). That branch was
> merged into this one (merge commit `77e86466f`), so the security audit now runs against **settled
> code** — satisfying the "security trails arch" coordination trigger. The arch-audit ledger lives
> in-tree at **`audit/`** (`PROGRAM.md` / `LEDGER.md` / `SYNTHESIS.md` / `chains/`); its final tally
> was 2 S1 (both fixed), 43 S2, 111 S3, 71 S4, ~30 V. Security-relevant arch findings already fixed
> include F-0044 (unauth ai-guard webhook), F-0060 (Hub device-token cross-Thing binding), F-0200
> (enrollment thingId takeover), F-0190 (SIEM log injection), F-0284 (admin tokens → httpOnly cookies),
> F-0269 (simulator SSRF). The security pass **confirms rather than re-finds** these and hunts what an
> arch-correctness lens does not: malicious paths, boundary conditions, broken trust assumptions.

## Goal

> **Goal:** Audit Nexus Gateway from a *real attacker's* perspective — not "is the architecture sound" but "where does a sound-looking architecture fail against malicious paths, boundary conditions, and broken trust assumptions." Organized **module → chain → whole-system**, with every finding adversarially verified and given attacker-PoC reasoning. Multi-phase, multi-session.

The prior architecture review concluded "the design is fine." This program deliberately takes the opposite stance: assume every trust boundary is hostile and try to break it.

## Scope correction (binding for this program)

- **No "tenant" concept exists.** The isolation unit is the **virtual key (VK)**. All "isolation" findings are framed as **cross-VK** isolation, never "cross-tenant."

## Threat actors (attacker personas)

| ID | Persona | Entry point |
|----|---------|-------------|
| A1 | External unauthenticated network attacker | AI Gateway `:3050`, Compliance Proxy `:3128`, CP `:3001` login, Hub `:3060` WS |
| A2 | Malicious holder of a *valid* virtual key | AI Gateway request path (cross-VK isolation, cache, quota, credential extraction) |
| A3 | Compromised / rogue Agent node | Hub enrollment, WS shadow channel, config/kill-switch push |
| A4 | Compromised upstream provider / on-path MITM | Gateway↔provider TLS bump, cert validation |
| A5 | Low-privilege insider admin | CP admin API, IAM tiers, 403 drift |
| A6 | Attacker with read access to a host / repo / logs | Secrets sprawl, key extraction, CA private key at rest |

## Trust boundaries (where we attack)

| ID | Boundary | Crown-jewel asset behind it |
|----|----------|------------------------------|
| TB1 | Internet ↔ Gateway/Proxy/CP/Hub | First auth decision |
| TB2 | VK ↔ VK | Per-VK data, cache, quota isolation |
| TB3 | Node ↔ Hub | Thing/shadow integrity, config authority |
| TB4 | Gateway ↔ Provider | Upstream TLS, real provider API keys |
| TB5 | Admin ↔ Admin | IAM action tiers |
| TB6 | Ciphertext ↔ Plaintext | **MITM CA private key** + **credential encryption key** |

## Crown-jewel asset inventory (blast-radius ranked)

1. **MITM CA private key** — `packages/compliance-proxy/internal/tls/issuer/issuer.go`, `packages/nexus-hub/internal/identity/agentca/ca.go`. Can sign trusted certs for *any* domain on every enrolled machine. Single largest blast radius.
2. **Credential encryption key/passphrase/salt** — `CREDENTIAL_ENCRYPTION_KEY` / `CREDENTIAL_ENCRYPTION_PASSPHRASE` / `CREDENTIAL_ENCRYPTION_SALT` / `CREDENTIAL_KEY_MAP`. Implementation: `packages/control-plane/internal/platform/crypto/aes_gcm.go`, `packages/ai-gateway/internal/credentials/decrypt/decrypt.go`. Decrypts every upstream provider API key.
3. **Internal-service token** — `INTERNAL_SERVICE_TOKEN`, `ADMIN_KEY_HMAC_SECRET`, `AI_GATEWAY_API_TOKEN`, `COMPLIANCE_PROXY_API_TOKEN`. Inter-service trust; drift = cross-service 403 or auth bypass.
4. **Virtual keys** — `packages/ai-gateway/internal/auth/vkauth/`. The thing A2 holds and the thing A1 wants to forge.
5. **Agent enrollment / identity** — `packages/nexus-hub/internal/identity/{handler/enroll,store/enrollstore,agentca}`, `packages/agent/internal/identity/`. Bootstraps node trust.

## Methodology

Each phase runs as a **deep multi-agent workflow** (fan-out finders by surface → adversarially verify every finding with N independent skeptics → only confirmed findings survive). The user opted into multi-agent scanning. I review results between phases and stay in the loop; phases are not auto-chained.

Finding severity: `CRITICAL` (crown-jewel compromise / auth bypass / cross-VK breach) > `HIGH` (privilege escalation / secret leak / DoS of safety-critical path) > `MEDIUM` (info leak / hardening gap) > `LOW` (defense-in-depth).

Every finding must carry: attacker persona, preconditions, attack steps (PoC reasoning), affected file:line, severity, and a remediation note. Findings that survive adversarial verification land in `docs/developers/security/findings/`.

## Concurrency with the arch-audit program (binding for this program)

A **second, structurally identical** program runs in parallel: the **pre-sale arch audit** — also 3-level (module / chain / whole-repo), **85+ tasks**, and it **changes code**. Its charter + ledger live at `~/workspaces/workspace-nexus/audit-2026-06/` (outside this repo; **will be moved into a worktree**). This security program and the arch program cover the same codebase from two angles: arch = correctness/structure (mutates code), security = adversarial (read-only findings).

**Decided coordination model — security trails arch, audits settled code:**

1. **Do not audit pre-refactor code.** At 85+ code-changing tasks, arch-audit will rewrite/delete most crown-jewel code. Auditing the old version = finding holes in code about to be deleted = wasted. **The security audit's main body runs AFTER arch-audit settles a unit** (per-unit) or after arch-audit completes — so it always audits final code. This is why this program is intentionally **paused** at scaffolding (see Status).
2. **Findings are invariant-anchored, not line-anchored.** Every finding records: attacker persona + violated **invariant** + a **repeatable re-check probe** (grep / probe script / test). `file:line@SHA` is only a pointer. Survives refactoring; re-locatable mechanically.
3. **Mandatory re-validation gate.** Before any fix, and after every arch-audit merge that touched audited code, re-run all probes against current `HEAD`. Each finding → `still-present` (fix) / `fixed-by-refactor` (close) / `morphed` (re-locate). This is the single hard sync point between the two programs.
4. **Feed-forward critical invariants.** Before arch-audit touches a crown jewel (CA, credential crypto, vkauth, IAM, enrollment), security hands it the security **invariants as acceptance criteria** so the rewrite is born secure; the later security pass confirms rather than re-finds.
5. **Shared decomposition.** Map this program's M1–M9 / C1–C6 / W1–W4 onto arch-audit's 85+ task units 1:1, and coordinate via a shared ledger (location TBD once arch-audit's charter moves into its worktree). A unit is **DONE** only when `arch-clean ∧ security-clean`.

**Base commit (pin):** `77e86466f` (merge of `origin/worktree-arch-audit` into `worktree-security-audit`, 2026-06-08). This is the settled, post-arch-audit code the security audit now runs against. (Original scaffold pin was `8abd8c6e0`.) Findings reference this SHA via `file:line@SHA` pointers; re-validate probes against current `HEAD` before any fix.

**Arch-audit ledger alignment:** arch-audit's units live in-tree at `audit/` (`PROGRAM.md` task board, `LEDGER.md` per-finding detail, `SYNTHESIS.md` §6 ordering, `chains/C1..C9.md` seam maps). The M1–M9 / C1–C6 / W1–W4 ↔ arch-unit 1:1 mapping is recorded per-finding as each Phase-1 module workflow runs (a finding that confirms or re-locates an arch F-ID cites it). A unit is DONE only when `arch-clean ∧ security-clean`.

## Phase plan (multi-level)

### Phase 0 — Scaffolding & attack-surface map *(this session)*
Establish this tracker, personas, boundaries, crown-jewel inventory. Enumerate every listening port, auth-bearing entry point, and the exact files behind each crown jewel. **Deliverable:** this doc + `findings/00-attack-surface-map.md`.

### Phase 1 — Module-level audit (Level 1: each crown jewel in isolation)
| Mod | Target | Primary attacker |
|-----|--------|------------------|
| M1 | Compliance Proxy CA issuer + Hub agentca (key gen entropy, at-rest protection, who-can-read, issuance constraints, name constraints, serial/validity) | A4, A6 |
| M2 | Credential crypto (nonce gen/reuse, KDF from passphrase+salt, AAD, key-map rotation, error oracle, plaintext lifetime, log leakage) | A2, A6 |
| M3 | VK auth (format, lookup, constant-time compare, at-rest hashing, revocation, scope binding, cross-VK enforcement) | A1, A2 |
| M4 | Node enrollment + attestation signer (token issuance/replay, CSR validation, identity binding) | A3 |
| M5 | Hub WS server + Thing auth (conn auth, per-message auth, can node A write node B's shadow, config/kill-switch authZ) | A3 |
| M6 | IAM catalog + middleware (action coverage vs UI `allowedActions`, 403 drift — run iam-impact-review per endpoint surface) | A5 |
| M7 | OAuth/PKCE authserver (PKCE enforcement, redirect_uri validation, token issuance/refresh, anon-login info leaks beyond known SSO-enum fix) | A1, A5 |
| M8 | NE transparent proxy fail-open (re-audit vs the 5 safety rules; bypass-compliance AND brick-the-host angles) | A2, A3 |
| M9 | Secrets management (env-vs-yaml leak, secrets in logs, MUST-MATCH drift, default/dev keys shipped) | A6 |

### Phase 2 — Chain / link audit (Level 2: trust boundaries & end-to-end flows)
| Chn | Flow | Question |
|-----|------|----------|
| C1 | Credential lifecycle: CP encrypt → DB → Hub shadow → WS push → AI Gateway decrypt → provider | Where does plaintext live; can it leak between stages |
| C2 | Enrollment & identity: node boot → enroll → cert → Hub trust → ongoing auth | Can A3 forge/replay any link |
| C3 | Config push: Admin → CP → Hub shadow → WS → node apply | Can a malicious actor inject config / kill-switch / allowlist to another node |
| C4 | Cache key derivation & isolation: request → cache key → response cache | Cross-VK leakage / cache poisoning |
| C5 | Audit/traffic event: request → traffic_event → audit | Can attacker forge/suppress audit, log injection |
| C6 | Quota / rate-limit: request → counter → enforce | Bypass, race, counter manipulation |

### Phase 3 — Whole-system / kill-chain (Level 3: cross-cutting & combined)
- W1 — Attacker kill-chains: chain Level-1/2 findings into end-to-end compromise per persona.
- W2 — Blast-radius & SPOF (CA key, master key, internal-service token).
- W3 — Fail-open/fail-closed safety review system-wide.
- W4 — Supply chain (`go.work` replace directives, build, deps).

### Phase 4 — Synthesis & remediation roadmap
Consolidated attacker-perspective threat model, ranked findings, remediation backlog, "must-fix-before-GA" gate.

### Phase 5 — Fix program (added 2026-06-08 per session goal)
After the audit (Phases 0–4) lands, **fix everything**, ordered by the Phase-4 roadmap. Policy:
- **HIGH + MEDIUM: mandatory fix.** Each follows the SEC-M2-01 standard — plan → real implementation
  (no stubs) → tests asserting the closed invariant → doc/openapi lockstep → re-run the finding's
  re-check probe to confirm `fixed` → commit per finding/cluster.
- **LOW: always recorded**; fix the simple ones (cheap, low-blast, no design decision). Defer only
  LOW that need a design call, and record the deferral reason.
- **Design-heavy fixes** (e.g. the M6 IAM permission-boundary cluster) get a brainstorm to pick the
  best product+architecture approach before coding.
- Re-validation gate: before each fix, re-run its probe against current `HEAD` (`still-present` /
  `fixed-by-refactor` / `morphed`). Auto-commit enabled.

## Status

> **PROGRAM COMPLETE except the closing review (2026-06-10)** — Phases 0–5 done; the G6 verify round (independent re-verification of all 41 shipped fixes) is COMPLETE. **41/41 findings have a landed fix**; the only outstanding items are the 2 platform-pending on-host verifications (SEC-M4-02, SEC-M8-02, plus SEC-M8-01's live UDP test), maintainer-owned via `Skill('build-agent')`. Layer A/C SHIPPED; Layer B CANCELLED (risk-accepted by user decision). Next: the mandatory closing arch+product two-round review, then program close. Per-finding verdicts: [`findings/LEDGER.md`](findings/LEDGER.md); closeout brief: `docs/handoffs/security-audit-g6-closeout.md`.

| Phase | State | Deliverable | Confirmed findings |
|-------|-------|-------------|--------------------|
| 0 | **done** (2026-06-08) | this doc + [`findings/00-attack-surface-map.md`](findings/00-attack-surface-map.md) | — (11 observations O1–O11 queued; 5 scaffold-drift corrections folded back) |
| 1 | **DONE** (M1–M9, 2026-06-08) | `findings/01-modules/*` | **23 confirmed** (8 HIGH [1 fixed: SEC-M2-01], 6 MEDIUM, 9 LOW); ~28 refuted by adversarial verification |
| 2 | **DONE** (C1–C6, 2026-06-08) | `findings/02-chains/*` | **12 confirmed** (2 CRITICAL, 4 HIGH, 5 MEDIUM, 1 LOW); 8 refuted |
| 3 | **DONE** (W1–W4, 2026-06-09) | `findings/03-system/{system-findings,kill-chains}.md` | **6 system** (3 HIGH SPOF, 2 MED, 1 LOW) + **19 kill-chains** (5 CRITICAL, several demonstrated) |
| 4 | **DONE** (2026-06-09) | [`findings/threat-model.md`](findings/threat-model.md) + [`findings/LEDGER.md`](findings/LEDGER.md) | threat model + 41 ranked + must-fix gate + 8 fix clusters |
| 5 | **DONE** (2026-06-10, incl. G6 verify round) | per-cluster commits + [`findings/LEDGER.md`](findings/LEDGER.md) G6 verdicts | **41/41 landed fixes** — 35 CLOSED (G6-verified with real regression tests); 2 platform-pending (M4-02 keystore, M8-02 NE watchdog — on-host build+test maintainer-owned); 3 MORPHED with residuals fixed (M1-01 pathlen recipes+warning `ab1dd6403`; C3-01 resync signing `ab1dd6403`) or risk-accepted (W2-02 — Layer B cancelled); W3-01 reopened by G6 → re-fixed (`03de276d1` + `dcced4d2c`); Layer A review fixes landed (`2a2b11c3a`); M8-01's NE-Swift half implemented (`14a98c6a2`, on-host test pending) |

> **FIX-5 gate RESOLVED — the user signed off and FIX-5/A+B+C all SHIPPED 2026-06-09 (historical gate rationale kept below for context).** SEC-W2-01 (`ADMIN_KEY_HMAC_SECRET` purpose-split), SEC-W2-02 (`INTERNAL_SERVICE_TOKEN` per-service), SEC-W2-03 (credential master-key HKDF class-separation + KDF + AAD + KEK/DEK + leak-response runbook + deferred KMS), and SEC-C1-02 (credential-ciphertext AES-GCM AAD identity-binding) are all **cross-service `[MUST MATCH]` secret/crypto CONTRACT changes** requiring coordinated **re-keying / re-provisioning** across CP + ai-gateway + Hub (+ compliance-proxy) and (for C1-02) app-side credential-id generation. A botched change breaks **all** credential decryption (crown-jewel asset #2 → data-plane outage). Per the binding *big-changes-discuss-first* + *high-blast-radius (credential encryption)* rules, these are **not** auto-committed; the brainstormed remediation design is recorded in `/tmp/sec-ledger.json` (gate_note) and the per-finding docs. Recommend executing as one coordinated, reviewed PR after sign-off. The fix program continues autonomously on the remaining non-gated HIGH/MED (C1-01, C5-01, M5-01, C4-01, W3-01, W4-01/02).

## Findings index

> One row per confirmed finding: ID · severity · persona · title · file:line · status. The Status
> column below is the discovery-time snapshot; **current per-finding status lives in
> [`findings/LEDGER.md`](findings/LEDGER.md)** (G6 final: 41/41 landed fixes).

### Phase 1 — Module-level (wave 1: M1–M3, base `77e86466f`)

| ID | Sev | Persona | Title | file:line | Status |
|----|-----|---------|-------|-----------|--------|
| [SEC-M2-01](findings/01-modules/M2-credential-crypto.md#sec-m2-01--ai-guard-external_url-backend-exfiltrates-any-decrypted-provider-api-key-to-an-admin-chosen-arbitrary-url--high-33) | **HIGH** | A5 insider admin | AI-Guard `external_url` backend exfiltrates any decrypted provider key to an arbitrary URL (no credential↔URL binding, no SSRF guard) | `aiguard/backend_external.go:60`, `wiring/aiguard.go:132-159`, `aiguard/handler/handler.go:188-193` | **fixed** 2026-06-08 |
| [SEC-M2-02](findings/01-modules/M2-credential-crypto.md#sec-m2-02--no-weakknown-key-rejection-at-boot-fixed-dev-key-and-all-zeros-pass-every-credential_encryption_key-validation-in-production--medium-33) | MEDIUM | A6 host/log read | No weak/known-key rejection at boot; committed dev key + all-zeros pass prod validation | `crypto/aes_gcm.go:80-90`, `credentials/decrypt/decrypt.go:33-35`, `dev-start.sh:111` | open |
| [SEC-M3-01](findings/01-modules/M3-vk-auth.md#sec-m3-01--revoke-user-access-disables-vks-in-db-but-pushes-no-virtual_keys-cache-invalidate-the-users-keys-keep-authenticating-for-the-full-cache-ttl-30s--medium-33) | MEDIUM | A5 admin / A2 revoked user | "Revoke user access" disables VKs in DB but pushes no cache invalidate; keys keep authenticating ≤30s | `users/handler/users.go:321-375` | open |
| [SEC-M3-02](findings/01-modules/M3-vk-auth.md#sec-m3-02--virtual-key-accepted-as-key-url-query-parameter--plaintext-vk-leaks-to-fronting-proxylb-access-logs-browser-history-on-path-referer--medium-33) | MEDIUM | A6 host/log read | VK accepted as `?key=` URL query param → plaintext VK leaks to access logs / history / Referer | `auth/vkauth/vkauth.go:237`, `ingress/proxy/ingress.go:201-214` | open |
| [SEC-M1-01](findings/01-modules/M1-ca-issuer.md#sec-m1-01--mitm-ca-agent-device-ca--proxy-issuer-ca-is-an-unconstrained-root-no-maxpathlenzero-no-name-constraints--medium-23) | MEDIUM | A6 / A4 (post key-compromise) | MITM CA is an unconstrained root: no `MaxPathLenZero`, no name constraints (blast-radius amplifier) | `agent/.../network/tls/engine.go` `generateCA`, `proxy/.../issuer/issuer.go:82-134` | open |
| SEC-M4-01 | **HIGH** | A3 rogue | Attestation pubkey grants permanent, unrevocable compliance-pipeline bypass — verifier enforces no cert expiry and no revocation lever exists | `packages/compliance-proxy/internal/proxy/server/attestation.` | open |
| SEC-M5-01 | **HIGH** | A3 rogue | Spill blob upload allows cross-node forensic-evidence tampering: mint binds no eventId↔node ownership, blob write deterministically overwrites another node's spill object | `packages/nexus-hub/internal/traffic/ingest/spill/spill_uploa` | open |
| SEC-M5-02 | **HIGH** | A3 rogue | Rogue node can flip the FLEET-WIDE kill-switch in either direction via a self-issued break-glass report (node-supplied desired-config provenance on a safety-critical key) | `packages/nexus-hub/internal/fleet/manager/break_glass.go:173` | open |
| SEC-M6-01 | **HIGH** | A5 low-priv-insider-admin | SIEM webhook config at `audit-log:write` tier exfiltrates the org-wide traffic + admin-audit stream and is an unguarded SSRF vector | `packages/control-plane/internal/observability/siem/handler/s` | open |
| SEC-M6-02 | **HIGH** | A5 low-priv insider admin | No privilege-ceiling on IAM policy create/update/attach — any delegated `iam-policy.*` or `iam-group.*` holder self-escalates to super-admin | `packages/control-plane/internal/identity/users/handler/iam_p` | open |
| SEC-M6-03 | **HIGH** | A5 low-priv insider admin | IAM grant operations (attach policy to principal/group, add group member) gated on generic `.update` verb with no permission-boundary check → privilege escalation to super-admin | `packages/control-plane/internal/identity/users/handler/iam.g` | open |
| SEC-M6-04 | **HIGH** | A5 low-priv insider admin who can manage a single IdP integration | SCIM User endpoints lack per-IdP ownership scoping — a SCIM token for one IdP can read/modify/suspend ANY Nexus user (cross-IdP account takeover via email change) | `packages/control-plane/internal/identity/scim/handler/scim.g` | open |
| SEC-M9-01 | **HIGH** | A6 host | ADMIN_KEY_HMAC_SECRET drift: Control Plane silently hashes admin keys + virtual keys with a committed public constant when the production flag is unset, while the AI Gateway unconditionally requires a real secret | `packages/control-plane/internal/identity/authn/apikey.go:18 ` | open |
| SEC-M4-02 | MEDIUM | A6 host | Attestation private key stored as plaintext 0600 PEM on agent disk (not the platform keystore the CA comment claims) — chains to unrevocable compliance bypass on key theft | `packages/agent/internal/identity/enrollment/enroll.go:392,39` | open |
| SEC-M6-05 | MEDIUM | A5 low-priv insider admin granted a narrow virtual-key:revoke or virtual-key:renew verb | VK approval-workflow handlers (revoke/renew) skip the owner re-check their sibling CRUD handlers enforce — a non-super-admin with virtual-key:revoke can revoke/renew any VK by id | `packages/control-plane/internal/ai/virtualkeys/handler/appro` | open |
| SEC-M7-01 | MEDIUM | A5 low-priv-insider | sso-enroll IAM evaluation hardcodes nexus:CurrentTime to empty string — time-windowed Deny conditions on device-enrollment silently never fire (fail-open) | `packages/control-plane/internal/identity/sso/handler/sso.go:` | open |
| SEC-M8-01 | MEDIUM | A5 | QUIC-fallback kill-list is never validated against the system DNS/DHCP/Push allowlist; a low-priv admin can close UDP for critical Apple networking daemons fleet-wide | `packages/control-plane/internal/settings/handler/settings/ag` | fixed (Go gates; NE-Swift half implemented `14a98c6a2`, on-host test pending) |
| SEC-M4-03 | LOW | A4 on-path | Enrollment-JWT replay guard (JTI cache) is in-memory only — a captured 5-min JWT is replayable across a Hub restart | `packages/nexus-hub/internal/identity/handler/enroll/jti_cach` | open |
| SEC-M5-03 | LOW | A3 rogue | Single compromised node can flip the fleet-wide killswitch (engage OR disengage) via break-glass shadow report with no operator approval | `packages/nexus-hub/internal/fleet/manager/break_glass.go:56-` | open |
| SEC-M5-04 | LOW | A3 rogue | A single compromised node can engage the fleet-wide kill-switch via break-glass, disabling TLS-bump / compliance inspection for every node of its type — no rate-limit, approval, or auto-revert | `packages/nexus-hub/internal/fleet/manager/break_glass.go:56-` | open |
| SEC-M5-05 | LOW | A3 rogue | Rejected/over-privileged break-glass attempts from a node produce no audit row and no SIEM event — silent on the security telemetry | `packages/nexus-hub/internal/fleet/manager/shadow.go:92-96` | open |
| SEC-M8-02 | LOW | A4 on-path-or-rogue-provider | Post-peek relay connection establishment has no fail-open watchdog — black-holed upstream/bridge SYN hangs a claimed flow until the ~75s OS TCP timeout (residual beyond F-0315d's flow.open-only watchdog) | `packages/agent/platform/darwin/NexusAgent/NexusAgentExtensio` | open |
| SEC-M9-02 | LOW | A6 host-or-log-read | Control Plane silently falls back to the committed dev HMAC secret unless a separate, easily-omitted production flag is set (asymmetric with AI Gateway fail-closed) | `packages/control-plane/internal/identity/authn/apikey.go:18 ` | open |

### Phase 2 — Chains (C1–C6, base post-arch-audit + SEC-M2-01 fix)

| ID | Sev | Persona | Title | file:line | Status |
|----|-----|---------|-------|-----------|--------|
| [SEC-C2-01](findings/02-chains/C2.md) | **CRITICAL** | A2 valid-VK | SSO enroll by physical_id takes over an unassigned token-enrolled node (F-0329 residual: assignment==nil branch) — cross-node identity takeover + trust-level escalation + victim token DoS | `packages/nexus-hub/internal/identity/handler/enroll/enrollment_handler.go:349` | open |
| [SEC-C3-01](findings/02-chains/C3.md) | **CRITICAL** | A3 rogue-node | Inter-Hub `nexus.hub.signal` MQ path injects forced fleet-wide config (incl. permissive killswitch / exemptions) with no publisher auth, no F-0139 allowlist, no schema check — bypassing the node version gate via Force | `packages/nexus-hub/internal/ws/signal.go:34-72` | open |
| [SEC-C1-01](findings/02-chains/C1.md) | **HIGH** | A2 valid-VK | Per-VK allowedModels allowlist bypassed by a routing rule's inline FallbackChain (recovery targets dispatched + their provider credential used without the allowlist check) | `packages/ai-gateway/internal/routing/resolver.go:142-15` | open |
| [SEC-C3-02](findings/02-chains/C3.md) | **HIGH** | A3 rogue | Single compromised node can flip the FLEET-WIDE kill-switch via a self-authored break-glass report (no operator authorization verified at the Hub seam) | `packages/nexus-hub/internal/ws/server.go:280-301 (accep` | open |
| [SEC-C5-01](findings/02-chains/C5.md) | **HIGH** | A3 rogue-node | Rogue agent forges cross-VK / cross-org attribution on traffic_event + SIEM via self-asserted entityId/orgId/identity (agent-audit ingest binds only thingId, never the attribution fields) | `packages/nexus-hub/internal/traffic/ingest/audit/agent_audit.go:196-201` | open |
| [SEC-C6-01](findings/02-chains/C6.md) | **HIGH** | A2 valid-VK | Quota policy-cache boot-load failure fails open silently and persistently — unmetered, no self-heal (all VKs spend uncapped) | `packages/ai-gateway/cmd/ai-gateway/wiring/quota.go:23-2` | open |
| [SEC-C6-02](findings/02-chains/C6.md) | **HIGH** | A5 | VK-scoped quota policy/override created in the UI is silently never enforced — CP stores targetType/scope="vk" while the gateway engine queries "virtual_key" (config→engine vocabulary split) | `WRITE (stores "vk"): packages/control-plane/internal/ai` | open |
| [SEC-C1-02](findings/02-chains/C1.md) | MEDIUM | A6 host | Provider-credential ciphertext is not AAD-bound to its credential id/provider — cross-credential ciphertext swap is silently decrypted and used as the wrong (higher-privilege) upstream key | `packages/control-plane/internal/platform/crypto/aes_gcm` | open |
| [SEC-C2-02](findings/02-chains/C2.md) | MEDIUM | A3 rogue-node | Attestation key trusted indefinitely — verify path ignores cert 90-day expiry and has no revocation; decoupled from device-token rotation/revoke | `packages/nexus-hub/internal/fleet/store/thing_registry.` | open |
| [SEC-C4-01](findings/02-chains/C4.md) | MEDIUM | A5 low-priv-admin | Semantic-cache prewarm lets a low-priv admin plant attacker-chosen responses under any victim VK's scope (cross-VK cache poisoning) | `packages/control-plane/internal/ai/cache/handler/semant` | open |
| [SEC-C5-02](findings/02-chains/C5.md) | MEDIUM | A2 valid-VK | CSV/formula injection in compliance-event exports — attacker-controlled traffic_event fields (targetHost, path, hookReasonCode, complianceTags, sourceIp) written to CSV without leading-character neutralization, executing as formulas in the auditor's spreadsheet | `packages/control-plane/internal/traffic/handler/traffic` | open |
| [SEC-C2-03](findings/02-chains/C2.md) | LOW | A5 low-priv-admin | SSO enrollment JWT (a device-enrollment grant) lets a low-priv user mint a fake SERVICE-type Thing — thingType is caller-controlled and unauthorized in the JWT path (F-0200 fixed only the token path) | `packages/nexus-hub/internal/identity/handler/enroll/enrollment_handler.go:315` | open |

### Phase 3 — Whole-system (W2–W4 + 19 kill-chains in 03-system/kill-chains.md)

| ID | Sev | Persona | Title | Status |
|----|-----|---------|-------|--------|
| [SEC-W2-01](findings/03-system/system-findings.md) | **HIGH** | A6 host | ADMIN_KEY_HMAC_SECRET is a single un-rotatable value that conflates TWO distinct authent | open |
| [SEC-W2-02](findings/03-system/system-findings.md) | **HIGH** | A3 rogue | INTERNAL_SERVICE_TOKEN is a single flat shared secret across four services that simultan | open |
| [SEC-W2-03](findings/03-system/system-findings.md) | **HIGH** | A6 host | CREDENTIAL_ENCRYPTION_KEY is a single master AES key (flat 32-byte, no KDF/salt, nil AAD | open |
| [SEC-W3-01](findings/03-system/system-findings.md) | MEDIUM | A5 low-priv insider ad | Compliance-proxy appliance silently downgrades admin-configured FailBehavior=fail-closed | open |
| [SEC-W4-01](findings/03-system/system-findings.md) | MEDIUM | A4 on-path attacker du | AMI appliance build pulls and compiles all runtime infra (Valkey + valkey-search vendore | open |
