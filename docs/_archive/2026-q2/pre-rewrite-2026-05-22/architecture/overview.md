---
doc: overview
area: index
service: platform
tier: 1
updated: 2026-05-20
---

# Nexus Gateway — System Architecture

> Audience: engineers building or maintaining Nexus Gateway. For the product narrative see `docs/users/product/overview.md`; for the per-area "what to read when editing X" map see `docs/developers/architecture/README.md`; for the full doc index see `docs/README.md`.

This document is the **high-level architectural mental model**. It says **what** each service does and **how** they fit together. It does **not** restate per-module internals — those live in dedicated module architecture docs (`docs/developers/architecture/**/*-architecture.md`).

---

## 1. The five-service split

Nexus Gateway is **five cooperating services**, not three. The earlier "Trinity" (AI Gateway / Compliance Proxy / Agent) names the three **traffic-interception paths**, but operationally every traffic path is supervised by a central platform tier:

| Service | Process | Port | Role |
|---|---|---|---|
| **Nexus Hub** | `packages/nexus-hub` | 3060 | Platform ops center. Thing Registry, Device Shadow, config-sync orchestration, scheduled jobs, metrics pipeline, agent CA, SIEM bridge, alert evaluation, audit sink. |
| **Control Plane** | `packages/control-plane` | 3001 | Admin API / BFF for the dashboard UI. IAM, OAuth+PKCE admin auth, SSO/IdP federation, config CRUD, analytics queries. Stateless; proxies config writes to Hub. |
| **AI Gateway** | `packages/ai-gateway` | 3050 | Serves `/v1/*` AI traffic. 20 first-class adapter codecs (11 native + 9 OpenAI-compat under `packages/ai-gateway/internal/providers/specs/`), routing engine, prompt cache, response cache, quota enforcement, hook pipeline. Consumer-surface and IDE traffic (separate from the codec set) is identified by 49 traffic adapters under `packages/shared/traffic/adapters/{api,web,ide,generic}/`. |
| **Compliance Proxy** | `packages/compliance-proxy` | 3040 | Transparent TLS-intercepting forward proxy. CONNECT tunneling, cert minting, hook pipeline, normalizer pipeline (text-first), audit emission. |
| **Agent** | `packages/agent` | local | Desktop endpoint binary (macOS / Windows / Linux). OS-level intercept (macOS NE + pf, Linux iptables, Windows WinDivert), local hook pipeline, mTLS enrollment, audit upload. |

The frontend, `packages/control-plane-ui` (port 3000), is a React+Vite SPA served separately during dev and bundled into the Control Plane binary in production.

All five **server-side** services register with Hub as **Things** via `packages/shared/transport/thingclient` (WebSocket primary, HTTP fallback). The Agent registers as a Thing too, with the same enrollment + shadow contract.

## 2. Hub-centric Thing model

The platform's central abstraction is the **Thing**: every managed entity (each of the 4 server services and every Agent) is a row in the `thing` table with optional extension rows (`thing_service` for backend services, `thing_agent` for desktop agents) and a per-Thing config shadow.

- Each Thing has a unique `thing_id`, a typed extension (`Service` / `Agent`), and a status (`online` / `degraded` / `offline`).
- Configuration is split into a **desired** state (what the admin wants) and a **reported** state (what the Thing currently applies). Drift between the two is observable.
- **Pull-only config model.** Things PULL config from Hub on boot and on a change-signal; Hub never pushes full state. Category B keys all carry `needsPull: true` and trigger a fetch when their version increments. This unified pull model is binding across all five Thing types.
- Internal vs external terminology is enforced: code, DB, and dev docs say "Thing / Shadow / desired / reported / drift"; user-facing surfaces say "node / service / device / config sync / target config / applied config / out of sync". See `docs/developers/architecture/cross-cutting/foundation/thing-model.md` §10.

The Thing model + shadow flow is the binding kernel for the platform; if you change anything in this layer, read `docs/developers/architecture/cross-cutting/foundation/thing-model.md` first (and `thing-config-sync-architecture.md`).

## 3. Control plane vs data plane

Two responsibilities, deliberately split:

- **Control plane** — Hub + Control Plane. Owns durable state: Thing registry, shadow, policies, IAM, credentials, alerts, scheduled jobs. Stateless service instances; central state in Postgres + Redis. CP itself is a Thing.
- **Data plane** — AI Gateway + Compliance Proxy + Agent. Handles live AI traffic. Stateless; pulls config from Hub on boot + on change-signal. Emits events (traffic / audit / metrics) to MQ and to Hub via HTTP/WS. Shares Go business logic via `packages/shared/*`.

