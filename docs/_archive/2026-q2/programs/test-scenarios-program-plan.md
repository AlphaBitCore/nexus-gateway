# API Automation Tests — Business Scenario Program Plan

> **In-flight program plan.** Created 2026-05-16 immediately after the open-source readiness review closed (commit `b382b8680`). Memory anchor: `[[project_api_automation_test_program]]` in maintainer's local memory. This file is the durable handoff for the next session(s) executing the program.
>
> When the program completes, fold the survivor content into `tests/scenarios/README.md` (the test directory's user-facing entry point) and archive this plan.

---

## 1. Mission

Move from current ad-hoc per-skill smoke tests (`smoke-gateway`, `test-compliance-proxy`, `test-cursor-adapter`, `test-geminiweb-adapter`, `test-openai-responses`) to **systematic scenario-driven end-to-end tests** covering every API surface across all 5 services.

**Why:** pre-OSS launch confidence — scenario tests are how an operator's confidence in the system is built. Per-endpoint smoke is necessary but not sufficient; the real product value is the *coordinated path* through multiple services.

**Coverage target:**
- Every admin API endpoint hit by ≥1 scenario
- Every `/v1/*` ingress by ≥3 scenarios (happy + error + edge)
- Every Hub-internal endpoint by ≥1 scenario

**Layering:** existing `/test-all`, `/smoke-gateway`, `/test-compliance-proxy`, etc., stay as **smoke layer**. Scenarios are a **new layer above** that asserts business outcomes (HTTP shape + DB cross-check + metric delta + audit row presence).

---

## 2. Architecture model (load-bearing facts)

**5 services + 2 UIs + shared library**, all communicating via Hub-centric Thing model.

| Component | Port | Role | Tier-1 doc |
|---|---|---|---|
| **Nexus Hub** | 3060 | Thing Registry, Device Shadow, scheduler, alert evaluator, audit sink, agent CA, SIEM bridge | `thing-model.md`, `thing-config-sync-architecture.md`, `jobs-architecture.md` |
| **Control Plane** | 3001 | Admin API / BFF, IAM evaluator, OAuth+PKCE AS, SSO (SAML/OIDC), analytics queries | `iam-identity-architecture.md`, `idp-sso-architecture.md`, `oauth-pkce-admin-auth-architecture.md` |
| **AI Gateway** | 3050 | `/v1/*` traffic, 19 provider adapters, routing engine, hooks, quota, prompt cache (E38), Responses API (E56) | `routing-architecture.md`, `provider-adapter-architecture.md`, `prompt-cache-architecture.md` |
| **Compliance Proxy** | 3040 | Transparent TLS forward proxy, CONNECT, MITM bump, compliance pipeline, exemption manager | `compliance-pipeline-architecture.md`, `compliance-proxy-details-architecture.md` |
| **Agent** | local | Desktop traffic interceptor (macOS NE / Windows WinDivert / Linux pf), local SQLCipher audit, Hub thingclient | `agent-forwarder-architecture.md`, `agent-ne-fail-open-architecture.md` |
| **Control Plane UI** | 3000 | React + Vite + TS admin dashboard | `sidebar-ia-architecture.md` |
| **Agent UI** | local | Wails tray dashboard | `agent-internals-sibling-pairs-architecture.md` |
| **shared** | — | Cross-service Go: hooks, traffic, configtypes, mq, thingclient, cache, audit, telemetry, etc. (35 subpkgs) | `shared-package-architecture.md` |

**Stack**: PostgreSQL 16 (Prisma dev-time + pgx runtime), Redis 7 (cache-only, no pub/sub), NATS JetStream (event bus), Go 1.25 + go.work workspace.

**Config sync**: pull-only. Hub never pushes full state. Cat A inline / Cat B pull-on-signal / Cat C template-fallback shadow keys. Every Thing registers via `shared/thingclient` (WS primary, HTTP fallback).

**Key flows** (per `multi-endpoint-coordination-architecture.md`):
1. Admin creates VK → CP → Hub shadow → AI GW pulls
2. Admin creates routing rule → canonical payload (E47) → executor
3. Admin enables hook → fans out to all matching Things
4. Kill switch (E48) → Cat A inline → 60s reconcile auto-revert
5. Agent enrollment → token → CSR → device cert (mTLS)
6. Provider credential rotation → credstate dirty set → next request
7. Traffic event lifecycle → MQ → Postgres + spillstore
8. Alert evaluation → MQ aggregators → channel fan-out

---

## 3. API surfaces (the test targets)

### AI Gateway `:3050` — `/v1/*` (consumer-facing)

| Endpoint | Ingress format | Notes |
|---|---|---|
| `POST /v1/chat/completions` | OpenAI Chat | Most-hit. Stream + non-stream. |
| `POST /v1/messages` | Anthropic Messages | Same router, different shape. |
| `POST /v1/responses` | OpenAI Responses (E56) | Recent — stateless only (no `previous_response_id` cross-format). |
| `POST /v1/embeddings` | OpenAI Embeddings | Cross-format support. |
| `POST /v1/classify` | aiguard | Direct judge-model classification. |
| `GET /v1/models` | OpenAI Models | Per-VK filtered. |
| `GET /v1/models/{model}` | Detail | Same. |
| 19 provider adapters | `spec_anthropic / spec_openai / spec_azure_openai / spec_bedrock / spec_cohere / spec_deepseek / spec_fireworks / spec_gemini / spec_glm / spec_groq / spec_huggingface / spec_minimax / spec_mistral / spec_moonshot / spec_perplexity / spec_replicate / spec_together / spec_vertex / spec_xai` | Each owns canonical↔wire translation per §3a 7-rule contract |
| Routing strategies | `Single / Fallback / LoadBalance / Conditional / A/B Split / PolicyNarrowing` | + smart-routing (LLM dispatch) |
| Hooks at `request` + `response` stages | shared `hooks/` (keyword filter, regex, PII, rate-limit, aiguard, content-safety, …) | onMatch schema E46-S4 |
| E48 emergency passthrough | `ResolvedRequest.BypassHooks` | Cat A kill-switch propagation |
| Multi-tier cache | `cache/` (Redis response) + `cachelayer/` (in-mem config) + `streamcache/` + `geminicache/` (E38) | |

Plus admin probe endpoints (under `/api/admin/*` actually):
- `POST /api/admin/providers/test-connection`
- `POST /api/admin/hooks/test` (`hooks_test_endpoint`)
- `POST /api/admin/routing-rules/simulate` (`routing_simulate_endpoint`)
- `POST /api/admin/credentials/probe` (`credential_probe_endpoint`)
- `POST /api/admin/cache/preview` (`admin_cache_preview`)

### Control Plane `:3001` — `/api/admin/*`

**Auth**: OAuth+PKCE bearer; helper `tests/lib/auth.sh` `cp_login` / `cp_curl`.

Major resource families (per `control-plane-internals-architecture.md`, ~76 handler files):

| Resource | Endpoints (CRUD shape) |
|---|---|
| **Orgs** | `GET/POST/PATCH/DELETE /api/admin/orgs` + hierarchy + tenancy ancestor path |
| **Users + IAM** | `/api/admin/users`, `/api/admin/iam/{policies,groups,bindings,catalog,actions}` — full NRN-based ACL |
| **SSO / IdPs** | `/api/admin/idps`, federated identity, JIT provisioning |
| **Virtual Keys** | `/api/admin/virtual-keys` — list, create, rotate, expire, quotas |
| **Providers + Models** | `/api/admin/providers`, `/api/admin/models`, model catalog, pricing |
| **Credentials** | `/api/admin/credentials` — encrypted at rest, rotation, health rollup, circuit-breaker state |
| **Routing rules** | `/api/admin/routing-rules` — strategy tree + admin guard (E47-S8) |
| **Hooks** | `/api/admin/hooks`, `/api/admin/rule-packs` |
| **Cache config** | `/api/admin/cache/{providers,rules,preview}` (E38 3-tier blob) |
| **Quota** | `/api/admin/quota/{policies,overrides,usage}` |
| **AIGuard** | `/api/admin/aiguard/{config,backends,detectors}` |
| **Alerts** | `/api/admin/alerts/{rules,channels,inbox,history}`, mute/ack/resolve/snooze |
| **Kill switch** | `/api/admin/kill-switch`, `/api/admin/passthrough` (E48 3-tier) |
| **Things (devices)** | `/api/admin/things` (translates internal `thing` → product `node` via `hubadapter`), enrollment tokens, agent cert ops |
| **Audit log** | `/api/admin/audit`, body retrieval via spillstore presign |
| **Traffic events** | `/api/admin/traffic-events`, drilldown, normalized view |
| **Analytics** | `/api/admin/analytics/{cost,usage,latency,health}` — rollups (5m / 1h / 1d / 1mo) |
| **Jobs (Hub-managed)** | `/api/admin/jobs` (40+ jobs catalogued in `jobs-architecture.md` §5) |
| **Exemptions (compliance-proxy)** | `/api/admin/exemptions` — manual + auto-pinning |
| **Diag** | `/api/admin/diag/{mode,events}` |
| **Service URLs** | `GET /api/admin/services/public-urls` (used by install-instructions page) |
| **Forward headers allowlist** | `/api/admin/forward-headers` (E60-style) |
| **System metadata** | `/api/admin/system-metadata` (Cat A inline shadow keys) |

**OAuth+PKCE AS** (`authserver/`):
- `GET /.well-known/openid-configuration` — includes `revocation_endpoint` post-`3da95d756`
- `GET /oauth/authorize`, `POST /oauth/token`, `POST /oauth/introspect`, `POST /oauth/revoke`
- `GET /.well-known/jwks.json`
- Local login: `POST /login` (password) + SSO callback flow

### Nexus Hub `:3060` — `/api/internal/*` + `/ws`

| Endpoint | Purpose |
|---|---|
| `/ws` | Thing WebSocket (mTLS for agent; bearer for server) — change-signal + heartbeat |
| `POST /api/internal/things/enroll` | Agent enrollment (token → CSR → cert) |
| `GET/PUT /api/internal/shadow/{thingId}/{key}` | Per-key shadow CRUD |
| `POST /api/internal/audit/upload` | Audit batch ingest (with spillstore presign for bodies >256KiB) |
| `POST /api/internal/spillstore/presign` | S3 presigned URL issuer |
| `/api/public/agent-bootstrap` | Pre-enrollment discovery (Hub URL + CP URL for the agent) |
| `/healthz`, `/metrics`, `/ready` | Standard ops |
| `/runtime/*` | Localhost-only introspection (`runtimeintrospect`) |

### Compliance Proxy `:3040`

- HTTP CONNECT (transparent TLS forward proxy) — every consumer-surface HTTPS request to AI providers / chatgpt-web / claude-web / cursor / etc.
- Localhost `/runtime/{exemptions,killswitch,health}` (operator API)

### Agent — localhost only

- Tray IPC socket / named-pipe — `statusapi/server.go`: GET_STATUS, QUERY_EVENTS, QUERY_LIFECYCLE, GET_APPLIED_CONFIG, AUTHENTICATE, SHUTDOWN, …

---

## 4. Business scenario catalog (the test program units)

Organized by operator journey. Each scenario crosses multiple services + asserts on traffic_event / audit / metrics outcomes.

### Onboarding family
- **S-001**: Bootstrap a fresh tenant (org → admin user → first SSO login → first VK) and make a hello-world `/v1/chat/completions` request that returns 200 + lands a traffic_event row
- **S-002**: Add an OpenAI provider with a (test/fake) credential → `POST /api/admin/providers/test-connection` succeeds → routing rule resolves to it → request succeeds
- **S-003**: Same as S-002 for each of 19 providers (Anthropic, Azure, Bedrock, Gemini, DeepSeek, …) — verifies the §3a 7-rule adapter contract

### Routing family
- **S-010**: Single-strategy routing rule, model match → expected (provider, model) selected
- **S-011**: Fallback chain — primary 5xx, fallback succeeds, traffic_event records both attempts in `routing_trace`
- **S-012**: LoadBalance weighted random across 2 candidates over N requests → distribution within tolerance
- **S-013**: Conditional on `messages[0].content` → branches correctly via canonical payload (E47-S2)
- **S-014**: PolicyNarrowing eliminates the only candidate → 403 with `reason=policy_narrowing_empty`
- **S-015**: Smart routing — confidence ≥ threshold wins; otherwise deterministic tree wins
- **S-016**: Cross-format request (Anthropic ingress → OpenAI upstream) — `canonicalbridge` translates correctly

### Compliance family
- **S-020**: Enable `keyword-filter` hook with keyword "Japan" → request containing it returns 451 + audit `request_hook_decision` records block
- **S-021**: Enable `pii-scanner` with `onMatch=redact` → PII in response gets redacted before client sees it
- **S-022**: `rate-limiter` hook — N requests in window → (N+1)st returns 429
- **S-023**: aiguard `prompt-injection` detector — judge-model classification fires; backend unavailable falls open per config
- **S-024**: `applicableIngress: ['aiGateway']` filter — hook fires on AI GW but not on compliance-proxy
- **S-025**: Streaming compliance mode — `buffer_full_block` rejects mid-stream → client gets 451 not partial bytes; `chunked_async` posts hook result post-hoc

### Kill switch / passthrough (E48) family
- **S-030**: Global kill-switch activates → all requests carry `bypassHooks=true` + traffic_event records `passthrough_reason`
- **S-031**: Org-scoped kill — only that org bypasses
- **S-032**: Provider-scoped kill — only that provider bypasses
- **S-033**: Auto-revert at expiry (max 8h) — Hub `passthrough.expiry` job reverts shadow + signals Things
- **S-034**: Cold-start fail-CLOSED — agent boots without shadow → defaults to enforced (not passthrough)

### Quota / cost family
- **S-040**: VK with `policy=quota-threshold:100req/min`, cross 80% → warning alert fires; cross 100% → 429 returned
- **S-041**: Cost-based quota (E59) — accumulate USD until threshold → alert
- **S-042**: Quota override per-org wins over policy default
- **S-043**: Sliding window — quota recovers as old requests age out

### Credentials family
- **S-050**: Rotate provider credential → in-flight requests use old; next request uses new (`credstate.MarkDirty` propagation)
- **S-051**: Bad credential → 401 from upstream → circuit breaker opens after threshold → `credential.circuit_open` alert
- **S-052**: Health rollup classifies credential as `degraded/unavailable/collecting`; fallback chain picks healthy alternative
- **S-053**: Credential expiry advances `rotationState` → `credential.expiring` alert

### Cache family
- **S-060**: Identical request twice within TTL → second hit comes from L1 (Redis response cache) — verifiable via `x-nexus-cache: HIT` header + metric delta
- **S-061**: E38 prompt-cache — Anthropic `cache_control` marker rides through; usage stats record cached input tokens
- **S-062**: Gemini provider-side cached content — `geminicache` reuses cache_key across qualifying requests
- **S-063**: Cache-quality monitor job — synthetic elevated error rate → auto-reverts to dry-run

### Agent lifecycle family
- **S-070**: Issue enrollment token from CP UI → run agent enroll → device cert minted by Hub CA → `thing_agent` row appears with `status=online`
- **S-071**: SSO-based enrollment — OAuth+PKCE through browser → agent receives device cert
- **S-072**: Agent intercepts a local app's HTTPS request → traffic_event row from agent source with `phase_*` columns
- **S-073**: Agent offline >threshold → `agent.offline` alert + `stale-thing-sweep` marks status

### Compliance proxy family
- **S-080**: CONNECT from allowlisted IP → TLS bump → request transits + hook runs
- **S-081**: CONNECT from non-allowlisted IP → 403 + `audit.proxy.connect_denied`
- **S-082**: TLS-pinning client (e.g. older Cursor) triggers auto-exemption after 3 failures
- **S-083**: Cursor `StreamChat` (HTTP/2 ALPN bypass) — captured via prod NE transparent path
- **S-084**: chatgpt-web batchexecute → text-first normalizer extracts user prompt

### Alerts family
- **S-090**: Provider `circuit_open` alert → fan-out to webhook + SIEM + inbox; ack via UI; resolve
- **S-091**: Builtin↔seed lockstep test — Go `BuiltinRules` set ⊆ DB `AlertRule` rows
- **S-092**: Channel test — admin "Test channel" produces a synthetic alert delivery

### Audit family
- **S-100**: End-to-end traffic_event lifecycle (AI GW → MQ → Hub consumer → Postgres) — request_id stitches everything
- **S-101**: Audit body >256KiB overflows to spillstore S3; CP UI drill-down retrieves via presign
- **S-102**: Admin audit log captures every `/api/admin/*` write (per `admin-audit-log-coverage.md`)
- **S-103**: PII redaction in audit body (`pii-redaction-policy-architecture.md`)

### IAM family
- **S-110**: User in `NexusViewer` group can `read` but not `write` admin resources — POST returns 403
- **S-111**: Custom policy with org-scoped NRN — allows only listed orgs
- **S-112**: Role binding with TTL — expires automatically (E52-S15)

### OAuth / SSO family
- **S-120**: Discovery `/.well-known/openid-configuration` advertises every endpoint including `revocation_endpoint`
- **S-121**: Authorization code → token → introspection → revocation full cycle
- **S-122**: Refresh token rotation — old refresh becomes invalid after use
- **S-123**: External IdP federation (SAML + OIDC) → JIT user creation with mapped roles

---

## 5. Existing test infrastructure (build on, don't rewrite)

| Path | What's there | How to use |
|---|---|---|
| `tests/.env.test` | VKs, admin creds, service URLs (local + prod) | `set -a && source tests/.env.test && set +a` |
| `tests/lib/auth.sh` | `cp_login`, `cp_curl`, `cp_curl_code`, `cp_curl_full` | OAuth+PKCE login + token cache + curl wrapper |
| `tests/lib/env.sh` | Shared env helpers | sourced by other harnesses |
| `tests/integration-go/` | Go integration tests | Per-package — keep for unit-ish; **NOT the scenario layer** |
| `tests/e2e-ui/` | Playwright | UI flows; useful for UI-driven scenarios |
| `tests/scripts/smoke-gateway.py` | Smoke gateway runner | Per-model surface — model coverage layer |
| `tests/run-all.sh` | Orchestrator | `/test-all` invokes this |
| `.claude/skills/test-compliance-proxy/` | CP smoke | Each provides a runnable harness |
| `.claude/skills/test-openai-responses/` | E56 Responses smoke | "" |
| `.claude/skills/test-cursor-adapter/` (maintainer-private, gitignored) | NE protobuf path | "" |
| `.claude/skills/test-geminiweb-adapter/` (gitignored) | Gemini web smoke | "" |
| `make test-all` | Runs all `go test -race -count=1` + Vitest | Coverage-gated to ≥95% per `scripts/check-go-coverage.sh` |

**Go test conventions** (per CLAUDE.md "Go"):
- `go test -race -count=1`
- Table-driven where appropriate
- `_test` package (black-box) preferred
- pgx → real DB (or `pgxmock`); Redis → `miniredis`; OAuth → `idptest`

**TS test conventions**: Vitest + `@testing-library/react`.

**Auth pattern in scenarios** (canonical):
```bash
set -a && source tests/.env.test && set +a
source tests/lib/auth.sh
cp_login                                                # caches token at /tmp/nexus_test_token
cp_curl /api/admin/virtual-keys                         # auth headers auto-attached
cp_curl -X POST /api/admin/routing-rules -d @rule.json  # writes
```

---

## 6. Test data sources

| Source | Use |
|---|---|
| `tools/db-migrate/seed/data/seed-baseline.sql` | Baseline seed; user confirmed synthetic; 1 super-admin (admin@nexus.ai/admin123) + sample orgs / VKs / providers / routing rules / alert rules |
| `tools/db-migrate/seed/seed.ts` | Loads `seed-baseline.sql` + redacts Credential encryption with local key |
| `tests/integration-go/helpers/` | Go test fixtures |
| Memory `project_local_test_vk` | Standing test VK for local AI Gateway smokes |
| `scripts/dev-start.sh` | One-shot bootstrap (Docker + DB + seed + UI) |

---

## 7. Binding rules (for the executing session)

### From CLAUDE.md (mandatory)
- **Plan first** before edits — written plan with approach/scope/risks
- **English only** for committed text; chat language can follow the user
- **No `git stash`** ever (parallel sessions share working tree)
- **Real implementation only** — no TODO/FIXME/XXX/stub/mock in production code
- **Development-phase greenfield** — no backward-compat shims, delete obsolete code outright
- **Pre-edit reading 3-doc rule** — open arch doc + feature doc + conventions.md before editing
- **Plan + Todo non-waivable for complex tasks** — Claude Code `TaskCreate`
- **Capture every user message as todos** — new asks don't interrupt current task
- **2-round completion self-audit** before reporting "done"
- **Commit reminder, no auto-commit** — ask before `git commit` unless authorized
- **Use explicit pathspec on `git commit`** to avoid sweeping other-session WIP
- **macOS NE provider must fail-open** (safety-critical)
- **Adapter format-translation §3a 7-rule contract** for any `spec_*/` change
- **IAM impact review** for any admin API endpoint / sidebar / route change
- **API stability for `shared/`** — additive-only once shipped in agent
- **Migration timestamp uniqueness** — pre-commit guards `tools/db-migrate/migrations/`
- **≥95% Go statement coverage per package** — pre-commit guard #9; allowlist requires user approval

### Auto-exec mode (per `feedback_open_source_program_autoexec`)
For multi-phase programs, treat reflection's "Recommendation" column as authorized; only pause for **genuinely material choices** (license, big architectural pivots, destructive cross-service refactors, irreversible operations).

### `feedback_test_coverage_autonomous_brainstorm`
"95% goal is ALL code; fully autonomous execution; use brainstorm on architectural blockers; tests must assert observable behavior not pad coverage."

---

## 8. Recent (pre-program) changes affecting testing

Renames / deletions a test author must know:

| What | Change |
|---|---|
| `packages/shared/normaliser/` | → `packages/shared/transport/wirerewrite/` (rename, all 4 consumer files updated) |
| `packages/shared/store/` | DELETED. `HookConfigRow` + `BuildHookConfig` moved to `packages/shared/policy/hooks/hookconfig_row.go` |
| Hub `credential_stats_flush.go` | Now uses `credstate.StatsKey()`, `credstate.StatsDirtySet`, `credstate.StatsField*` constants (not local strings) |
| `AgentInfo.DownloadURL` | New field; daemon-composed from `cfg.CpURL + platform-specific suffix`; agent UI reads `status.agent.downloadURL` |
| `revocation_endpoint` | Now advertised in `/.well-known/openid-configuration` (was stale-comment'd as not-implemented) |
| `jobs-architecture.md` §5 | Catalogue now lists 36 jobs (was 14) — `scripts/check-jobs-catalogue.sh` enforces lockstep |
| 7 prod-private skills | `git rm --cached` + `.gitignore`d — not in tracked tree |
| `docs/dev/_open-source-review/` | DELETED |
| `docs/dev/migrations/` | DELETED (orphan SQL) |
| `.prod.yaml` × 5 | renamed to `.prod.yaml.example` |
| `taskforce10x.com` references in tracked files | sanitized to `example.com` |

New Tier-2 / Tier-3 docs to consult:
- `shared-wirerewrite-architecture.md`
- `shared-utility-subpackages-architecture.md`
- `nexus-hub-internals-architecture.md`
- `aiguard-architecture.md`
- `ai-gateway-internals-architecture.md`
- `control-plane-internals-architecture.md`
- `agent-internals-sibling-pairs-architecture.md`

---

## 9. Suggested program structure

Mirror the OSS-readiness program shape (worked well):

**Phase 0 — Inventory + scope sign-off**
- Build `tests/scenarios/00-catalog.md` mapping every API endpoint → at least one scenario from §4
- Identify gaps (endpoints with no scenario coverage)
- Get user sign-off on scenario priority + which framework (Go test harness? bash? Playwright? Python?)

**Phase 1 — Scenario family by family**
- Roughly 1 family per work-unit (Onboarding / Routing / Compliance / Killswitch / Quota / Credentials / Cache / Agent / Compliance-Proxy / Alerts / Audit / IAM / OAuth)
- Per family: write the scenarios + run them + commit
- Each scenario asserts: HTTP shape + DB cross-check + metric delta + audit row presence

**Phase 2 — CI integration**
- Wire `tests/scenarios/` into `make test-all` or a new `make test-scenarios`
- Decide nightly vs PR-gating (full scenario suite is probably nightly + smoke on every PR)

**Phase 3 — Coverage report**
- Generate "every endpoint × scenarios covering it" matrix
- Identify weak spots (endpoints with only 1 scenario, error paths uncovered)

**Phase 4 — Final gate** — comprehensive run + readiness sign-off

**First concrete steps for the next session:**
1. Read `tests/.env.test`, `tests/lib/auth.sh`, `tests/run-all.sh` to ground in existing infra
2. Read the 13 family headings in §4 above + pick one to prototype (recommend **Onboarding S-001** as the canonical hello-world)
3. Brainstorm framework (Go-test? bash + curl? Python? Vitest?) and propose to user
4. Build the harness shell + one scenario + one assertion suite
5. Iterate

---

## 10. Repo state snapshot at program start

- Branch: `main` at `b382b8680` (= origin/main, plus this plan-doc commit)
- Working tree: parallel session WIP may exist — leave alone, use explicit pathspec on commits
- Memory anchored: `project_api_automation_test_program.md` + `MEMORY.md` updated
- Pre-commit hooks: 9 hard guards including `jobs-catalogue lockstep` + `go coverage ≥95%`
- ~80 architecture docs in `docs/dev/` are clean + lockstep-verified by CI
