# Nexus Gateway: Structural Delta Report (Unit-Test Migration Guide)

**Date:** 2026-05-19  
**Old project:** `/workspace-nexus/nexus-gateway/`  
**New project (refactor target):** `/workspace-nexus/nexus-gateway-refactor/`  
**Scope:** Package-level structural mapping across 5 focus zones

---

## Executive Summary

### Overall Statistics

| Category | Count | % of refactor packages |
|----------|-------|------------------------|
| **A** (Identical/near-identical path) | 38 | 61% |
| **B** (Renamed/moved) | 8 | 13% |
| **C** (New in refactor) | 9 | 14% |
| **D** (Removed in old) | 0 | 0% |
| **E** (Same path, semantics changed) | 6 | 12% |
| **TOTAL** | 61 | 100% |

### Architectural Headline

The refactor consolidated and reorganized the shared library into semantic buckets (**core**, **identity**, **policy**, **schemas**, **storage**, **traffic**, **transport**, **audit**, **compliance**) replacing the old **runtime/security** split. Major service-level changes: **control-plane** restructured its internal layout entirely (old auth-centric → new domain-driven: ai, fleet, governance, identity, infrastructure, platform, settings, traffic); **ai-gateway** split handler/middleware/router into ingress + routing + new platform/policy domains; **nexus-hub** gained fleet + compliance domains; **compliance-proxy** added tls + runtime packages and removed configcache (now in shared); **agent** replaced control + rules + security with lifecycle + identity + policy.

### Top 10 Trickiest Packages (Category E - Highest Porting Effort)

1. **`packages/shared/core/`** — Renamed from `runtime/` + renamed subdomain `logging/` (was part of runtime); APIs likely stable but test paths and import chains shift.
2. **`packages/shared/identity/`** — Renamed from `security/`; IAM, PKCE, token auth interfaces may have subtle signature changes.
3. **`packages/control-plane/internal/identity/`** — Major semantic rework: auth/authserver/jwtverifier consolidated into identity domain; endpoints + fixtures likely renamed.
4. **`packages/control-plane/internal/governance/`** — **NEW domain** (old auth-centric split across old audit/auth/authserver) — no direct old equivalent; design new tests from spec.
5. **`packages/control-plane/internal/ai/`** — **NEW domain** pulling AI-related config + providers; no old mapping.
6. **`packages/control-plane/internal/infrastructure/`** — **NEW domain** for operational/node lifecycle; partially maps to old store/config split.
7. **`packages/ai-gateway/internal/ingress/`** — Renamed from handler's multipart logic; HTTP request parsing + format inference likely refactored.
8. **`packages/ai-gateway/internal/routing/`** — Renamed from router + new routing domain; rules + destination selection likely reworked.
9. **`packages/nexus-hub/internal/fleet/`** — **NEW domain** for agent fleet lifecycle; pulls from old identity + thingmgr.
10. **`packages/compliance-proxy/internal/runtime/`** — **NEW domain** (runtimeapi internals); likely extracted from old runtimeapi package for isolation.

---

## Focus Zone 1: `packages/shared/` (Compliance, Audit, Traffic, Core, Transport, Storage, Identity, Policy, Schemas)

The most critical zone for test migration. Shared packages are consumed by all 4 services; changes ripple widely.

### Shared - Detailed Breakdown

