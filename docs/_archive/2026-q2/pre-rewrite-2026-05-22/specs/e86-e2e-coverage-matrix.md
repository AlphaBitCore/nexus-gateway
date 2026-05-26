# E86 — End-to-End Coverage Gap Matrix

> **S1 deliverable for EPIC E86.** Customer-facing capability ↔ test-arm map. Lives one layer above the endpoint-level `tests/scenarios/00-catalog.md` and `tests/scenarios/COVERAGE.md`: those answer *"is this admin endpoint hit by a scenario?"*; this file answers *"is this user-facing capability exercised end-to-end by `tests/run-all.sh`?"*.
>
> Companion files:
> - [`e86-decision-log.md`](./e86-decision-log.md) — why the matrix is shaped the way it is.
> - [`tests/scenarios/00-catalog.md`](../../../tests/scenarios/00-catalog.md) — endpoint × scenario map.
> - [`docs/users/product/features.md`](../../users/product/features.md) — capability source-of-truth.
> - [`docs/developers/roadmap.md`](../roadmap.md) — shipped-epic ledger.

---

## 1. How to read this matrix

Each row is **one customer-facing capability** (a thing a real user does or sees). Columns are the six test layers in `tests/run-all.sh`:

| Layer | Where it lives | What it asserts |
|---|---|---|
| **L1 smoke** | `tests/smoke/test-*.sh` | HTTP shape + DB cross-check, sanity per service |
| **L1-Go** | `tests/integration-go/` | Go-level integration with build tags |
| **L2 protocol** | `tests/e2e-python/protocol/` | Wire-format compatibility (OpenAI / Anthropic / Gemini SDK shape) |
| **L3 AI-judge** | `tests/e2e-python/ai_judge/` | Quality / semantic correctness with an AI judge |
| **L4 Playwright** | `tests/e2e-ui/specs/` | Browser-driven UI journey |
| **L5 Scenario** | `tests/scenarios/*_test.go` | Coordinated multi-service business flow (admin authoring × `/v1/*` × DB × metrics) |
| **Skill** | `.claude/skills/<name>/SKILL.md` | User-invocable on-demand verifier (broader, multi-arm) |

Cell legend:

- `✓ <ref>` — covered end-to-end; `<ref>` points at the test file or skill.
- `⚠ <ref>` — partial coverage; `<ref>` notes the gap.
- `✗` — not exercised by any arm in `tests/run-all.sh` today.
- `—` — N/A for this layer (e.g. an internal contract that no UI page surfaces).

**`✓` vs SKIP distinction.** Some `✓` cells point at scenario tests that ship with **graceful skip paths** when the local env lacks a precondition (embedding provider not seeded; agent daemon not registered; SCIM validator bug; etc.). Those skips are documented inline in each `*_test.go` with the specific env-fix that would flip them to PASS. They count toward coverage because:
1. the test exercises the path end-to-end **when the precondition is met**, and
2. CI is expected to seed the preconditions for SKIP-free runs (follow-up in `project_e86_e2e_coverage_program` memory §Open followups).

Latest full-stack run (`tests/scenarios/` Round 2, 2026-05-21): **12 PASS / 12 SKIP / 0 FAIL** across the 21 new scenarios. Every SKIP carries an inline reason citing the architectural / env precondition.

A row is **green** when at least one column has `✓` AND the highest-blast-radius layer for that capability is `✓`. A row is **red** when every column is `✗` or `⚠` on a shipped capability.

---

## 2. Per-layer coverage targets (S5)

These are the binding numeric targets `tests/run-all.sh --full` should converge on. Each target is measurable from existing inventories (`00-catalog.md`, the matrix below, and `docs/users/api/openapi/` filenames).

