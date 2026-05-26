# Doc Audit Decision Log — 2026-05-21

## Program metadata

- **Goal**: Deep-audit every doc under `docs/` (excluding `_archive/`, `developers/specs/`, and `users/api/openapi/`) against the live codebase, fix drift so docs become the single source of truth, brainstorm best-architecture/best-product resolutions for ambiguous cases, and conservatively clean up dead code surfaced during the audit.
- **Worktree**: `./worktrees/doc-audit` on branch `feature/doc-audit-2026-05-21` (from `develop` @ 05f147735).
- **Coordinator**: Opus 4.7 (1M context).
- **Executors**: Sonnet subagents (audit + fix) dispatched per cluster.
- **Conservative-cleanup rule**: T4 (dead code) defaults to `keep-with-TODO`; delete only after multi-surface grep + runtime-registry check + Opus diff review (user-confirmed 2026-05-21).
- **Date opened**: 2026-05-21.
- **Date closed**: _pending_.

## Scope

**In scope (~120 markdown files)**:

- `docs/README.md`
- `docs/developers/README.md`, `docs/developers/roadmap.md`
- `docs/developers/architecture/**` (overview + project-structure + cross-cutting/{foundation,observability,safety,shared,storage,ui} + services/{agent,ai-gateway,compliance-proxy,control-plane,hub}) — 62 files
- `docs/developers/workflow/**` — 8 files
- `docs/operators/**` (ops + runbooks) — 17 files
- `docs/users/README.md`
- `docs/users/features/{cp-ui,agent-ui,flows}/**` — 27 files
- `docs/users/product/**` — 5 files
- `docs/users/api/ai-gateway-client-guide.md`

**Out of scope (excluded per user 2026-05-21)**:

- `docs/_archive/**` — historical
- `docs/developers/specs/**` — SDD, not open-sourced
- `docs/users/api/openapi/**` — SDD-scope (~50 yamls), not open-sourced

## Clusters