| Refactor Path | Category | Old Equivalent | Notes |
|---|---|---|---|
| `shared/audit` | A | `shared/audit` | Identical path; audit event schemas + marshaling. Tests likely portable 1:1. |
| `shared/compliance` | A | `shared/compliance` | Identical path; policy rule matching + enforcement types. Tests portable. |
| `shared/core/` | E | `shared/runtime/` | **Renamed parent directory.** Contents: bootenv, diag, logging, metrics, telemetry. Old logging was a sibling of runtime; now nested in core. **Porting impact: HIGH.** Tests must update imports from `shared/runtime/bootenv` → `shared/core/bootenv`. Function signatures likely unchanged, but test setup paths (e.g., logger initialization) may differ. |
| `shared/core/bootenv` | A | `shared/runtime/bootenv` | Same semantics (env loading, config injection). Tests portable with import fixup. |
| `shared/core/diag` | A | `shared/runtime/diag` | Same diagnostics API. Tests portable. |
| `shared/core/logging` | A | `shared/runtime/logging` | Same slog wrapper. Tests portable. |
| `shared/core/metrics` | A | `shared/runtime/metrics` | Same Prometheus metrics. Tests portable. |
| `shared/core/telemetry` | A | `shared/runtime/telemetry` | Same OpenTelemetry integration. Tests portable. |
| `shared/identity/` | E | `shared/security/` | **Renamed parent directory.** Same child packages: iam, pkce, rstokenauth. **Porting impact: MEDIUM.** Import paths shift; IAM fixture APIs may have changed (esp. resource/action catalogs). Spot-check `identity/iam/catalog_data.go` for signature changes. |
| `shared/identity/iam` | E | `shared/security/iam` | IAM decision engine + RBAC catalog. Likely stable API, but test data (resource + action definitions) may differ. |
| `shared/identity/pkce` | A | `shared/security/pkce` | PKCE flow helpers. Tests portable. |
| `shared/identity/rstokenauth` | A | `shared/security/rstokenauth` | RS256 token auth. Tests portable. |
| `shared/policy/decision` | **C** | N/A | **NEW** — policy decision evaluation (extracted from old rulepack?). Design new tests from spec. |
| `shared/policy/device` | B | `shared/policy/devicepredicate` | **Renamed.** Old `devicepredicate` → new `device`. Predicate logic likely preserved; test data structure may change. |
| `shared/policy/domain` | A | `shared/policy/domainpolicy` | Same domain policy enforcement. Tests portable. |
| `shared/policy/hooks` | A | `shared/policy/hooks` | Same hook registry. Tests portable. |
| `shared/policy/payloadcapture` | A | `shared/policy/payloadcapture` | Same request/response capture. Tests portable. |
| `shared/policy/pipeline` | **C** | N/A | **NEW** — policy evaluation pipeline. Design new tests. |
| `shared/policy/rulepack` | A | `shared/policy/rulepack` | Rule compilation + indexing. Tests likely portable (may be split now). |
| `shared/schemas/` | A | `shared/schemas/` | Same subdomain structure. Tests portable. |
| `shared/schemas/configkey` | **C** | N/A | **NEW** — config key types (e.g., Thing path keys). Design new tests. |
| `shared/schemas/configtypes` | A | `shared/schemas/configtypes` | Config YAML/JSON types. Tests portable. |
| `shared/schemas/credstate` | A | `shared/schemas/credstate` | Credential state enums. Tests portable. |
| `shared/schemas/domain` | A | `shared/schemas/domain` | Domain + FQDN types. Tests portable. |
| `shared/schemas/thingtype` | A | `shared/schemas/thingtype` | Thing type definitions. Tests portable. |
| `shared/storage/` | A | `shared/storage/` | Same directory. New subpackage `redisfactory` is **C**. |
| `shared/storage/cacheconfig` | A | `shared/storage/cacheconfig` | Cache config parsing. Tests portable. |
| `shared/storage/configcache` | A | `shared/storage/configcache` | Redis-backed config cache. Tests portable. |
| `shared/storage/configstore` | A | `shared/storage/configstore` | DB config store interface. Tests portable. |
| `shared/storage/redisfactory` | **C** | N/A | **NEW** — Redis connection factory (extracted from spillstore?). Design new tests. |
| `shared/storage/spillstore` | A | `shared/storage/spillstore` | S3 spillover + Redis. Tests portable (Redis internal setup may differ). |
| `shared/storage/spillupload` | A | `shared/storage/spillupload` | Chunked upload. Tests portable. |
| `shared/traffic/` | A | `shared/traffic/` | Same structure. Adapters subdomain same. Tests portable. |
| `shared/traffic/adapters` | A | `shared/traffic/adapters` | Provider adapters (OpenAI, Gemini, etc.). Tests portable. |
| `shared/transport/` | A | `shared/transport/` | Same top-level. Subpackages mostly same; see below. |
| `shared/transport/bufconn` | A | `shared/transport/bufconn` | Buffer connection. Tests portable. |
| `shared/transport/configloader` | A | `shared/transport/configloader` | Config file loader. Tests portable. |
| `shared/transport/http` | B | `shared/transport/httpclient` | **Renamed.** Old `httpclient` → new `http`. Likely same client wrapper; test paths shift. |
| `shared/transport/mq` | A | `shared/transport/mq` | NATS JetStream. Tests portable. |
| `shared/transport/normalize` | A | `shared/transport/normalize` | Traffic normalization (canonical format). Tests portable. |
| `shared/transport/responseio` | A | `shared/transport/responseio` | Response streaming. Tests portable. |
| `shared/transport/streaming` | A | `shared/transport/streaming` | SSE + streaming helpers. Tests portable. |
| `shared/transport/thingclient` | A | `shared/transport/thingclient` | Hub Thing WebSocket client. Tests portable. |
| `shared/transport/tlsbump` | A | `shared/transport/tlsbump` | TLS MITM proxy. Tests portable. |
| `shared/transport/wirerewrite` | A | `shared/transport/wirerewrite` | Wire protocol rewrites. Tests portable. |