The data-plane services are **fail-open by default** for hook errors and for absent control-plane connectivity, so a Hub or CP outage degrades enforcement (alerts fire) but does not break traffic. The macOS NE TransparentProxyProvider applies a stricter version of this rule because it is in the host's outbound packet path — see CLAUDE.md "macOS NE proxy must fail-open" and `agent-ne-fail-open-architecture.md`.

## 4. Storage layer

| Store | Role | Notes |
|---|---|---|
| **PostgreSQL** | Durable system of record | Schema managed by Prisma (`tools/db-migrate/`). Go services read with hand-written SQL + pgx; Go types are codegen'd from the Prisma schema. No `sqlc`. |
| **Redis** | **Cache only** — no pub/sub | Sessions, IAM cache (60 s TTL), rate-limit counters, response cache, desired-state cache, cert cache (compliance proxy LRU + Redis), quota counters. Config invalidation is **NOT** Redis pub/sub — it is Hub WebSocket push. |
| **NATS JetStream** | MQ via `shared/mq` interface | Pluggable; current driver is NATS. Streams carry traffic events, audit events, ops metrics. Hub coordinates consumer groups. |
| **S3 spillstore** | Body overflow store | Bodies ≥ 256 KiB written through `shared/spillstore` (currently S3; legacy local-FS in dev). Audit rows store content-hashed references; admin UI fetches via presigned URLs. Production cutover landed 2026-05-14. |
| **SQLCipher (Agent)** | Local audit queue on endpoints | Encrypted local store; platform keystore holds the key. Drained over mTLS to Hub. |

CLAUDE.md "Deleted packages" lists what the migration removed (`shared/heartbeat`, CP `internal/pubsub`, CP `internal_registry`) — none of those exist any more; if a doc or comment still references them, treat it as stale.

## 5. Cross-service primitives

These primitives are referenced by every service. Detailed mechanics live in the listed module docs.

| Primitive | Where to read more |
|---|---|
| Enrollment + Hub CA + device cert | `agent-enrollment-architecture.md` |
| Thing shadow (desired / reported, Cat A/B/C keys, change-signal, drift) | `thing-model.md` + `thing-config-sync-architecture.md` |
| Config flow (admin → CP HTTP → Hub HTTP → shadow write → WS change-signal → Thing pull → apply → reported) | `service-call-framework.md` + `multi-endpoint-coordination-architecture.md` |
| MQ usage (subjects, streams, consumer groups, MQ vs HTTP/WS) | `mq-architecture.md` |
| `trace_id` / `request_id` propagation | `trace-id-propagation-architecture.md` |
| Cache tiers (in-process LRU, Redis, cert cache, IAM cache, desired-state cache, response cache, quota counters) | `cache-multi-tier-architecture.md` |
| Prompt cache (provider-side + shared Redis + request-level) | `prompt-cache-architecture.md` |
| Quota engine (Redis sliding window, org / provider / model scope) | `quota-architecture.md` |
| Audit pipeline (event schema → MQ → Hub sink → Postgres → analytics) | `admin-audit-log-coverage.md` + `audit-pipeline-architecture.md` |
| Error taxonomy (`ProviderError.Code`, `ErrorClass`, retry, 429 envelope, circuit breaker) | `error-taxonomy-architecture.md` |
| IAM (NRN, action catalog, iamMW, `allowedActions`, JIT, super-admin invariants) | `iam-identity-architecture.md` |
| IdP / SSO (Nexus is the SP; external IdPs Okta / Azure AD / OIDC / SAML; Nexus Local is the implicit fallback) | `idp-sso-architecture.md` |
| Tenancy (organisation hierarchy, ancestor path, policy inheritance) | `tenancy-architecture.md` |

## 6. Three traffic paths (high-level)

Each path has its own module doc; this is the elevator summary.

### Path A — AI Gateway (SDK proxy)

Applications send requests to `/v1/chat/completions`, `/v1/embeddings`, `/v1/models` (OpenAI-compatible surface) using a **Virtual Key** (bearer token) instead of raw provider credentials. The gateway:

1. Authenticates the VK and resolves project / organization scope.
2. Runs the request-stage hook pipeline.
3. Resolves the target via the routing engine (rule match → strategy tree → canonical payload → ResolvedRequest).
4. Decrypts the provider credential (AES-256-GCM) for the request lifetime.
5. Forwards to the provider; streams or buffers per provider/route policy.
6. Runs the response-stage hook pipeline (incl. streaming compliance modes).
7. Emits a traffic event + audit record (MQ → Hub sink).

Routing detail in `routing-architecture.md`; adapter framework in `provider-adapter-architecture.md`; emergency-passthrough fallback (E48) is covered in `emergency-passthrough-architecture.md`.

### Path B — Compliance Proxy (transparent TLS)

Applications point their HTTPS proxy at the compliance proxy (no SDK changes). The proxy:

1. Receives a CONNECT, runs access control (source IP allowlist, domain allowlist).
2. Bumps TLS: dynamically mints a leaf cert from a local CA (LRU + Redis cache).
3. Detects pinning failures and auto-exempts.
4. Matches the `interception_domain` ruleset to pick the traffic adapter.
5. Runs the hook pipeline (same `shared/hooks` set as the AI Gateway).
6. Forwards to the upstream provider.
7. Emits traffic + audit events; the SIEM bridge optionally fans out.

Detail in `compliance-pipeline-architecture.md`; the text-first normalizer + Tier-2 NonJSONDetector framework in `normalization-architecture.md`.

### Path C — Agent (desktop endpoint)

The agent intercepts AI-bound traffic at the OS level — macOS NETransparentProxyProvider + pf, Linux iptables, Windows WinDivert — then forwards through a local pipeline structurally identical to the compliance proxy: **intercept → req_hooks → upstream_ttfb → upstream_total → resp_hooks**. The agent **does** measure upstream TTFB and total on its forward path (same instrumentation as the compliance proxy); it is not a passive sniffer. Phase boundaries and trace_id stitching are documented in `agent-forwarder-architecture.md`.

Agent-specific concerns: enrollment + mTLS, encrypted local audit queue (SQLCipher + platform keystore), config-sync listener, autoupdater (with Ed25519 signature verification), platform paths abstraction (all filesystem paths come from `platform.DefaultPaths()` — never hardcoded). The macOS NE provider is safety-critical and obeys five fail-open rules — see CLAUDE.md.

The three paths are **independent and parallel**: traffic intercepted by one does not flow through another. Hook configs propagated from the Control Plane apply to all three (subject to `applicableIngress` filtering). Audit events from all paths land in the unified audit timeline.

## 7. Hooks & enforcement

Hooks are the cross-cutting enforcement mechanism. They live in `packages/shared/policy/hooks` and run in **all three paths** with the same code.

- **Three stages:** request-stage (pre-forward), response-stage (post-receive), and per-chunk for streaming.
- **Decisions:** Approve / Modify / Reject (hard or soft) / Abstain. The pipeline aggregates: first hard reject wins; any soft reject without a hard reject yields a soft rejection; otherwise the highest data-classification label is recorded.
- **`HookConfig.onMatch`** is the canonical schema (E46-S4). Built-in actions include `block-hard`, `redact`, `flag`, `log-only`. The PII scanner was running `block-hard` by accident before 2026-05-13; fixed via migration.
- **Streaming compliance modes** (per host / per provider): `passthrough` (no hook, no body capture), `buffer_full_block` (assemble before forwarding any byte; hook runs at stream end; HTTP 451 on hard reject), `chunked_async` (relay in real time, hook runs per chunk + at stream end; cannot stop already-sent bytes).
- **Body capture** is a separate dimension. Bodies < 256 KiB go inline into `traffic_event_payload`; larger bodies overflow to spillstore and the row stores a hashed reference.
- **Agent reality (audit 2026-05-14):** the agent runs hooks locally (3 stages) but currently **does not consult exemptions** delivered via shadow, and **does not consume rule packs**. The CP Policies-page Exemptions card is, today, a vanity surface for the agent. Server-side enforcement is the only effective place to block agent traffic until that wires up.

See `hook-architecture.md` for the canonical reference.

## 8. Observability & control

