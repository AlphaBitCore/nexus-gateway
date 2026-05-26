# Nexus Gateway — Epic Roadmap

> **Single living tracker of all major Nexus epics — canonical source.** Read this file FIRST when asking "what's the status of E<n>?", "what big features haven't been done yet?", or "what's queued for after E61?". Each entry links to its requirements / SDD / OpenAPI / memory. Detailed per-epic blocks live in the sections below the headline table. The doc replaces and absorbs the former `docs/_archive/2026-q2/programs/epic-status.md`.

## Production state (2026-05-19) — READ THIS FIRST

**Nexus Gateway is in production. The architecture is shipped, code paths are wired end-to-end, and prod Control Plane + AI Gateway + Compliance Proxy + Hub + 3-platform Agent are all serving real traffic today.** (Prod hostnames intentionally elided from this public-readable doc; operators see them via `tests/.env.prod` / `prod-login` skill.)

This roadmap is **NOT a "what's left to build" list**. It's a **"what to extend / verify / productize / certify"** list. Counting the 26 headline entries below and concluding "Nexus is a half-baked product" is wrong — the architecture is **complete**, the gateway **works in prod**, and customer traffic is flowing through it as of this commit. Almost every epic below is one of: enhancement of an already-shipped system, verification of already-coded surfaces, test/quality debt cleanup, or productization (wiki / website / SDK / compliance certs).

### What's production-validated and serving real traffic today

- **AI Gateway** — **5 providers in prod** (OpenAI / Anthropic / Gemini / Moonshot / DeepSeek) × **35 models**. **4 ingress shapes shipped** (OpenAI `/v1/chat/completions`, OpenAI `/v1/responses`, Anthropic `/v1/messages`, Gemini `:generateContent`). Full canonical-bus + cross-format routing + prompt-cache integration + per-VK quota + audit pipeline. `/smoke-gateway --all-ingress` baseline green.
- **Compliance Proxy** — MITM TLS bump on `:3128` with admin-trusted CA. Tier-1 adapters verified end-to-end for `chatgpt.com`, `claude.ai`, `gemini.google.com`, Cursor IDE + the 5 AI-Gateway-deployed API hosts. Hook pipeline running per Hub-pushed `HookConfig`. Audit drain via mTLS + MQ fallback. `/test-compliance-proxy` baseline green.
- **Agent (macOS / Linux / Windows)** — code complete + installable on all three platforms. macOS metadata-only via `NETransparentProxyProvider` (content-aware hooks **deliberately blocked** by NE limitations; E74 fixes); Linux `pf` + Windows WinDivert intercept fully wired with content-aware hooks active.
- **Nexus Hub** — Thing Registry, Device Shadow (Cat A / B / C config taxonomy), agent CA + Ed25519 attestation, audit chain pipeline with body capture + spillstore, scheduled jobs (retention purge, drift checker, agent-cert rotation), Prometheus metrics rollup.
- **Control Plane (admin API)** — full IAM (resource catalog, NRN permissions, super-admin policy, fixtures), virtual-key CRUD with budgets + quotas, provider/model catalog, routing rules (priority + weighting + LLM-dispatch smart routing), analytics (cost / savings / latency / cache-ROI), hook-config, kill switch, SIEM bridge, IdP / SSO (OIDC + SAML JIT provisioning), JWT verifier (multi-issuer).
- **Control Plane UI** — admin dashboard, theme system, i18n (EN / ZH / ES), design-token framework (light / dark themes), traffic audit drawer with NormalizedPayload viewer, observability surfaces.
- **Operational tooling** — `/prod-login`, `/prod-deploy`, `/prod-debug`, `/test-all` (covering ~75 business flows via L1 smoke + L2 protocol + L3 AI-judge + L4 Playwright UI), `/build-agent`, per-IDE / per-web adapter test skills.

### What this roadmap actually is — by bucket