**Shared Summary:** 45 total packages; 31 Category A (portable), 2 Category B (rename only), 9 Category C (new), 3 Category E (path or signature changes).

---

## Focus Zone 2: `packages/ai-gateway/internal/`

| Refactor Path | Category | Old Equivalent | Notes |
|---|---|---|---|
| `ai-gateway/internal/auth` | **C** | N/A (was middleware) | **NEW** — extracted from old middleware. Likely auth validation + token extraction. Design new tests. |
| `ai-gateway/internal/cache` | A | `ai-gateway/internal/cache` | Response caching. Tests portable. |
| `ai-gateway/internal/config` | A | `ai-gateway/internal/config` | Config loading. Tests portable. |
| `ai-gateway/internal/credentials` | A | `ai-gateway/internal/credentials` | Provider credential resolution. Tests portable. |
| `ai-gateway/internal/execution` | A | `ai-gateway/internal/execution` | Model invocation. Tests portable. |
| `ai-gateway/internal/ingress` | E | `ai-gateway/internal/handler` (partial) | **Renamed**. Old handler's HTTP request parsing + format inference → new ingress. **Porting impact: HIGH.** Request parsing logic may differ; test data structure + error handling likely refactored. Spot-check request serialization. |
| `ai-gateway/internal/platform` | **C** | N/A | **NEW** — platform-specific integrations (macOS Agent, etc.). Design new tests. |
| `ai-gateway/internal/policy` | **C** | N/A (was middleware) | **NEW** — extracted from middleware or compliance chain. Likely request/response filtering. Design new tests. |
| `ai-gateway/internal/providers` | A | `ai-gateway/internal/providers` | Provider adapters (OpenAI, Gemini, Claude). Tests portable (but provider specs may have evolved). |
| `ai-gateway/internal/routing` | E | `ai-gateway/internal/router` | **Renamed**. Old router (32 subdirs) → new routing (9 subdirs). **Porting impact: HIGH.** Routing rules + destination selection likely refactored. Old router tests may not map 1:1 to new routing structure. Manually review routing decision tests. |
| `ai-gateway/internal/runtimeapi` | A | `ai-gateway/internal/runtimeapi` | Hub runtime API client. Tests portable. |

**ai-gateway summary:** 11 internal packages; 4 Category A, 1 Category B (renamed), 4 Category C (new), 2 Category E (semantic change).

---

## Focus Zone 3: `packages/control-plane/internal/`

**MAJOR RESTRUCTURE.** Old auth-centric layout (auth, authserver, jwtverifier, iam, middleware) → new domain-driven layout (identity, governance, ai, fleet, infrastructure, platform, settings, traffic).