| Cluster | Owner wave | Docs | Code surface | Key bindings (CLAUDE.md / memory) |
|---|---|---|---|---|
| A. AI Gateway + Adapters | W1 | `services/ai-gateway/*` (11) + `users/api/ai-gateway-client-guide.md` + `users/features/cp-ui/ai-gateway.md` + flows/{dry-run, v1-estimate-compare, traffic-event-lifecycle} | `packages/ai-gateway/**`, `packages/shared/canonical*`, `packages/shared/traffic/**`, `packages/shared/transport/normalize/**` | adapter-conformance-check §3a; cost lockstep; cache-mandatory-all-ingress |
| B1. Agent core | W1 | services/agent: enrollment, forwarder, paths-abstraction, keystore, browser-opener, autoupdater, backpressure-rollup, attestation, telemetry, tray-ipc | `packages/agent/**` except `platform/**` | agent_is_pure_forwarder; agent_traffic_upload_level; agent_audit_empty_string_stripping; agent_platform_paths_abstraction |
| B2. Agent platform | W2 | services/agent: macos, linux, windows, ne-fail-open, build-signing, internals-sibling-pairs, policy-eval, protection-pause, exemption-grants, sso-enrollment | `packages/agent/platform/**` | NE fail-open (5 rules); macOS Agent builds via build-agent skill |
| C. Compliance proxy + Hub | W1 | services/compliance-proxy/*(4) + services/hub/*(1) + ops/runbooks/{compliance-proxy-smoke,cursor-traffic-capture-debug} | `packages/compliance-proxy/**`, `packages/nexus-hub/**` | compliance-proxy text-first; metrics_sample WS |
| D. Control Plane + IAM + IdP | W2 | services/control-plane/*(7) + features/cp-ui/iam.md + flows/{idp-federation,vk-lifecycle} | `packages/control-plane/**`, `packages/shared/identity/**` | sp_idp_positioning; iam_resource_nrn; vk_org_dual_join_chain |
| E. Cross-cutting foundation | W1 | foundation/* (10) | `packages/shared/transport/**`, `packages/shared/core/**`, `packages/shared/schemas/configkey/**` | thing_config_pull_model; configuration-architecture R1-R5; secrets-env-only |
| F. Cross-cutting observability | W2 | observability/* (10) | `packages/shared/traffic/**`, `packages/shared/audit/**`, services' `obs/` | server_slog_sink_di_bypass; tier2_nonjson_detector_framework |
| G. Cross-cutting safety+storage+shared | W1 | safety/* (6) + storage/* (4) + shared/* (4) | `packages/shared/{policy,storage,traffic}/**` | emergency-passthrough; credentials-architecture; shared-package vetted set |
| H. Cross-cutting UI | W1 | ui/* (7) | `packages/control-plane-ui/**`, `packages/ui-shared/**` | i18n mandatory; design-token strict; default_theme_monochrome |
| I. Workflow + Operators | W2 | workflow/* (8) + operators/ops/* (10) + runbooks (7) | `scripts/**`, CI configs, `tools/db-migrate/**` | scoped-retest-only; tests-only-own-data; migration_timestamp_unique |
| J. Product + Features overview + READMEs | W2 | product/* (5) + features/{cp-ui,agent-ui}/overview.md + 4 READMEs | global + `docs/developers/roadmap.md` cross-check | claudemd_no_archaeology |

## Drift inventory (per-cluster counts)

| Cluster | Docs audited | Drift items | Heaviest pattern |
|---|---|---|---|
| A. AI Gateway | 16 | 110 | Path-rename (R6 decomp: `internal/<flat>/` → `internal/<bucket>/<sub>/`) + hook enum/onMatch shape drift |
| B1. Agent core | 10 | 51 | Aspirational docs (telemetry, autoupdater channels, backpressure 3-layer, keystore SSO) + paths struct drift |
| B2. Agent platform | 10 | 50 | Bundle id `com.nexus.agent` → `com.nexus-gateway.agent` + SSO flow paths + stale audit memory |
| C. Compliance+Hub | 7 | 42 | Forward-header doc fully fictional + runtime API routes deleted + Hub paths nested |
| D. CP+IAM+IdP | 10 | 53 | IAM action format `admin:ReadProvider` → `admin:provider.read` + NRN grammar + R6 handler decomp |
| E. Foundation | 11 | 43 | `thing_shadow*` tables don't exist + mq subpkg names virtual + nexus-response-markers 70% stale |
| F. Observability | 12 | 61 | `nexus.*` span attrs never written + `audit.Emit` API doesn't exist + runtime endpoints overstated |
| G. Safety+Storage+Shared | 14 | 95 | `shared/` 35-flat-subpkg doc vs 8-bucket reality + redact.go files non-existent + Adapter vs Route tier |
| H. UI | 7 | 30 | useApi hook claims auth-injection it doesn't do + QueryClient defaults inverted + recharts hooks renamed |
| I. Workflow+Operators | 23 | 35 | redis-setup describes banned pub/sub + migration-timestamp guard silent no-op + 4 candidate dead scripts |
| J. Product+READMEs | 12 | ~22 | Roadmap E61 status stale + Linux platform omitted + IoT terminology leaked into product/* |
| **Total** | **132** | **~590** | |

Raw per-line drift lives in `.audit-scratch/<cluster>.md` (consumed by Phase 5 fix subagents; deleted at Phase 8).

## Triage table (drift-group level)

Each row groups drift items by pattern. T-tag is Opus's final call. Per-line items inherit the group's T-tag unless explicitly overridden in the scratchpad.

### Group 1: Internal path-rename drift (~150 items, mostly stale-name kind)

Pattern: doc cites `internal/<flat>/<foo>` but R6/R8 decomp moved code to `internal/<bucket>/<foo>/<subpkg>/`. Code is the source of truth.

| Cluster | Examples | Triage |
|---|---|---|
| A | `internal/router/` → `internal/routing/`; `internal/executor/` → `internal/execution/executor/`; `internal/pipeline/aiguard/` → `internal/policy/aiguard/`; `internal/cachelayer/` → `internal/cache/layer/`; `internal/promptcache/` → `internal/cache/`; flat `streaming/`/`metrics/`/`audit/`/`store/` → `internal/platform/{streaming,metrics,audit,store}/`; `internal/handler/` → `internal/ingress/proxy/` | **T1** |
| B2 | `policy/` → `policy/core/`; `bridge/` → `network/bridge/`; `intercept/` → `network/intercept/`; `lifecycle/{bootstrap,killswitch,protectionpause}` nesting; non-existent `internal/hubhttp/` | **T1** |
| C | `internal/handler/` → `internal/proxy/`; `internal/conn/` → `internal/proxy/conn/`; `internal/cert/` → `internal/tls/`; `internal/configcache/`+`configloader/` → `internal/config/{cache,loaders,shadow}/`; `internal/runtimeapi/` → `internal/runtime/{auth,breakglass,...}`; Hub `selfreg/`+`selfshadow/`+`consumer/` → `internal/self/{reg,shadow}/` + `jobs/consumer/` | **T1** |
| D | `internal/iam/` → `internal/identity/iam/`; `internal/auth/`+`authserver/`+`jwtverifier/` → `internal/identity/{authserver,jwt}/`; `internal/configreconcile/` → `internal/platform/configreconcile/`; flat `handler/` → per-domain subpackages | **T1** |
| F | `eval/builtin/` → `engine/rules/`; `handler/agent_audit.go` → `traffic/ingest/audit/agent_audit.go`; `observability/audit/` → `traffic/ingest/audit/`; `diag/mode/`+`diag/triage/` don't exist | **T1** |
| G | `internal/crypto/` → `internal/platform/crypto/`; `pipeline/quota/` → `policy/quota/`; `pipeline/vkauth/` → `auth/vkauth/`; `internal/router/` → `internal/routing/`; `internal/handler/passthrough.go` → `internal/governance/passthrough/handler/`; spillstore `presign` Hub handler path | **T1** |

### Group 2: Renamed/removed symbols (~60 items)

| # | Drift | Triage |
|---|---|---|
| 2.1 | Verdict enum `RejectSoft` → `BlockSoft` (cluster A) | **T1** doc-fix |
| 2.2 | Hook interface arg `InterceptedTransaction` → `HookInput` (cluster A) | **T1** doc-fix |
| 2.3 | Hook stage values: doc `request/response/streaming` → code `request/response/connection`; no streaming stage (cluster A) | **T1** doc-fix |
| 2.4 | onMatch shape: doc `{action, redactStrategy, redactFields}` → code `{storageAction, inflightAction, replacement?}` (cluster A) | **T1** doc-fix |
| 2.5 | applicableIngress case: doc `[aiGateway, complianceProxy, agent]` → code `ALL/AI_GATEWAY/COMPLIANCE_PROXY/AGENT` (cluster A) | **T1** doc-fix |
| 2.6 | IAM action format: doc `admin:ReadProvider` PascalCase → code `admin:provider.read` kebab-dot (cluster D, many) | **T1** doc-fix per [[T3-DEC-003]] |
| 2.7 | NRN grammar: doc 4-seg → code 5-seg with slash (cluster D, E, J) | **T1** doc-fix per [[T3-DEC-004]] |
| 2.8 | Recharts hooks: `useChartColors()` → `useChartSeriesColors/PieColors/SemanticColor/PhaseColor` (cluster H) | **T1** doc-fix |
| 2.9 | useApi return: `isLoading` → `loading`; options sig change (cluster H) | **T1** doc-fix |
| 2.10 | Paths struct: `DefaultPaths` → `Paths`; 7 fields renamed (cluster B1) | **T1** doc-fix |
| 2.11 | Keystore interface `Keystore` → `Store`; `Put/Get/Delete/List` → `Get/Set/Delete` (cluster B1) | **T1** doc-fix |
| 2.12 | trayipc methods: doc 10 methods → code 4 methods (cluster B1) | **T1** doc-fix |
| 2.13 | JWT verifier: doc multi-issuer interface → code single-issuer struct (cluster D) | **T2** doc-fix per [[T3-DEC-005]] |
| 2.14 | OAuth endpoints: doc `/api/admin/oauth/*` → code `/oauth/*` (cluster D) | **T1** doc-fix per [[T3-DEC-006]] |
| 2.15 | Bundle id: doc `com.nexus.agent` → code `com.nexus-gateway.agent` (cluster B2, many) | **T1** doc-fix |
| 2.16 | SSO redirect URI: doc `localhost:17000-17999/agent-callback` → code `127.0.0.1:0/callback` (cluster B1, B2) | **T1** doc-fix |
| 2.17 | Enrollment endpoints: doc `/api/hub/enrollment/{redeem,renew}` → code `/api/internal/things/enroll` (cluster B1) | **T1** doc-fix |
| 2.18 | Tenancy: doc `ancestor_path` array → code `path` string column (cluster D) | **T2** doc-fix |
| 2.19 | AlertRule tables: doc `alert_inbox` → code `Alert`; `agent_audit` table doesn't exist (cluster F) | **T1** doc-fix |
| 2.20 | `auditEventToMap` function name doesn't exist (cluster B1, mentioned in comments only) | **T1** doc-fix |
| 2.21 | NATS subjects: `nexus.traffic`/`nexus.audit`/`nexus.alerts`/`nexus.ops_metrics` → `nexus.event.{ai-traffic,compliance,agent,admin-audit,alert,diag}` (cluster C) | **T1** doc-fix |
| 2.22 | Prometheus prefix `nexus_compliance_proxy_*` dropped pre-GA → dotted names (cluster C, runbook) | **T1** doc-fix |
| 2.23 | Anthropic model code: doc `claude-3-7-sonnet` → seed `claude-opus-4-7`/`claude-sonnet-4-6`/... (cluster A, 7+ sites) | **T1** doc-fix |

### Group 3: Aspirational doc content (no code) (~30 items)

Pattern: doc describes features that never landed in code. Per CLAUDE.md "Real implementation only" + "no aspirational" + user "代码与文档一致" — default action is **trim doc to match code** (T1), not build the feature (T3 build-up). T3-DEC numbers track the architectural calls; see Brainstorm section.

| # | Drift | Triage | Decision |
|---|---|---|---|
| 3.1 | `agent-telemetry-architecture.md` 100% fictional (operational telemetry + crash reports + anonymous mode); code is plain OTEL wrapper | **T2** | [[T3-DEC-001]] — rewrite doc to describe what exists (OTEL provider only); flag PII-redaction / crash-report as roadmap |
| 3.2 | `agent-autoupdater-architecture.md` channel system (stable/beta/canary) + manifest URL + go:embed key — none exist | **T2** | [[T3-DEC-001]] — rewrite to describe Hub `/api/internal/things/update-check` + caller-supplied pubkey |
| 3.3 | `agent-backpressure-rollup-architecture.md` 3-layer pipeline (ringbuffer→SQLCipher→Hub) + capacity caps — only `atomic.Bool` throttle exists | **T2** | [[T3-DEC-001]] — rewrite to describe atomic.Bool + watermark hysteresis |
| 3.4 | `agent-keystore-architecture.md` SSO bearer + refresh cache via `user.sso.bearer`/`user.sso.refresh` keys + libsecret on Linux — none exist | **T2** | [[T3-DEC-001]] — rewrite: only `nexus-agent-audit-db-key`; Linux uses FileStore not libsecret |
| 3.5 | `agent-tray-ipc-architecture.md` JSON-RPC envelope + 10 methods + 5 server events — code is text-cmd + 4 methods + no event stream | **T2** | [[T3-DEC-001]] — rewrite to actual text-cmd protocol |
| 3.6 | `prompt-cache-architecture.md` 3-tier model (Tier1 in-process / Tier2 Redis / Tier3 provider) + `prompt_cache_tier_hit`/`prompt_cache_key_hash` columns + `prompt_cache_policy` Prisma model — none exist | **T2** | [[T3-DEC-001]] — rewrite to actual `cache/{layer,stream,gemini,core,semantic,freshness,budget}` + CacheGlobal/Adapter/Provider Prisma models |
| 3.7 | `audit-pipeline-architecture.md` `audit.Emit` API + `Event{request_id,org_id,...}` struct + `agent_audit` table — none exist | **T2** | [[T3-DEC-001]] — rewrite to `Writer` interface + `AuditEvent` struct (traffic-shape) + AdminAuditLog/TrafficEvent tables |
| 3.8 | `admin-audit-log-coverage.md` canonical fields zero overlap with real AdminAuditLog columns | **T2** | [[T3-DEC-001]] — rewrite §3 fields to match actual `actorId/actorLabel/actorRole/sourceIp/action/entityType/entityId/...` |
| 3.9 | `otel-span-attributes-architecture.md` 17 `nexus.*` mandatory attrs + per-span-type table — zero SetAttributes calls in code | **T3** | [[T3-DEC-002]] — keep doc as binding-target OR delete? See decision |
| 3.10 | `runtime-introspection-architecture.md` 10 endpoints (pprof, hooks, routing, credentials/health) — only 4 exist | **T2** | [[T3-DEC-001]] — rewrite to actual surface |
| 3.11 | `diag-event-triage-architecture.md` `diag/mode/` + `diag/triage/` subdirs + triage buckets + bucket-keyed silence — none exist | **T2** | [[T3-DEC-001]] — rewrite to actual `slog_sink.go` + `multi_handler.go` + DiagSilence (messageHash-keyed) |
| 3.12 | `siem-bridge-architecture.md` per-vendor channel types + `packages/shared/siem/` interface lib + `siem_channel` table — code has single generic HTTPSink, no shared/siem/, no siem_channel | **T2** | [[T3-DEC-001]] — rewrite |
| 3.13 | `forward-header-allowlist-architecture.md` per-route/per-VK DB-driven allowlist + admin CRUD + `forward_header_allowlist` table — code is yaml-only | **T2** | [[T3-DEC-001]] — rewrite (also move from compliance-proxy/ → ai-gateway/ folder per [[T3-DEC-007]]) |
| 3.14 | `compliance-proxy-details-architecture.md` runtime API surface (exemptions, killswitch, cert-cache/*, cpu-profile, goroutine-dump) all deleted per slimming refactor | **T2** | [[T3-DEC-001]] — rewrite to actual `/runtime/config{,/{key}}, /runtime/sync-status, /runtime/health` |
| 3.15 | `domain-device-predicate-architecture.md` Matcher interface + NewGlobMatcher/OSPredicate/IPRangePredicate constructors — none exist; real API is Engine.Swap/MatchHost + JSON wire-shape | **T2** | [[T3-DEC-001]] — rewrite |
| 3.16 | `test-harness-architecture.md` `tests/lib/prom.sh`, `integration-go/fake-providers/`, `testharness/harness.go` with HarnessNew*/Seed*/Cleanup — none exist | **T2** | [[T3-DEC-001]] — rewrite |
| 3.17 | `agent-sso-enrollment-architecture.md` two-layer device+user identity model with user session + token refresh — code is one-shot enrollment | **T2** | [[T3-DEC-001]] — rewrite |
| 3.18 | `idp-sso-architecture.md` + `idp-federation.md` SAML flow described; no SAML handler in code (type enums only) | **T3** | [[T3-DEC-008]] |

