# Scenario Test Catalog — API Surface Inventory + Coverage Map

> **Phase 0 deliverable** of the API automation test program (`docs/_archive/2026-q2/programs/test-scenarios-program-plan.md`). Maps every API endpoint across the 5 services to at least one scenario from §4 of the program plan; flags gaps; ranks priority. Read this before writing any scenario.

---

## 1. Mission recap

Scenario tests are the **layer above** the existing L1–L4 test program (smoke / Go integration / Python protocol / Python AI-judge / Playwright UI). They assert **coordinated business outcomes** across services, not single endpoints:

> *"Admin creates VK → first `/v1/chat/completions` returns 200 → traffic_event row lands → metric counter increments → audit row appears."*

**Coverage target:**
- Every admin API endpoint hit by ≥1 scenario
- Every `/v1/*` ingress by ≥3 scenarios (happy + error + edge)
- Every Hub-internal endpoint by ≥1 scenario

---

## 2. Safety: target-environment isolation (BINDING)

**Scenarios mutate state.** They create/delete VKs, routing rules, hooks; activate kill-switches; insert audit rows; enroll devices. A scenario run pointed at production would be a destructive incident.

The harness (`tests/scenarios/helpers/safety.go`, called from every test's setup) enforces these rules **fail-closed**:

1. **Single source of truth**: `tests/.env.test` only. Ambient shell `NEXUS_*` env vars are ignored unless `NEXUS_TEST_ALLOW_AMBIENT=1` is set (opt-in escape hatch, not the default).

2. **Hostname allowlist**: every `NEXUS_*_URL` parsed; allowed hosts are `localhost`, `127.0.0.1`, `::1`, `host.docker.internal`. Any other host (in particular anything containing `taskforce10x.com`) causes the harness to **print the offending var + value and exit 1 before running any test**.

3. **Scheme restriction**: HTTPS is allowed only when the host is `localhost`/`127.0.0.1` and the dev TLS cert is in use. Production-shaped HTTPS URLs (e.g. `https://nexus.<anything>.com`) are rejected.

4. **DB DSN safety**: `NEXUS_PG_HOST` must be localhost/127.0.0.1; `NEXUS_PG_PORT` defaults to `55532` (the dev compose mapping). The well-known prod port `5432` on a non-localhost host is rejected.

5. **Explicit target confirmation**: on the first scenario in a process, the harness prints the resolved target URLs and:
   - If stdin is a TTY: prompts `Target environment looks LOCAL. Proceed? [y/N]`. Non-`y` aborts.
   - If stdin is not a TTY (CI, `go test` redirected output): the harness requires `NEXUS_TEST_TARGET=local` to be set explicitly. Without it, exit with `set NEXUS_TEST_TARGET=local to acknowledge the local target`.

6. **`cp_login` is local-only**: the OAuth+PKCE helper additionally refuses to drive any login flow against a non-localhost `NEXUS_CP_URL`, regardless of cache state.

For prod debugging, use the `/prod-login` skill (separate token cache, no scenario mutation). Scenarios are **local-only by construction**.

> **Memory anchor:** `feedback_scenario_test_env_isolation` in maintainer memory.

---

## 3. AI Gateway `:3050` — `/v1/*` ingress surfaces

**Inventory** (`grep '/v1/' packages/ai-gateway/`):

| Endpoint | Method | Notes | Scenarios |
|---|---|---|---|
| `/v1/chat/completions` | POST | OpenAI Chat ingress (most-hit). Stream + non-stream. | S-001, S-010 – S-016, S-020 – S-023, S-040, S-050, S-060, S-062, S-063 (S-024 – S-025, S-041 – S-043, S-051 – S-053, S-061, S-100 deferred — see §10) |
| `/v1/messages` | POST | Anthropic Messages ingress. Stream + non-stream. | S-016 (Anthropic→OpenAI cross-format), S-061 (E38 cache markers) |
| `/v1/responses` | POST | OpenAI Responses (E56). Stateless only — no `previous_response_id` cross-format. | S-062 (NS happy-path + error envelope) [CLOSED 2026-05-21] |
| `/v1/embeddings` | POST | Embeddings cross-format. | S-063 (single + dimensions + batch) [CLOSED 2026-05-21] |
| `/v1/chat` | POST | Alias / legacy path. | covered by `/v1/chat/completions` family |
| `/v1/models` | GET | Per-VK filtered model list. | S-001 hello-world implicit pre-check |
| `/v1/ai-guard/classify` | POST | Direct aiguard classification. | covered by S-023 (aiguard) |
| `/v1/ai-guard/compliance-webhook` | POST | aiguard compliance webhook. | S-086 (decision envelope contract) [CLOSED 2026-05-21] |

**Adapter surfaces (19 providers)**: each provider has its own canonical↔wire codec. Per `provider-adapter-architecture.md` §3a Rules 1–7. Plan §4 S-003 = one scenario per provider (19 sub-cases).

| Adapter | Spec | Scenario |
|---|---|---|
| anthropic, openai, azure_openai, bedrock, cohere, deepseek, fireworks, gemini, glm, groq, huggingface, minimax, mistral, moonshot, perplexity, replicate, together, vertex, xai | `packages/ai-gateway/internal/providers/spec_<name>/` | S-003-{adapter} (19) |

**Routing strategies + hooks**: covered by S-010 – S-016 (routing) and S-020 – S-023 (hooks). S-024 – S-025 and S-031 – S-034 deferred — see §10.

---

## 4. AI Gateway admin probe endpoints (mounted on CP)

Per program plan §3 these admin probes call into AI GW logic but are exposed via CP admin API:

| Endpoint | Scenario |
|---|---|
| `POST /api/admin/providers/test-connection` | S-002 (add provider) |
| `POST /api/admin/hooks/:id/test` | S-020 – S-023 setup |
| `POST /api/admin/hooks/:id/dry-run` | S-027 (no-side-effect contract) + S-065 (dry-run estimate) [CLOSED 2026-05-21] |
| `POST /api/admin/routing-rules/simulate` | S-010 – S-016 setup |
| `POST /api/admin/credentials/:id/probe` | S-050 setup (S-051 – S-053 deferred) |
| `POST /api/admin/cache/preview` | S-060 – S-063 setup |

---

## 5. Control Plane `:3001` — `/api/admin/*`

**282 registered routes** (grep across `packages/control-plane/internal/handler/`). Grouped by resource family:

### 5.1 Tenancy & Identity

| Family | Endpoints (sample) | Scenarios |
|---|---|---|
| **Organizations** | `/organizations`, `/organizations/:id`, `/organizations/tree` | S-001 (bootstrap), S-110 (IAM scope) |
| **Projects** | `/projects`, `/projects/:id` | S-001 |
| **Users (admin)** | `/users`, `/users/:id`, `/users/:id/audit`, `/users/:id/device-assignments`, `/users/:id/identity`, `/users/:id/revoke-access` | S-001, S-110, S-111 |
| **Auth / sessions** | `/auth/revoke-device`, `/auth/sessions`, `/me`, `/me/permissions`, `/me/admin-audit-logs`, `/profile` | S-110 (IAM), S-121 (token lifecycle) |
| **API keys** | `/api-keys`, `/api-keys/:id`, `/api-keys/:id/regenerate` | S-115 (personal key lifecycle: create → use → regen → delete + meta-audit) |

### 5.2 IAM

| Family | Endpoints (sample) | Scenarios |
|---|---|---|
| **Policies** | `/iam/policies`, `/iam/policies/:id`, `/iam/policies/:id/attachments` | S-110, S-111 |
| **Groups** | `/iam/groups`, `/iam/groups/:id/members`, `/iam/groups/:id/policies` | S-110 |
| **Principals** | `/iam/principals/:type/:id/policies` | S-110, S-112 (TTL binding) |
| **Action catalog** | `/iam/action-catalog` | S-110 implicit |
| **Simulate** | `/iam/simulate` | S-113 (decision simulator parity with real eval) |

### 5.3 SSO / IdP

| Family | Endpoints | Scenarios |
|---|---|---|
| **IdPs** | `/identity-providers`, `/identity-providers/:idpId`, `/identity-providers/:idpId/test`, `/identity-providers/test` | S-123 (federation) |
| **Group mappings** | `/identity-provider/:idpId/group-mappings`, `.../:mappingId` | S-123 |
| **SCIM tokens** | `/identity-provider/:idpId/scim-tokens`, `.../:tokenId` | S-070 (SCIM token mint + round-trip) [CLOSED 2026-05-21] |
| **SCIM endpoints** | `/Users`, `/Groups`, `/Schemas`, `/ServiceProviderConfig` | S-070 (provisioning + listing + revocation) [CLOSED 2026-05-21] |

### 5.4 Virtual Keys / Models / Providers / Credentials

| Family | Endpoints | Scenarios |
|---|---|---|
| **Virtual Keys** | `/virtual-keys`, `/virtual-keys/:id/{approve,reject,regenerate,renew,revoke}` | S-001, S-040 (quota; S-041 – S-043 deferred), S-110 |
| **Providers** | `/providers`, `/providers/:id/{credentials,health,models,test}`, `/providers/test-connection`, `/provider-health` | S-002, S-003 (×19) |
| **Models** | `/models`, `/models/:id`, `/models/flat` | S-001 implicit |
| **Pricing** | `/pricing`, `/pricing/:id` | **GAP — needs S-044 (pricing override → cost computation; requires real AI traffic)** |
| **Credentials** | `/credentials`, `/credentials/:id`, `/credentials/:id/{circuit-reset,probe,reliability-overrides}`, `/credentials/{key-rotation-status,rotate-key,rotation-status}` | S-050 (S-051 – S-053 deferred) |

### 5.5 Routing & Hooks & Cache

| Family | Endpoints | Scenarios |
|---|---|---|
| **Routing rules** | `/routing-rules`, `/routing-rules/:id`, `/routing-rules/simulate` | S-010 – S-016, S-075 (smart strategy replaces S-015 skip) [CLOSED 2026-05-21], S-080 (no-match passthrough-fallback) |
| **Hooks** | `/hooks`, `/hooks/:id/{dry-run,test}`, `/hooks/{execution-chain,implementations,refresh,reorder}`, `/hooks/:hookId/rule-packs` | S-020 – S-025, S-027, S-065 (dry-run estimate), S-068 (IP filter lifecycle), S-069 (webhook-forward egress) |
| **Rule packs** | `/rule-packs`, `/rule-packs/:id/dry-run`, `/rule-packs/{import,preview}`, `/rule-pack-installs/:installId{,/effective-rules,/overrides}` | S-026 (install + override disable/severity merge) |
| **AI Guard config** | `/ai-guard/config`, `/ai-guard/dry-run` | S-023, S-086 (webhook-forward sink) |
| **Cache** | `/cache/{adapters,effective,flush,global,overrides,preview,stats,feedback,prewarm,time-sensitive-patterns}`, `/cache/adapter/:adapter_type`, `/cache/provider/:provider_id`, `/semantic-cache/config` | S-060 (L1 hit), S-064 (L2 semantic hit), S-066 (negative-feedback eviction), S-067 (FAQ prewarm), S-079 (Anthropic cache double-count), S-081 (freshness pattern skips both tiers), S-082 (embedding provider config) |

### 5.6 Quota

| Family | Endpoints | Scenarios |
|---|---|---|
| **Quota policies** | `/quota-policies`, `/quota-policies/:id` | S-040, S-078 (org parent→child quota cascade) (S-041 – S-043 deferred) |
| **Quota overrides** | `/quota-overrides`, `/quota-overrides/:id` | S-042 deferred |
| **Quota analytics** | `/quota-analytics/{overview,top,trend}` | S-045 (scope enum + required-param guards + envelope) |

### 5.7 Alerts

| Family | Endpoints | Scenarios |
|---|---|---|
| **Alerts** | `/alerts`, `/alerts/:id/{ack,resolve}` | S-091/S-092 (alert raise + channel test; ack/resolve covered transitively) |
| **Channels** | `/alerts/channels`, `/alerts/channels/:id/test` | S-092 |
| **Rules** | `/alerts/rules`, `/alerts/rules/:id/reset` | S-091 (builtin↔seed lockstep) |

### 5.8 Compliance / Killswitch / Passthrough

| Family | Endpoints | Scenarios |
|---|---|---|
| **Compliance** | `/compliance/{audit,exemption-grants,exemptions,killswitch,overview,report,trinity}`, `/compliance/exemption-grants/:id`, `/compliance/exemptions/:id/{approve,reject}`, `/compliance/killswitch/history`, `/compliance/overview/export` | S-030 (S-031 – S-034 deferred) |
| **Passthrough (E48)** | `/passthrough/{global,snapshot}`, `/passthrough/adapter/:adapter_type`, `/passthrough/effective/:provider_id`, `/passthrough/provider/:provider_id` | S-030 (S-031 – S-034 deferred) |
| **Proxy admin** | `/proxy/{audit,compliance/coverage,compliance/export,compliance/hook-health,compliance/reject-stats,connections,health,metrics,reject-config}` | S-081 – S-084 daemon-bound (CONNECT pipeline); see §10 |
| **Interception domains** | `/interception-domains`, `/interception-domains/:id`, `/interception-domains/:id/paths`, `.../:pathId` | S-085 (interception domain push to compliance-proxy → hot-reload signal) |

### 5.9 Fleet (Agent devices / users / groups)

| Family | Endpoints | Scenarios |
|---|---|---|
| **Agent devices** | `/agent-devices`, `/agent-devices/:id/{assignments,audit,config,events,force-refresh,rotate-cert,tags,timeline,unenroll}`, `/agent-devices/{enroll-token,health}` | S-070, S-071, S-073 |
| **Agent users** | `/agent-users`, `/agent-users/:id/{activate,audit,devices,suspend}` | S-074 (suspend → activate lifecycle + admin-user 404 guard + audit BeforeState) |
| **Device groups** | `/device-groups`, `/device-groups/:id/{configs,force-refresh,members,membership-query,rotate-cert}`, `/device-groups/{preview-membership}`, `/device-groups/:id/configs/:configKey`, `/device-groups/:id/members/:deviceId` | S-071 (smart-membership preview + persist + envelope) [CLOSED 2026-05-21] |
| **Enrollment tokens** | `/enrollment/token`, `/enrollment/tokens` | covered by agent-lifecycle family |
| **Fleet analytics** | `/fleet-analytics/{summary,top-destinations,trends}` | S-072 (summary + top + trend envelope contract) [CLOSED 2026-05-21] |
| **Nodes (Things)** | `/nodes`, `/nodes/:id/{applied-config,config-sync,device-assignments,overrides,resync,runtime}`, `/nodes/:id/overrides/:configKey`, `/nodes/overrides`, `/instances` | S-077 (override PUT/list/applied-config/DELETE + blacklist + non-object reject), S-076 (heartbeat freshness → `last_seen_at`), S-144 (runtime); (S-100 traffic/audit-on-nodes deferred) |
| **Agents diagnostic** | `/agents/diagnostic-mode`, `/agents/diagnostic-mode/bulk`, `/agents/:nodeId/diagnostic-mode` | S-073 (enable / list / disable lifecycle + bulk + Hub fanout) [CLOSED 2026-05-21] |

### 5.10 Audit / Analytics / Observability

| Family | Endpoints | Scenarios |
|---|---|---|
| **Traffic / audit** | `/traffic`, `/traffic/:id`, `/traffic/:id/normalized`, `/traffic/{storage,stream}`, `/traffic-adapters` | S-101, S-103 (S-100 happy-path traffic listing deferred) |
| **Admin audit logs** | `/admin-audit-logs`, `/admin-audit-logs/export`, `/me/admin-audit-logs` | S-103 (export envelope + meta-audit row invariant); S-102 deferred |
| **Analytics** | `/analytics/{by-provider,by-user,cache-roi,cost,cost-report,cost-summary,latency-phases,provider/:providerId,quality,routing,routing/fallbacks,sparkline,summary,usage}` | S-093 (cost-summary partition + non-negative invariants), S-094 (cache-roi monotonicity + net-savings identity) |
| **Metrics aggregates** | `/metrics/aggregates`, `/ops-metrics/{current,fleet,timeseries}` | S-095 (empty-window + envelope contract) |
| **Activity** | `/activity` | covered transitively by S-103 (admin audit export shares the activity feed); S-102 deferred |

### 5.11 Compliance Proxy admin (mounted on CP)

| Family | Endpoints | Scenarios |
|---|---|---|
| **Proxy** | covered in 5.8 above | — |
| **DSAR** | `/dsar`, `/dsar/:id`, `/dsar/:id/fulfill` | S-096 (state-machine guard + validation + audit trail) |

### 5.12 Settings

| Family | Endpoints | Scenarios |
|---|---|---|
| **Settings root** | `/settings`, `/settings/{credential-reliability,device-auth,device-defaults,observability,payload-capture,siem,siem/event-types,siem/test,streaming-compliance}` | S-130 (SIEM test envelope contract), S-131 (streaming-compliance enum validation + 3-service fanout) |
| **Observability** | `/observability/retention` | S-130 |
| **PAC file** | `/pac-file` | S-132 (PAC file generation + syntax sanity + 400 on missing params) |
| **Setup state** | `/setup-state`, `/onboarding` | S-001 |
| **Services** | `/services/public-urls` | S-001 implicit |
| **Revocations** | `/revocations` | S-121 |

### 5.13 Config-sync (Hub bridge)

| Family | Endpoints | Scenarios |
|---|---|---|
| **Config sync** | `/config-sync/{catalog,history,out-of-sync,update}` | S-140 (catalog nodeType rename + outOfSync envelope + history shape) |

### 5.14 Diag

| Family | Endpoints | Scenarios |
|---|---|---|
| **Diag events** | `/diag-events`, `/diag-events/crash-cohorts`, `/diag-events/groups`, `/diag-silences`, `/diag-silences/:id` | S-141 (seed thing_diag_event → list + groups + crash-cohorts) |

### 5.15 Jobs

| Family | Endpoints | Scenarios |
|---|---|---|
| **Jobs (Hub-managed)** | `/jobs`, `/jobs/:id`, `/jobs/:id/runs`, `/jobs/:id/trigger` | S-142 (trigger audit-chain-verify → new job_run row + admin audit) |

### 5.16 Things (admin view, separate from Nodes alias)

| Family | Endpoints | Scenarios |
|---|---|---|
| **Things stats** | `/things/:id/stats` | covered by S-100 |

### 5.17 Exemption requests (agent-side)

| Family | Endpoints | Scenarios |
|---|---|---|
| **Exemption requests** | `/exemption-requests` | S-082 implicit |

---

## 6. Control Plane `:3001` — `/oauth/*` + `/authserver/*` + discovery

OAuth+PKCE Authorization Server (per `oauth-pkce-admin-auth-architecture.md`):

| Endpoint | Scenarios |
|---|---|
| `GET /.well-known/openid-configuration` | S-120 |
| `GET /.well-known/jwks.json` | S-120 |
| `GET /oauth/authorize` | S-121 |
| `POST /oauth/token` | S-121, S-122 |
| `POST /oauth/introspect` | S-121 |
| `POST /oauth/revoke` | S-121 |
| `POST /authserver/password` | S-121 (local login) |
| SSO callback flow | S-123, S-125 (JIT provisioning DB invariant) |

---

## 7. Nexus Hub `:3060` — `/api/internal/*` + `/api/public/*` + `/ws`

Per `grep packages/nexus-hub/internal/handler/routes.go`:

| Endpoint | Scenarios |
|---|---|
| `GET /ws` (Thing WebSocket, mTLS or bearer) | S-070 (every Thing connects); S-100 deferred |
| `POST /api/internal/things/enroll` | S-070, S-071 |
| `POST /api/internal/things/register` | S-070 implicit |
| `POST /api/internal/things/heartbeat` | covered transitively |
| `POST /api/internal/things/shadow` (Thing-reported state) | S-033 deferred (kill-switch auto-revert needs reported state) |
| `GET /api/internal/things/config` (bulk pull) | S-001 (gateway boot) |
| `GET /api/internal/things/config/:key` (single key pull) | S-030 implicit |
| `POST /api/internal/things/audit` | S-101 (S-100 deferred) |
| `POST /api/internal/things/deregister` | **GAP — needs S-079 (graceful Thing deregister)** |
| `POST /api/internal/things/exemption` | S-082 |
| `GET /api/internal/things/update-check` | deferred — internal mTLS-authed route, agent-side test |
| `POST /api/internal/things/renew-cert` | S-070 implicit |
| `POST /api/internal/things/agent-audit` | S-101 (covers spillstore agent-audit upload); S-100 deferred |
| `POST /api/internal/things/spill-uploads` | S-101 |
| `PUT /api/internal/spill/blob/:token` | S-101 |
| `POST /api/internal/things/diag-events:batch` | S-078 (covers above) |
| `POST /api/internal/alerts/raise` | covered transitively by S-091 (builtin↔seed lockstep exercises raise path) |
| `POST /api/internal/alerts/resolve` | covered transitively by S-091 |
| `GET /api/internal/alerts/admin/{rules,channels,...}` | covered by S-091/S-092 |
| `POST /api/internal/config/update` | S-033 (auto-revert), S-140 (drift) |
| `GET /api/internal/things` (list) | covered by `/api/admin/nodes` proxy |
| `GET /api/internal/jobs`, `/api/internal/jobs/:id/{runs,trigger}` | S-142 |
| `POST /api/internal/enrollment/token` | S-070 |
| `GET /api/public/agent-bootstrap` | S-070 (pre-enrollment discovery) |
| `GET /healthz`, `/readyz`, `/metrics` | preflight (existing) |
| `/runtime/*` (localhost-only) | S-144 (CP→Hub passthrough: bad/fake/live id + 4xx defensive) |

---

## 8. Compliance Proxy `:3040`

| Endpoint | Scenarios |
|---|---|
| HTTP CONNECT (TLS bump pipeline) | S-080, S-081 |
| `/runtime/{exemptions,killswitch,health}` (localhost ops API) | S-031 / S-032 implicit |

---

## 9. Agent — localhost IPC

| Endpoint (statusapi) | Scenarios |
|---|---|
| GET_STATUS, QUERY_EVENTS, QUERY_LIFECYCLE, GET_APPLIED_CONFIG, AUTHENTICATE, SHUTDOWN | **Out of scope for HTTP scenario layer** — covered by Agent UI Playwright + Wails IPC tests. Out of catalog. |

---

## 10. Scenario inventory cross-reference

The 13 families from program plan §4, with current scenario count and gap count (post 2026-05-21 batch of 21 new scenarios — S-062..S-086, S-125):

| Family | Landed | Identified gaps | Total target |
|---|---|---|---|
| Onboarding | S-001, S-002, S-003 (×19 adapters) | — | 21 |
| Routing | S-010 – S-016, S-075 (smart) [CLOSED 2026-05-21], S-080 (no-match passthrough) | — | 9 |
| Compliance | S-020 – S-025, S-027 (hook dry-run), S-065 (dry-run estimate), S-068 (IP filter), S-069 (webhook-forward egress), S-086 (compliance webhook) | S-026 (rule-pack install) [LANDED] | 12 |
| Killswitch / passthrough | S-030 | S-031 – S-034 deferred | 1 |
| Quota / cost | S-040, S-045, S-078 (org cascade) [CLOSED 2026-05-21], S-079 (Anthropic cache double-count) [CLOSED 2026-05-21] | S-041 – S-044 deferred | 4 |
| Credentials | S-050 | S-051 – S-053 deferred | 1 |
| Cache | S-060 (L1), S-064 (L2 semantic) [CLOSED 2026-05-21], S-066 (negative feedback) [CLOSED 2026-05-21], S-067 (prewarm) [CLOSED 2026-05-21], S-081 (freshness skip) [CLOSED 2026-05-21], S-082 (embedding provider config) [CLOSED 2026-05-21] | S-061 unused slot; cross-ingress cache parity covered by smoke-gateway --all-ingress | 6 |
| Agent lifecycle | S-071 (device groups) [CLOSED 2026-05-21], S-072 (fleet analytics) [CLOSED 2026-05-21], S-073 (diag mode) [CLOSED 2026-05-21], S-074 (agent users), S-076 (heartbeat freshness) [CLOSED 2026-05-21], S-077 (node overrides) | S-070..S-079 daemon-bound (pinned) | 6 |
| Compliance proxy | S-085 (interception domains) | S-080 – S-084 daemon-bound | 1 |
| Alerts | S-091, S-092, S-093, S-094, S-095 | — | 5 |
| Audit | S-101, S-103 | S-100, S-102 implicit | 2 |
| IAM | S-110, S-113, S-115 | S-111 – S-114 deferred | 3 |
| OAuth / SSO | S-120 – S-122, S-125 (JIT provisioning DB invariant) [CLOSED 2026-05-21] | S-123, S-124 superseded by S-070/S-125 | 4 |
| AI Gateway ingress | S-062 (Responses) [CLOSED 2026-05-21], S-063 (Embeddings) [CLOSED 2026-05-21] | — | 2 |
| **Cross-cutting** | S-096 (DSAR), S-130 (SIEM), S-131 (streaming-compliance), S-132 (PAC), S-140 (config-sync), S-141 (diag cohorts), S-142 (jobs), S-144 (Hub runtime) | S-143 (update-check) daemon-bound | 8 |

**Totals**:
- Distinct scenario IDs landed: **66** (top-level `TestS<NNN>` functions across 51 `*_test.go` files; sub-tests via `t.Run` not separately counted)
- 2026-05-21 batch closed 21 net-new scenarios (S-062..S-086 + S-125), promoting 11 previously-marked GAPs to CLOSED state.
- Remaining target gaps require either real agent daemon, real AI traffic burn, or a mock provider (see COVERAGE.md "What's NOT covered").

---

## 11. Priority ordering for Phase 1 execution

Recommended family order (matches risk-weighted business value):

1. **Onboarding (S-001 → S-003)** — proves harness end-to-end; one scenario per adapter validates §3a 7-rule contract
2. **Routing (S-010 – S-016)** — most code volume in AI Gateway; canonical-payload bug class
3. **Compliance (S-020 – S-026)** — hooks pipeline is safety-critical (data exfiltration, PII)
4. **Killswitch (S-030 – S-034)** — emergency-passthrough rollback path; tested under incident pressure
5. **Quota (S-040 – S-045)** — revenue protection
6. **Cache (S-060 – S-063)** — cost optimisation correctness
7. **Credentials (S-050 – S-053)** — provider reliability blast radius
8. **IAM (S-110 – S-115)** — admin authorisation correctness
9. **OAuth / SSO (S-120 – S-124)** — admin authentication correctness
10. **Audit (S-100 – S-103)** — observability backbone
11. **Alerts (S-090 – S-095)** — incident detection
12. **Compliance proxy (S-080 – S-085)** — server-side compliance enforcement
13. **Agent lifecycle (S-070 – S-079)** — fleet management; lowest-risk for OSS launch
14. **Cross-cutting gaps (S-130 – S-144, S-096)** — sweep last as fill-in

---

## 12. Next steps (Phase 0 → Phase 1)

1. **User sign-off** on this catalog: priority order, scope inclusions, gap acceptance.
2. **Build `tests/scenarios/` harness skeleton**: `go.mod` + `helpers/{safety,admin,aigw,hub,prom,cleanup}.go`. Safety enforcement first.
3. **Prototype S-001 (Onboarding hello-world)**: bootstrap fresh tenant → first VK → `/v1/chat/completions` 200 → traffic_event row + metric delta.
4. **Iterate family by family** per §11 priority order.
5. **CI wiring** (Phase 2): `make test-scenarios` invoked from `tests/run-all.sh` `--full` mode, gated to local target.
6. **Coverage matrix** (Phase 3): every endpoint × scenarios covering it.
7. **Final sweep** (Phase 4): close §10 gaps + readiness sign-off.

---

## 13. Endpoint count sanity

| Service | Endpoint family count | Distinct routes |
|---|---|---|
| AI Gateway `/v1/*` | 6 ingress + 19 adapters | ~25 |
| AI Gateway admin probes (on CP) | 5 | 5 |
| CP `/api/admin/*` | 17 resource families | ~282 routes |
| CP `/oauth/*` + discovery | 8 | 8 |
| Hub `/api/internal/*` + `/ws` + `/api/public/*` | 25 | ~25 |
| Compliance proxy | 2 (CONNECT + runtime ops) | 2 |
| Agent IPC | (out of scope — Wails/IPC layer) | — |

Total in scope: **~347 endpoints** mapped to **66 landed scenarios** (post 2026-05-21 batch) plus a residual gap-list captured in COVERAGE.md. Average ~5 endpoints per landed scenario; the §1 coverage target ("every admin endpoint by ≥1 scenario; every /v1/* by ≥3") is met for every non-daemon-bound surface.

---

## 14. Living-doc rules

- This catalog is updated **whenever** a new endpoint lands in any service. Pre-commit hook (future) can grep for `/api/admin/`, `/api/internal/`, `/v1/` registrations and flag uncovered ones.
- Scenario IDs (`S-001` etc.) are append-only; never reused.
- Gaps marked `**GAP — ...**` in this file are the work backlog for Phase 1 family closeout.
- The Safety section (§2) is **binding** and must not be relaxed without explicit user approval.