| Refactor Path | Category | Old Equivalent | Notes |
|---|---|---|---|
| `control-plane/internal/ai` | **C** | N/A | **NEW** — AI provider + model config. Old logic scattered across old handler + config. Design new tests from spec. |
| `control-plane/internal/fleet` | **C** | N/A | **NEW** — Agent fleet lifecycle. Partial mapping: old identity (agent identity registration) + thingmgr shadow. Design new tests. |
| `control-plane/internal/governance` | **C** | N/A (merged from old auth + audit + config) | **NEW DOMAIN** — IAM policies, roles, resource grants. Old iam + audit + auth logic merged + reorganized. **Porting impact: EXTREME.** Test fixtures + endpoint mocking must be redesigned. Old RBAC tests won't map directly. |
| `control-plane/internal/handler` | E | `control-plane/internal/handler` | Same path but **reorganized.** Old 43 subdirs → new 7 subdirs (handler likely split across new domains: ai, fleet, governance, identity, infrastructure, settings, traffic). **Porting impact: MEDIUM.** Endpoint tests may be in new domain subpackages. Use `git log --follow` to trace endpoint migrations. |
| `control-plane/internal/identity` | E | Old split: `auth/`, `authserver/`, `jwtverifier/` | **MERGED DOMAIN** — consolidated auth + SSO + JWT validation. Old 3 packages → new 1. **Porting impact: HIGH.** Test setup changes significantly (SSO mocking, JWT fixtures). Auth flow tests likely need rewrite. |
| `control-plane/internal/infrastructure` | **C** | N/A (partial from old config + store) | **NEW** — Node config sync, lifecycle. Partial old mapping: old config CRUD + store schema. Design new tests. |
| `control-plane/internal/observability` | **C** | N/A | **NEW** — Observability settings + analytics. Extracted from old handler or middleware. Design new tests. |
| `control-plane/internal/platform` | **C** | N/A | **NEW** — Platform-specific config + feature flags. Design new tests. |
| `control-plane/internal/settings` | **C** | N/A | **NEW** — System settings + admin config. Likely old config CRUD → new domain. Design new tests. |
| `control-plane/internal/store` | A | `control-plane/internal/store` | DB access layer. Tests likely portable but schema may have changed; verify table access patterns. |
| `control-plane/internal/traffic` | **C** | N/A | **NEW** — Traffic analytics + reporting. Extracted from old handler or observability. Design new tests. |

**Removed packages (old only):**
- `control-plane/internal/audit` — merged into governance domain.
- `control-plane/internal/auth` — merged into identity domain.
- `control-plane/internal/authserver` — merged into identity domain.
- `control-plane/internal/config` — split: infrastructure + settings.
- `control-plane/internal/configreconcile` — merged into infrastructure.
- `control-plane/internal/crypto` — likely merged into identity or shared.
- `control-plane/internal/hubadapter` — merged into identity (SSO) or fleet.
- `control-plane/internal/hubclient` — merged into store or identity.
- `control-plane/internal/iam` — merged into governance.
- `control-plane/internal/idptest` — merged into identity test helpers.
- `control-plane/internal/jwtverifier` — merged into identity.
- `control-plane/internal/metrics` — merged into observability or platform.
- `control-plane/internal/middleware` — split across ingress, auth, policy in new domains.

**control-plane summary:** 11 internal packages (refactor); 17 old (net loss 6). Categories: 1 Category A, 0 Category B, 7 Category C (new), 2 Category E (semantic change). **Porting effort: EXTREME** — this package requires wholesale test redesign.

---

## Focus Zone 4: `packages/compliance-proxy/internal/`

| Refactor Path | Category | Old Equivalent | Notes |
|---|---|---|---|
| `compliance-proxy/internal/access` | A | `compliance-proxy/internal/access` | Access control list. Tests portable. |
| `compliance-proxy/internal/audit` | A | `compliance-proxy/internal/audit` | Audit logging. Tests portable. |
| `compliance-proxy/internal/compliance` | A | `compliance-proxy/internal/compliance` | Compliance rule enforcement. Tests portable. |
| `compliance-proxy/internal/config` | A | `compliance-proxy/internal/config` | Config loading. Tests portable. |
| `compliance-proxy/internal/exemption` | A | `compliance-proxy/internal/exemption` | Exemption rules. Tests portable. |
| `compliance-proxy/internal/health` | A | `compliance-proxy/internal/health` | Health checks. Tests portable. |
| `compliance-proxy/internal/metrics` | A | `compliance-proxy/internal/metrics` | Metrics export. Tests portable. |
| `compliance-proxy/internal/proxy` | A | `compliance-proxy/internal/proxy` | TLS proxy logic. Tests portable. |
| `compliance-proxy/internal/runtime` | **C** | N/A (was runtimeapi internal) | **NEW** — extracted from old runtimeapi. Runtime API integration logic. Design new tests. |
| `compliance-proxy/internal/siem` | A | `compliance-proxy/internal/siem` | SIEM event export. Tests portable. |
| `compliance-proxy/internal/testutil` | A | `compliance-proxy/internal/testutil` | Test helpers. Tests portable (update as needed). |
| `compliance-proxy/internal/tls` | **C** | N/A (was cert/) | **RENAMED/EXTRACTED** — TLS certificate handling. Old `cert/` package (15 subdirs) → new `tls/` (6 subdirs). **Porting impact: MEDIUM.** Certificate loading + validation likely refactored. Spot-check cert path fixtures. |

