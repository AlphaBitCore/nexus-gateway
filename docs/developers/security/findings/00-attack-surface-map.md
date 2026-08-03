# Phase 0 — Attack-Surface Map

> **Phase 0 deliverable** of the adversarial security audit (`audit-program.md`).
> **Pinned base:** `77e86466f` (settled post-arch-audit code, 2026-06-08).
> **Status:** enumeration only. This map is *not* a findings list — it is the ground truth of
> "what listens, what's behind it, and how it's reached" that Phase 1+ attacks. Items flagged
> **⚠ attacker-relevant** are observations to be adversarially verified in later phases, not yet
> confirmed findings.
>
> Built from two read-only Opus recon sweeps over the merged tree; every claim carries a
> `file:line` pointer (valid `@77e86466f`).

---

## 0. How to read this

- **Bind address matters.** A surface on `0.0.0.0` is reachable from any host that can route to
  the machine; a `127.0.0.1` surface is reachable only by local processes. The audit treats every
  `0.0.0.0` listener as an A1 (external unauthenticated) entry point.
- **Gate ≠ strength.** This map records *which* gate is attached (vkauth, internal-service token,
  cookie/JWT, peer-cred, none). Whether the gate actually holds is Phase 1's job.

---

## 1. Listening surfaces (by service)

### ⚠ Cross-cutting: default bind is `0.0.0.0`, loopback is opt-in

All three Echo/`http.Server` services (Hub, CP, AI Gateway) compute their listen address via
`ServerConfig.BindAddr()` = `fmt.Sprintf("%s:%d", Host, Port)`. **`Host` is empty in every shipped
yaml** (`nexus-hub.config.yaml`, `control-plane.config.yaml`, `ai-gateway.dev.yaml` set only
`port:`), so `BindAddr()` returns `:port` → **bind on all interfaces by default**. Loopback-only
binding is opt-in via the `server.host` yaml key or `NEXUS_HUB_HOST` / `CONTROL_PLANE_HOST` /
`AI_GATEWAY_HOST`. The arch-audit "loopback binding" seam exists in code but **is not engaged by the
default config** — to be examined in M-context and the W-level deployment review (does the prod
appliance config set `server.host`? The appliance-hardening commit `d18ed5bdf` claims loopback
binding via `server.host` — verify it is actually wired in the shipped prod yaml, not just
available).

### nexus-hub (Echo, single listener)

| Port | Bind | Protocol | file:line |
|------|------|----------|-----------|
| 3060 | 0.0.0.0 (default; `NEXUS_HUB_HOST` → loopback) | HTTP + WebSocket (`/ws` upgrades on same port) | `cmd/nexus-hub/main.go:143` (`e.Start`), addr `config.go:62` `BindAddr()` |

Route-group → gate (`internal/handler/routes.go`; health/introspect in `wiring/routes.go`):

| Route group | Gate | file:line |
|-------------|------|-----------|
| `GET /healthz` `/readyz` `/metrics` | **none** (public) | `wiring/routes.go:87-91` |
| `GET /debug/runtime` | token (`InternalServiceToken`) | `wiring/routes.go:95-98` |
| `/api/hub/*` | `ServiceAuth(InternalServiceToken)` | `routes.go:133` |
| `/api/internal/things/*` (register, heartbeat, shadow, shadow/break-glass, config, audit, agent-audit, spill-uploads, enroll, renew-token, diag-events) | `enroll.DeviceOrServiceAuth` (device mTLS token OR service token) | `routes.go:188`, enroll `:313` |
| `PUT /api/internal/spill/blob/:token` | **path-embedded HMAC token** (outside deviceAuth group) ⚠ | `routes.go:230` |
| `/api/v1/alerts/*` (raise/resolve) | `enroll.DeviceOrServiceAuth` | `routes.go:252` |
| `/api/v1/admin/alerts/*` | `ServiceAuth(InternalServiceToken)` | `routes.go:267` |
| `GET /api/public/agent-bootstrap` | **none** (explicitly public) ⚠ | `routes.go:299` |
| `GET /ws` | **no Echo middleware**; auth/origin enforced inside `ws.Server.HandleUpgrade` ⚠ | `routes.go:322`, `internal/ws/server.go:113` |

### control-plane (Echo, single listener)

| Port | Bind | Protocol | file:line |
|------|------|----------|-----------|
| 3001 | 0.0.0.0 (default; `CONTROL_PLANE_HOST` → loopback) | HTTP | `cmd/control-plane/wiring/shutdown.go:28`, addr `config/config.go:123` |