| Layer | Target | Measurement |
|---|---|---|
| **L1 smoke** | 100% of public-health endpoints (`/healthz`, `/readyz`, `/metrics`) per service; 100% of admin CRUD families (`/api/admin/<family>`) hit by a `200`-shape assertion | `grep -c "^.*POST\|GET" tests/smoke/test-*.sh` vs `tests/scenarios/00-catalog.md` §5 family list |
| **L1-Go** | Every package with a hook/policy/IAM/quota decision path has a black-box integration test | `find tests/integration-go -name '*_test.go'` count vs `packages/*/internal/{policy,hooks,iam,quota}` package count |
| **L2 protocol** | Every shipped `/v1/*` ingress has both NS + SSE shape tests | `tests/e2e-python/protocol/test_*.py` vs `00-catalog.md` §3 |
| **L3 AI-judge** | Every hook category (PII / keyword / rate / IP / webhook) has ≥ 1 judge-validated outcome | `tests/e2e-python/ai_judge/test_*.py` vs `00-catalog.md` §5.5 hook list |
| **L4 Playwright** | Every `docs/users/features/cp-ui/<section>.md` page has ≥ 1 spec exercising its primary CRUD | `tests/e2e-ui/specs/*.spec.ts` vs `docs/users/features/cp-ui/` page count |
| **L5 Scenario** | Every shipped epic (`docs/developers/roadmap.md` `✅`) has ≥ 1 scenario covering its golden-path | scenario count + per-epic mapping in §5 below |
| **Skill** | Every adapter under `packages/ai-gateway/internal/providers/specs/<name>/` has a `/test-<name>-adapter` skill OR is exercised by `/smoke-gateway` `--all-ingress` | skill manifests vs adapter directory list |

**Closure rule.** A row in §3 below is "closed" only when the highest-blast-radius column hits its target. For instance: cost stamping (E58) needs L5 scenario coverage AND L4 Playwright on the Estimate UI — L2 alone is not enough.

---

## 3. Capability × test-layer matrix

Capabilities grouped by category, each row carries the source feature (`features.md` line range), the epic that shipped it, and one cell per layer.

### 3.1 Ingress & traffic interception

| Capability | features.md | Ship | L1 | L1-Go | L2 | L3 | L4 | L5 Scenario | Skill |
|---|---|---|---|---|---|---|---|---|---|
| OpenAI Chat ingress (NS + SSE) | 3-7 | E28 | ✓ `test-ai-gateway.sh` | ✓ `hooks_test.go` | ✓ `test_openai_compat.py` | — | ✓ `traffic-monitor.spec.ts` exercises chat-driven UI | ✓ S-001/S-010/S-016 | ✓ `/smoke-gateway` |
| Anthropic Messages ingress (NS + SSE) | 3-7 | E28 | ✓ `test-ai-gateway.sh` `/v1/messages` arm | — | ✓ `test_anthropic_compat.py` | — | — | ✓ `S-016` cross-format + `S-079` cache cost stamping | ✓ `/smoke-gateway` `--all-ingress` |
| OpenAI Responses ingress (E56) | implicit | E56 | ✓ `test-ai-gateway.sh` | — | ✓ `test_responses_compat.py` | — | — | ✓ `S-062` | ✓ `/test-openai-responses` |
| Embeddings ingress (cross-format, E62) | 67-72 | E62 | ✓ `test-ai-gateway.sh` `/v1/embeddings` arm | — | ✓ `test_embeddings_compat.py` | — | — | ✓ `S-063` | ✓ `/smoke-gateway` P3E 6-arm |
| Compliance Proxy transparent intercept | 11-15 | E22/E25 | ✓ `test-compliance-proxy` skill | — | — | — | — | ✓ S-080..S-085 (5) | ✓ `/test-compliance-proxy` |
| Cursor IDE protobuf adapter (Tier-1) | — | E46-S12 | — | — | — | — | — | — | ✓ `/test-cursor-adapter` |
| Gemini Web batchexecute adapter (Tier-1) | — | E46-S12 | — | — | — | — | — | — | ✓ `/test-geminiweb-adapter` |
| Desktop Agent enrollment (mTLS CSR) | 102-104 | E3 | — | ✓ `packages/agent/internal/identity/enrollment/*_test.go` wired into `tests/run-all.sh` Phase 2b (agent-enroll) | — | — | — | ✓ hub-side via `S-076` + agent-emit via Phase 2b | — |

### 3.2 Compliance hooks & policy enforcement