### Group 4: Wrong-flow / wrong-shape (~50 items)

| # | Drift | Triage | Notes |
|---|---|---|---|
| 4.1 | `trace-id-propagation` doc UUID v7 + ignore inbound → code UUID v4 + reuse inbound | **T1** doc-fix |
| 4.2 | Emergency-passthrough tier names `Global/Provider/Route` → schema `Global/Adapter/Provider` + 3 flags (bypassHooks/Cache/Normalize), not single `enabled` | **T2** doc-fix |
| 4.3 | Kill-switch `kill_switch_activation` history table doesn't exist; history via admin_audit | **T1** doc-fix |
| 4.4 | Credentials encrypted_blob → 3 columns (encryptedKey/iv/tag) + rotationState + circuitState + multi-window health all missing from doc | **T2** doc-fix |
| 4.5 | PII redaction §11 4 `redact.go` files don't exist; always-on layer ungrouped in code | **T1** doc-fix; flag for follow-up |
| 4.6 | thing_shadow / thing_shadow_reported tables don't exist; shadow lives in JSONB columns on `thing` (cluster E) | **T2** doc-fix |
| 4.7 | nexus-response-markers 32+ markers → ~22 actual ExposeHeaders; 70% stale per own E59-S2 notice | **T2** doc-fix per [[T3-DEC-010]] |
| 4.8 | mq-architecture `shared/mq/natsmq` + `shared/mq/memmq` + `shared/dedup` packages don't exist | **T1** doc-fix |
| 4.9 | configuration-architecture §7/§8 missing live keys (ResponseCacheTimeSensitivePatterns, SemanticCacheConfig, ResponseCacheExtractConfig, InstalledRulePacks, UserContext, SIEM); contains removed keys (AccessControl, ComplianceStreaming) | **T2** doc-fix per [[T3-DEC-012]] |
| 4.10 | shared-package-architecture 35-flat vs 8-bucket nested layout (~30 sub-drifts) | **T2** doc-fix per [[T3-DEC-011]] |
| 4.11 | Per-route cache config deleted; doc still says "configurable per route" (features.md, cp-ui/ai-gateway.md) | **T1** doc-fix |
| 4.12 | Linux platform present but `(macOS, Windows)` in product/overview, features, architecture | **T1** doc-fix |
| 4.13 | E61 status "pending merge" → merged in 05f147735 PR #33 (cluster J, 4 sites) | **T1** doc-fix |
| 4.14 | IoT terminology (Thing/Shadow/drift) leaked into product/overview + product/architecture | **T1** doc-fix |
| 4.15 | Redis pub/sub channels in redis-setup.md + deployment.md (BANNED per CLAUDE.md) | **T1** doc-fix |
| 4.16 | VK lifecycle owner: doc Hub-creates → code CP-owned | **T1** doc-fix per [[T3-DEC-007]] |
| 4.17 | Tenancy membership model: doc `(user, org, role)` → code single `organizationId` per user | **T1** doc-fix |
| 4.18 | Agent telemetry "operational telemetry" / agent-policy-eval / agent-exemption-grants — "binding gap" claims STALE since 2026-05-18/19 wiring (exemptions + rule packs consumed) | **T1** doc-fix + memory update [[T3-DEC-016]] |
| 4.19 | Agent attestation §4 still describes CONNECT-line peek topology; §0.2 already corrected to inner-request injection. §3.5 "trust-without-hash" superseded | **T2** doc-fix (rewrite §3.5 + §4) |
| 4.20 | Agent forwarder phase columns `phase_*_at` (timestamps) → real `*_ms` (durations) | **T1** doc-fix |