| Route group | Gate | file:line |
|-------------|------|-----------|
| `GET /healthz` `/metrics` `/readyz` | **none** | `wiring/echosetup.go:18-21`, `wiring/readiness.go` |
| `GET /debug/runtime` | token | `main.go:104` |
| `/api/admin/*` (incl. `POST /api/admin/ai-gateway-simulator/forward`) | `middleware.AdminAuth` (authserver JWT **or** admin API key) | `wiring/routes.go:172-178,225,231` |
| `/api/internal/*` | `rstokenauth.Middleware(InternalServiceToken)` | `wiring/routes.go:235` |
| `/api/my/*` | `middleware.AdminAuth` | `wiring/routes.go:248-253` |
| `/scim/v2/*` | `h.scimAuth` (SCIM bearer) | `scim/handler/scim.go:95` |
| `GET /.well-known/jwks.json` `/openid-configuration` | **public** | `authserver/mount.go:111-113` |
| `GET /oauth/authorize` `POST /oauth/token` `/introspect` `/revoke` | **public** (protocol client/code/PKCE checks, no middleware) | `mount.go:143,186,197,209` |
| `POST /oauth/device-binding` | `middleware.AgentMTLSAuth` | `mount.go:231` |
| `POST /authserver/logout` | cookie session verify + CSRF (in handler) | `mount.go:254` |
| `GET /authserver/idps` `POST /authserver/password` | **public** SPA login (password has own rate limiter) ⚠ | `mount.go:263,275` |
| `POST /authserver/approve` | `JWTVerifier.MiddlewareWithCookieFallback` (Bearer OR cp-ui cookie + CSRF) | `mount.go:293-300` |
| `GET /authserver/idp/:idpId/start` `.../logout` `oidc/callback` `POST saml/acs` `GET saml/metadata` | **public** external-IdP SSO legs (state/PKCE/InResponseTo in handlers) ⚠ | `mount.go:348-377` |
| `POST /api/agent/sso-enroll` | **no group mw**; signed auth-code in handler ⚠ | `wiring/authserver.go:97` |

### ai-gateway (`http.ServeMux`, single listener)

| Port | Bind | Protocol | file:line |
|------|------|----------|-----------|
| 3050 | 0.0.0.0 (default; `AI_GATEWAY_HOST` → loopback) | HTTP | `cmd/ai-gateway/main.go:140`, addr `config/config.go:223` |

⚠ **VK data-plane and operator `/internal/*` routes share one `0.0.0.0` port** — config docstring
(`config.go:207-212`) explicitly flags this as why loopback binding matters.

| Route group | Gate | file:line |
|-------------|------|-----------|
| `GET /healthz` | **none** | `wiring/routes.go:89` |
| `GET /metrics` | token (`InternalServiceToken`) | `wiring/routes.go:102` |
| `/internal/*` (provider-test, routing-simulate, credentials/{id}/probe, hooks-test, embedding-probe, semantic-prewarm) | `guard.require` (internal-service token, `subtle.ConstantTimeCompare`, fail-closed 503 on empty) | `wiring/routes.go:176-199`, `wiring/internalauth.go:36` |
| `/v1/chat/completions` `/embeddings` `/responses` `/messages` `/estimate`, Gemini `/v1beta/...`, Azure `/openai/deployments/...`, GLM `/api/paas/v4/...` | **VK auth inside handler** (`ServeProxy`→`authenticate`) | `wiring/routes.go:202-253`, `internal/ingress/proxy/proxy.go:424` |
| `GET /v1/models` `/{model}` `/v1/usage` `/usage/daily` | VK auth + per-VK read rate limit (`vkReadRateLimit`) | `wiring/routes.go:271-275` |
| `/v1/ai-guard/classify` `/compliance-webhook` | `rstokenauth.MiddlewareHTTP(InternalServiceToken)` (F-0044 fix) | `wiring/thingclient.go:395-403` |
| `GET /debug/runtime`, `/runtime/config*` `/sync-status` `/health` | token (`InternalServiceToken`; F-0243 folded `AI_GATEWAY_API_TOKEN` in) | `wiring/runtimeapi.go:174,181`, `internal/runtimeapi/server.go:56-59` |

### compliance-proxy (THREE listeners)

| Port | Bind | Protocol | file:line |
|------|------|----------|-----------|
| 3128 | **0.0.0.0** | HTTP CONNECT / transparent **MITM** (TLS bump) | `internal/proxy/server/server.go:626-635`, addr `main.go:226` |
| 9090 | **0.0.0.0** | HTTP health/metrics | `cmd/compliance-proxy/wiring/servers.go:148`, `main.go:204` |
| 3040 | **127.0.0.1** | HTTP runtime/break-glass API | `internal/runtime/server/server.go:107-137`, addr `servers.go:113-116` |