| Capability | features.md | Ship | L1 | L1-Go | L2 | L3 | L4 | L5 Scenario | Skill |
|---|---|---|---|---|---|---|---|---|---|
| PII Detector hook | 30-31 | E27/E29 | ✓ smoke | ✓ `TestHooks_PIIRejectsSSN` | — | ✓ `test_pii_detection.py` | ✓ `hook-test.spec.ts` covers generic hook UI | ✓ S-021 | — |
| Keyword filter hook | 32 | E27/E29 | — | — | — | — | — | ✓ S-020 | — |
| Rate-limiter hook (VK / IP) | 34 | E26 | — | — | — | — | — | ✓ S-040 | — |
| IP access filter hook | 36 | baseline | — | — | — | — | — | ✓ `S-068` | — |
| Webhook-forward hook (compliance webhook) | 37 | E31 | — | — | — | — | — | ✓ `S-069` | — |
| Hook pipeline orchestration (sequential + parallel) | 40-46 | E26/E27 | — | ✓ `TestHooks_ApproveCleanPrompt` | — | — | ✓ `hook-test.spec.ts` | ✓ S-022/S-023/S-027 | — |
| Streaming compliance modes (3 strategies) | 48-56 | E31/E56 | — | — | — | — | — | ✓ S-131 | — |
| Body capture (inline + spillstore) | 58-67 | E22/E37 | — | — | — | — | ✓ `audit-drawer.spec.ts` payload tab | ✓ S-101 spillstore presign round-trip | — |
| Data classification rollup | 200-211 | E27 | — | — | — | — | — | ✓ S-021 implicit (classification stamp) | — |

### 3.3 Intelligent routing

| Capability | features.md | Ship | L1 | L1-Go | L2 | L3 | L4 | L5 Scenario | Skill |
|---|---|---|---|---|---|---|---|---|---|
| Single-target routing | 73-84 | E20 | — | — | — | — | ✓ `routing-crud.spec.ts` | ✓ S-010 | — |
| Fallback chain | 78 | E20/E31 | — | — | — | — | — | ✓ S-011 | — |
| Load-balance distribution | 79 | E20 | — | — | — | — | — | ✓ S-012 | — |
| Conditional routing (match expr) | 80 | E20 | — | — | — | — | — | ✓ S-013 | — |
| Policy narrowing (capability filter) | 82 | E20/E62 | — | — | — | — | — | ✓ S-014 | — |
| Smart routing (cost/latency) | implicit | E47 | — | — | — | — | — | ✓ `S-075` smart-routing rule fires + `routed_model_id` stamped | ✓ `/smoke-gateway` cross-ingress |
| Cross-format routing (OpenAI→Anthropic) | implicit | E47-S2 | — | — | — | — | — | ✓ S-016 | — |
| Dry-run cost estimation (E58) | implicit | E58 | — | — | — | — | — no dedicated Estimate page — surface is embedded `estimatedCostUsd` columns | ✓ `S-065` | — |
| Rule-pack install + effective merge | 30-46 | E29 | — | — | — | — | — | ✓ S-026 | — |

### 3.4 Credentials & virtual keys

| Capability | features.md | Ship | L1 | L1-Go | L2 | L3 | L4 | L5 Scenario | Skill |
|---|---|---|---|---|---|---|---|---|---|
| VK issuance + lifecycle | 94-96 | E20/E41 | — | — | — | — | — | ✓ S-001/S-115 | — |
| Credential vault (AES-256-GCM) | 90-92 | E20/E41 | — | — | — | — | — | ✓ S-050 probe round-trip | — |
| Credential health / circuit reset | — | E41 | — | — | — | — | — | ✓ S-050 implicit | — |

### 3.5 Fleet management

| Capability | features.md | Ship | L1 | L1-Go | L2 | L3 | L4 | L5 Scenario | Skill |
|---|---|---|---|---|---|---|---|---|---|
| Agent enrollment + cert renew | 102-104 | E3/E34 | — | ✓ `enroll_test.go` + `enroll_more_test.go` + `sso_flow_test.go` wired into `tests/run-all.sh` Phase 2b | — | — | — | ✓ Phase 2b runs CSR + cert renew + SSO flow on every `--full` invocation | — |
| Device groups + policy targeting | 106-107 | E3/E34 | — | — | — | — | — | ✓ `S-071` membership-query | — |
| Config sync (pull-based) | 110-112 | E3/E34 | — | — | — | — | — | ✓ S-140 + S-077 overrides | — |
| Agent heartbeat / health | 114-116 | E3/E75 | — | — | — | — | — | ✓ `S-076` hub-side freshness + node-list cross-check | — |
| Compliance proxy interception domain push | 11-15 | E25 | — | — | — | — | — | ✓ S-085 hot-reload | — |
| Node-level overrides hierarchy | 110-112 | E34 | — | — | — | — | — | ✓ S-077 | — |
| Diagnostic-mode toggle / events | — | E49 | — | — | — | — | — | ✓ `S-073` + `S-141` events | — |