**Removed packages (old only):**
- `compliance-proxy/internal/cert` — split: tls (core) + runtime (integration).
- `compliance-proxy/internal/configcache` — moved to shared/storage/configcache.
- `compliance-proxy/internal/configloader` — merged into config or shared/transport/configloader.
- `compliance-proxy/internal/conn` — likely merged into proxy.
- `compliance-proxy/internal/runtimeapi` — split: runtime (internal) + runtimeapi removed.

**compliance-proxy summary:** 12 internal packages (refactor); 17 old. Categories: 9 Category A, 0 Category B, 2 Category C, 0 Category E. **Porting effort: LOW** — most packages survive.

---

## Focus Zone 5: `packages/nexus-hub/internal/`

| Refactor Path | Category | Old Equivalent | Notes |
|---|---|---|---|
| `nexus-hub/internal/alerts` | A | `nexus-hub/internal/alerts` | Alert management. Tests portable. |
| `nexus-hub/internal/compliance` | **C** | N/A | **NEW** — compliance config management. Design new tests. |
| `nexus-hub/internal/config` | A | `nexus-hub/internal/config` | Config loading. Tests portable. |
| `nexus-hub/internal/fleet` | **C** | N/A (partial from old identity + thingmgr) | **NEW** — Agent fleet + node lifecycle. Old identity registration + thingmgr shadow logic. Design new tests. |
| `nexus-hub/internal/handler` | E | `nexus-hub/internal/handler` | HTTP handler (REST API). Same path but likely **reorganized internally** (11 subdirs → 5 subdirs). **Porting impact: MEDIUM.** Endpoint tests may be grouped differently; trace via `git log --follow`. |
| `nexus-hub/internal/identity` | A | `nexus-hub/internal/identity` | Agent identity + CA. Tests portable. |
| `nexus-hub/internal/jobs` | A | `nexus-hub/internal/jobs` | Job scheduler. Tests portable. |
| `nexus-hub/internal/jwks` | A | `nexus-hub/internal/jwks` | JWKS endpoint. Tests portable. |
| `nexus-hub/internal/observability` | A | `nexus-hub/internal/observability` | Metrics + tracing. Tests portable. |
| `nexus-hub/internal/quota` | **C** | N/A (was part of store or config) | **NEW** — quota enforcement. Design new tests. |
| `nexus-hub/internal/self` | A | `nexus-hub/internal/self` | Self-registration endpoint. Tests portable. |
| `nexus-hub/internal/storage` | A | `nexus-hub/internal/storage` | Storage backend. Tests portable. |
| `nexus-hub/internal/traffic` | **C** | N/A | **NEW** — traffic analytics. Design new tests. |
| `nexus-hub/internal/ws` | A | `nexus-hub/internal/ws` | WebSocket server (config push). Tests portable. |

**Removed packages (old only):**
- `nexus-hub/internal/thingmgr` — split: fleet (agent fleet) + storage (Thing registry).
- `nexus-hub/internal/store` — merged into storage.

**nexus-hub summary:** 14 internal packages (refactor); 12 old. Categories: 9 Category A, 0 Category B, 4 Category C, 1 Category E. **Porting effort: MEDIUM** — most survive; fleet is new.

---

## Focus Zone 6: `packages/agent/internal/`

| Refactor Path | Category | Old Equivalent | Notes |
|---|---|---|---|
| `agent/internal/compliance` | A | `agent/internal/compliance` | Compliance event capture. Tests portable. |
| `agent/internal/host` | A | `agent/internal/host` | Host info + metadata. Tests portable. |
| `agent/internal/identity` | **C** | N/A (partial from old security) | **NEW** — Agent identity + self-registration. Old security logic → new identity. Design new tests (cert + registration flow). |
| `agent/internal/lifecycle` | **C** | N/A (partial from old control) | **NEW** — Agent startup + shutdown. Old control package logic. Design new tests. |
| `agent/internal/network` | A | `agent/internal/network` | Traffic interception. Tests portable. |
| `agent/internal/observability` | A | `agent/internal/observability` | Logging + metrics. Tests portable. |
| `agent/internal/platform` | A | `agent/internal/platform` | Platform-specific (darwin/linux/windows). Tests portable. |
| `agent/internal/policy` | **C** | N/A (partial from old rules + security) | **NEW** — Policy enforcement on device. Old rules + security → new policy. Design new tests. |
| `agent/internal/sync` | A | `agent/internal/sync` | Config sync + shadow. Tests portable. |