| Surface | Gate | file:line |
|---------|------|-----------|
| `:3128` MITM intercept | verifies `X-Nexus-Attestation` header, then TLS-bump + pipeline ⚠ | `config.go:142-143`, server.go relay |
| `:9090` `/healthz` `/readyz` `/metrics` | **none** ⚠ (distinct from token-gated `:3040` /metrics) | `internal/health/handler.go:63,78,124` |
| `:9090` `/debug/runtime` | token | `wiring/health.go:135` |
| `:9090` `/management/ca-cert` | **none** — unauthenticated CA *cert* (public key) download ⚠ | `wiring/health.go:141` |
| `:3040` `/metrics` `/connections` `/runtime/config*` (PUT = break-glass) `/sync-status` `/health` | `tokenAuth.Require` (`COMPLIANCE_PROXY_API_TOKEN`) | `internal/runtime/server/server.go:48-101` |
| `:3040` `/healthz` | **none** | `server.go:59` |

### agent (no `0.0.0.0` listener; loopback + local IPC only)

| Socket | Bind | Protocol | file:line |
|--------|------|----------|-----------|
| `127.0.0.1:19080` (Linux/Windows) | loopback | TCP transparent intercept (iptables REDIRECT / WFP) | `internal/platform/linux/linux_linux.go:137`, `windows/windows_windows.go:134` |
| `127.0.0.1:9443` (macOS) | loopback | TCP bridge (Swift NE → Go TLS-bump) | `internal/network/bridge/listener.go:98` |
| macOS NE control socket | filesystem (chmod 0600) | Unix domain socket | `internal/platform/darwin/platform_darwin.go:146` |
| Status IPC socket | Linux 0600 / macOS 0666 + `LOCAL_PEERCRED` UID check; Windows named pipe (owner SDDL) | local IPC | `internal/sync/status/statusapi_listen_other.go:31`, `_windows.go:18` |
| `127.0.0.1:0` (ephemeral) | loopback | one-shot OAuth/SSO enroll callback (validates `state`) | `internal/identity/enrollment/sso_server.go:29` |

Gates: transparent-proxy listeners have **no app-layer auth** — trust boundary is loopback bind +
OS firewall redirect. Status IPC gated by filesystem perms / peer-UID / pipe ACL. No service-token
HTTP API. Agent is a pure client to Hub/CP plus these local-only sockets.

### control-plane-ui (static SPA, no Go service)

| Port | Bind | Protocol | file:line |
|------|------|----------|-----------|
| 3000 (Vite dev) | **0.0.0.0** (`server.host: '0.0.0.0'`) | HTTP dev | `vite.config.ts:86-88` |

No auth at the UI listener; serves the React bundle and proxies `/api` `/oauth` `/.well-known`
`/authserver` `/idp` to the CP. Vite `allowedHosts` is a Host-header allowlist, not auth.
The dev server is the package's only listener: in production the built bundle is static files
served by the deployment host's front nginx on `:443` (`nexus-ami/artifacts/configs/nginx-nexus.conf`),
which is inventoried as deployment infrastructure, not a service listener of this package.

---

## 2. Crown-jewel asset inventory (confirmed at HEAD `77e86466f`)

> Re-verified against settled code. **Scaffold drift is flagged** — the original inventory was
> written pre-arch-audit and several claims are now wrong.

### CJ-1. MITM CA private key — **two distinct CAs**

- **Compliance-proxy TLS-bump CA** (the actual MITM CA): `compliance-proxy/internal/tls/issuer/issuer.go`.
  - Local mode: CA cert+key from disk PEM (`cfg.CA.CertPath`/`KeyPath`), `NewIssuer` (`issuer.go:82-134`);
    key file may be KMS-wrapped — `kmsProvider.Decrypt` once at startup (`issuer.go:105`).
  - Leaf signing: `SignCert`, ECDSA P-256, 24h validity (`issuer.go:38,170-176`).
  - **Remote-signing mode** (`NewIssuerWithRemoteSigner`, `remote_signer.go:117`): CA private key
    never local — every signature proxies to a KMS sign command (`CommandSigner.Sign`,
    `remote_signer.go:66`); branch select `wiring/cert.go:51` on `cfg.CA.KMS.SigningMode=="remote"`.
  - ⚠ **Scaffold drift:** scaffold listed only `issuer.go`; it omitted the remote-signing path and
    KMS-wrapped on-disk key. Both are M1 attack surface (KMS command-injection, fail-open on KMS
    error, key-at-rest when local mode is configured).