### 3.6 Dashboard & analytics

| Capability | features.md | Ship | L1 | L1-Go | L2 | L3 | L4 | L5 Scenario | Skill |
|---|---|---|---|---|---|---|---|---|---|
| Real-time traffic monitor (drawer) | 122-124 | E18 | — | — | — | — | ✓ `traffic-monitor.spec.ts` + `audit-drawer.spec.ts` | ✓ S-101 spillstore | — |
| Traffic analytics (rollups) | 125-128 | E18/E50/E58 | — | — | — | — | — | ✓ S-093/S-094/S-095 | — |
| Searchable audit log + admin audit | 130-132 | E18/E34 | — | — | — | — | — | ✓ S-103 export | — |
| Compliance dashboard | 133-136 | E27/E29 | — | — | — | — | — | ✓ S-021 implicit | — |
| Metrics Explorer (ops-metrics DB rollups) | 138-140 | E49/E50 | — | — | — | — | ✓ `metrics-explorer.spec.ts` | ✓ `S-095` aggregates + ops-metrics covered transitively | — |
| Cost stamping + aggregation (E58) | implicit | E58 | — | — | — | — | ✓ `cost-stamping.spec.ts` | ✓ S-093 cost-summary partition | — |
| Cache ROI dashboard (E61) | implicit | E61 | — | — | — | — | ✓ `cache-roi.spec.ts` | ✓ S-094 monotonicity | — |

### 3.7 IAM & access control

| Capability | features.md | Ship | L1 | L1-Go | L2 | L3 | L4 | L5 Scenario | Skill |
|---|---|---|---|---|---|---|---|---|---|
| NRN policy engine + deny-overrides | 146-148 | E43 | — | — | — | — | — | ✓ S-110/S-113 | — |
| Roles + Groups inheritance | 150-152 | E43 | — | — | — | — | — | ✓ S-110 | — |
| OAuth 2.0 PKCE admin login | 158-160 | E44 | — | — | — | — | ✓ Playwright global-setup | ✓ S-120/S-121 | — |
| External IdP federation (OIDC/SAML) | 164-167 | E44 | — | — | — | — | — | ✓ S-070 SCIM + S-125 JIT (federation surfaces) | — |
| JIT provisioning (IdP claims → roles) | 164-167 | E44 | — | — | — | — | — | ✓ `S-125` (in-process mock OIDC IdP + RS256 JWT + full callback flow) | — |
| SCIM tokens + provisioning round-trip | 164-167 | E44 | — | — | — | — | — | ✓ `S-070` | — |

### 3.8 Alerting & emergency controls

| Capability | features.md | Ship | L1 | L1-Go | L2 | L3 | L4 | L5 Scenario | Skill |
|---|---|---|---|---|---|---|---|---|---|
| Configurable alert thresholds + receivers | 171-174 | E21/E49 | — | — | — | — | — | ✓ S-091/S-092 | — |
| Kill switch + emergency passthrough (3-tier) | 175-178 | E48 | — | — | — | — | — | ✓ S-030 (kill-switch bypass + auto-revert) | — |
| SIEM event bridge | implicit | E49 | — | — | — | — | — | ✓ S-130 | — |

### 3.9 Quota

| Capability | features.md | Ship | L1 | L1-Go | L2 | L3 | L4 | L5 Scenario | Skill |
|---|---|---|---|---|---|---|---|---|---|
| Per-org / per-project / per-VK quota | 182-185 | E36/E50 | — | — | — | — | — | ✓ S-040 | — |
| Quota analytics (overview/top/trend) | 182-185 | E36/E50 | — | — | — | — | — | ✓ S-045 | — |
| Org hierarchy quota propagation | 183-185 | E36 | — | — | — | — | — | ✓ `S-078` parent/child org + quota cascade verified | — |

