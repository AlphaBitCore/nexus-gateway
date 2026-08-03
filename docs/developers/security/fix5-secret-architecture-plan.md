# FIX-5 — Secret & crypto architecture hardening (SEC-W2-01 / W2-02 / W2-03 / C1-02)

> **Status:** PLAN (2026-06-09). Gated cluster, user signed off ("continue", translated). This doc is the design/SDD source of truth for the four-finding crypto/secret re-architecture. Implemented sub-phase by sub-phase, each with a regression test, doc/finding/ledger lockstep, prod-deploy execution steps, and its own commit on `worktree-security-audit`.

## Goal

Three flat, dual-purpose, weakly-scoped secrets each collapse two independent trust domains into one un-rotatable value. Replace them with **cryptographically class/domain-separated, identity-bound, per-edge-scoped, rotatable** key material — without backward-compat shims (dev-phase: no installed users, re-seed is acceptable; prod execution documented in the prod-deploy doc).

## Binding constraints

- **Dev-phase policy:** no migration code for dev records, no parallel legacy paths, no rollback flags. Changing a derivation scheme invalidates existing ciphertext/hashes → **re-seed (dev) / re-enter or re-seal (prod)**. Rollback is `git revert`.
- **Prod execution lives in the prod-deploy doc (binding, user-stated 2026-06-09):** every new env var, key-generation command, and re-seed/re-seal step MUST be written into `.claude/skills/prod-deploy/SKILL.md` — the prod-deploy flow reads that file. `.env.example` carries the `[MUST MATCH]` contract for each new var.
- **Secrets are env-only, never yaml.** New secrets get an env var + `.env.example` entry; cross-service shared ones tagged `[MUST MATCH]`.
- **[MUST MATCH] wire contracts:** the CP Vault and the ai-gateway Decryptor are SEPARATE implementations of the same scheme — any derivation/AAD change must land in BOTH identically or every decrypt 500s. Same for HMAC (CP admin-key hash vs ai-gw VK hash) and the per-edge tokens (each service + Hub).
- **Per-finding pattern:** re-validate probe → fix → regression test asserting the closed invariant → build all 5 services → full affected-service sweep → doc/finding/ledger + prod-deploy lockstep → commit `-F -` with the Opus co-author footer.

## Current state (re-validated 2026-06-09, all four still-present)

| Secret | Domains it conflates | Keyring today | Gap |
| --- | --- | --- | --- |
| `ADMIN_KEY_HMAC_SECRET` | admin/user API-key admission (CP) **+** virtual-key admission (ai-gw) | **none** | one leak forges both planes; no rotation |
| `INTERNAL_SERVICE_TOKEN` | `/api/hub` config-write **+** act-as-any-thing on `/api/internal/things/*` (service-token arm no-ops `requireThingMatch`) | **none** | one service's leak = fleet config-injection + node impersonation |
| `CREDENTIAL_ENCRYPTION_KEY` | provider API keys (CP seal / ai-gw open) **+** Hub alert-channel secrets | `CREDENTIAL_KEY_MAP` (`v1:hex,v2:hex`) exists | no KDF (raw key), **nil AAD** (C1-02 swap), no class separation |

## Design

### Sub-phase A — credential encryption: HKDF class separation + AAD binding (W2-03 + C1-02)

The foundational, lowest-regret change. Both findings touch the same seal/open code.

1. **HKDF-SHA256 class separation.** The master key becomes a KDF input, never a direct AEAD key:
   - `k_provider = HKDF-Expand(HKDF-Extract(salt=∅, master), info="nexus/cred/provider-api-key/v1", 32)`
   - `k_alert    = HKDF-Expand(..., info="nexus/cred/alert-channel-secret/v1", 32)`
   Provider creds seal/open under `k_provider`; Hub alert secrets under `k_alert`. Leaking one class's derived key (e.g. via a memory-scoped disclosure) no longer yields the other class or the master. Applied per keyring version (each `vN` master → its own derived sub-keys).