- **Hub agent-attestation CA**: `nexus-hub/internal/identity/agentca/ca.go` (unchanged). Self-signed
  ECDSA P-256, auto-gen or load `dir/ca.pem`+`dir/ca-key.pem` (0600). Signs **Ed25519** attestation
  CSRs only, **no ClientAuth EKU** (key-separation invariant). This is *not* the MITM CA — different
  blast radius; M1/M4 must keep them distinct.

### CJ-2. Credential encryption — **raw hex key, no KDF** (scaffold was wrong)

- CP vault: `control-plane/internal/platform/crypto/aes_gcm.go` — AES-256-GCM (`InitVault:67`,
  `Encrypt:107`, `Decrypt:138`, `MultiVault:206`).
- AI-gw decryptor: `ai-gateway/internal/credentials/decrypt/decrypt.go` (pkg `creddecrypt`),
  decrypt-only AES-256-GCM.
- ⚠ **Scaffold drift (material):** there is **NO** `CREDENTIAL_ENCRYPTION_PASSPHRASE` / `_SALT` /
  KDF for credential encryption. The key is a **raw 64-hex (32-byte) value** hex-decoded directly
  (`aes_gcm.go:80-90`, `decrypt.go:29-35`). Those env vars do not exist anywhere. `CREDENTIAL_KEY_MAP`
  (`id:hex64,...`, `*` = current key, `aes_gcm.go:199`) takes precedence. Env reads:
  `control-plane/.../config.go:335,338`, `ai-gateway/internal/config/config.go:323,326`,
  Hub `internal/alerts/engine/secret.go:65`. **M2 implication:** key strength = env-var hygiene
  only; no passphrase-stretching layer to attack, but also none to protect a weak/leaked key.
- **F-0019 envelope (DEK→KMS Encrypt→Redis SETNX→Decrypt) exists but is the compliance-proxy
  cert-cache DEK**, NOT the credential vault: `compliance-proxy/internal/tls/issuer/cert_cache_dek.go`
  (`bootstrapCertCacheDEK:69`, SETNX `nexus:proxy:cert-cache-dek`, fail-closed). The credential
  vault has no such envelope. ⚠ Scaffold implied F-0019 covers credentials — it does not.

### CJ-3. Inter-service tokens

- **INTERNAL_SERVICE_TOKEN** — read in all four services' config; boot-required (`validate`).
  Server checks: Hub `ServiceAuth` constant-time (`handler/middleware.go:15`); ai-gw `/internal/*`
  guard `subtle.ConstantTimeCompare` fail-closed 503 (`wiring/internalauth.go:36`); ai-gw runtime
  `internal/runtimeapi/auth.go:42`; CP `/api/internal` `rstokenauth.Middleware`. **F-0001 confirmed:**
  ai-gw `/internal/*` token-gated, 6+ CP callers send it.
- **ADMIN_KEY_HMAC_SECRET** — HMAC-hashes VK/admin keys before DB lookup (`authn/apikey.go:30,53`).
  Not network-presented. **`[MUST MATCH]` across CP-mint and gateway-verify** (see CJ-4).
- **AI_GATEWAY_API_TOKEN** — ⚠ **REMOVED** (F-0243); `/runtime/*` folded onto `INTERNAL_SERVICE_TOKEN`.
  Scaffold lists it as live; it no longer exists.
- **COMPLIANCE_PROXY_API_TOKEN** — gates `:3040` runtime API + break-glass PUT, fail-closed
  (`compliance-proxy/internal/runtime/auth/auth.go`). CP→proxy BFF sends it (`cfg.BFF.ComplianceProxyAPIToken`).

### CJ-4. Virtual keys — `ai-gateway/internal/auth/vkauth/`

- Format: `nvk_` + 64 hex (32 random bytes). Minted CP-side (`virtualkeys/handler/handler.go:204`).
- At-rest: never plaintext. `keyHash = HMAC-SHA256(key, ADMIN_KEY_HMAC_SECRET)` (`authn/apikey.go:53`);
  only `key_hash` + 12-char `key_prefix` persisted.
- Verify: gateway extracts token from multiple carriers (`X-Nexus-Virtual-Key`, `Bearer`, `x-api-key`,
  `x-goog-api-key`, `?key=`, `api-key`; `vkauth.go:217`), recomputes the **same HMAC** (`vkauth.go:283`),
  exact DB lookup `GetVirtualKeyByHash`. The HMAC hash *is* the lookup key ⇒ CP-mint and gateway-verify
  **must share `ADMIN_KEY_HMAC_SECRET`** (drift = total VK-auth failure). Status/enabled/expiry
  re-checked per request (`vkauth.go:152-160`).