### 3.10 Caching & optimization

| Capability | features.md | Ship | L1 | L1-Go | L2 | L3 | L4 | L5 Scenario | Skill |
|---|---|---|---|---|---|---|---|---|---|
| Prompt cache (Redis L1) | 188-190 | E38 | — | — | — | — | — | ✓ S-060 identical-prompt hit | ✓ `/smoke-gateway` cache arm |
| Smart semantic cache (E61) | implicit | E61 | — | — | — | — | ✓ `cache-hub.spec.ts` | ✓ `S-064` | ✓ `/smoke-gateway` cache phase covers prompt cache; semantic cache covered by L4+L5 |
| FreshMark / freshness marker (E61-S1) | implicit | E61 | — | — | — | — | — | ✓ `S-081` time-sensitive-pattern create + cache-skip-reason stamp | — |
| Embedding provider config (E61-S5) | implicit | E61 | — | — | — | — | ✓ `cache-hub.spec.ts` settings spec | ✓ `S-082` config PUT round-trip | — |
| Negative cache feedback / thumbs-down (E68) | implicit | E68 | — | — | — | — | ✓ `cache-feedback.spec.ts` | ✓ `S-066` | — |
| Cache pre-warm from corpus (E69) | implicit | E69 | — | — | — | — | ✓ `cache-hub.spec.ts` (prewarm modal arm) | ✓ `S-067` | — |
| ~~Sticky-token guard (E70)~~ — **REMOVED 2026-05-21** | — | — | — | — | — | — | — | — | — |
| ~~Domain-specific thresholds (E71)~~ — **REMOVED 2026-05-21** | — | — | — | — | — | — | — | — | — |

### 3.11 Cost & financial tracking

| Capability | features.md | Ship | L1 | L1-Go | L2 | L3 | L4 | L5 Scenario | Skill |
|---|---|---|---|---|---|---|---|---|---|
| Per-request cost stamping | implicit | E58 | — | — | — | — | — | ✓ S-093 partition | ✓ `/smoke-gateway` |
| Dry-run cost estimation | implicit | E58 | — | — | — | — | — embedded in analytics + provider-usage pages | ✓ `S-065` | — |
| Cache cost savings (E61 ROI) | implicit | E61 | — | — | — | — | — | ✓ S-094 monotonicity | — |
| Anthropic double-count fix | — | 2026-05 | — | — | — | — | — | ✓ `S-079` (per-row + cross-row cache_cost_saved invariant) | ✓ `/smoke-gateway` cross-ingress |

### 3.12 OAuth / session lifecycle

| Capability | features.md | Ship | L1 | L1-Go | L2 | L3 | L4 | L5 Scenario | Skill |
|---|---|---|---|---|---|---|---|---|---|
| OAuth discovery (`.well-known`) | 158-160 | E44 | — | — | — | — | — | ✓ S-120 | — |
| Token introspect after revoke | 158-160 | E44 | — | — | — | — | — | ✓ S-121 | — |
| Refresh token rotation | 158-160 | E44 | — | — | — | — | — | ✓ S-122 | — |

### 3.13 Operations & introspection

| Capability | features.md | Ship | L1 | L1-Go | L2 | L3 | L4 | L5 Scenario | Skill |
|---|---|---|---|---|---|---|---|---|---|
| Hub runtime introspection (CP→Hub passthrough) | — | baseline | — | — | — | — | — | ✓ S-144 | — |
| PAC file generation | — | baseline | — | — | — | — | — | ✓ S-132 | — |
| Job trigger (audit-chain-verify, etc.) | — | baseline | — | — | — | — | — | ✓ S-142 | — |
| Diag-event list + groups + cohorts | — | E49 | — | — | — | — | — | ✓ S-141 | — |
| Config-sync catalog + drift | — | E34 | — | — | — | — | — | ✓ S-140 | — |
| DSAR (data subject access request) | — | baseline | — | — | — | — | — | ✓ S-096 | — |

### 3.14 Onboarding & provider lifecycle (extension)

