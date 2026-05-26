# Scenario harness — coverage matrix (Phase 3)

Snapshot of what the `tests/scenarios/` Go harness covers (66 scenarios
landed across 51 `*_test.go` files as of 2026-05-21, full suite green on
local) and what it deliberately doesn't. This is the program's
controlled-acknowledgement doc: every uncovered area below has a
one-line rationale so the next session knows whether to attack it next
or pin it as a known gap.

The harness sits at **layer 5** above the existing test stack:

1. `tests/smoke/` — bash smoke against `/v1/*`.
2. `tests/integration-go/` — Go integration tests with build tags.
3. `tests/e2e-python/` (protocol) — protocol-level e2e.
4. `tests/e2e-python/` (AI judge) — quality e2e.
5. **`tests/scenarios/`** — admin-API scenario harness (this file).
6. `tests/e2e-ui/` (Playwright) — UI flows.

## What's covered (66 scenarios across 51 files)

| Family | Scenarios landed | Total endpoints touched | Notes |
|---|---|---|---|
| **Onboarding** | S-001/002/003 | `/auth/login`, `/my/virtual-keys`, `/admin/providers`, `/v1/chat/completions` + 19 adapters | Full PKCE + provider CRUD + 19-adapter matrix |
| **Routing** | S-010..S-016, S-075 [CLOSED 2026-05-21], S-080 [CLOSED 2026-05-21] | `/admin/routing-rules` + executor + passthrough-fallback sentinel | Smart-routing now lands as PASS via S-075 (replaces S-015 skip); S-080 verifies E31-S3 no-match passthrough-fallback contract |
| **Compliance hooks** | S-020/021/022/023, S-068 [CLOSED 2026-05-21], S-069 [CLOSED 2026-05-21], S-086 [CLOSED 2026-05-21] | `/admin/hooks`, `/v1/chat/completions`, `/v1/ai-guard/compliance-webhook`, rule-pack patterns | + IP-access-filter lifecycle, webhook-forward outbound POST, compliance-webhook decision envelope |
| **Rule packs** | S-026 | `/admin/rule-packs`, `/rule-pack-installs/:id/{overrides,effective-rules}` | Override disable + severity demote |
| **Hook authoring** | S-027, S-065 [CLOSED 2026-05-21] | `/admin/hooks/:id/dry-run`, `/admin/hooks/:id/estimate` | Side-effect-free contract + cost estimate |
| **Kill switch** | S-030 | 3-tier passthrough toggles + bypassHooks | E48 emergency path |
| **Quota** | S-040, S-078 [CLOSED 2026-05-21] | `/my/virtual-keys` rateLimitRpm → 429, parent→child org quota cascade | Single-VK rate limit + org-hierarchy cap aggregation |
| **Quota analytics** | S-045 | `/admin/quota-analytics/{overview,top,trend}` | Scope enum + required-param + envelope |
| **Credentials** | S-050 | `/admin/providers/:id/credentials` + test endpoint | Probe round-trip |
| **Cache** | S-060, S-064 [CLOSED 2026-05-21], S-066 [CLOSED 2026-05-21], S-067 [CLOSED 2026-05-21], S-079 [CLOSED 2026-05-21], S-081 [CLOSED 2026-05-21], S-082 [CLOSED 2026-05-21] | `/v1/chat/completions` + `/v1/messages` cache paths, `/admin/cache/{feedback,prewarm,time-sensitive-patterns}`, `/admin/semantic-cache/config` | L1 hit + L2 semantic + negative feedback + FAQ prewarm + Anthropic cache double-count fix + freshness skip + embedding provider config |
| **AI Gateway ingress** | S-062 [CLOSED 2026-05-21], S-063 [CLOSED 2026-05-21] | `/v1/responses` (NS + error envelope), `/v1/embeddings` (single + dimensions + batch) | Closes the two §3 ingress gaps |
| **Agent users** | S-074 | `/admin/agent-users/:id/{suspend,activate}` | Lifecycle + admin-user 404 guard |
| **Node overrides** | S-077 | `/admin/nodes/:id/{overrides,applied-config}` | Blacklist + shape gate + merge |
| **Device groups** | S-071 [CLOSED 2026-05-21] | `/admin/device-groups/{:id,/preview-membership}` | Smart-membership preview vs persist split + envelope stability |
| **Fleet analytics** | S-072 [CLOSED 2026-05-21] | `/admin/fleet-analytics/{summary,top-destinations,trends}` | Envelope contract on empty-window (no agents enrolled) |
| **Diag mode** | S-073 [CLOSED 2026-05-21] | `/admin/agents/{diagnostic-mode,:nodeId/diagnostic-mode,diagnostic-mode/bulk}` | Enable / list / disable lifecycle + bulk fanout |
| **Agent heartbeat** | S-076 [CLOSED 2026-05-21] | `/admin/nodes` `last_seen_at` round-trip | Heartbeat → admin Nodes page freshness, server-Thing-driven (no daemon needed) |
| **Compliance proxy** | S-085 | `/admin/interception-domains` | Hot-reload signal to proxy |
| **Alerts** | S-091/092 | `/admin/alerts/{channels,rules}` + builtin↔seed lockstep | Channel test envelope + Go⊆DB |
| **Analytics** | S-093/094/095 | `/admin/analytics/{cost-summary,cache-roi}`, `/metrics/aggregates` | Non-neg + identity + empty-window |
| **DSAR** | S-096 | `/admin/dsar/*` | State-machine + validation + audit |
| **Audit** | S-101/103 | `/admin/{traffic,admin-audit-logs/export}` | Spillstore + meta-audit |
| **IAM** | S-110/113/115 | `/admin/iam/{simulate,policies}`, `/admin/me`, `/my/api-keys` | Viewer-policy + simulate parity + key lifecycle |
| **OAuth + JIT** | S-120/121/122, S-125 [CLOSED 2026-05-21] | `/.well-known/...`, `/oauth/{token,introspect,revoke}`, `JITProvisionUser` tx | Discovery + introspect-after-revoke + refresh rotation + OIDC JIT DB invariant (NexusUser + UserFederatedIdentity) |
| **SCIM** | S-070 [CLOSED 2026-05-21] | `/admin/identity-providers`, `/admin/identity-provider/:idpId/scim-tokens`, `/scim/v2/{Users,Groups,...}` | Token mint + SCIM user provisioning + listing + revocation |
| **Settings root** | S-130/131 | `/admin/settings/{siem,streaming-compliance}` | SIEM envelope + streaming 3-service fanout |
| **PAC** | S-132 | `/admin/setup/proxy/:tid/pac-file` | Generation + syntax sanity |
| **Config sync** | S-140 | `/admin/config-sync/{catalog,out-of-sync,history}` | Rename surface + envelope |
| **Diag events** | S-141 | `/admin/diag-events{/groups,/crash-cohorts}` | List + groups + cohorts |
| **Jobs** | S-142 | `/admin/jobs/:id/trigger` | Audit-chain-verify trigger |
| **Hub runtime** | S-144 | `/admin/nodes/:id/runtime` | CP→Hub passthrough + 4xx defensive |