### CJ-5. Agent enrollment / identity

- Hub: `nexus-hub/internal/identity/handler/enroll/enrollment_handler.go` + `store/enrollstore/`,
  `agentca/`. Agent: `packages/agent/internal/identity/{enrollment,keystore,auth}/`.
- **F-0200 confirmed:** thingId is **server-assigned** (`resolveThingID:455-461`, 8 random bytes,
  no caller-supplied ID); token-type is **authoritative** from the enrollment token (`:264-271`).
- Two credential paths (`Enroll:226-242`): (1) Bearer enrollment JWT (RSA-only, aud
  `nexus-hub-enrollment`, issuer-pinned, JTI replay-guard) → binds device to JWT `sub`
  (`UpsertDeviceAssignment:388`) with F-0329 cross-user-takeover guard on attacker-controllable
  `DeviceFingerprint`/`physical_id` (`:337-358`); (2) X-Enrollment-Token (legacy) consume-first
  single-use (`ConsumeToken:253-292`).
- Device token: minted via `agentca.GenerateDeviceToken`, plaintext to agent, only **SHA-256(token)**
  stored (`StoreDeviceTokenHash:494`), 30-day TTL. Optional Ed25519 attestation CSR signed by Hub
  agent CA. Agent private keys in platform keystore (`keystore_{darwin,linux,windows}.go`).

---

## 3. Attacker-relevant observations carried into Phase 1+ (NOT yet findings)

These are the ⚠ flags above, consolidated as the to-verify queue. Each becomes a candidate finding
only after adversarial verification with a re-check probe.

| # | Observation | Surface | Phase/Module |
|---|-------------|---------|--------------|
| O1 | Default bind is `0.0.0.0` for Hub/CP/AI-gw/proxy:3128/proxy:9090; loopback opt-in not in default yaml | all | W (deployment) + per-module |
| O2 | AI-gw VK data-plane shares `0.0.0.0:3050` with operator `/internal/*` (token-gated, but co-located) | ai-gw | M3/M6 |
| O3 | Compliance-proxy `:9090/metrics` **unauthenticated** on 0.0.0.0; `/management/ca-cert` unauthenticated CA-cert download | proxy | M1/M9 |
| O4 | Hub `/ws` upgrade has no Echo middleware — all auth/origin inside `HandleUpgrade` | Hub | M5 |
| O5 | Hub `GET /api/public/agent-bootstrap` explicitly public | Hub | M4/M9 |
| O6 | Hub `PUT /api/internal/spill/blob/:token` authorized solely by path-embedded HMAC token, outside deviceAuth group | Hub | M5/C1 |
| O7 | CP public login surface: `/authserver/password`, `/idps`, external-IdP SSO legs — info-leak / enumeration / state-handling | CP | M7 |
| O8 | `POST /api/agent/sso-enroll` has no group middleware; gated only by in-handler signed auth-code | CP | M4/M7 |
| O9 | Credential encryption is a flat env hex key — no passphrase stretching; weak/leaked key = full provider-key compromise | crypto | M2/M9 |
| O10 | Two CAs (MITM TLS-bump vs Hub attestation) must stay key-separated; MITM CA has local-disk + remote-KMS modes, KMS via shell command | proxy/Hub | M1 |
| O11 | `ADMIN_KEY_HMAC_SECRET` is the single shared secret behind both admin-key and VK auth — drift or leak is high-blast | CP/ai-gw | M3/M9 |

---

## 4. Scaffold-drift corrections folded back

Recorded so the program tracker's crown-jewel inventory is not trusted blindly:

1. Credential crypto: **no `_PASSPHRASE`/`_SALT`/KDF** — raw 32-byte hex key. (tracker §Crown-jewel #2 outdated)
2. F-0019 KMS envelope = **compliance-proxy cert-cache DEK**, not credential vault.
3. **`AI_GATEWAY_API_TOKEN` removed** (F-0243) — folded onto `INTERNAL_SERVICE_TOKEN`. (tracker §Crown-jewel #3 outdated)
4. F-0001 (`/internal/*` token-gate) and F-0200 (server-assigned thingId) confirmed live in current code.
5. MITM "CA private key" is **two CAs** with different blast radius.

---

## 5. Next step

Phase 1 module-level audit (M1–M9), each a deep multi-agent workflow (fan-out finders → Opus
adversarial verification → only confirmed findings survive). The O1–O11 queue above seeds the
finder prompts. Report to user between phases — phases are not auto-chained.