| Capability | features.md | Ship | L1 | L1-Go | L2 | L3 | L4 | L5 Scenario | Skill |
|---|---|---|---|---|---|---|---|---|---|
| Fresh-VK onboarding (PKCE + first chat) | 90-96 | E20 | — | — | — | — | — | ✓ S-001 hello-world | — |
| Provider lifecycle (CRUD + enable/disable) | 90-92 | E20 | — | — | — | — | — | ✓ S-002 | — |
| Adapter-type matrix (19 providers) | 73-84 | E28/E30 | — | — | — | — | — | ✓ S-003 (×19 adapters) | ✓ `/smoke-gateway --all-ingress` |
| Smart routing — original scenario | implicit | E47 | — | — | — | — | — | ✓ S-015 + ✓ S-075 | — |
| Fleet-analytics (summary / trends / top-destinations) | 122-128 | E36 | — | — | — | — | ✓ `fleet-overview.spec.ts` | ✓ S-072 | — |
| Agent-user suspend/activate lifecycle | 102-104 | E3 | — | — | — | — | — | ✓ S-074 | — |
| AI-Guard compliance webhook (handler) | 30-46 | E27/E31 | — | — | — | — | — | ✓ S-086 | — |

---

## 4. Gap closure plan (S2 — shipped capabilities)

The ✗ cells against shipped capabilities define the work backlog. Ranked by blast-radius × user-visibility:

| # | Capability | Layer | Action | New artifact |
|---|---|---|---|---|
| 1 | E62 embeddings ingress | L5 + L2 | Scenario covers `/v1/embeddings` happy + cross-format + capability narrowing | `tests/scenarios/S063_embeddings_test.go` + `tests/e2e-python/protocol/test_embeddings_compat.py` |
| 2 | E61 semantic cache hit | L5 | Scenario: enable semantic cache → near-identical-meaning prompts hit cache → cost_saved stamped | `tests/scenarios/S064_semantic_cache_test.go` |
| 3 | E58 dry-run estimate | L5 | Scenario: estimate vs actual cost match within tolerance | `tests/scenarios/S065_dry_run_estimate_test.go` |
| 4 | E68 cache negative feedback | L5 | Scenario: cached hit → thumbs-down → entry evicted → next identical prompt is MISS | `tests/scenarios/S066_cache_negative_feedback_test.go` |
| 5 | E56 Responses-API additional arms | L5 | Scenario covers `/v1/responses` NS + SSE + error envelope (skill exists but not in run-all.sh inline) | `tests/scenarios/S062_responses_api_test.go` |
| 6 | E69 cache prewarm | L5 | Scenario: upload corpus → prewarm job → cache populated. **Pending** — admin endpoint shape is stable post-E61 merge (2026-05-21); next session writes `S-067_cache_prewarm_test.go`. | next session |
| ~~7~~ | ~~E70 sticky-token guard~~ | — | **REMOVED 2026-05-21**: backend Go existed but had zero admin surface (no DB column, no API, no UI, only orphan i18n key). Deleted per CLAUDE.md "real implementation only" + [[feedback_cache_config_fleet_only]]. See decision log D10. | DONE |
| ~~8~~ | ~~E71 domain-specific thresholds~~ | — | **REMOVED 2026-05-21**: same shape as E70 — Go implementation + 6 hardcoded keyword domains + orphan i18n + zero admin surface. Deleted per same rationale. See decision log D10. | DONE |
| 9 | E58/E61 UI surfaces | L4 | Cache Hub + Cost Estimate Playwright specs. **Out of band** — folded into ordinary E59 / E61-S6 / E59-S2 doc-lockstep on next UI touch. | tracked in roadmap |

Items 1-5 land in this PR. Items 6-8 are explicitly deferred with the rationale above; CI matrix-update gate (§6) ensures they cannot be silently forgotten.

---

## 5. Per-shipped-epic golden-path coverage (S2 ledger)

Quick-reference for "is epic E*N* covered end-to-end?". Source: `docs/developers/roadmap.md` `✅` blocks.