## What's NOT covered (and why)

### Pinned — requires running agent daemon

These need a real `nexus-agent` process registered as a Thing, with mTLS
cert + device-token enrollment. The scenario harness intentionally does
not spin up an agent; the existing `packages/agent/test/` suite owns
that surface.

> Note: the 2026-05-21 batch renumbered several IDs originally listed under "deferred — needs daemon" because their actual contract turned out to be testable without a running agent. See the closed rows tagged `[CLOSED 2026-05-21]` in the table above.

| ID | Endpoint(s) | Why deferred |
|---|---|---|
| Agent enrollment / cert renew (pre-2026-05-21 S-070..S-073 placeholder block) | `/api/internal/things/enroll`, `/renew-cert` | Needs ECDSA keygen + CSR fixture; agent-bound. S-070, S-071, S-072, S-073 IDs were reused by the 2026-05-21 batch for non-daemon surfaces (SCIM, device-groups, fleet-analytics, diag-mode admin path) — actual daemon-bound enrollment scenarios stay deferred without a numbered slot. |
| S-143 | `/api/internal/things/update-check` | Internal mTLS-authed; agent-side test |
| S-079 placeholder (graceful Thing deregister) | `/api/internal/things/deregister` | Internal mTLS-authed; ID reused by S-079 Anthropic-cache double-count (2026-05-21). Daemon-bound deregister remains pinned without a slot. |
| S-080..S-084 placeholder (Compliance proxy CONNECT smoke) | Compliance proxy CONNECT pipeline | Needs `tlsbump` fixture + CA install on client. S-080 ID reused by 2026-05-21 no-match passthrough-fallback routing scenario; CONNECT smoke stays pinned without a slot. |

### Pinned — requires real AI traffic (cost / quality / streaming)

These need either (a) a live provider API key burning real $ or (b) a
deterministic mock provider the harness doesn't currently ship.

| ID | Endpoint(s) | Why deferred |
|---|---|---|
| S-044 | `/admin/pricing/:id` | Pricing override → cost computation needs an AI call to validate the override took effect |
| `/v1/embeddings` SSE / cross-format | POST | S-063 covers happy-path NS today; SSE-style streaming + cross-format ingress→canonicalisation parity still needs scenario coverage |
| `/v1/responses` SSE arm | POST | S-062 covers NS + error envelope; SSE arm pinned until streaming session diff oracle stabilises |

### Pinned — external mock complexity