### Group 5: Misc small (~70 items)

Path tweaks, file-rename source citations, default-value mismatches (JWT 60s→5m, JWKS 5m→15m, etc.), missing-doc additions like 8 MiB body cap, `useChartPhaseColor`. All **T1** unless flagged otherwise in scratchpad.

### Group 6: Code-fix tasks (T2)

| # | Item | Code path |
|---|---|---|
| 6.1 | `scripts/check-migration-timestamps.sh:10` targets `tools/db-migrate/prisma/migrations/` (doesn't exist) → real `tools/db-migrate/migrations/` → CI gate silent no-op | `scripts/check-migration-timestamps.sh:10` |

(Note: no other T2 code-fix tasks. Cluster F's "alert builtin drift" is documented + acknowledged but the actual 3 missing Go rules are a separate alerting program, not this audit's scope.)

### Group 7: Code cleanup register (T4) — conservative

| # | Candidate | Sources | Verdict |
|---|---|---|---|
| 7.1 | `scripts/smoke-e33-dual-pipeline.sh` | cluster I | Phase 6 grep — likely keep-with-TODO |
| 7.2 | `scripts/e2e-smoke-test.sh` | cluster I | Phase 6 grep — likely keep-with-TODO |
| 7.3 | `scripts/test-all.sh` | cluster I | Phase 6 grep — shadowed by Makefile `test-all`; check divergence |
| 7.4 | `scripts/count-packages-loc.sh` + `count-packages-code-loc.py` | cluster I | Phase 6 grep — likely keep-with-TODO |
| 7.5 | SAML type enum entries (IdP types include `saml` but no handler) | cluster D | [[T3-DEC-008]] — defer to roadmap; do NOT remove type enum (schema risk) |

### Group 8: T5 defer

| # | Item | Routed to |
|---|---|---|
| 8.1 | `configuration-architecture-migration.md` archive eligibility | Update PR statuses now (T2); revisit archive in next program |
| 8.2 | Memory `project_agent_compliance_audit_2026_05_14` is stale (exemptions + rule packs consumed) | Phase 8 memory update [[T3-DEC-016]] |
| 8.3 | Alerting builtin drift (Go=27 / prod=30) — known | Separate alerting drift program (already in [[project_alerting_builtin_drift_2026_05_15]]) |
| 8.4 | SlogSink DI re-assign binding not in observability docs | Add to follow-ups (next cycle) |
| 8.5 | E62 embedding endpoints `/v1/embed`, `/v2/embed`, `:embedContent`, `:batchEmbedContents` aren't ingress routes (only upstream URLs); ai-gateway-client-guide.md misrepresents them | T2 doc-fix in cluster A; flag for product clarity |
| 8.6 | Roadmap cleanup beyond E61 status | T1 in cluster J |
| 8.7 | Add Linux to product/overview, features, architecture | T1 in cluster J |
| 8.8 | timezone.md historic migration reference to collapsed baseline | T3 cluster I |

## Brainstorm decisions (T3)

### T3-DEC-001 — Aspirational architecture docs: trim or build?

**Problem**: ~18 docs describe features that don't exist in code (telemetry, autoupdater channels, backpressure 3-layer, prompt-cache 3-tier, audit.Emit, otel nexus.* attrs, runtime introspection 10 endpoints, SSO refresh, SAML flow, forward-header DB-driven, compliance-proxy runtime API, etc.). Most are 2024-2025 design artifacts that were superseded or never built.

**Candidates**:
- **A**: Build all missing features (huge multi-quarter program; explodes scope of this audit).
- **B**: Delete each affected section from each doc; docs become smaller, code becomes truth.
- **C**: Rewrite each doc to describe what exists + a short "Roadmap" stub for unimplemented; if not on roadmap, drop entirely.

**Judgment**: Per CLAUDE.md "Real implementation only" + "Development-phase policy: no backward compatibility, no defer" + user binding "代码与文档一致" + less-is-more (Half 2: delete instead of add). The user said docs are future single source of truth — so docs MUST shrink to truth, not require building features. Option C with bias toward dropping (no roadmap stubs unless [[docs/developers/roadmap.md]] already lists the item).

**Decision**: **C-minus** — each affected doc gets rewritten to describe **what code does today**. No "Roadmap" stubs unless the item is already in `docs/developers/roadmap.md`. Aspirational sections deleted outright. Memory anchors for known incidents kept.

**Scope impact**: 18 doc rewrites, all T1 routed (≤5 docs per subagent batch). Subagent prompts say "describe what code does, do not invent or restore deleted content".

**Routed to**: T1 pool.

### T3-DEC-002 — otel `nexus.*` span attribute binding: keep or delete?

**Problem**: `otel-span-attributes-architecture.md` declares 17 mandatory `nexus.*` span attributes with per-span-type requirements; zero SetAttributes calls in code use any `nexus.*` key. So either the binding is real-but-unenforced (and code needs to start adding attributes) or the binding is aspirational and should be deleted.

**Candidates**:
- **A**: Keep binding, build helpers + enforcement, retrofit every span site.
- **B**: Delete binding, document existing OTEL-vanilla span behavior.
- **C**: Keep binding, mark it "binding goal not yet enforced", add explicit follow-up.

**Judgment**: Span attributes have real observability value (cardinality control + cross-service correlation). But the binding has zero coverage today, so it's not load-bearing. Building helpers is a separate program. Less-is-more says delete unless there's a near-term commit. No memory anchor for this binding.

**Decision**: **B** — delete the binding. Rewrite doc to describe what code currently emits (vanilla otel http span attrs from `httptrace.go`). Add a one-line "Future work" pointer if/when someone decides to add nexus.* attrs.

**Routed to**: T1 (folded into T3-DEC-001 batch).

### T3-DEC-003 — IAM action format canonical: code or doc?

Code uses `admin:<resource>.<verb>` kebab-dot (e.g., `admin:provider.read`); doc family universally uses `admin:<Verb><Resource>` PascalCase. Code is load-bearing (`packages/shared/identity/iam/catalog.go` + seed-baseline.sql + actual middleware matching). Memory [[project_iam_resource_nrn_bug]] is exactly this class of failure.

**Decision**: Code wins. T1 doc-fix sweep across all cluster D docs + cp-ui/iam.md + flows/idp-federation.md. Verb taxonomy paragraph rewritten. All examples updated.

### T3-DEC-004 — NRN grammar

Code uses 5-segment `nrn:nexus:<service>:<scope>:<resourceType>/<resourceID>` per `iam.BuildNRN`. Doc uses 4-segment without slash. Code is load-bearing.

**Decision**: Code wins. T1 doc-fix sweep wherever NRN appears.

### T3-DEC-005 — JWT verifier shape: rewrite

Code: single-issuer struct (`Verifier`), RS256-only, 5min skew default, 15min JWKS TTL, MQ-based revocation. Doc: multi-issuer interface, RS256/ES256, 60s skew, 5min JWKS TTL, Redis SISMEMBER.

**Decision**: Code wins. T2 doc-fix to rewrite §1-§5. Multi-issuer is a future architecture decision — not in scope of this audit. Add "Future: per-IdP Verifier orchestration" one-line stub if useful.

**Routed to**: T2 (jwt-verifier-architecture.md needs full §1-§5 rewrite).

### T3-DEC-006 — OAuth endpoint paths

Code: `/oauth/{authorize,token,introspect,revoke,device-binding}` + `/.well-known/{jwks.json,openid-configuration}`; refresh is grant_type on `/oauth/token` (RFC). Doc: `/api/admin/oauth/*` + separate `/refresh`.

**Decision**: Code wins (RFC-compliant). T1 doc-fix.

### T3-DEC-007 — VK lifecycle owner

Doc puts VK creation on Hub. Code: Control Plane owns VK creation; CP writes to Postgres + shadow-pushes via Hub. Hub stores `virtual_keys` in shadow but doesn't author it.

**Decision**: Code wins. T1 doc-fix to `flows/vk-lifecycle.md` step 1.

Also: `forward-header-allowlist-architecture.md` is in `services/compliance-proxy/` but describes AI Gateway. T2 — move file to `services/ai-gateway/` (Phase 5 subagent does the `git mv`).

### T3-DEC-008 — SAML

Doc: full SAML AuthnRequest + signed-assertion flow described in `idp-sso-architecture.md` + `idp-federation.md`. Code: IdP type enum includes `saml` and SAMLAdminConfig/SAMLClaimConfig structs exist but no SAML callback handler, no AuthnRequest emitter, no `internal/identity/authserver/login/saml.go`.

**Candidates**:
- A: Delete SAML claims from docs + delete SAML type enum (schema migration).
- B: Keep SAML claims in docs as roadmap; keep type enum as future binding.
- C: Trim doc SAML claims to "Planned: SAML"; keep type enum.

**Judgment**: SAML is a real enterprise feature, IdP type enum is forward-compatible. Schema migration to remove the enum value is risky for no near-term gain. But active doc claiming SAML works misleads users.

**Decision**: **C** — replace SAML flow §s in `idp-sso-architecture.md` + `idp-federation.md` with "Planned: SAML (type enum stub exists; no runtime handler shipped)". Type enum stays. Add a one-line entry to `docs/developers/roadmap.md` under "Queued — IdP/Auth" if not already there. **Cluster J subagent verifies roadmap; if absent, adds it.**

**Routed to**: T1 doc-fix + T5 cleanup register row 7.5 keep-with-comment.

### T3-DEC-009 — Tenancy `Membership` table

Doc: separate Membership table with `(user_id, org_id, role)`. Schema: single `organizationId` FK on NexusUser (one-to-many user-to-org). Doc invented multi-org membership.

**Decision**: Code wins. T1 doc-fix; rewrite §"Memberships" to describe single-org-per-user model. Cross-org assignment is "future architecture" if needed later.

### T3-DEC-010 — nexus-response-markers

Doc lists 32+ markers under various `x-nexus-aigw-*` prefixes that don't exist in `ExposeHeaders`. Doc's own E59-S2 notice admits per-header subsections weren't rewritten.

**Decision**: T2 doc-fix — rewrite §s for the ~22 real `ExposeHeaders` entries; delete sections for deleted headers (or move to a small "Removed in E59-S2" appendix). SoT pointer at the top updated.

### T3-DEC-011 — shared-package-architecture flat-vs-nested

Doc lists 35 flat subpackages at `packages/shared/<name>/`. Reality: 8 buckets (`audit/ core/ identity/ policy/ schemas/ storage/ traffic/ transport/`) with nested subpackages.

**Decision**: T2 — full rewrite of §2 catalogue to reflect 8-bucket structure. Cluster G subagent walks `find packages/shared -maxdepth 3 -type d` and rebuilds the table. Removed-symbol rows for `compliance/`, `domainpolicy/`, `siem/`, `opsmetrics/`, `httpclient/`, `slogsink/` are deleted (subpkgs that never existed at top level).

### T3-DEC-012 — configuration-architecture §7 catalog parity

Binding R3/R4 makes §7 + §8 the lockstep target with `packages/shared/schemas/configkey/`. Live registry has keys missing from doc (ResponseCacheTimeSensitivePatterns, SemanticCacheConfig, ResponseCacheExtractConfig, InstalledRulePacks, UserContext, SIEM). Doc still lists removed `AccessControl`, `ComplianceStreaming`.

**Decision**: T2 doc-fix — Cluster E subagent walks `configkey.go` + `validation.go` + `typed.go` and rewrites §7 (per-key catalog) + §8 (reference Go block) to perfect parity. **Then** updates the §6 "Renames executed in this migration" tables to mark which are LANDED (with seed-baseline.sql evidence) vs in-flight.

### T3-DEC-013 — configuration-architecture-migration.md status

Header says "Ready for execution 2026-05-20"; seed-baseline.sql evidence shows PR-0..PR-6 landed.

**Decision**: T2 doc-fix — sweep per-PR status (DONE/IN-FLIGHT/PENDING) against seed/reconcile/configdispatch evidence; tick acceptance checklist §11; fill (or strip) Decision log; keep doc as operator runbook for now. **Do NOT archive yet.** Re-evaluate archive eligibility next cycle.

### T3-DEC-014 — T4 dead-script grep (defer to Phase 6)

Decision rule (Opus to apply in Phase 6 per user's "代码清理偏保守"):

For each candidate, run `git grep -nE '<basename>' -- ':!docs/_archive' ':!worktrees' '.'`. If returns zero non-self references AND not in any Cron/CI/Makefile/skill — propose delete. Otherwise `keep-with-TODO` annotation.

### T3-DEC-015 — Not needed (already T1 routed).

### T3-DEC-016 — Memory update — agent compliance audit

`project_agent_compliance_audit_2026_05_14` says "exemptions received via shadow but NEVER consulted; rule packs not consumed at all" — both B1 + B2 audits confirm this is now STALE. `Engine.SetExemptionStore` is wired and `Engine.Evaluate` checks `exemptionStore.IsExempt(host)` before hook pipeline (engine.go:79-89). `AgentPipeline.ApplyRulePacksShadowState` + `injectRulePacks` consume `installed_rule_packs` Cat B key (pipeline.go:259, 279, 341).

**Decision**: In Phase 8, **update** the memory entry to reflect current code state. Leave incident date noted but tag "SUPERSEDED 2026-05-21 for exemption + rule pack arms; `auto_exempt_cert_pinned` knob still not consumed at runtime (store.go:223-224)".



## Code cleanup register (T4) — final

User-adjusted rule mid-program (2026-05-21): "明确死的就删，不确定的才保守 keep-with-TODO，删之前做充分检查". Conservative still applies for ambiguous cases.

| # | Candidate | grep evidence | Runtime registry | Decision |
|---|---|---|---|---|
| 7.1 | `scripts/smoke-e33-dual-pipeline.sh` | Zero non-self refs across `packages/`, `scripts/`, `docs/` (excl. _archive), `.github/`, `Makefile`, `.claude/skills/`, `package.json`. Last touched 2026-04-28 (commit 4132d027b). | Not in any Cron/CI/Makefile/skill | **DELETE** — E33 epic completed; superseded by `tests/scripts/smoke-gateway.py`. |
| 7.2 | `scripts/e2e-smoke-test.sh` | Zero non-self refs. Last touched 2026-04-17 (commit ed09883b0). Cookie-auth retired (CP is OAuth+PKCE now). | Not in any Cron/CI/Makefile/skill | **DELETE** — superseded by `tests/run-all.sh` + `tests/smoke/*.sh`. |
| 7.3 | `scripts/test-all.sh` | 1 reference in closed SDD `docs/developers/specs/e27/e27-s01-ai-guard-endpoint.md:31`. Shadowed by Makefile `test-all` target (the live entry point). | Makefile `test-all` is canonical | **DELETE** — divergence risk; consolidate to Makefile target. SDD spec reference is closed and frozen; harmless. |
| 7.4 | `scripts/count-packages-loc.sh` + `scripts/count-packages-code-loc.py` | Zero non-self refs. cloc-based one-off LOC measurement helper. | Not wired anywhere | **KEEP-WITH-TODO** — low-harm utility, no maintenance cost; revisit next cycle. |
| 7.5 | SAML type-enum entries in IdP-related code | IdP type enum (`local|oidc|saml`), `SAMLAdminConfig`, `SAMLClaimConfig` structs exist; no SAML callback handler, no AuthnRequest emitter, no `login/saml.go`. Schema migration to remove would be risky. | Code-level forward-compatibility stub | **KEEP** — doc-side already marked "Planned: SAML"; type enum stays as forward-compatibility per [[T3-DEC-008]]. |

**Delete batch executed**: `git rm scripts/{smoke-e33-dual-pipeline.sh,e2e-smoke-test.sh,test-all.sh}`. 3 scripts removed. `scripts/count-packages-loc.sh` and `scripts/count-packages-code-loc.py` retained with TODO header comment.

## Fix PR / commit list

Single program-scoped commit recommended (or per-cluster commits if user prefers smaller diffs). Scope summary:

- **120 docs modified** across `docs/developers/architecture/**`, `docs/developers/workflow/**`, `docs/operators/**`, `docs/users/{product,features,api}/**`, `docs/_archive/2026-q2/programs/` (decision log).
- **1 doc renamed**: `forward-header-allowlist-architecture.md` moved from `services/compliance-proxy/` to `services/ai-gateway/` (binding code lives at `packages/ai-gateway/internal/execution/forwardheader/`).
- **3 scripts deleted** (T4 cleanup register row 7.1, 7.2, 7.3).
- **2 scripts modified**: `scripts/check-migration-timestamps.sh` (T2 code-fix — guard now scans real migrations dir, no longer silent no-op); `scripts/count-packages-loc.sh` (TODO comment).
- **1 trigger-map row updated**: `docs/developers/architecture/README.md` line 75 — forward-header doc path corrected after move.

**Verification commands run (PASS)**:
- `npm run check:terminology` — Terminology check passed.
- `npm run check:arch-doc-triggers` — 82 architecture doc(s) referenced in trigger map.
- `bash scripts/check-migration-timestamps.sh` — all migration prefixes unique (no longer silent no-op).
- `npm run check:doc-lockstep` — no changed files vs main (PR-diff scan; not applicable on fresh branch).

**NOT run** (not required by binding rules):
- `tests/scripts/smoke-gateway.py` — no `packages/ai-gateway/**` code touched. Doc-only edits + the migration script (which is independent of AI gateway). Per CLAUDE.md "AI Gateway / traffic_event changes require ai-gateway smoke", smoke is only mandatory when code under that surface changes.
- `go test -race -count=1 ./...` — no Go code touched.

## Deferred items (T5)

| # | Item | Routed to |
|---|---|---|
| 8.1 | `configuration-architecture-migration.md` archive eligibility | Per-PR status table inserted in this cycle; archive eligibility to be re-evaluated next cycle. |
| 8.2 | Memory `project_agent_compliance_audit_2026_05_14` is stale (exemptions + rule packs ARE consumed) | Phase 8 → user memory update at session end. Code-anchor: `packages/agent/internal/policy/core/engine.go:79-89`, `packages/agent/internal/compliance/pipeline.go:259,279,341`. Only `auto_exempt_cert_pinned` knob remains unconsumed. |
| 8.3 | Alerting builtin drift (Go=27 / seed=30, 3 credential.* rules missing in Go) | Separate alerting program; already tracked in memory [[project_alerting_builtin_drift_2026_05_15]]. |
| 8.4 | SlogSink DI-reassign binding not in observability docs | Add to follow-ups (next doc cycle). Reason: low-impact, doesn't cause silent failures unless explicitly bypassed. |
| 8.5 | `admin:user.jit_provisioned` audit event-type doesn't exist in code | Either implement the event emission OR drop the doc claim entirely. Cluster D docs now state explicitly "event does not exist today". |
| 8.6 | E62 embedding ingress endpoints clarified in `ai-gateway-client-guide.md` | Resolved this cycle — 4 phantom ingress routes deleted; if E62 plans to expose `:embedContent` as an ingress later, doc must be updated then. |
| 8.7 | `endpoint-typology-architecture.md` heavy forward-looking content | Left as design-intent doc per audit rule (no shipped-but-broken claims found). Revisit when E62-E67 typology lands. |
| 8.8 | Multi-issuer JWT Verifier ("Future: per-IdP Verifier orchestration") | Add to roadmap if/when multi-IdP federation requires runtime issuer routing. |
| 8.9 | PII redaction always-on layer has no centralized `redact.go` library | Doc-side marked "Status: per-caller helpers; no centralized library. Future: extract." Code follow-up out of scope this cycle. |
| 8.10 | Test harness `testharness/harness.go` library described in `test-harness-architecture.md` doesn't exist | Doc now reflects current state ("Roadmap: shared test harness library"). Build if/when E?? lands. |

## Memory updates (queued for Phase 8 end-of-session)

- **Update** `project_agent_compliance_audit_2026_05_14`: append "SUPERSEDED 2026-05-21 for exemption + rule-pack consumption arms; `Engine.Evaluate` consults exemption store (engine.go:79-89), `AgentPipeline.ApplyRulePacksShadowState` consumes `installed_rule_packs` Cat B key (pipeline.go:259, 279, 341). Only `auto_exempt_cert_pinned` knob remains unconsumed (store.go:223-224)."
- **Add** `project_doc_audit_2026_05_21_done`: this program — 120 docs aligned to code, 3 scripts deleted, 1 trigger-map row fixed, 1 migration-script no-op bug fixed. Decision log at `docs/_archive/2026-q2/programs/doc-audit-2026-05-21-decision-log.md`.

## Closing

Program reaches "ready to commit" state. Cleanup of temp files (`.audit-register.md`, `.audit-scratch/`) deferred until just before commit. User to decide commit shape (single bundle vs per-cluster).