2. **AAD identity-binding (C1-02).** `Encrypt(plaintext, aad)` / `Decrypt(..., aad)` thread row identity:
   - provider cred AAD = `nexus/cred/v1|cred:<credentialID>|provider:<providerID>`
   - alert secret AAD = `nexus/alert/v1|channel:<channelID>`
   A cross-credential ciphertext swap now fails GCM auth (`Open` → error) instead of yielding the wrong upstream key. Plumb `credentialID`+`providerID` through `CreateCredential` / `UpdateCredentialEncryption` / `rotateOne` (write) and `manager.decrypt` (read).
> **Decision (2026-06-09, investigated):** `Credential.id` is `@default(uuid())` (DB-generated), so the id is NOT known at seal time. AAD binds to the immutable `id` (NOT the mutable `@unique name`, which `UpdateCredential` can change and would break the binding on rename). Therefore the create path must **generate the credential id app-side (Go uuid) before insert** and pass it explicitly — matching the finding's "app-side id-gen" note. `providerId` is already known at create time.

3. **Re-seal, not migrate.** The existing rotation worker re-seals every row under the new (derived + AAD) scheme. Dev: re-seed. Prod: documented one-time re-seal pass (provider keys are still present in the CP DB plaintext-at-rest? No — they're sealed under the OLD raw key; the re-seal step decrypts-old → seals-new in place, run once at deploy). **Wire format gets a scheme tag** (e.g. `encryption_scheme` column or a version prefix on the key_id) so old rows are unambiguously identified for the one-time pass; after the pass, only the new scheme is accepted.

Touch: `control-plane/internal/platform/crypto/aes_gcm.go` (Vault + MultiVault), `ai-gateway/internal/credentials/decrypt/decrypt.go` (Decryptor + MultiDecryptor), `nexus-hub/internal/alerts/engine/secret.go`, `ai-gateway/internal/credentials/manager/manager.go`, `control-plane/internal/ai/providers/handler/key_rotation.go`, the credential create/update handlers. Shared HKDF helper (new, e.g. `packages/shared/identity/keyderive/` so CP + ai-gw derive identically — a [MUST MATCH] guarantee in code, not just convention).

### Sub-phase B — HMAC secret: HKDF domain separation + versioned keyring (W2-01)

1. **Domain separation via HKDF.** `k_admin = HKDF(master, info="nexus/apikey/admin/v1")`, `k_vk = HKDF(master, info="nexus/apikey/virtual-key/v1")`. CP admin-key hashing uses `k_admin`; ai-gw VK hashing uses `k_vk`. One domain's leak no longer forges the other. (Reuses the shared keyderive helper from A.)
2. **Versioned keyring.** Add `ADMIN_KEY_HMAC_KEY_MAP` (`v1:secret,v2:secret`) mirroring `CREDENTIAL_KEY_MAP`; stamp `keyVersion` on `Account.keyHash` + `VirtualKey.keyHash` rows (schema change); verify against the stamped version; lazy re-hash on next successful auth. A compromised version rotates out without a fleet-wide lockstep re-issue.
3. **Re-issue reality.** Changing the derivation invalidates every existing key_hash. Admin/user API keys and VKs cannot be re-hashed (raw keys not stored) → they must be **re-issued**. Dev: re-seed mints fresh keys. Prod: documented — admin re-creates keys post-deploy. (The keyring then makes FUTURE rotations non-disruptive.)

Touch: `control-plane/internal/identity/authn/apikey.go`, `control-plane/internal/platform/middleware/adminauth.go`, `ai-gateway/internal/auth/vkauth/vkauth.go`, both `config.go` env loaders, schema (`Account`/`VirtualKey` keyVersion columns + seed).

### Sub-phase C — internal service token: per-edge scoping + authority split (W2-02)

1. **Per-edge tokens.** Replace the one shared `INTERNAL_SERVICE_TOKEN` with distinct per-edge credentials the Hub knows the set of: `HUB_TOKEN_CONTROL_PLANE`, `HUB_TOKEN_AI_GATEWAY`, `HUB_TOKEN_COMPLIANCE_PROXY`. Each service holds only its own; the Hub maps token→edge identity. One service's leak is contained to that edge.
2. **Separate the two authorities.** `/api/hub` config-write requires the CP-edge credential only (CP is the sole writer). The service-token arm of `DeviceOrServiceAuth` must NOT silently bypass `requireThingMatch` for fleet-scoped mutations — require a real thing identity or a dedicated operator credential on break-glass / shadow-write / audit-upload (coordinated with the already-shipped SEC-C3-02 / SEC-C5-01 guards; verify no conflict — `requireThingMatch` today still returns authorized for service-token callers, so the generic act-as-any-thing surface remains and is in scope here).
3. **Versioned/rotatable** per-edge tokens (accept old+new during a window) so rotation doesn't force a lockstep flip → the documented inter-service 403 outage.

Touch: `nexus-hub/internal/handler/{routes.go,middleware.go}`, `nexus-hub/internal/fleet/handler/hubapi/internal_things.go`, the three peer auth loaders (`ai-gateway/internal/runtimeapi/auth.go`, `compliance-proxy/internal/runtime/auth/auth.go`, `control-plane/internal/handler/helpers.go`), all `config.go` env loaders, `.env.example`.

## Work order & sequencing

A → B → C (A is foundational + lowest-regret; B reuses A's keyderive helper; C is independent auth-edge work). Each sub-phase is a separate commit (or small commit set) with its own tests + prod-deploy doc update. Re-validate each finding's probe before its sub-phase; re-run after.

## Risks

- **Wire-format drift (highest):** CP and ai-gw must derive + AAD identically. Mitigation: shared `keyderive` helper in `packages/shared/` so both import one implementation; cross-impl parity test.
- **Re-seal correctness:** the one-time prod re-seal must decrypt-old → seal-new atomically per row and be idempotent/resumable. Mitigation: scheme-tag rows; the pass skips already-new rows; dry-run count first.
- **Lockstep outage during token rotation:** mitigated by accept-old+new windows.
- **Schema changes** (keyVersion columns) — dev re-seed; prod documented.

## FIX-5/C execution design (W2-02) — concrete, ready to execute

Investigated 2026-06-09. The core W2-02 risk is "one `INTERNAL_SERVICE_TOKEN` gates TWO qualitatively different authorities: (1) config-WRITE on `/api/hub/*` and (2) act-as-any-thing on the `/api/internal/things/*` service-token arm." Split it in two stages, lowest-blast-radius first.

> **C1 STATUS: SHIPPED (2026-06-09).** `HUB_CONFIG_TOKEN` added (required + fail-closed on both Hub and CP), gating `/api/hub` + `/api/v1/admin/alerts`; `INTERNAL_SERVICE_TOKEN` retained for `/api/internal/things` + WS. Caller-verification gate executed: CP confirmed the sole caller of both groups. Regression `auth_authority_split_test.go`; env/dev-start/prod-deploy lockstep done. **C2 (requireThingMatch) is the next commit.**
>
> **Why a NEW env var, not an HKDF sub-key of `INTERNAL_SERVICE_TOKEN` (rationale — design-review fold 2026-06-09).** FIX-5/B and W2-03 split *domains that share a root* by deriving in-process (no new env var), and the obvious instinct is to do the same here: `k_config = keyderive.DeriveSubkey(INTERNAL_SERVICE_TOKEN, …)`. That is **cryptographically insufficient for W2-02** and would NOT close the finding. W2-02's persona is *a data-plane service (ai-gateway / compliance-proxy) leaks its environment*, and those services hold `INTERNAL_SERVICE_TOKEN` (it is `[MUST MATCH all 4 services]`). The HKDF info-string is public in the open-source tree, so an attacker who leaks `INTERNAL_SERVICE_TOKEN` recomputes any sub-key derived from it offline — including `k_config`. Containment therefore REQUIRES the config-write authority to be a **distinct root that the data-plane services do not possess**; only CP and Hub hold `HUB_CONFIG_TOKEN`. This is the categorical difference from B/W2-03 (there, the shared root is NOT itself the credential whose leak is modeled; here it is). A distinct env var is also the natural seam for the deferred C3 independent rotation. The operator cost (a new `[MUST MATCH]` secret — the repo's most-cited drift/outage source) is real and is mitigated by the prod-deploy parity preflight; it is the price of actually containing the modeled leak.

**C1 — authority split (separate the config-write credential).**
- New env `HUB_CONFIG_TOKEN` ([MUST MATCH] CP ↔ Hub ONLY). Hub gates `/api/hub` (routes.go:133) and `/api/v1/admin/alerts` (routes.go:267) with `ServiceAuth(cfg.HubConfigToken)`. `INTERNAL_SERVICE_TOKEN` stays for `/api/internal/things` + the **WS registration path** (untouched → the riskiest path is unchanged).
- CP's `hub.Client` (`internal/platform/hub/client.go`, the sole `/api/hub` caller — Bearer token) is constructed with `HUB_CONFIG_TOKEN`. CP keeps `INTERNAL_SERVICE_TOKEN` for its WS Thing registration → CP holds BOTH; ai-gw + compliance-proxy hold only `INTERNAL_SERVICE_TOKEN` → their leak can no longer inject config.
- **CALLER-VERIFICATION GATE (mandatory before shipping C1):** enumerate every caller of `/api/hub/*` and `/api/v1/admin/alerts/*` and confirm each is CP. A missed non-CP caller = `403` (the finding's documented fleet-wide outage failure mode). Mitigation: accept-old-token window OR ship C1 with Hub temporarily accepting EITHER token on those groups during one deploy, then tighten.

> **C2 STATUS: SHIPPED (2026-06-09) — with a revised shape.** The mandatory caller-verification gate found the plan's premise wrong: register/heartbeat/shadow/deregister/break-glass have **legitimate service-token callers** (CP/ai-gw/compliance-proxy self-register via the thingclient HTTP fallback; compliance-proxy sends break-glass), each only ever on its **own** backend-service Thing. So a blanket "reject service-token" would brick fleet self-registration. User chose **type-discrimination** (AskUserQuestion, 2026-06-09): `requireThingMatch` → `requireMutationAuthority` — device callers bound to their own id; service-token callers allowed only on a `thingtype.IsBackendService` Thing, never an agent (closes act-as-any-AGENT: forged shadow / break-glass kill-switch / deregister / exemption). Residual = cross-backend-service impersonation → **C3** (deferred). Gate placed immediately before the mutation (after shape validation). Regression in `internal_things_authority_test.go`.
>
> Original per-handler analysis (kept for reference; superseded by the type-discrimination shape above):

**C2 — `requireThingMatch` impersonation fix (Hub-only, no env change).** `internal_things.go:45` returns authorized for any service-token caller. Per-handler analysis of the `/api/internal/things` group (routes.go:189-212):
- DEVICE-only mutations (agent-with-mTLS callers): `register`, `heartbeat`, `shadow`, `deregister`, `exemption` — a pure service-token caller should be REJECTED (services don't own a thing identity; these are device ops). `shadow/break-glass` (C3-02) and `agent-audit` (C5-01) already hardened.
- Legitimate cross-thing READS by services: `GET /config`, `/config/:key`, `/update-check`, `/:id/attestation-pubkey` (CP/ai-gw/compliance-proxy read for any agent) — keep allowing service-token.
- Fix shape: a per-route `requireDeviceIdentity` on the device-only mutation handlers (reject service-token), leaving reads on the existing `DeviceOrServiceAuth`. **Requires confirming no service legitimately calls those mutation endpoints first.**

**C3 (further refinement, optional pre-GA):** full per-edge tokens distinguishing ai-gw vs compliance-proxy (`HUB_TOKEN_AI_GATEWAY` / `HUB_TOKEN_COMPLIANCE_PROXY`) so one of those services' leak is contained to its own edge. Higher [MUST MATCH] surface; defer like the W2-01 keyring.

**Execution note:** C1 + C2 each touch the inter-service auth contract where a wrong caller assumption = fleet 403. Both need the per-caller verification done with fresh attention — NOT rushed. This is the single highest-blast-radius change in FIX-5.

## Open design decisions (surface to user before coding the affected sub-phase)

1. **HMAC keyring scope (B):** full versioned keyring + schema columns (finding's "closed when" target) vs. HKDF domain-separation only (no schema change, but no rotation capability). Per "perfect product / no complexity compromise," default = full keyring.
2. **Per-edge token model (C):** three named per-service tokens vs. a single Hub-issued signed token carrying a service claim (mTLS client certs are the long-term ideal but heavier). Default = three named tokens (simplest that achieves per-edge containment + rotation).