| ID | Endpoint(s) | Why deferred |
|---|---|---|
| (formerly S-124) | SCIM tokens + SCIM endpoints | **[CLOSED 2026-05-21 as S-070]** — `scim_test.go` lands the round-trip without an external IdP by minting the SCIM bearer through the admin API and exercising the SCIM 2.0 endpoints with a plain `http.Client`. |
| (formerly S-075) | `/admin/device-groups/:id/membership-query` | **[CLOSED 2026-05-21 as S-071]** — `device_group_membership_test.go` exercises preview-vs-persist split + envelope stability without requiring an enrolled device; the admin contract is testable on an empty group. |
| (formerly S-076) | `/admin/fleet-analytics/*` | **[CLOSED 2026-05-21 as S-072]** — `fleet_analytics_test.go` lands the envelope-contract assertions (status, JSON shape, non-negativity) which is the actually-useful invariant on empty windows; populated rollups stay implicitly out of scope. |

### Surfaced product issues (logged, not blocked on)

| Scenario | Issue surfaced | Documented |
|---|---|---|
| S-091 | 3 `credential.*` rules in seed not in Go BuiltinRules | `project_alerting_builtin_drift_2026_05_15` memory |
| S-093 | Rollup pipeline emits per-org rows but skips the global (dimensionKey="") row → totals=0 but byOrg.sum > 0 | Inline t.Logf in `analytics_test.go:115` |
| S-094 | Gateway hits=38 but cache_read_tokens=0 — response-cache short-circuit (expected, not a bug; logged for awareness) | Inline t.Logf |
| S-077 | Hub-side override audit may stamp entityType differently than CP-side audit → audit row count check is logged-only | Inline t.Logf |
| catalog §10 | `tlt-ks-*` transient Things from kill-switch traffic-list tests never cleaned up | Visible in S-140 catalog count (102 vs 5 canonical types) |

## Coverage by service area

Crude tally of unique admin endpoints touched (read OR write) per service (post 2026-05-21 batch):

| Service | Endpoints in catalog | Touched by scenarios | Coverage |
|---|---|---|---|
| Control Plane | ~310 | ~120 | ~38% (high-blast-radius surfaces covered; SCIM, device-groups, fleet-analytics, diag-mode, cache feedback/prewarm/freshness/embedding-config newly covered) |
| AI Gateway `/v1/*` | ~12 (incl. adapter matrix) | 8 + 19 adapter sub-cases | ~67% (`/v1/responses`, `/v1/embeddings`, `/v1/ai-guard/compliance-webhook` now landed) |
| Nexus Hub `/api/internal/*` | ~28 | 0 (all mTLS-authed) | 0% — deliberate |
| Compliance Proxy | ~3 admin reads | 1 (via hot-reload signal) | 33% |
| Agent | ~20 daemon endpoints | 0 — needs daemon | 0% — deliberate |

The Control Plane coverage skew reflects the program's deliberate
priority: admin authoring surfaces (where wrong answers are silent and
expensive) get scenario coverage; read-only listing surfaces that
already have unit tests get token coverage. The unit-test layer hits
≥95% per Go package (`scripts/check-go-coverage.sh`), so the gap here
is for end-to-end orchestration testing, not statement coverage.

## What this harness will and will not catch

**Will catch:**
- Schema drift across CP↔Hub renames (`thingType`→`nodeType`).
- Audit-trail gaps (any mutation that should emit an `AdminAuditLog`
  row but doesn't).
- Hot-reload break (config write → service didn't increment the
  applies counter).
- Validation enum drift (a scope/mode value that silently defaults).
- Cross-ingress asymmetry (one ingress accepts what another rejects).
- State-machine breaks (DSAR PENDING→COMPLETED skip; OAuth
  introspect-after-revoke; quota 429-after-N).

**Will not catch:**
- Wrong cost numbers in dashboards (no real AI traffic).
- Bad PAC syntax that JavaScript-parses but routes wrong (no PAC engine
  execution).
- Browser PAC interaction with system DNS.
- Streaming hook latency under load.
- macOS NE proxy startup / fail-open contract (Swift code in
  `packages/agent/platform/darwin/`).

The latter are owned by other test layers — Playwright for UI,
`packages/agent/test/` for NE proxy, manual smoke for cost dashboards.

## Maintenance discipline

- Every new admin endpoint that crosses a CP↔Hub or CP↔Service
  boundary SHOULD acquire a scenario in this harness. The CLAUDE.md
  binding `feedback_cache_mandatory_all_ingress` model
  (cross-ingress test on every cache change) applies here too.
- Add the scenario AND the catalog row in the same commit; CI does
  not yet enforce lockstep but the discipline is the same as
  `architecture-doc-triggers.md`.
- Surfaced product issues (the "Logged, not blocked on" table above)
  should be tracked in real issues / memories, not just inline
  `t.Logf` calls — at last review there are 5 outstanding follow-ups.