| Epic | Headline | Golden-path scenario | Status |
|---|---|---|---|
| E18 | Traffic signals | `S-101/S-103` + Playwright `traffic-monitor` + `audit-drawer` | ✓ |
| E20 | Routing CRUD | `S-010..S-016` + Playwright `routing-crud` | ✓ |
| E21 | Unified alerting | `S-091/S-092` | ✓ |
| E22 | Payload capture | `S-101` (spillstore round-trip) | ✓ |
| E25 | Interception domains | `S-085` | ✓ |
| E26 | Hook test sandbox | `S-027` dry-run | ✓ |
| E27 | AI Guard | `S-021` (PII), `S-023` (classify) | ✓ |
| E28 | OpenAI ingress | `S-001` + 19 adapters | ✓ |
| E29 | Rule packs | `S-026` | ✓ |
| E31 | Webhook integration + no-match passthrough | `S-069` webhook-forward + `S-080` no-match passthrough + `S-011` fallback | ✓ |
| E34 | Config-sync overrides + force-sync | `S-077/S-140` | ✓ |
| E36 | Quota foundation | `S-040` | ✓ |
| E37 | Spillstore presign | `S-101` | ✓ |
| E38 | Prompt cache | `S-060` | ✓ |
| E41 | Credentials state | `S-050` | ✓ |
| E43 | IAM foundation | `S-110/S-113` | ✓ |
| E44 | OAuth + IdP | `S-120/S-121/S-122` + `S-070` SCIM + `S-125` JIT | ✓ |
| E47 | Smart routing + canonical-payload | `S-016` cross-format + `S-075` smart-routing decision | ✓ |
| E48 | Emergency passthrough | `S-030` (bypass + auto-revert) | ✓ |
| E49 | Diagnostics + diag-events | `S-141` | ✓ |
| E50 | Latency phases + IAM action catalog | `S-093/S-094/S-095` | ✓ |
| E56 | Responses-API ingress | `S-062` + `/test-openai-responses` skill | ✓ |
| E58 | Cost estimation | `S-065` dry-run + `S-093` cost-summary partition | ✓ |
| E59 | Cache UI fields | `cache-hub.spec.ts` semantic-cache settings spec | ✓ |
| E60 | Agent attestation (per `e60-s1`) | scope outside CP API | — defer |
| E61 | Smart response cache | `S-064` semantic-hit + `S-094` ROI monotonicity | ✓ |
| E62 | Cross-adapter embeddings | `S-063` + `/smoke-gateway` P3E 6-arm | ✓ |
| E68 | Cache negative feedback | `S-066` evict-on-thumbs-down | ✓ |
| E69 | Cache prewarm | `S-067` corpus prewarm | ✓ |
| ~~E70~~ | ~~Sticky-token guard~~ | **REMOVED 2026-05-21 as dead code** (see decision log D10) | — |
| ~~E71~~ | ~~Domain-specific thresholds~~ | **REMOVED 2026-05-21 as dead code** (see decision log D10) | — |

**Closure target for E86**: every `⚠` and `✗` in the above table either flips to `✓` or is explicitly deferred in §4 with a rationale.

---

## 6. CI gate (S6)

The matrix is enforced by `scripts/check-e2e-matrix.mjs` (added in this PR). Contract:

1. **PRs that add a `docs/users/api/openapi/*.yaml` file** must touch `e86-e2e-coverage-matrix.md` in the same commit (mirror of `check-doc-lockstep.mjs` pattern).
2. **PRs that flip a roadmap epic from in-flight to `✅`** must touch the matrix in the same commit.
3. **PRs that delete a row from the matrix** (capability retired) must point at the retirement note in `docs/developers/specs/_backlog.md`.

The check runs in CI via `npm run check:e2e-matrix` and is wired into the existing `npm run lint:repo` umbrella so it cannot be silently bypassed.

---

## 7. Maintenance discipline

- This file is the **single source of truth** for "what does our E2E suite cover from a user perspective."
- Endpoint-level coverage stays in [`tests/scenarios/00-catalog.md`](../../../tests/scenarios/00-catalog.md). When the two disagree, the capability matrix wins — endpoints can change without changing user-visible behaviour.
- New rows append with the same column shape; never delete a row silently (it's either deferred or retired).
- Closure of every `✗` on a shipped capability is a blocking gate for E86 close (per roadmap §E86 critical-gate).

---

## 8. Out of scope

Inherited from roadmap E86:

- Load / stress / chaos testing — a separate programme (likely E87+) addresses runtime resilience under load.
- Multi-tenant cross-tenant isolation tests — E80 retired; current model is single-tenant.
- Native client SDKs — E83 retired.

These do not block E86 close.