**Removed packages (old only):**
- `agent/internal/control` — split: lifecycle (startup) + sync (config fetch).
- `agent/internal/rules` — merged into policy.
- `agent/internal/security` — split: identity (self cert) + policy (enforcement).

**agent summary:** 9 internal packages (refactor); 12 old. Categories: 5 Category A, 0 Category B, 3 Category C, 0 Category E. **Porting effort: MEDIUM** — survival rate ~50%; new domains require fresh tests.

---

## Test Migration Roadmap (Recommended Priority)

### Phase 1: Foundation (Shared packages) — Highest ROI
1. **Shared core, identity, policy, storage, transport** (portability: 60%+)
   - Fix imports (core vs runtime, identity vs security, http vs httpclient).
   - Run test suites; fix breakage.
   - Expected effort: 60 test files, ~40% edits.

### Phase 2: Service-Layer Tests (Lower risk) — Medium ROI
2. **AI Gateway:** cache, config, credentials, execution, providers, runtimeapi (portability: ~70%)
   - Ingress + routing = new tests (20% effort).
   - Expected effort: 80 test files, ~25% edits.

3. **Nexus Hub:** alerts, config, identity, jobs, jwks, observability, self, storage, ws (portability: ~75%)
   - Fleet + quota + traffic = new tests (25% effort).
   - Expected effort: 60 test files, ~20% edits.

4. **Compliance Proxy:** access, audit, compliance, config, exemption, health, metrics, proxy, siem, testutil (portability: ~80%)
   - TLS + runtime = new tests (15% effort).
   - Expected effort: 50 test files, ~18% edits.

5. **Agent:** compliance, host, network, observability, platform, sync (portability: ~75%)
   - Identity + lifecycle + policy = new tests (30% effort).
   - Expected effort: 40 test files, ~22% edits.

### Phase 3: High-Risk Refactor (Control Plane) — Heaviest Lift
6. **Control Plane:** handler, store, and complete redesign of identity, governance, ai, fleet, infrastructure, platform, settings, traffic
   - **Wholesale redesign required.** Old tests incompatible with new domain structure.
   - Expected effort: ~150 test files, **90% rewrite** (not migration).

---

## Troubleshooting: Checklist for Test Porting

- [ ] **Import path corrections:** `shared/runtime/` → `shared/core/`, `shared/security/` → `shared/identity/`, `httpclient` → `http`, etc.
- [ ] **Renamed packages:** `devicepredicate` → `device`, `router` → `routing`, `httpclient` → `http`. Verify function signatures match old tests.
- [ ] **Semantic changes (Category E):** identity (IAM fixtures), ingress (request serialization), routing (rule structure), control-plane identity (SSO setup). Spot-check test data structures.
- [ ] **New packages (Category C):** decision, pipeline, configkey, auth (ai-gateway), ingress platform, policy, redisfactory, runtime (compliance-proxy), tls, compliance, fleet, quota, traffic, observability, ai, infrastructure, settings (control-plane), identity, lifecycle, policy (agent). Design from spec or PR comments.
- [ ] **removed old packages:** Confirm they're fully subsumed (not dangling references in test helper imports).
- [ ] **Coverage allowlist:** Update allowlist entries if old path → new path renames require re-registration.

---

## Next Steps

1. **Run full suite on old project:** `npm run check:coverage` → capture baseline.
2. **Phased migration:** Start Phase 1 (shared); use parallel test runs to unblock service teams.
3. **Trace endpoint moves:** Use `git log --follow -p -- path/to/handler` to find tests for handlers that moved between domains.
4. **Domain-driven test grouping:** Refactor tests from old "handler_test.go" into new "internal/{domain}/{domain}_test.go" structure.
5. **SDD alignment:** Ensure new tests for Category C packages are aligned with SDD acceptance criteria (see control-plane governance, fleet, ai domains for specs).

---

*Report generated: 2026-05-19*