| Bucket | Epics | Nature |
|---|---|---|
| **Shipped this cycle (in optimization)** | E61, E62, E68, E69, E70, E71 | Code merged to `main` (E61 via PR #33 / commit `05f147735`, merged 2026-05-21); ongoing optimization-phase fixes land on `develop` |
| **Enhancement of shipped systems** | E74, E78, E79, E81 | Extending an existing capability — not first-build |
| **Verification of already-coded surfaces** | E72, E73, E75 | Code exists; needs end-to-end matrix coverage |
| **Quality / coverage debt** | E85, E86 | Tighten the test net on shipped code |
| **Productization** | E76 | Public-facing Wiki for OSS evaluators |
| **Operational maturation** | E82 | Observability polish on top of existing Prometheus + alert baseline |

Retired / deferred epic numbers (E63-E67 multimodal family, E77 marketing website, E80 SaaS, E83 SDK, E84 compliance certs) live in `docs/developers/specs/_backlog.md` — they are NOT reused. Open this index when you see a missing epic number and want to know why.

### What "production-validated" does NOT yet mean

To be precise so the production claim is not mis-read:

- It does NOT mean every adapter in the codebase has been exercised in prod (E72 covers the 14 unexercised spec adapters; E73 covers ~40 unverified Tier-1 adapters — see E73 motivation for why this is load-bearing for the compliance-gateway value prop).
- It does NOT mean the system is multi-tenant SaaS-ready — Nexus is single-tenant on-prem / OSS-deployable by design.
- It does NOT mean high-availability is set up (E81 — today single-instance baseline; HA is enhancement).
- It does NOT mean every endpoint typology is supported — chat + responses-api + embeddings are shipped; audio / image / video are deferred in `_backlog.md` pending customer signal.

These caveats are **scope limits**, not architecture gaps. The architecture handles all of them in principle; the work is filling in the matrix.

---

## Status enum

Every epic and every story carries one of these statuses. Keep the enum closed — do not invent new values.

| Status | Meaning |
|---|---|
| `Shipped` | Code merged, smoke green, in production (or production-equivalent). |
| `In-progress` | Code work underway; some stories may already be merged. |
| `Planned` | Requirements + SDDs drafted; code work not yet started. Eligible to start. |
| `Draft` | Requirements + SDDs drafted but not yet approved. Decisions pending. |
| `Deferred` | Documented but explicitly de-scoped from the current quarter / cycle. Will be revisited. |
| `Cancelled` | Documented but explicitly killed. Kept in the index for archaeology. |

**Frontmatter binding:** every `docs/developers/specs/eN-*.md` and `docs/developers/specs/eN-sN-*.md` carries `> Status: <one-of-above>` in its frontmatter. AI sessions can grep `^> Status: ` across `docs/developers/specs/` and `docs/developers/specs/` to filter by status. Keep the requirements doc's status as the **single source of truth** for the epic; SDDs follow.

## How to query (for AI sessions + new contributors)

```
"What big features are not done?"            → Read this file's Headline list + jump to per-epic block
"Why is E<n> missing from the headline list?" → Read docs/developers/specs/_backlog.md (retired / deferred)
"Which epics are deferred / cancelled?"      → grep -l '^> Status: \(Deferred\|Cancelled\)' docs/developers/specs/*.md
"What's the next epic to start?"             → Headline list — first 🟢 Planned row, lowest E-number
"What's blocking E<n>?"                       → Per-epic block "Blocked by" if listed
"Where do I start implementing E<n>?"         → Per-epic block "Reading order" + lowest-number story with no blocker
```

## Headline list

> **Active focus (2026-05-21):** E61 merged to `main` via PR #33 (commit `05f147735`) on 2026-05-21. Ongoing optimization-phase fixes continue on `develop`. The next-to-start epic is up to you to pick from the 🟢 Planned rows below — no implicit ordering.

| Epic | Title | Phase | Critical gate before close |
|---|---|---|---|
| **E61** | Smart Response Cache (extract + semantic + freshness) | ✅ Shipped 2026-05-21 (PR #33 / commit `05f147735`, merged to `main`) | `/smoke-gateway --all-ingress` ✓ |
| **E62** | Cross-adapter embeddings + endpoint typology foundation | ✅ Shipped — merged into `feature/E61` via `631eac01d` | `/smoke-gateway --all-ingress` (P3E phase) ✓ |
| **E68** | Negative-feedback channel for cache poisoning | ✅ Shipped — `d93843acb` + drawer thumbs-down `bae7e1473` | — |
| **E69** | Pre-warm L2 from FAQ / Q&A corpus | ✅ Shipped — `d93843acb` | — |
| **E70** | Sticky-token exact-match guard | ✅ Shipped — `d93843acb` | — |
| **E71** | Domain-specific semantic thresholds | ✅ Shipped — `d93843acb` | — |
| **E72** | AI Gateway adapter verification (14 spec adapters not exercised in prod) | 🟢 Planned — ready to scope | `/smoke-gateway --all-ingress` per provider added |
| **E73** | Compliance Proxy + Agent Tier-1 adapter verification (~40 adapters) | 🟢 Planned — load-bearing for compliance-gateway value prop | `/test-compliance-proxy` per adapter + per-IDE / per-web synthetic tests |
| **E74** | macOS pf-intercept replacement of NETransparentProxyProvider (content-aware hooks parity) | 🟢 Planned — surfaced by E62 §8.7.2 review | content-aware hooks active on macOS for chat + embeddings + future endpoints |
| **E75** | Three-platform Agent end-to-end verification (macOS NE + Linux pf + Windows WinDivert; dev-complete, test-incomplete) | 🟢 Planned | every platform passes install → intercept → hook → audit → uninstall on clean-VM synthetic test |
| **E76** | GitHub Wiki content — public-facing repo wiki (getting started, architecture overview, deployment guide, FAQ, contribution guide) | ✅ Shipped 2026-05-21 (PR #42, merge commit `72524a26`) — 143 publishable pages across 19 groups under `docs/_wiki/` | wiki published with all sections live ✓ |
| **E78** | Self-hosted local inference for AI Guard + AI Routing + Semantic Embedding (one OpenAI-compat server, three downstream consumers) | 🟢 Planned — design locked (memory anchor `project_local_inference_server_direction`); deployment + model choice + cutover not started | local server serves all 3 consumers; external-API fallback honoured; smoke green |
| **E79** | Traffic event storage migration (PostgreSQL → columnar store; Clickhouse / similar candidate) | 🟢 Planned — current writes scale poorly past prod traffic volume | new store ingests `traffic_event_*` with ≤10s lag; admin dashboards query new store; legacy Postgres tables retired |
| **E81** | High-availability + multi-instance clustering | 🟢 Planned — current single-instance is uptime risk; should land before E78 to avoid concentrating risk on one host | rolling restart of any single node causes 0 request loss; documented RTO/RPO SLOs |
| **E82** | Observability stack completion (Grafana dashboards + Alertmanager + OTel trace search + log aggregation) | 🟢 Planned — Prometheus metrics + alert rules baseline exist but no dashboards / no central log aggregation | 1-click on-call dashboard load; alert pager-routes wired; trace search across all 4 services |
| **E85** | Unit-test coverage 95% — close the pre-existing under-coverage Go packages currently sitting in `scripts/.coverage-allowlist` | 🟢 In-progress — `feature/E85` worktree (Phase 1 types-only `doc_test.go` shipped; Phase 2 configdispatch/replay/breakglass tests closing; Phase 3 wiring per-function ledger drafted) | allowlist contains only category A-F entries with rationale |
| **E86** | End-to-end test coverage uplift — define gap matrix vs `tests/run-all.sh` (~75 flows today), close priority gaps | 🟡 In flight — S1/S2 (5 priority scenarios)/S4/S5/S6 landed on `feature/E86`; S3 (per-epic close gate) inherits from S6 lockstep | gap matrix published; CI gate via doc-lockstep entry `e2e-coverage-matrix` |
| **E87** | SAML SSO support — IdP type enum stub already shipped (`IdPType.saml` + `SAMLAdminConfig` / `SAMLClaimConfig` structs); runtime AuthnRequest emitter, signed-assertion verifier, and JIT-provisioning callback handler still pending | 🟢 Planned — IdP/Auth queue | SAML AuthnRequest end-to-end against at least one external IdP (Okta or Azure AD); signed-assertion verification passes; JIT user provisioning closes the loop |

Phase emoji legend: ✅ Shipped · 🟢 Planned · ❌ Cancelled (see `_backlog.md`)

---

## ✅ Recently completed (shipped to `main` 2026-05-21 via PR #33 / commit `05f147735`)

### E61 — Smart Response Cache

**Status**: ✅ Shipped 2026-05-21 (PR #33 / commit `05f147735`, merged to `main`). All 12 stories (S1, S2, S2-T1a IAM rider, S2b, S3, S4, S5, S6, S6b, S6c, S6d, S7) closed. Optimization-phase fixes continue on `develop`. Memory anchor `project_e61_smart_response_cache` carries the high-level decisions.

**Key shipping commits**:

- `578275157` — Foundation: schema migration, audit constants, IAM, configkey, freshness detector, inputstaging primitive, embedding adapter.
- `0e908d0d3` — Valkey 8.x + L2 semantic cache + Hub reindex job + L1 ConfigStore + admin API.
- `90f3ead01` — L2 read path + `classifyCachePreLookup` + ai-gateway wiring + per-route budget.
- `4e7847abf` — Cache Settings UI consolidation + Cache Embedding page + InputStagingSelector + Traffic Audit hook.
- `9af19a203` — `smoke-e61` + `smoke-gateway --embedding` phase.
- `c902f4e07` — Single-tenant revert (SemanticCacheConfig multi-tenant column dropped — E80 SaaS direction retired).
- `f6203b4d8` / `57e8a0d71` — Optimization-phase fixes (provider picker by Model.type; snake_case column in routing SQL).

**Architecture decisions retained (one-line each)**:

- Two-layer split — L1 singleton `semantic_cache_config` mirror of `ai_guard_config`; L2 per-route policy on routing rule.
- Fleet-wide single Redis Vector index, blue/green versioned naming (`v1`→`v2`→`v3`), atomic CREATE-SWAP-DROP.
- Fingerprint tag on every L2 entry — defence-in-depth against ConfigCache lag.
- Embedding singleflight with independent context + 100 ms hard timeout + circuit breaker (closed/open/half_open).
- Per-route embedding-cost daily ceiling (runaway-cost guard).
- 12 audit skip reasons covering the full failure-mode catalog.
- Shared `inputstaging` primitive — E61 first consumer; routing + ai-guard adopted later via `cd8437484`.
- Valkey 8.x + valkey-search (BSD3) — chosen over redis-stack (SSPL) for OSS-readiness.

**Reading order if you need to debug or extend**:

1. Memory: `project_e61_smart_response_cache` (auto-loaded).
2. Requirements: `docs/developers/specs/e61/e61-smart-response-cache.md`.
3. Architecture: `docs/developers/architecture/services/ai-gateway/response-cache-architecture.md`.
4. SDDs: `docs/developers/specs/e61/e61-s*.md`.
5. OpenAPI: `docs/users/api/openapi/e61-*.yaml`.

---

### E62 — Cross-adapter embeddings + endpoint typology foundation

**Status**: ✅ Shipped. Merged into `feature/E61` via `631eac01d`. All 6 stories closed. Deferred items D-1 through D-6 also closed in follow-up commits (see below); D-7 promoted to standalone epic E74; the multimodal-family Deferred items D-8 through D-11 were retired with their parent epics into `_backlog.md`.

**Key shipping commits**:

- `7b5bf00f9` — Main implementation: typology framework, canonical bridge, capability matrix, extended SchemaCodec, endpoint+modality hook framework, smoke phase fan-out, three-traffic-path consistency.
- `ffde52d45` — Closes D-1 (GLM embeddings codec) + D-2 (Voyage AI + Bedrock embedding adapters).
- `d93843acb` — Closes D-3 (routing default-extensions UI), D-4 (routing-fallback admin visibility), D-6 (`Model.capabilityJson` admin UI) + capability columns.

**Architecture decisions retained (one-line each)**:

- Per-endpoint canonical: OpenAI shape for chat + embed.
- `SchemaCodec` widening uses structured `EncodeResult` / `DecodeResult` (headers + URLOverride + artifacts). The single-time breaking change is paid; the framework now accommodates future endpoint types if reactivated from `_backlog.md`.
- Routing pre-filter drops single targets silently; HTTP 400 fires only when full candidate list exhausted; error envelope carries `available_capabilities` for self-debug.
- `capabilityJson` pre-parsed snapshot on routing hot path (atomic.Pointer swap, no per-request JSONB parse).
- `BillableUnits` cost abstraction avoids switch-sprawl in `proxy.go`.
- Hook Class A (content-scanning, per modality) / Class B (metadata, modality-agnostic) split at Pipeline build time, NOT decide time.
- Three-source consistency invariant: AI Gateway + CP + Agent produce byte-identical `NormalizedPayload` for the same upstream response; fixture test enforces.
- Embedding inputs ride the existing chat retention pipeline — no new privacy surface.

**Deferred-item status (final)**:

- D-1 GLM `/api/paas/v4/embeddings` codec — ✅ shipped `ffde52d45`.
- D-2 Voyage AI + Bedrock embedding adapters — ✅ shipped `ffde52d45`.
- D-3 Routing default-extensions UI editor — ✅ shipped `d93843acb`.
- D-4 Routing-fallback admin visibility — ✅ shipped `d93843acb`.
- D-5 Streaming embeddings — backlog (no provider supports it today).
- D-6 `Model.capabilityJson` admin UI — ✅ shipped `d93843acb`.
- D-7 macOS pf-intercept replacement — promoted to **E74** (active Planned epic).
- D-8 `AsyncAdapter` interface signature, D-9 cross-path drift detector — retired into `_backlog.md` along with E65.
- D-10 Per-modality content hooks — retired with E67.
- D-11 Image / audio / video classifier rules — retired with E63 / E64 / E66.

---

### E68-E71 — Cache follow-ups (poison / pre-warm / sticky-token / domain-thresholds)

**Status**: ✅ Shipped. All four queued cache follow-ups closed in `d93843acb` ("close E68 poison + E69 prewarm + E70 sticky + E71 domain"), with the UI surface landed in `d4afc942f` ("close Cache Policy tab + Capabilities + Drawer thumbs-down + Prewarm + Sticky/Domain") and the negative-feedback drawer test added in `bae7e1473`. Stickier capabilities documentation lives in the E61 SDDs.

---

## 🟢 Planned (Verification-class epics)

### E72 — AI Gateway adapter verification

**Status**: Planned. No code work yet; requirements + SDD to be drafted. The scope is **verifying the spec adapters that exist in code but have never run real traffic through prod** — they may or may not actually work against current upstream APIs.

**Motivation** (from prod survey 2026-05-19, commit `28701b67` follow-up):

Prod CP today has 5 Providers configured with 35 models across 5 adapter types: `openai`, `anthropic`, `gemini`, `moonshot` (via `specs/compat/moonshot`), `deepseek` (via `specs/compat/deepseek`). Those adapter packages are **production-validated** — they serve real traffic, smoke tests pass, traffic_event rows write correctly, cost stamping is accurate.

**14 spec adapter packages exist in code but are NOT exercised by any prod Provider — verification is required before declaring them production-ready.**

**Adapter packages to verify** (split by category):

| Category | Spec package | Wire shape | Notable models | Known gaps |
|---|---|---|---|---|
| **Top-level specs** | `specs/azure` | Azure OpenAI | All OpenAI models via Azure deployments | No prod Provider; `?api-version=` URL template, `api-key` header auth |
| | `specs/bedrock` | AWS Bedrock (Anthropic + Cohere via Bedrock + Titan) | claude-haiku/sonnet/opus via Bedrock; titan-embed; cohere-embed-via-bedrock | SigV4 auth not exercised; multipart-style streaming differs from native |
| | `specs/cohere` | Cohere `/v1/chat`, `/v1/embed`, `/v1/rerank` | command-r-plus; embed-multilingual-v3 | E62-S3 introduces embed codec; chat codec untested in prod |
| | `specs/glm` | ZhipuAI GLM (`/api/paas/v4/*`) | glm-4, glm-4-flash, glm-4-air | Embedding ingress route open (`/api/paas/v4/embeddings`) but no codec (E62 D-1 deferred) |
| | `specs/minimax` | MiniMax (Hailuo) | abab6.5, abab6.5s | Streaming SSE format differs from OpenAI's |
| | `specs/replicate` | Replicate Predictions API | Many community models via Replicate | Sync-only adapter; async-job lifecycle deferred (E65 retired into `_backlog.md`) — verify the sync path only |
| | `specs/vertex` | Google Vertex AI | gemini-2.5-pro-via-vertex, claude-via-vertex | OAuth2 service-account auth; region routing |
| **OpenAI-compat siblings** (`specs/compat/*`) | `compat/fireworks` | Fireworks AI | Llama, Mixtral, Qwen | OpenAI-compat shape; verify per-model strip rules don't trigger |
| | `compat/groq` | Groq Cloud | Llama-3.x, Mixtral | OpenAI-compat; check rate-limit headers + retry semantics |
| | `compat/huggingface` | HF Inference Endpoints (TGI) | Any HF-served model | OpenAI-compat only when endpoint is TGI; non-TGI is generic-http fallback |
| | `compat/mistral` | Mistral La Plateforme | mistral-large, codestral | OpenAI-compat; `safe_prompt` field unique to Mistral |
| | `compat/perplexity` | Perplexity API | sonar, sonar-reasoning | Citations field unique to Perplexity; check passthrough |
| | `compat/together` | Together AI | Many community models | OpenAI-compat; check `repetition_penalty` extension |
| | `compat/xai` | xAI (Grok) | grok-2, grok-beta | OpenAI-compat; `Live Search` field is unique |

**Verification scope per adapter** (per-spec story template):

1. Seed Provider + Model rows in dev seed.
2. Acquire upstream credential (test account, prod-quality VK).
3. Smoke test via `/smoke-gateway`: non-stream, SSE stream, 2-turn cache, dry-run.
4. Cross-format routing test: route OpenAI ingress → this adapter; route this adapter as ingress → OpenAI target (where wire format supports).
5. traffic_event cross-check: `endpoint_type`, tokens, cost, cache_status all correct.
6. Per-model wire quirks documented with empirical 400 citations (Rule 7).
7. `/adapter-conformance-check` skill passes.
8. Add seed Provider to prod recommendation list (if customer demand exists).

**Per-adapter story breakdown** (14 stories — each adapter is one story):

- [ ] **S1** — `specs/azure` verification.
- [ ] **S2** — `specs/bedrock` verification (claude-via-bedrock + cohere-via-bedrock + titan-embed).
- [ ] **S3** — `specs/cohere` verification (chat; embed is covered by E62).
- [ ] **S4** — `specs/glm` chat verification (`/api/paas/v4/chat/completions`).
- [ ] **S5** — `specs/minimax` verification.
- [ ] **S6** — `specs/replicate` sync verification (async support deferred; E65 retired).
- [ ] **S7** — `specs/vertex` verification (gemini-via-vertex + claude-via-vertex).
- [ ] **S8** — `compat/fireworks` verification.
- [ ] **S9** — `compat/groq` verification.
- [ ] **S10** — `compat/huggingface` (TGI mode) verification.
- [ ] **S11** — `compat/mistral` verification.
- [ ] **S12** — `compat/perplexity` verification (incl. citations passthrough).
- [ ] **S13** — `compat/together` verification.
- [ ] **S14** — `compat/xai` (Grok) verification.

**Critical gate before closing each story**: `/smoke-gateway --models <new-models>` passes; `/adapter-conformance-check` passes; one prod-equivalent VK exercised end-to-end with traffic_event verified.

**Implementation order**: Customer-demand driven. The OpenAI-compat siblings (S8-S14) are lower-effort (no codec, mostly seed work). The bespoke-codec adapters (S1-S7) are heavier (each needs per-model strip rules + maybe streaming session).

**Out of scope (E72)**: implementing adapter codecs for endpoints not yet in canonical (image / audio / video — retired into `_backlog.md` pending customer signal; reactivation would draft per-modality SDDs first).

---

### E73 — Compliance Proxy + Agent Tier-1 adapter verification

**Status**: Planned. No code work yet; requirements + SDD to be drafted.

**Motivation**: The Compliance Proxy and Agent paths share the `packages/shared/traffic/adapters/` Tier-1 adapter framework. **44 adapters** are registered in code across three categories (`api/`, `web/`, `ide/`), but only a handful have been verified end-to-end against real upstream traffic.

**Verified set (already exercised against real upstream)**:

| Category | Adapters | Verification source |
|---|---|---|
| `api/` (5) | openai, anthropic, gemini, moonshot, deepseek | AI Gateway prod Providers (E72 verifies these continuously); CP/Agent inherit Tier-1 from same code |
| `web/` (3) | chatgptweb, claudeweb, geminiweb | `/test-compliance-proxy` skill + manual prod CP captures |
| `ide/` (1) | cursor | `/test-cursor-adapter` skill exercises end-to-end |

**Unverified set (40 adapters)**:

| Category | Adapter | Wire shape | Verification target |
|---|---|---|---|
| `api/` (15) | azure | Azure OpenAI HTTPS | Tied to E72-S1 |
| | bedrock | AWS Bedrock HTTPS | Tied to E72-S2 |
| | cohere | Cohere HTTPS | Tied to E72-S3 |
| | fireworks | Fireworks HTTPS | Tied to E72-S8 |
| | glm | GLM HTTPS | Tied to E72-S4 |
| | groq | Groq HTTPS | Tied to E72-S9 |
| | huggingface | HF Inference HTTPS | Tied to E72-S10 |
| | minimax | MiniMax HTTPS | Tied to E72-S5 |
| | mistral | Mistral HTTPS | Tied to E72-S11 |
| | perplexity | Perplexity HTTPS | Tied to E72-S12 |
| | replicate | Replicate HTTPS | Tied to E72-S6 |
| | together | Together HTTPS | Tied to E72-S13 |
| | vertex | Vertex AI HTTPS | Tied to E72-S7 |
| | xai | xAI HTTPS | Tied to E72-S14 |
| `web/` (19) | anthropicconsoleweb | console.anthropic.com web | new SDD |
| | boltweb | bolt.new web | new SDD |
| | characterweb | character.ai web | new SDD |
| | chatglmweb | chatglm.cn web | new SDD |
| | copilotmsweb | copilot.microsoft.com web | new SDD |
| | deepseekweb | chat.deepseek.com web | new SDD |
| | devinweb | devin.ai web | new SDD |
| | githubcopilotweb | github.com/copilot web | new SDD |
| | googleaistudioweb | aistudio.google.com web | new SDD |
| | grokweb | grok.com web | new SDD |
| | huggingchatweb | huggingface.co/chat web | new SDD |
| | kimiweb | kimi.moonshot.cn web | new SDD |
| | m365copilotweb | M365 copilot web | new SDD |
| | mistralweb | chat.mistral.ai web | new SDD |
| | openaiplatformweb | platform.openai.com web | new SDD |
| | perplexityweb | perplexity.ai web | new SDD |
| | poeweb | poe.com web | new SDD |
| | v0web | v0.dev web | new SDD |
| | youweb | you.com web | new SDD |
| `ide/` (5) | codeium | Codeium IDE plugin | new SDD |
| | continuedev | continue.dev IDE | new SDD |
| | githubcopilot | github copilot IDE | new SDD |
| | replitai | replit.com / IDE | new SDD |
| | tabnine | tabnine IDE | new SDD |

**Verification scope per adapter** (template):

1. Capture real upstream traffic (manual session in browser / IDE; tcpdump or compliance-proxy raw capture).
2. Confirm wire-shape classifier (Tier-1 `Normalize` produces correct `Kind=ai-chat` or future `Kind=ai-*`).
3. Confirm extracted `Inputs` / `Messages[]` round-trip readable text.
4. Confirm `NormalizedPayload.Confidence ≥ threshold`.
5. End-to-end test via `/test-<adapter>-skill` (new skill per adapter, modeled on `/test-cursor-adapter` / `/test-geminiweb-adapter`).
6. Per-host `interception_domain` rule seeded in prod CP for the verified host.
7. Add adapter ID to `/test-compliance-proxy` rotation so future smoke runs catch regressions.

**Per-adapter story breakdown** (40 stories):

- [ ] **S1-S14** — Tied to E72-S1 through E72-S14 (api/ adapter verification co-runs with corresponding spec verification).
- [ ] **S15-S33** — One per `web/` adapter (19 stories).
- [ ] **S34-S38** — One per `ide/` adapter (5 stories).

**Critical gate before closing each story**: synthetic test passes against real upstream; CP `interception_domain` rule + audit row populated with correct Kind; `/test-<adapter>` skill repeats green.

**Out of scope (E73)**:

- New Tier-1 adapters not yet in code (every fresh provider needs its own adapter PR).
- Image / audio / video Kind extensions — retired into `_backlog.md` pending customer signal.
- macOS NE content-aware coverage (blocked by E74 pf-intercept replacement).

**Implementation order**: Customer-demand driven. `web/` adapters track customer-side traffic that's already flowing through CP today (verifying them gives immediate audit coverage); `ide/` adapters track agent installs.

---

### E74 — macOS pf-intercept replacement of NETransparentProxyProvider

**Status**: Planned. Surfaced by E62 §8.7.2 architecture review.

**Motivation**: macOS today DOES capture HTTPS content — the NE Swift extension forwards inspect-mode flows to a localhost `:9443` bridge socket and the Go daemon's `tlsbump` package does the actual MITM (admin-trusted leaf cert, terminate client TLS, run hooks on plaintext, re-encrypt to upstream). Content-aware hooks run end-to-end. E74 is NOT about restoring missing TLS-bump — it is about closing the inherent **coverage + ergonomics gaps** that the NE-extension architecture imposes:

1. **NE opt-in surface**: NE only sees flows the OS routes through it. Apps using raw sockets, app-bundled DoH/DoT, certain helper-process designs, or VPN-on-VPN can bypass NE entirely. pf hooks at the packet layer — coverage is universal.
2. **QUIC/UDP blind spot**: NE cannot reliably MITM QUIC. Today we work around via `forceQUICFallbackBundles` (an 8-bundle list seeded in `system_metadata.agent.settings`) that forces specific browsers to TCP. Every new Electron AI desktop app + every new browser is manual list maintenance. pf can drop UDP/443 selectively and let happy-eyeballs do the fallback for any process.
3. **Fail-open cost paid as visibility loss**: because NE hangs kill the whole Mac's network (incident 2026-05-15), every inspect path carries a fallback timeout (SNI peek 500 ms, daemon IPC `requestDecision` 2 s). Timeout → passthrough → that flow is metadata-only. pf's kernel path makes the trade-off differently and reduces the fraction of traffic that falls back.
4. **Per-hop latency**: NE → IPC → Go daemon → bridge socket → tlsbump adds two user-space crossings (NE Extension sandbox ↔ daemon). pf reduces this.
5. **Process attribution drift**: NE bundle-ID attribution via `NEFilterControlProviderProtocol` is shaky for helper-process apps (Chrome helpers, Electron utility processes). pf + `libnetwork-conntrack` can attribute by uid + parent process.

E62 already added embeddings as a new endpoint and the coverage gaps above proportionally hurt as the surface grows; any future endpoint family reactivated from `_backlog.md` would magnify them further.

**Approach**: complement (not necessarily replace) `NETransparentProxyProvider` with a pf-based transparent intercept (analogous to the existing Linux pf adapter). Decision pending in SDD: full replacement vs. hybrid (NE for default app traffic, pf for the coverage-gap classes).

**Scope outline** (full SDD pending):

- New pf rule installation flow (admin / installer privileges); ship-trusted cert install path stays as-is (already used by tlsbump).
- pf <-> Go daemon flow handoff contract analogous to the NE bridge socket (so the existing tlsbump pipeline is reused — no second MITM stack).
- Per-process attribution by uid + libnetwork-conntrack (closes the Chrome-helper drift).
- Migration path for existing NE-installed agents (decide: drop NE entirely vs. keep NE + add pf, controlled via agent config).
- Safety-critical fail-open invariants transfer: no hangs in pf handlers; bypass for system services (DNS, DHCP, mDNS, NTP, Push); kill-switch reachability.
- Smoke verification: extend `/test-compliance-proxy` skill arm for macOS-pf path; smoke must demonstrate at least one of the five gap classes above is closed (e.g., a raw-socket test app's HTTPS now sees content hooks).

**Critical gate before closing E74**: at least one of the five gap classes above is empirically closed in synthetic tests; fail-open invariants pass stress tests (NE-grade safety transferred to the pf path); macOS Agent install + uninstall remains clean; content-aware hooks demonstrate coverage parity vs. NE on the chat + embeddings happy path.

**Out of scope (E74)**: Windows / Linux pf paths unchanged; NetExtension fallback for incompatible kernels (treat as separate story).

---

### E75 — Three-platform Agent end-to-end verification

**Status**: Planned. Surfaced 2026-05-19. Code for the three platform agents (macOS NE + Linux pf + Windows WinDivert) is **development-complete**; full end-to-end test verification is **incomplete**. **Coverage requirement: every platform must pass, no exceptions** — a platform that fails any verification story is a release blocker until fixed. Partial-platform coverage is not acceptable at close-out; either all three platforms ship together or the epic stays open.

**Motivation**: Code for the three platform agents lives in:

- `packages/agent/internal/platform/darwin/` + `packages/agent/platform/darwin/NexusAgent/NexusAgentExtension/`
- `packages/agent/internal/platform/linux/`
- `packages/agent/internal/platform/windows/`

The shared forwarder (`packages/agent/internal/`) runs uniformly across all three. Each platform has its own intercept mechanism, install/uninstall flow, IPC contract, and OS-specific safety constraints. **Today no platform has a comprehensive synthetic test suite exercising the full install→intercept→hook→audit→uninstall flow.** Issues that surface only with full verification:

- **macOS NE**: fail-open invariants under daemon stalls (recurring concern per CLAUDE.md "macOS NE proxy must fail-open" — already had a 2026-05-15 incident requiring manual `launchctl unload` recovery).
- **Linux pf**: iptables/pf rule installation correctness across distros (Ubuntu / RHEL / Arch); systemd unit start order vs nftables conflicts.
- **Windows WinDivert**: kernel-mode capture stability under high-load IDE traffic; Service Control Manager state machine; named-pipe IPC race conditions during install/uninstall.

**Reading order**:

1. Architecture (Tier-1): `docs/developers/architecture/services/agent/agent-forwarder-architecture.md` (cross-platform forwarder + phase model).
2. macOS-specific: `docs/developers/architecture/services/agent/agent-ne-fail-open-architecture.md` (binding safety rules) + `docs/developers/architecture/services/agent/agent-macos-platform-architecture.md`.
3. Linux-specific: `docs/developers/architecture/services/agent/agent-linux-platform-architecture.md`.
4. Windows-specific: `docs/developers/architecture/services/agent/agent-windows-platform-architecture.md`.
5. Cross-platform: `agent-enrollment-architecture.md`, `agent-keystore-architecture.md`, `agent-autoupdater-architecture.md`, `agent-tray-ipc-architecture.md`, `agent-paths-abstraction-architecture.md`, `agent-backpressure-rollup-architecture.md`.

**Stories pending implementation** (20 — split into 3 platform arms + 1 cross-platform arm):

**macOS arm (5 stories)**:

- [ ] **S1** — macOS install / uninstall flow verification (`build-agent` skill end-to-end + system extension activation + Tray UI launch + uninstall completeness: no leftover plist, kext, helper).
- [ ] **S2** — NE fail-open synthetic stress test (verify DNS / DHCP / mDNS / NTP / APNS allowlist; simulate daemon stall → 2 s timeout → passthrough; QUIC fallback; emergency recovery via `launchctl unload` documented + automated).
- [ ] **S3** — macOS IDE intercept (`/test-cursor-adapter` against macOS-installed agent end-to-end; protobuf decode; audit row stamps `traffic_event.source='agent'` + `Kind=ai-chat`).
- [ ] **S4** — macOS consumer-web intercept (chatgpt.com / claude.ai / gemini.google.com via the macOS agent path; today the macOS NE path is metadata-only — verify metadata capture is correct; full content-aware path waits for E74 pf-intercept).
- [ ] **S5** — macOS audit drain (Hub upload via mTLS + HTTP fallback + SQLCipher queue empties after drain + reconnect after network blip).

**Linux arm (5 stories)**:

- [ ] **S6** — Linux install / uninstall on Ubuntu 22.04 LTS + RHEL 9 + Arch (systemd unit installs; pf/iptables rule placement; cleanly uninstalls without orphan rules).
- [ ] **S7** — Linux pf intercept correctness (TLS bump succeeds with admin-trusted cert; content-aware hooks run on chat + embedding traffic; verified against api.openai.com, api.anthropic.com, generativelanguage.googleapis.com).
- [ ] **S8** — Linux fail-open behavior (agent crash → pf rules auto-removed within timeout; no stuck NAT-redirect; no DNS resolution breakage).
- [ ] **S9** — Linux audit drain (mTLS WS primary + HTTP fallback + retry-with-backoff on network blip + SQLCipher local queue rotates without loss).
- [ ] **S10** — Linux uninstall completeness (no orphan pf rules; no orphan systemd unit; SQLCipher DB cleanup; cert + keystore cleanup).

**Windows arm (5 stories)**:

- [ ] **S11** — Windows install / uninstall on Windows 11 22H2+ (WinDivert kernel driver activation; SCM service state machine: Stopped → StartPending → Running → StopPending → Stopped; clean uninstall).
- [ ] **S12** — Windows WinDivert intercept correctness (kernel-mode capture stable under sustained high-load IDE traffic; TLS bump; content-aware hooks).
- [ ] **S13** — Windows fail-open behavior (driver unload safety; no BSOD under capture failure; agent crash → driver cleanly unloads + system networking returns).
- [ ] **S14** — Windows named-pipe IPC stability (race-condition stress: install / restart / upgrade overlap; daemon ↔ Tray UI roundtrip).
- [ ] **S15** — Windows audit drain + uninstall completeness (SQLCipher cleanup; cert cleanup; SCM service deregistration; no orphan registry keys).

**Cross-platform arm (5 stories)**:

- [ ] **S16** — mTLS enrollment flow (CSR + cert issuance from Hub; rotation; revocation; re-enrollment after cert expiry).
- [ ] **S17** — Auto-updater (Ed25519 signature verification on release manifest; rollback on bad signature; staged rollout via release channels).
- [ ] **S18** — Keystore migration (across SQLCipher DB-key rotation; data preservation; failure-mode test when key lost — must wipe local queue + report incident).
- [ ] **S19** — Kill-switch reachability (Hub-pushed shadow → local kill within ≤ 30 s; verified on all 3 platforms; emergency-disable UI button works).
- [ ] **S20** — Tray IPC protocol cross-platform parity (daemon ↔ UI socket (macOS) / named pipe (Windows) / socket (Linux); same wire shape; platform-specific quirks captured in test fixtures).

**Critical gate before closing E75**: every platform arm passes a synthetic test program that exercises the full install → intercept → hook → audit → uninstall flow on a **clean VM image** (Vagrant / multipass / similar). Per the all-platforms-must-pass coverage requirement above: no platform can be partial-pass at close-out review. A platform with even one red story keeps the epic 🟢 Planned, not ✅ Shipped.

**Implementation order**: Parallel across platforms is allowed (3 separate VM test environments). Cross-platform arm (S16-S20) waits for at least one platform's per-platform arm to ship — then runs on whichever platforms are ready. Recommended start: Linux (lowest install friction for VM-based CI) → Windows → macOS.

**Out of scope (E75)**:

- macOS pf-intercept replacement of NE — that is **E74** (separately tracked). E75 verifies the *current* NE-based macOS code; E74 will replace it and extend E75 coverage retroactively (running the same S3 / S4 / S5 against the new pf path).
- New platform support (BSD, ChromeOS, mobile) — not in scope.
- Performance benchmarking under sustained load — correctness-only.
- Per-IDE / per-web adapter verification (those are **E73**) — E75 verifies the platform mechanism handles SOME real traffic; full adapter matrix is E73's job.

---

### E76 — GitHub Wiki content

**Status**: ✅ Shipped 2026-05-21 (PR #42, merge commit `72524a26` on `develop`). The IA v2 expansion delivered 143 publishable pages across 19 groups (Product / Getting Started / Technical / Development / Community), authored via 4-phase × 5-Sonnet parallel runs and signed off by a 2-round product+architecture review. Tracking files moved out of `docs/_wiki/` to `docs/handoffs/E76-wiki-expansion/` so GitHub Wiki only publishes the publishable set. Deferred follow-ups: 17 archaeology fixes in arch docs, MAINTAINERS.md, air-gapped runbook polish. Memory anchor `project_e76_wiki_oss`.

**Motivation**: Today, all the developer docs live under `docs/developers/architecture/` and `docs/developers/workflow/` (architecture, SDDs, requirements, openapi, runbooks). These are deep technical references — too dense for first-time evaluators. A Wiki is the **shallow surface** that orients new readers and routes them to the deep docs when they want detail. Also serves as the canonical source for "how do I get Nexus running on my machine" — a question that should NOT require reading 50 markdown files.

**Reading order before starting**:

1. Existing dev-doc inventory: `docs/README.md`.
2. Architecture overview: `docs/developers/architecture/overview.md`.
3. Product overview (if any): `docs/users/product/overview.md`, `docs/users/product/features.md`.
4. Source-of-truth docs for Wiki content: `docs/developers/architecture/overview.md` (system topology), `docs/developers/workflow/conventions.md` (code conventions), `docs/developers/workflow/local-dev-debugging.md` (operational dev rules); CLAUDE.md only carries the binding distillation of these.

**Stories pending implementation** (sketch; full SDD on epic kickoff):

- [ ] **S1** — Wiki structure design (sidebar IA: Home / Getting Started / Architecture / Deployment / FAQ / Contributing / Roadmap / Security).
- [ ] **S2** — Getting Started: clone → docker-compose → seed → first request → first admin UI login. Target: ≤ 15 min new-user time-to-first-success.
- [ ] **S3** — Architecture overview (Wiki version): 1-page summary linking out to `docs/developers/architecture/overview.md` + per-service deep dive.
- [ ] **S4** — Deployment guide: dev (docker-compose) / staging (EC2) / prod (current single-EC2 baseline + future K8s). Link out to `docs/operators/ops/runbooks/`.
- [ ] **S5** — FAQ: common questions surfaced from any customer / evaluator interaction so far (e.g. "Why Valkey not Redis?", "How is this different from LiteLLM / Helicone?", "Can I self-host?").
- [ ] **S6** — Contributing: PR flow, the Plan-first-Todo-always workflow from CLAUDE.md (sanitised for public), code conventions, how to claim a new epic number.
- [ ] **S7** — Security: vulnerability reporting address, threat model overview, IAM model overview, secrets-handling guidance, SBOM if available.
- [ ] **S8** — Roadmap snippet: top of this `roadmap.md` rendered as a Wiki page (or a short summary with link back to this canonical file).
- [ ] **S9** — Wiki publish + maintenance contract (how to keep it in sync with `docs/developers/architecture/`).

**Critical gate before closing E76**: a new evaluator can reach "AI Gateway serving its first chat completion" from the Wiki Getting Started page in ≤ 15 minutes without reading a separate doc.

**Implementation order**: S1 → S2 ∥ S3 ∥ S4 → S5 ∥ S6 ∥ S7 → S8 → S9.

**Out of scope (E76)**:

- Replacing `docs/developers/architecture/` (the dev docs stay where they are — Wiki links *to* them, not replaces).
- Translation to non-English (Wiki ships English first; ZH / ES can follow once the EN surface is stable).
- API reference auto-generation (separate; might come from OpenAPI specs into Wiki).
- Separate marketing website — Wiki is the single public-facing surface (see `_backlog.md`).

---

### E78 — Self-hosted local inference for AI Guard + AI Routing + Semantic Embedding

**Status**: Planned. Surfaced 2026-05-19. Memory anchor `project_local_inference_server_direction` (auto-loaded) locks the architectural direction: **one local OpenAI-compatible inference server serves all three downstream consumers, all via the existing `Provider` system**. **No parallel "internal AI client" framework.** What's NOT done is the actual deployment, model selection, performance budget, and the cutover that flips today's external-API defaults to local-first for each of the three consumers.

**Three downstream consumers today** (each currently calls an external API):

1. **AI Guard** — judge-model classification of request/response content (Class-A semantic content hook). Today external API call per evaluation; cost + latency dominated by upstream.
2. **AI Routing (LLM-dispatch)** — smart routing engine that uses an LLM to choose target model from canonical request semantics. Today external API call per "smart" routing decision; cost + decision latency.
3. **Semantic Embedding for Response Cache (E61 L2)** — embedding inference for vector cache reads + writes. Today defaults to OpenAI `text-embedding-3-small` per E61 seed; admin can swap to a local server already (per E61-S5) but the local server itself is not built.

**Reading order before starting**:

1. Memory: `project_local_inference_server_direction` (auto-loaded).
2. Architecture: `docs/developers/architecture/services/ai-gateway/aiguard-architecture.md` (AI Guard consumer) + `docs/developers/architecture/services/ai-gateway/smart-routing-architecture.md` (AI Routing consumer) + `docs/developers/architecture/services/ai-gateway/response-cache-architecture.md` §3.2 + `docs/developers/specs/e61/e61-s5-embedding-provider.md` (Embedding consumer).
3. Adapter framework: `docs/developers/architecture/services/ai-gateway/provider-adapter-architecture.md` (`specs/compat/*` is the canonical landing zone for any OpenAI-compat server).

**Stories pending implementation** (sketch; full SDD on epic kickoff):

- [ ] **S1** — Inference-server stack choice (vLLM vs Ollama vs LiteLLM vs Hugging Face TGI vs llama.cpp+OpenAI-shim) + model-family choice per consumer (e.g. Llama-3 8B for AI Guard, Qwen2 7B for AI Routing, BGE-M3 for embedding). Decisions captured in SDD with capacity / latency / accuracy benchmarks.
- [ ] **S2** — Deployment pattern (single-node GPU host vs CPU-only on prod EC2 vs separate compute cluster); env binding + cert handling; resource limits + autoscale strategy.
- [ ] **S3** — Provider catalog seed for the local server (one `Provider` row with `adapter_type=openai-compat`; multiple `Model` rows per consumer use case).
- [ ] **S4** — AI Guard cutover: switch default classifier to local model; external-API fallback on local-server-down; observable failure modes via Prometheus.
- [ ] **S5** — AI Routing cutover: same pattern for routing-decision LLM; latency budget mandatory ≤ 200 ms p95 (else routing decisions slower than the request itself).
- [ ] **S6** — Embedding cutover: flip E61 L2 default from OpenAI cloud → local; E61-S6c admin UI surfaces the new default; cost-savings calculation reflects local-vs-cloud.
- [ ] **S7** — Smoke + ops: `/smoke-gateway` arm exercises all three local-model code paths; `/prod-debug` skill recipes for "local model not responding"; rolling-restart playbook.

**Critical gate before closing E78**: all three consumers default to local on a fresh deploy; full-surface smoke green; fallback-to-external paths exercised + verified.

**Out of scope (E78)**:

- Training / fine-tuning custom models — use off-the-shelf open models.
- Multi-GPU clustering / advanced inference optimisation — single-host baseline first; scale-out is a follow-up.
- GPU-vs-CPU cost benchmarking deep-dive — make a deployable choice, iterate.

---

### E79 — Traffic event storage migration (PostgreSQL → columnar store)

**Status**: Planned. Surfaced 2026-05-19. Today `traffic_event`, `traffic_event_payload`, `traffic_event_normalized` all live in PostgreSQL via Prisma. Write volume is per-request; read patterns are dashboard-style range scans + aggregates. **PostgreSQL is the right store for transactional state (VKs, routing rules, IAM policies, hook configs) but the wrong store for analytics-style append-mostly traffic data at scale.**

**Candidate target stores**:

- **Clickhouse** — most common pick for traffic-event-style data; columnar, ridiculous compression, fast range scans on time-bucketed data, MergeTree TTLs map naturally to retention policy.
- **TimescaleDB** — Postgres-extension; lower migration cost (keep Prisma, add hypertables); not as fast as Clickhouse but easier ops.
- **DuckDB / Parquet on S3** — for fully cold analytics tier; live queries via DuckDB.
- **Hybrid** — recent N days in TimescaleDB, older in Clickhouse / Parquet — common compromise.

**Reading order before starting**:

1. Architecture: `docs/developers/architecture/cross-cutting/observability/audit-pipeline-architecture.md` (the current PG flow), `docs/developers/architecture/cross-cutting/storage/spillstore-architecture.md` (binary overflow), `docs/developers/architecture/services/ai-gateway/normalization-architecture.md` (NormalizedPayload sidecar table — §"Storage strategy" already flagged the inline-vs-spill bet that COULD break).
2. Current schema: `tools/db-migrate/prisma/schema.prisma` (`TrafficEvent`, `TrafficEventPayload`, `TrafficEventNormalized`).
3. Query call sites: `packages/control-plane/internal/traffic/store/trafficstore/**`, `packages/control-plane/internal/observability/**`, `packages/nexus-hub/internal/traffic/chain/**`.

**Stories pending implementation** (sketch; full SDD on epic kickoff):

- [ ] **S1** — Target store decision + perf benchmark vs current PG baseline (ingest tps + query latency on representative dashboards).
- [ ] **S2** — Schema design in target store (preserve all existing columns + add per-endpoint metadata JSONB or columnar equivalent; sketch index strategy).
- [ ] **S3** — Dual-write phase: Hub audit consumer writes to both PG and target store simultaneously for a confidence period; reconciliation job verifies parity.
- [ ] **S4** — Query layer migration: admin API + dashboard queries gradually move to target store; PG queries retained as fallback.
- [ ] **S5** — PG-side trimming: retention policy on PG narrows from "everything" to "last 24h hot path"; older data lives only in target store.
- [ ] **S6** — Tooling: backup / restore / migration for the new store; ETL for historical PG → target.
- [ ] **S7** — Observability: ingest lag SLO (≤ 10 s from request to queryable); query-layer error tracking.

**Critical gate before closing E79**: target-store-only mode runs for ≥ 7 days in prod without dashboard regression; PG `traffic_event` tables truncated to hot-path window.

**Out of scope (E79)**:

- Transactional state migration (VK / routing-rule / IAM-policy tables stay in PG — they're the right fit).
- Spillstore migration (S3 binary overflow already separate from PG inline).
- Real-time stream processing of audit events (separate concern, may emerge as E89+ if needed).

---

### E81 — High-availability + multi-instance clustering

**Status**: Planned. Surfaced 2026-05-19. Today: single-EC2, single Valkey, single Postgres. Constraint flagged in E61-NFR-5 + CLAUDE.md "Current state — single-EC2 single Valkey for this epic". For enterprise SLAs (99.9% uptime, rolling deploys, blast-radius limits) HA is required.

**What's needed**:

- Gateway → multi-instance behind LB; stateless request handling; shared Redis / Valkey / NATS.
- HA Postgres (managed or self-managed: RDS Multi-AZ, Patroni, etc.).
- Shared Valkey cluster (not single-node).
- NATS clustered (already supports clustering).
- Hub leadership election for cron-style jobs (so jobs don't double-run on multiple instances).
- Config push lag handling under multi-instance (existing thingclient should handle, but verify under load).
- Rolling-deploy procedure (zero-downtime + drain semantics + health-check + traffic shift).
- DR / backup / restore validated (RTO/RPO SLOs published).

**Reading order before starting**:

1. `docs/developers/architecture/overview.md` (current single-EC2 topology).
2. `docs/developers/architecture/cross-cutting/foundation/thing-model.md` + `docs/developers/architecture/cross-cutting/foundation/multi-endpoint-coordination-architecture.md` (Hub-side coordination already supports multi-instance Things in concept).
3. `docs/developers/architecture/cross-cutting/foundation/mq-architecture.md` (NATS clustering).
4. `docs/operators/ops/runbooks/` (existing operational runbooks).

**Stories pending implementation** (sketch — also multi-epic-sized):

- [ ] **S1** — HA Postgres deployment + DR validation.
- [ ] **S2** — Valkey cluster (cluster-mode or sentinel — choice in SDD).
- [ ] **S3** — Multi-instance gateway behind LB (TLS termination + draining behaviour + health checks).
- [ ] **S4** — Hub leadership election for cron jobs (jetstream KV lock or similar).
- [ ] **S5** — Rolling-deploy automation (`prod-deploy` skill upgrade for multi-instance).
- [ ] **S6** — RTO/RPO SLOs measured + published (in CLAUDE.md and operations docs).

**Critical gate before closing E81**: rolling restart of any single node causes 0 request loss; full DR drill (kill primary DB, fail over to replica, verify) executed quarterly.

**Out of scope (E81)**:

- Multi-region (separate effort, layer on top of HA).
- Geo-distributed Valkey (single-region first; geo is its own epic).

---

### E82 — Observability stack completion

**Status**: Planned. Surfaced 2026-05-19. Prometheus metrics are emitted by all services (per `prometheus-naming-architecture.md`). Alert rules baseline exists in seed (`AlertRule` rows via `docs/developers/architecture/cross-cutting/observability/alerting-architecture.md`). **What's missing**: the consumption layer — Grafana dashboards, Alertmanager wiring, on-call rotation, log aggregation strategy, OTel trace search.

**What's needed**:

- Grafana dashboard library (per-service + per-customer-flow dashboards; saved-search bookmarks for common questions).
- Alertmanager configuration (pager routing + escalation policies + de-dupe rules).
- On-call programme (rotation + incident-response playbook + post-mortem template).
- OTel trace search backend (Tempo / Jaeger / SaaS).
- Log aggregation (today: `journalctl` on prod EC2; eventually: Loki / SaaS).
- SLO measurement + error budget tracking.

**Stories pending implementation**:

- [ ] **S1** — Grafana dashboard library (commit to repo at `tools/grafana/dashboards/*.json`; provision via Grafana API on prod).
- [ ] **S2** — Alertmanager rule set + receivers (Slack / PagerDuty / email; per-severity routing).
- [ ] **S3** — OTel trace backend integration (Tempo or SaaS; trace search wired into admin UI traffic drawer).
- [ ] **S4** — Log aggregation (Loki or SaaS; structured log shipping from all 4 services).
- [ ] **S5** — SLO definition + error-budget tracking dashboard.
- [ ] **S6** — On-call playbook + post-mortem template + first synthetic incident drill.

**Critical gate before closing E82**: a new on-call person can answer "is the system healthy?" + "what's the worst alert right now?" + "where's the trace for request X?" entirely from the observability surfaces, no `ssh` required.

**Out of scope (E82)**: customer-facing status page (not in scope for this OSS repo; downstream deployers can layer this on).

---

### E85 — Unit-test coverage 95% (close the allowlist)

**Status**: 🟢 In-progress on `feature/E85` worktree. Started 2026-05-21. Phase 1 (types-only `doc_test.go` conversion) shipped; Phase 2 (configdispatch / replay / breakglass / ingress-proxy debt closure via parallel Sonnet subagents) closing.

**Motivation**: The coverage gate is the load-bearing quality signal. Every allowlist entry is a hole — it accepts a regression in that package as long as the package as a whole stays under the existing line. Closing the allowlist makes the gate's promise meaningful again.

**Reading order**:

1. `scripts/.coverage-allowlist` — the file itself.
2. `scripts/check-go-coverage.sh` — the gate logic; `--strict-allowlist` mode shows entries that can already be removed.
3. CLAUDE.md "Unit test coverage" rule (categories A-F + rationale requirement).
4. `docs/developers/specs/e85/e85-unit-test-coverage-95.md` — E85 requirements + story list.
5. `docs/developers/specs/e85/decision-log.md` — D-1..D-N architectural choices.
6. `docs/developers/specs/e85/baseline-coverage.md` — Phase-0 snapshot.

**Stories shipped / in flight**:

- ✅ **S1** — Baseline inventory: 40 active allowlist entries enumerated; per-package coverage measured; entries classified as already-structural / convertible / genuine-debt.
- ✅ **S2** — Phase 1 quick wins: 10 types-only / sentinel-only packages converted from `[no test files]` → `[no statements]` via `doc_test.go`; removed from allowlist.
- 🟢 **S3-S8** — Phase 2 debt closure (6 packages, parallel Sonnet subagents): `breakglass` 0→100%, `replay` 16→96.7%, `control-plane/configdispatch` 25→95.8%, `compliance-proxy/configdispatch` 18→95.5%; `ai-gateway/configdispatch` + `ingress/proxy` closing.
- 🟢 **S9** — Phase 3 wiring per-function ledger: Explore-agent audit (97 files / 5,400 LOC across 5 wiring packages) classified every exported function as A-pure / logic / OS-bound; wiring packages stay (A) with detailed per-package rationale; `platformshim` re-categorized (A)→(D) per D-4.
- 🟢 **S10** — Phase 4 decision log: D-1 to D-8 captured; subsequent entries appended as Phase 2/3 closes.
- ⬜ **S11** — Phase 5 self-audit + final gate run: 4Q × 2 rounds; `--strict-allowlist` returns "0 removable entries".

**Critical gate before closing E85**: allowlist length stable at the structural-minimum count; pre-commit gate enforces 95% on every Go package without falling back to allowlist for new code.

**Out of scope (E85)**: Frontend coverage (Vitest) — separate metric / separate target.

---

### E86 — End-to-end test coverage uplift

**Status**: Planned. Surfaced 2026-05-19. The current `tests/run-all.sh` skill covers ~75 business flows across all 5 services per its description. **What's missing**: a formal **gap matrix** — what flows are NOT yet covered, ranked by customer impact / regression cost. Today coverage is "what we have" rather than "what we need".

**Motivation**: Every closing epic claims `/test-all` green as a release signal. If `/test-all` has gaps, "green" can hide regressions in real customer flows. The gap matrix lets epic-close decisions be informed: "E62 close — is the embedding flow you just shipped exercised by `/test-all`? If not, this epic can't close yet."

**Reading order**:

1. `tests/run-all.sh` + the test program plan at `docs/_archive/2026-q2/programs/test-scenarios-program-plan.md`.
2. `docs/developers/architecture/cross-cutting/observability/test-harness-architecture.md` (the harness design).
3. Per-test-layer skills: `/smoke-gateway`, `/test-compliance-proxy`, `/test-cursor-adapter`, `/test-geminiweb-adapter`, `/test-openai-responses`.

**Stories pending implementation** (sketch):

- [x] **S1** — Gap matrix definition. Output: `docs/developers/specs/e86-e2e-coverage-matrix.md` (~48 customer-facing capabilities × 6 test layers). Companion decision log at `docs/developers/specs/e86-decision-log.md`.
- [x] **S2** — Closed five highest-impact missing cells on shipped capabilities: `S-062` Responses-API arms (E56), `S-063` embeddings ingress (E62), `S-064` semantic-cache hit (E61), `S-065` dry-run estimate (E58), `S-066` cache negative-feedback (E68). Three lower-impact rows (E69 prewarm / E70 sticky-token / E71 domain-thresholds) explicitly deferred with rationale in matrix §4 + decision log D4.
- [ ] **S3** — Close gaps for every Planned epic BEFORE the epic claims close (gating contract). Now enforced structurally by S6 below.
- [x] **S4** — `tests/run-all.sh` emits an "E86 matrix snapshot" (✓ / ⚠ / ✗ counts) in the unified markdown report. Snapshot is informational; the PR-level gate is S6 (rationale: decision log D6).
- [x] **S5** — Per-layer coverage targets documented in matrix §2 (L1 / L1-Go / L2 / L3 / L4 / L5 / Skill with measurement methodology).
- [x] **S6** — CI integration: `scripts/doc-lockstep.config.mjs` entry `e2e-coverage-matrix` requires the matrix to be touched in any PR that adds/changes `docs/users/api/openapi/**`, `docs/users/features/**`, or `docs/developers/roadmap.md`.

**Critical gate before closing E86**: gap matrix at 0 ✗ for shipped capabilities; CI enforces matrix update on new-feature PRs.

**Implementation order**: S1 (matrix) → S2-S3 (close gaps) ∥ S4 → S5 → S6.

**Out of scope (E86)**: Load / stress / chaos testing — different concerns (own programme, may emerge as E88+).

---

## ❌ Retired / deferred

Detailed reasons + reactivation criteria live in `docs/developers/specs/_backlog.md`. Summary:

- **E63 / E64 / E65 / E66 / E67** — Multimodal endpoint family (audio / image / video / async-job / modality-aware hooks). Retired 2026-05-20 — no customer signal; the foundational typology framework shipped with E62 stays in code, but the per-modality epics wait for an explicit ask.
- **E77** — Official product website. Retired 2026-05-20 — E76 GitHub Wiki is the single public-facing surface; a separate marketing site would duplicate content.
- **E80** — SaaS multi-tenant migration. Retired 2026-05-20 — Nexus ships as single-tenant / OSS-deployable; the `org_id` columns serve internal multi-org structure inside a single tenant, not cross-tenant SaaS isolation.
- **E83** — Client SDKs (Python + Go + TypeScript). Retired 2026-05-20 — upstream provider SDKs work transparently against `/v1/*` via the canonical bus; a Nexus-branded SDK waits for an explicit customer ask.
- **E84** — Compliance certifications (SOC2 / ISO27001 / GDPR / HIPAA). Retired 2026-05-20 — Nexus Gateway is open source; certification is an enterprise-of-record obligation owned by each deploying organisation, not by the upstream project.

Retired epic numbers are NOT reused. If any of the above is reactivated, the requirements / SDDs are drafted under the original number.

---

## Maintenance checklist

When you add a new in-flight epic:

1. Append a one-line entry to the **Headline list** with phase emoji + critical-gate column.
2. Add a detailed block under the appropriate phase section (use the existing E78 block as a template — status, reading order, story checklist, critical gate, scope-outs, architecture decisions).
3. Cross-link from memory (write a `project_e<n>_<name>.md` memory file and add to `MEMORY.md` index).
4. Reserve continuous epic numbers for related stories — do NOT reuse a number after it's been written to commits.

When an epic phase transitions:

1. Move its block to the `✅ Recently completed` section with a brief "shipped on YYYY-MM-DD via PR #NN / commit `<sha>`" note pointing at the closeout commit. Drop the per-story detail once the entry stabilises.
2. Update the phase emoji in the Headline list.
3. Strike through completed-story checkboxes; do not delete them — they document what was actually done.

When you retire an epic (decision to NOT build):

1. Add the retirement row to `docs/developers/specs/_backlog.md` with date + reason + reactivation criteria.
2. Add a one-line summary to the "❌ Retired / deferred" section above.
3. Delete the active block from this file. Do NOT renumber.

When you add a new epic number:

1. Check existing numbers — do NOT reuse a slot. Retired epic numbers (E63-E67, E77, E80, E83, E84) are also not reusable; reactivation reuses the original number.
2. Next free slot is **E88** at the time of this file revision (2026-05-21). Always update this maintenance note when you claim the next number.

---

## See also

- `docs/developers/specs/_backlog.md` — index of retired / deferred epic numbers with reactivation criteria.
- `docs/developers/architecture/README.md` — which architecture doc to read when editing which code area.
- `docs/developers/architecture/overview.md` — system-level overview.
- `docs/developers/architecture/cross-cutting/foundation/endpoint-typology-architecture.md` — the endpoint typology framework that shipped with E62 (still load-bearing; covers chat + responses + embeddings).
- `docs/README.md` — full doc index.