| Surface | What it is | Where to read |
|---|---|---|
| Prometheus metrics | Per-service counters / histograms (hook latency, pipeline decisions, request rates, provider errors). Promauto-registered. | `prometheus-naming-architecture.md` |
| OpenTelemetry traces | Spans across services; correlated with metrics via `trace_id` / `request_id`. | `otel-pipeline-architecture.md` + `trace-id-propagation-architecture.md` |
| Audit pipeline | Traffic events + admin audit log. MQ → Hub audit sink → Postgres → CP analytics queries. PII redaction at emit time. Body storage tiered to spillstore. Agent `auditEventToMap` always sets string fields (incl. `""`); Hub `AuditUpload` must stamp-unconditionally or strip-empty for any CHECK-constrained column. | `admin-audit-log-coverage.md` + `audit-pipeline-architecture.md` |
| Alerts | Two parallel sources: Go `BuiltinRules` registry and DB `AlertRule` rows. Lockstep test was removed after E41-v2 drift (2026-05-15); replacement design pending. Channels: webhook, SIEM, email. | `alerting-architecture.md` |
| Kill switch | Cat A inline shadow config. Cascades from Hub through WS change-signal → Thing pull → AI GW / agent passthrough. 3-tier toggle in E48 (org / provider / route). | `kill-switch-architecture.md` |
| Emergency passthrough (E48) | When the compliance pipeline can't run, traffic flows through unhooked. `ResolvedRequest` carries the L4 passthrough decision; max 8 h expiry (default 1 h); fail-closed cold-start; Hub reconciles every 60 s and auto-reverts. Audit trail is non-optional. | `emergency-passthrough-architecture.md` |
| Traffic-upload level | `agent_settings.trafficUploadLevel` ∈ {`all`, `processed`, `blocked`}; default `processed`; filtered at agent emit-time. `deny` / `block` / `error` always bypass the filter. | (binding feedback memory) |
| Cost & cache savings | Per-request USD cost stamped on every `traffic_event` from a single `metrics.CalculateCost` function. Inputs: canonical `Usage` (uncached input, cached read, cached write, output incl. reasoning) + per-`Model` price row (4 fields: input, output, cached input read, cached input write). Cache savings derive from the same numbers: gateway response-cache hits = full upstream cost avoided; provider prompt-cache hits = `cache_read` tokens billed at the discount rate. The gateway response cache has **two tiers**: an extract (exact-match) tier in `packages/ai-gateway/internal/cache/layer/` and a semantic vector tier in `packages/ai-gateway/internal/cache/semantic/` (live since E61); both stamp `gateway_cache_kind` ∈ {`extract`, `semantic`} on hits. See `response-cache-architecture.md` for the canonical description. | `cost-estimation-architecture.md` |
| Cost estimation (dry_run) | Pre-flight cost estimate that runs the canonical pipeline (canonicalize → route → cache lookup → estimator) but short-circuits before the upstream call. Triggered via `nexus.dry_run: true` in the request body. Same estimator core powers the optional `/v1/estimate` compare endpoint and future cost guardrails / cost-aware routing / budget forecasting. | `cost-estimation-architecture.md` |

## 9. Cross-cutting concerns

Three patterns appear everywhere and break things when forgotten:

1. **Token-field stamp sweep.** Adding a new usage / token field needs 5 stamp sites in `proxy.go` + `proxy_cache.go`, not just `handleNonStream`. Missing the 4 cache sites = all prod cache traffic NULL on the new column. (E53-S4 incident.)
2. **Migration timestamp uniqueness.** Two migration folders sharing the `YYYYMMDDHHMMSS` prefix make Prisma silently skip one. Pre-commit guard `ls migrations/ | cut -c1-14 | sort | uniq -d` must be empty. (2026-05-14 16 h audit-gap incident.)
3. **Agent platform paths abstraction.** All agent filesystem paths must come from `platform.DefaultPaths()` — never hardcode `/Library/`, `/var/`, `/etc/`, `/tmp/`, or `C:\`. (2026-05-13 QuitFlag incident.)

These are kept as binding rules in CLAUDE.md and as `feedback_*` memories; new contributors should internalise them.

## 10. Trust boundaries & auth

| Channel | Auth |
|---|---|
| Admin UI ↔ Control Plane | OAuth+PKCE bearer token. Cookie-based login is gone; the helper `tests/lib/auth.sh` (`cp_login` / `cp_curl`) is the canonical local-dev entry point. |
| External IdP ↔ Control Plane | SAML / OIDC. JIT user provisioning on first successful federation. Nexus is the **SP**; Nexus Local is the implicit fallback IdP, not a peer. |
| Application ↔ AI Gateway | Bearer Virtual Key. VKs are project-scoped and may restrict accessible models. |
| Agent ↔ Hub | mTLS. Hub's self-issued ECDSA P-256 CA issues device certs on enrollment (CSR-based). |
| Service ↔ Hub | WebSocket primary, HTTP fallback. Service Things authenticate with bootstrap tokens during first registration, then with their issued cert. |
| Provider credentials | Encrypted at rest with AES-256-GCM. Decrypted in memory only for the request lifetime. Dirty-set tracking propagates rotations to running gateways. |

## 11. Deployment topology (current)

Today's production runs as a **single EC2 node** (Hub + CP + AI GW + Compliance Proxy + Postgres + Redis + NATS, fronted by nginx) plus per-endpoint Agent installs. The pre-GA "no installed user base" policy applies: refactors are greenfield, no backwards-compatibility shims, no phased compatibility rollouts. See CLAUDE.md "Development-phase policy".

Future multi-instance + region-split deployment is structurally supported — services are stateless and the Thing / shadow model is multi-instance-aware — but is not in scope for any current PR.

## 12. Cost, pricing & estimation

Cost is computed from three pieces of data, and one canonical extraction rule glues them together. Getting any of these wrong silently corrupts billing, savings reporting, and pre-flight estimates — so the trio is called out as its own section.

### 12.1 Unified protocol parser (one source of truth)

Three subsystems consume upstream provider responses for the same wire formats (OpenAI Chat + Responses, Anthropic Messages, Gemini generateContent, plus ~14 OpenAI-compatible variants and a long tail of consumer-surface web protocols):

- **AI Gateway** (`packages/ai-gateway/internal/providers/spec_*`) — when Nexus is the API endpoint.
- **Compliance proxy** (via `packages/shared/traffic/adapters/*`) — when Nexus MITMs real provider traffic.
- **Agent** (via the same `shared/traffic/adapters/*`) — when the desktop endpoint intercepts traffic.

After E58-S0, all three delegate parsing to **one** set of Tier-1 normalizers under `packages/shared/transport/normalize/` (`OpenAIChatNormalizer`, `AnthropicMessagesNormalizer`, `GeminiGenerateNormalizer`, plus per-host adapter delegates and the Tier-2 pattern probe / non-JSON detector chain). The output is `normalize.NormalizedPayload` — the AST that the audit pipeline, the hook engine, and the UI all already consume. The gateway adds a thin projection (`canonicalbridge.DecodeViaShared`) that turns the NormalizedPayload into its own wire-shape canonical (OpenAI chat-completions form) plus the canonical `providers.Usage` it stamps on `traffic_event`. The gateway's wire-emission half — `EncodeRequest`, `PrepareBody`, the per-model parameter-strip rules — stays in `spec_*/` because it is tightly coupled to the upstream provider's wire-format requirements (per `provider-adapter-architecture.md` § 3a Rules 1–7).

When OpenAI ships a new `prompt_tokens_details.audio_tokens` field, we add the alias chain in one place — `shared/normalize/openai_chat.go` — and the gateway, the compliance proxy, the agent, and the audit pipeline all pick it up. Before E58-S0 this required four parallel edits with high drift risk; the doc trail is in `normalization-architecture.md` § "Ai-gateway codec delegation (E58-S0)".

### 12.2 Pricing data model (per-`Model` row, four fields)

Price is a property of a (Provider, Model) pair. The `Model` table is already provider-scoped (`Model.providerId` FK + `(providerId, providerModelId)` unique), so each row in `Model` carries the four prices that describe that pair:

| Field | What it bills | Provider examples |
|---|---|---|
| `inputPricePerMillion` | Uncached input tokens | All providers |
| `outputPricePerMillion` | Output tokens (includes reasoning tokens for OpenAI o-series, Claude extended thinking, Gemini 2.5) | All providers |
| `cachedInputReadPricePerMillion` | `cache_read` tokens at the discounted rate | Anthropic 0.1× input, OpenAI 0.5× input, Gemini 0.25× input; others = input price |
| `cachedInputWritePricePerMillion` | `cache_creation` tokens (write-side cache surcharge) | Anthropic 1.25× input; others = input price |

Pre-`E58-S1` the codebase had three pricing tables (`Model` + `ModelPricing` + `ProviderPricing`) with overlapping ownership; `ProviderPricing` had the cache fields but no code path read them. `E58-S1` collapses to the single-table design above and `metrics.CalculateCost` consumes all four fields. There is no time-effective pricing history table — price changes are captured by `AdminAuditLog` and applied immediately (development-phase policy; if billing reconstruction becomes a requirement post-GA, a history table can be added without changing the runtime path).

### 12.3 Cost calculation (one function, four lines of arithmetic)

`metrics.CalculateCost(usage Usage, p ModelPrices) Cost` is the single function that turns counts × prices into USD. It returns the four-component breakdown explicitly:

```
Cost{
    UncachedInput = (PromptTokens − CachedTokens − CacheCreationTokens) × inputPrice / 1e6
    CacheRead     = CachedTokens × cachedInputReadPrice / 1e6
    CacheWrite    = CacheCreationTokens × cachedInputWritePrice / 1e6
    Output        = CompletionTokens × outputPrice / 1e6        // includes reasoning tokens
    Total         = sum of the above
}
```

Stamping sites: 5 in `ai-gateway/handler/proxy.go` + `proxy_cache.go` (the well-known "token-field sweep" — Section 9 rule 1). All five sites must call the same function with the same `ModelPrices` to keep cached / non-cached / streaming / non-streaming paths consistent.

### 12.4 Estimator core (`packages/ai-gateway/internal/execution/estimator/`)

A pure-function package — no HTTP surface — that, given a canonical request + a resolved target + the model's price row, returns a low / expected / high estimate with assumptions. Composition:

- **Tokenizer** — `tiktoken-go` for OpenAI / Azure; documented character-ratio heuristic for Anthropic / Gemini / others. Provider-count-tokens APIs are explicitly *not* used (latency cost outweighs accuracy gain).
- **Output budget** — static per-(model, `reasoning_effort`) token-range table for the "expected" anchor; the low / high envelope widens with stated `reasoning_effort`. A future history-regression mode can replace the static anchor without changing callers.
- **Cache lookup** — read-only check against the gateway response cache (full-hit probability) and the upstream prompt-cache prefix (cached-read tokens probability).
- **Routing dry-run** — uses the same routing engine that real traffic uses, with `smart` strategies short-circuited to their fallback chain (the smart strategy itself calls an LLM, which would make estimation cost more than the request).

Reasoning intent is **read from the request body** (`reasoning_effort` for OpenAI shape; `thinking.budget_tokens` for Anthropic shape; `thinking_config.thinking_budget` for Gemini) — there is no parallel `reasoningMode` parameter.

### 12.5 Pre-flight estimation surface (`nexus.dry_run`)

The estimate-only request path is **a flag on the existing request body**, not a separate endpoint. Setting `nexus.dry_run: true` (in the `nexus.*` canonical extension namespace, per provider-adapter Rule 4) makes the gateway:

1. Run the full pipeline through routing + cache lookup.
2. Skip the upstream call.
3. Encode the estimator output into the ingress format's normal response shape with `choices: []` (or the per-format equivalent), `usage` filled from the estimator, and an `x-nexus-estimate` response header carrying the full JSON breakdown.

This guarantees the estimate and the real request share one code path — estimates can't drift from reality because they are produced by the same routing + cache + pricing code. All ingresses (OpenAI chat completions, OpenAI Responses API, Anthropic messages, Gemini generateContent) inherit dry-run for free.

The optional `POST /v1/estimate` endpoint is sugar for "estimate the same prompt against N targets" — its body wraps the original request with a `compareTargets` array, and internally it dispatches dry-runs per target.

### 12.6 Why this is one section instead of five

Cost, cache savings, and estimation are not separate features that happen to share data. They are *the same data* viewed three ways: (a) per-request cost = `Σ tokens × price`, (b) cache savings = "the price that did not happen" = a counterfactual cost, (c) estimate = the same calculation with the token counts predicted instead of measured. Wiring them through one extraction layer + one pricing model + one cost function is what keeps the three numbers reconcilable; any of them computed by a parallel pipeline would eventually report a different total than the others.

## 13. Where to go next

- **About to edit code in area X?** → `docs/developers/architecture/README.md` → find X → read the listed module doc(s).
- **Want the doc inventory?** → `docs/README.md`.
- **Want product context?** → `docs/users/product/overview.md`.
- **Shipping to prod?** → `docs/operators/ops/deployment.md` + `docs/operators/ops/runbooks/*.md` + `.claude/skills/prod-deploy`.

If something here contradicts a module doc, the **module doc wins** (it is closer to the code). Raise the contradiction so this file gets updated.
