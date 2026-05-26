# E76 Wiki — IA v2 (143 publishable pages, locked 2026-05-21)

> **Status**: locked 2026-05-21 (supersedes the 8-page IA from E76-DEC-001). All
> subagents writing wiki pages MUST read this file first; it is the authoritative
> sidebar map, the per-page contract, the source-doc registry, and the phase plan.

## Output target

GitHub Wiki for `github.com/alphabitcore/nexus-gateway`. The wiki is a sibling
repo (`nexus-gateway.wiki.git`). Pages are authored as standalone markdown
files in `docs/_wiki/`; a maintainer pushes them to the wiki repo when ready.

## Sidebar IA — 5 super-buckets, 19 groups, ~95 pages

Top-level partitioning by audience/purpose, following the IA patterns of mature
OSS docs (HashiCorp Vault / Envoy / Linkerd / Coder):

```
🏠 LANDING                 — Home (routing page, always visible)
📦 PRODUCT                 — evaluators / decision-makers
🚀 GETTING STARTED         — first-run readers (shared bridge)
⚙️ TECHNICAL               — builders / operators / security reviewers
🧪 DEVELOPMENT             — contributors / fork-adopters
👥 COMMUNITY               — governance / support
```

Diátaxis influence: Concepts pages are *explanation*; Getting Started is
*tutorial*; Recipes is *how-to*; API Reference is *reference*. Subsystem groups
mix concepts + how-to for that subsystem.

## Naming convention

Flat file namespace (GitHub Wiki requirement). Files named `<Group>-<Page>.md`
with hyphens. URL slug derives from filename; rendered title shows hyphens as
spaces.

Examples:
- `AI-Gateway-Prompt-Cache.md` → URL `/wiki/AI-Gateway-Prompt-Cache` → title "AI Gateway Prompt Cache"
- `Workbench-CLAUDE-md-Anatomy.md`
- `Recipe-Adding-A-Provider-Adapter.md`

Inter-wiki links use bracket-name syntax: `[AI Gateway Prompt Cache](AI-Gateway-Prompt-Cache)`.
Down-links into the main repo use absolute GitHub URLs (E76-DEC-010 still
binding for the v2 expansion).

## Wiki ↔ `docs/` boundary (unchanged)

- Wiki **synthesizes** across multiple `docs/` files into a single readable page.
- Wiki **does not duplicate** canonical reference (architecture docs, OpenAPI
  YAML, runbooks). Each wiki page footer carries a "Canonical docs" section
  with 1-4 absolute GitHub URLs back to authoritative sources.
- Wiki **does not duplicate README**. Where README is adequate, link to README.
- Wiki **is not** in `scripts/doc-lockstep.config.mjs` — periodic regeneration,
  not per-PR enforcement (E76-DEC-005).

---

## Page-by-page (full map)

Per-page contract — every page MUST follow:
1. **H1 = page title**. No YAML frontmatter (E76-DEC-007).
2. **One paragraph what/why** at top — reader decides whether to keep reading.
3. **2-5 substantive sections** scoped to the page's role.
4. **"Canonical docs" footer** — 1-4 absolute GitHub URLs into the main repo +
   "Adjacent pages" mini-list (3-6 other wiki pages).
5. **Length budget**: 200-500 lines. Anything bigger → split. Anything smaller →
   merge or expand.
6. **Voice**: same as `docs/users/product/overview.md` — concise, declarative,
   every claim backed by code or doc.
7. **Mermaid allowed** (E76-DEC-008). No SVGs, no images.
8. **English only** (CLAUDE.md binding).

Format below — for every page: filename · audience · 1-line job · source docs.

---

### 🏠 LANDING (1 page)

| File | Audience | Job | Sources |
|---|---|---|---|
| `Home.md` | anyone landing | Orient + route reader to right next page; 5-service Mermaid; "this repo is also" workbench note | `README.md`, `docs/users/product/overview.md`, `docs/developers/architecture/overview.md` |

---

### 📦 PRODUCT — Overview (5 pages)

| File | Audience | Job | Sources |
|---|---|---|---|
| `What-Is-Nexus-Gateway.md` | evaluator first-encounter | Product pitch; 3-coverage-gap framing; three traffic paths in one paragraph each | `docs/users/product/overview.md`, `README.md`, `docs/users/product/features.md` |
| `Why-Nexus.md` | evaluator comparing | Why this exists; what gap closes; non-goals; the 5-service rationale | `docs/users/product/overview.md`, `docs/users/product/competitive-landscape.md` |
| `Use-Cases.md` | evaluator mapping fit | Concrete use cases: compliance audit, multi-provider cost arb, vendor lock-in escape, AI dev-tool governance, SDK gateway | `docs/users/product/features.md`, `docs/users/product/competitive-landscape.md` |
| `Comparisons.md` | evaluator triaging tools | vs LiteLLM, Portkey, Bifrost, Helicone, Cloudflare AI Gateway, AWS Bedrock router — head-to-head matrix + "when to pick which" | `docs/users/product/competitive-landscape.md` |
| `Production-State.md` | evaluator checking maturity | What is serving real traffic today; what is adapter-only; HA status; air-gapped status | `docs/developers/roadmap.md`, `docs/users/product/overview.md` |

---

### 📦 PRODUCT — Features (10 pages, one capability each)

The Features group is **the product cards** — each page is a deeplink-worthy
sales surface for a specific capability. Pages are 250-400 lines, mostly
prose + 1 diagram, ending with "How to enable this" pointer into Concepts /
operator docs.

| File | Audience | Job | Sources |
|---|---|---|---|
| `Features-Index.md` | anyone | Catalog page — 1-line per feature + link out | `docs/users/product/features.md` |
| `Feature-Multi-Provider-Routing.md` | evaluator | Declarative routing rules, model catalog, fallback chain | `docs/developers/architecture/services/ai-gateway/routing-architecture.md`, `docs/users/features/cp-ui/ai-gateway.md` |
| `Feature-Smart-Routing.md` | evaluator | Cost-aware + failover routing | `docs/developers/architecture/services/ai-gateway/smart-routing-architecture.md` |
| `Feature-Prompt-Cache.md` | evaluator | Anthropic explicit + OpenAI auto + Google contextCache; ROI surfacing | `docs/developers/architecture/services/ai-gateway/prompt-cache-architecture.md` |
| `Feature-Response-Cache.md` | evaluator | Response-cache scope + threshold + traffic_event linkage | `docs/developers/architecture/services/ai-gateway/response-cache-architecture.md` |
| `Feature-Cost-Tracking.md` | evaluator | Single source of truth (Model row prices); per-request cost stamping; UI breakdown | `docs/developers/architecture/services/ai-gateway/cost-estimation-architecture.md` |
| `Feature-PII-Redaction.md` | compliance lead | Redact vs block; classifier model; per-route policy | `docs/developers/architecture/cross-cutting/safety/pii-redaction-policy-architecture.md` |
| `Feature-Audit-And-SIEM.md` | compliance lead | Every traffic event recorded; spillstore; SIEM forwarding | `docs/developers/architecture/cross-cutting/observability/audit-pipeline-architecture.md`, `docs/developers/architecture/cross-cutting/observability/siem-bridge-architecture.md` |
| `Feature-IAM-And-SSO.md` | security lead | Resources/actions/policies; Okta/Azure AD SSO | `docs/developers/architecture/services/control-plane/iam-identity-architecture.md`, `docs/developers/architecture/services/control-plane/idp-sso-architecture.md` |
| `Feature-Desktop-Agent.md` | compliance lead | OS-level capture for endpoint AI tools | `docs/developers/architecture/services/agent/agent-forwarder-architecture.md`, `docs/users/features/agent-ui/overview.md` |
| `Feature-Hooks-Framework.md` | evaluator | 3-stage hook pipeline; extensibility | `docs/developers/architecture/services/ai-gateway/hook-architecture.md` |

---

### 📦 PRODUCT — Roadmap & Releases (3 pages)

| File | Audience | Job | Sources |
|---|---|---|---|
| `Roadmap-Active.md` | evaluator / contributor | What's in flight this cycle | `docs/developers/roadmap.md` |
| `Roadmap-Queued.md` | evaluator / contributor | What's queued, by bucket (enhancement/verification/quality/productization/operational) | `docs/developers/roadmap.md`, `docs/developers/_backlog.md` if present |
| `Release-History.md` | evaluator / contributor | prod-YYYYMMDD tag history; what landed each cycle | `docs/developers/roadmap.md`, `git tag -l 'prod-*'`, memory anchor `[[project_prod_releases]]` |

---

### 📦 PRODUCT — FAQ (3 pages)

| File | Audience | Job | Sources |
|---|---|---|---|
| `FAQ-Product.md` | evaluator | 15-20 product questions: drop-in replacement? streaming? reasoning? air-gapped? license? | `docs/users/product/overview.md`, `docs/users/product/features.md`, `README.md` |
| `FAQ-Comparisons.md` | evaluator | 5-10 "how does Nexus compare to X" questions | `docs/users/product/competitive-landscape.md` |
| `Glossary.md` | newcomer / contributor | Thing/Shadow/VK/Hook/Spec/Canonical/Ingress/desired-vs-reported/normalize | `docs/developers/architecture/cross-cutting/foundation/thing-model.md`, `docs/users/product/overview.md` §glossary |

---

### 🚀 GETTING STARTED (8 pages)

| File | Audience | Job | Sources |
|---|---|---|---|
| `Prerequisites.md` | first-runner | Node 20+, Go 1.25+, Docker, OpenSSL; OS notes | `README.md`, `docs/developers/workflow/local-dev-debugging.md` |
| `Quickstart.md` | first-runner | `git clone` → `./scripts/dev-start.sh` → working stack | `README.md`, `scripts/dev-start.sh`, `.claude/skills/run-local/` |
| `First-Admin-Login.md` | first-runner | OAuth+PKCE flow; seeded super-admin credentials; UI tour entry | `docs/developers/architecture/services/control-plane/oauth-pkce-admin-auth-architecture.md`, `tools/db-migrate/prisma/seed.ts` |
| `Your-First-AI-Request.md` | first-runner | Create VK in UI; curl /v1/chat/completions; inspect traffic_event row | `examples/01-hello-world/`, `docs/users/api/openapi/ai-gateway/ai-gateway-v1.yaml`, `docs/users/features/cp-ui/ai-gateway.md` |
| `Your-First-Compliance-Capture.md` | first-runner | Configure SDK against compliance-proxy; trigger capture; query DB | `docs/developers/architecture/services/compliance-proxy/compliance-proxy-details-architecture.md`, `.claude/skills/test-compliance-proxy/` |
| `Installing-The-Desktop-Agent.md` | first-runner | macOS .pkg install; enrollment flow; quick verification | `docs/developers/architecture/services/agent/agent-enrollment-architecture.md`, `docs/users/features/flows/agent-enrollment.md` |
| `Examples-Catalog.md` | first-runner / explorer | `examples/` directory map + when to use which | `examples/README.md`, `examples/01-hello-world/` |
| `Troubleshooting-First-Run.md` | first-runner | Top 10 first-run failures: Docker not running, port conflicts, Prisma fail, missing `-config <svc>.dev.yaml`, `go.work` issues, GOWORK=off stale snapshots | `docs/developers/workflow/local-dev-debugging.md`, `.claude/skills/run-local/` |

---

### ⚙️ TECHNICAL — Concepts (8 pages)

| File | Audience | Job | Sources |
|---|---|---|---|
| `Architecture-Overview.md` | evaluator / contributor | System topology, control vs data plane, 5-service Mermaid (richer than Home) | `docs/developers/architecture/overview.md` |
| `The-Five-Services.md` | contributor | Hub / CP / AI Gateway / Compliance Proxy / Agent — purpose + ports + ownership | `docs/developers/architecture/overview.md`, `docs/developers/architecture/services/*/...-internals-architecture.md` |
| `Three-Traffic-Paths.md` | evaluator | Independent + parallel paths, attestation header dedup | `docs/developers/architecture/overview.md`, `docs/users/product/overview.md` |
| `Thing-Model-And-Config-Sync.md` | contributor | Pull-only config; shadow blob; needsPull; IoT terminology mapping | `docs/developers/architecture/cross-cutting/foundation/thing-model.md`, `docs/developers/architecture/cross-cutting/foundation/thing-config-sync-architecture.md` |
| `Control-Plane-Vs-Data-Plane.md` | evaluator | Why Hub down ≠ traffic stops; fail-open posture; emergency passthrough preview | `docs/developers/architecture/overview.md`, `docs/developers/architecture/cross-cutting/safety/emergency-passthrough-architecture.md` |
| `Fail-Open-Posture.md` | security reviewer | Where & why fail-open; the macOS NE binding; emergency passthrough; kill-switch | CLAUDE.md "NE fail-open" binding, `docs/developers/architecture/services/agent/agent-ne-fail-open-architecture.md`, `docs/developers/architecture/cross-cutting/safety/emergency-passthrough-architecture.md` |
| `Trust-Boundaries.md` | security reviewer | Service-to-service auth, internal token, mTLS, cookie/session boundaries | `docs/developers/architecture/overview.md` §10, `docs/developers/architecture/cross-cutting/safety/credentials-architecture.md` |
| `Canonical-Vs-Wire-Format.md` | contributor | Why canonical = OpenAI shape; per-spec adapter ownership; §3a rules summary | `docs/developers/architecture/services/ai-gateway/provider-adapter-architecture.md` |

---

### ⚙️ TECHNICAL — AI Gateway (13 pages)

| File | Audience | Job | Sources |
|---|---|---|---|
| `AI-Gateway-Overview.md` | contributor | Endpoint set, ingress catalog, capability matrix, request lifecycle Mermaid | `docs/developers/architecture/services/ai-gateway/ai-gateway-internals-architecture.md` |
| `AI-Gateway-Providers-And-Models.md` | evaluator / contributor | 29-model table, prod-validated vs adapter-only, capability flags | `docs/developers/architecture/services/ai-gateway/provider-coverage.md`, `tools/db-migrate/prisma/seed.ts` |
| `AI-Gateway-Provider-Adapters.md` | contributor | Canonical=OpenAI; §3a rules 1-8; codec/stream/error trio; adding new spec | `docs/developers/architecture/services/ai-gateway/provider-adapter-architecture.md` |
| `AI-Gateway-Ingress-Endpoints.md` | contributor | /v1/chat /v1/messages /v1/responses /v1/embeddings /v1/models — when each is used | `docs/developers/architecture/services/ai-gateway/normalization-architecture.md`, `docs/users/api/openapi/ai-gateway/*.yaml` |
| `AI-Gateway-Routing-Rules.md` | operator | Declarative match → resolved_model; rule ordering; admin UI flow | `docs/developers/architecture/services/ai-gateway/routing-architecture.md`, `docs/users/features/flows/routing-rule-lifecycle.md` |
| `AI-Gateway-Smart-Routing.md` | operator | Failover chain, cost-aware, sticky session | `docs/developers/architecture/services/ai-gateway/smart-routing-architecture.md` |
| `AI-Gateway-Prompt-Cache.md` | operator | Anthropic explicit (cache_control), OpenAI auto, Google contextCache; ROI fields; tier-config | `docs/developers/architecture/services/ai-gateway/prompt-cache-architecture.md` |
| `AI-Gateway-Response-Cache.md` | operator | Scope, threshold, traffic_event linkage; cache-hit / cache-miss-evict events | `docs/developers/architecture/services/ai-gateway/response-cache-architecture.md` |
| `AI-Gateway-Cost-Estimation.md` | operator | Single source of truth (Model row); cost stamping at handler + cache + dry-run paths | `docs/developers/architecture/services/ai-gateway/cost-estimation-architecture.md` |
| `AI-Gateway-Streaming.md` | contributor | SSE parity across ingresses; mid-stream hook abort; nonce isolation | `docs/developers/architecture/services/ai-gateway/normalization-architecture.md`, `docs/developers/architecture/services/ai-gateway/provider-adapter-architecture.md` §6 |
| `AI-Gateway-Hooks.md` | operator | 3-stage pipeline (request / pre-upstream / response); decisions: allow/redact/block; streaming hook semantics | `docs/developers/architecture/services/ai-gateway/hook-architecture.md`, `docs/users/features/flows/hook-rollout.md` |
| `AI-Gateway-Virtual-Keys-Quotas.md` | operator | VK creation; scopes; rate/quota enforcement; org/project/personal join chains | `docs/developers/architecture/cross-cutting/safety/quota-architecture.md`, `docs/users/features/flows/vk-lifecycle.md` |
| `AI-Gateway-Error-Taxonomy.md` | contributor | Provider errors normalized via spec error normalizer; canonical error shapes | `docs/developers/architecture/cross-cutting/safety/error-taxonomy-architecture.md` |

---

### ⚙️ TECHNICAL — Compliance Proxy (7 pages)

| File | Audience | Job | Sources |
|---|---|---|---|
| `Compliance-Proxy-Overview.md` | evaluator | Where it sits, what it captures, capability vs AI Gateway | `docs/developers/architecture/services/compliance-proxy/compliance-proxy-details-architecture.md` |
| `Compliance-Proxy-TLS-Interception.md` | operator | MITM mechanics; root CA install; cert chain; bypass for pinned hosts | `docs/developers/architecture/services/compliance-proxy/compliance-pipeline-architecture.md` |
| `Compliance-Proxy-Traffic-Event-Taxonomy.md` | contributor | source/kind/spec/endpoint_type matrix; tier-1 vs tier-2 detection | `docs/developers/architecture/cross-cutting/foundation/endpoint-typology-architecture.md`, `docs/developers/architecture/services/compliance-proxy/compliance-proxy-details-architecture.md` |
| `Compliance-Proxy-Normalization.md` | contributor | Tier-1 canonical normalize; Tier-2 NonJSONDetector framework | `docs/developers/architecture/services/ai-gateway/normalization-architecture.md`, code under `packages/shared/transport/normalize/extract/detector.go` |
| `Compliance-Proxy-PII-Redaction.md` | operator | Policy model; redact vs block; per-route policy | `docs/developers/architecture/cross-cutting/safety/pii-redaction-policy-architecture.md` |
| `Compliance-Proxy-Domain-Device-Predicates.md` | operator | Allow/deny model; device groups; predicate evaluation order | `docs/developers/architecture/services/compliance-proxy/domain-device-predicate-architecture.md` |
| `Compliance-Proxy-Connecting-Your-SDK.md` | first-runner / operator | Configure OpenAI SDK, Anthropic SDK, Cursor IDE, Claude Code to route through the proxy | `.claude/skills/test-compliance-proxy/`, `.claude/skills/test-cursor-adapter/` |

---

### ⚙️ TECHNICAL — Desktop Agent (8 pages)

| File | Audience | Job | Sources |
|---|---|---|---|
| `Agent-Overview.md` | evaluator / contributor | Forwarder model; parity with compliance-proxy; OS-level capture rationale | `docs/developers/architecture/services/agent/agent-forwarder-architecture.md`, `docs/developers/architecture/services/agent/agent-internals-sibling-pairs-architecture.md` |
| `Agent-macOS-NE-Architecture.md` | contributor | NETransparentProxyProvider; fail-open invariants; signing chain | `docs/developers/architecture/services/agent/agent-macos-platform-architecture.md`, `docs/developers/architecture/services/agent/agent-ne-fail-open-architecture.md`, `docs/developers/architecture/services/agent/macos-build-signing-architecture.md` |
| `Agent-Linux-Platform.md` | operator | systemd unit; paths abstraction; deployment status | `docs/developers/architecture/services/agent/agent-linux-platform-architecture.md`, `docs/developers/architecture/services/agent/agent-paths-abstraction-architecture.md` |
| `Agent-Windows-Status.md` | operator | Current status, roadmap (E-numbers), gating items | `docs/developers/architecture/services/agent/agent-windows-platform-architecture.md`, `docs/operators/ops/agent-windows-build.md` |
| `Agent-Enrollment-Attestation.md` | operator / security | First-boot ceremony; mTLS keystore; attestation | `docs/developers/architecture/services/agent/agent-enrollment-architecture.md`, `docs/developers/architecture/services/agent/agent-sso-enrollment-architecture.md`, `docs/developers/architecture/services/agent/agent-attestation-architecture.md`, `docs/users/features/flows/agent-enrollment.md` |
| `Agent-Auto-Update.md` | operator / security | Ed25519 signature verification; rollback policy; rings | `docs/developers/architecture/services/agent/agent-autoupdater-architecture.md`, `docs/operators/ops/runbooks/r-version-pinning-rollout-rings.md` |
| `Agent-Policy-Evaluation.md` | contributor | Local hook stages; shadow inputs; what exemptions actually do vs not | `docs/developers/architecture/services/agent/agent-policy-eval-architecture.md`, `docs/developers/architecture/services/agent/agent-exemption-grants-architecture.md` |
| `Agent-Privacy-Data-Flows.md` | security reviewer / end-user | Exactly what leaves the device, what is redacted, what auditors see | `docs/developers/architecture/services/agent/agent-telemetry-architecture.md`, `docs/users/features/agent-ui/settings.md` (AboutFooter on Settings) |

---

### ⚙️ TECHNICAL — Control Plane (10 pages)

| File | Audience | Job | Sources |
|---|---|---|---|
| `Control-Plane-Overview.md` | contributor | Admin REST + UI; IAM-gated; Echo framework; package layout | `docs/developers/architecture/services/control-plane/control-plane-internals-architecture.md` |
| `Control-Plane-Admin-UI-Tour.md` | new admin | Section-by-section walkthrough of the CP UI | `docs/users/features/cp-ui/overview.md` and all `docs/users/features/cp-ui/*.md` |
| `Control-Plane-IAM-Model.md` | security reviewer / contributor | Resources, actions, NRN, policies, allowedActions; the IAM impact review binding | `docs/developers/architecture/services/control-plane/iam-identity-architecture.md`, `.cursor/rules/iam-impact-review.mdc`, `docs/users/features/cp-ui/iam.md` |
| `Control-Plane-Authentication.md` | contributor / new admin | Admin OAuth+PKCE; session lifecycle; cookie/header flows | `docs/developers/architecture/services/control-plane/oauth-pkce-admin-auth-architecture.md`, `docs/developers/architecture/services/control-plane/jwt-verifier-architecture.md` |
| `Control-Plane-SSO-Okta-AzureAD.md` | operator | IdP wiring; SP/IdP positioning (Nexus is SP); IdP federation flow | `docs/developers/architecture/services/control-plane/idp-sso-architecture.md`, `docs/users/features/flows/idp-federation.md` |
| `Control-Plane-Multi-Tenancy.md` | operator | Org / Project / NexusUser join chains; VK org resolution (dual chain) | `docs/developers/architecture/services/control-plane/tenancy-architecture.md`, `docs/developers/architecture/services/control-plane/vk-org-resolution.md` |
| `Control-Plane-Audit-Log.md` | compliance lead | Schema, retention, query patterns, immutability | `docs/developers/architecture/cross-cutting/observability/audit-pipeline-architecture.md`, `docs/developers/architecture/cross-cutting/observability/admin-audit-log-coverage.md` |
| `Control-Plane-Alerting-Rules.md` | operator | Built-in vs custom rules; rule evaluation flow | `docs/developers/architecture/cross-cutting/observability/alerting-architecture.md`, `docs/users/features/cp-ui/alerts.md`, `docs/users/features/flows/alert-evaluation.md` |
| `Control-Plane-SIEM-Bridge.md` | compliance lead | Forwarder, OTEL/Webhook sinks, payload shapes | `docs/developers/architecture/cross-cutting/observability/siem-bridge-architecture.md` |
| `Control-Plane-Infrastructure-Pages.md` | operator | CP UI Infrastructure section: Nodes, Config Sync, Jobs, Kill Switch | `docs/users/features/cp-ui/infrastructure.md`, `docs/developers/architecture/cross-cutting/foundation/jobs-architecture.md`, `docs/developers/architecture/cross-cutting/safety/kill-switch-architecture.md` |

---

### ⚙️ TECHNICAL — Cross-Cutting (9 pages)

| File | Audience | Job | Sources |
|---|---|---|---|
| `Hub-Coordination.md` | contributor | Why Hub exists; Thing registry; service-call framework | `docs/developers/architecture/services/hub/nexus-hub-internals-architecture.md`, `docs/developers/architecture/cross-cutting/foundation/thing-model.md` |
| `Service-Call-Framework.md` | contributor | Hub-routed RPC; envelope format; auth headers | `docs/developers/architecture/cross-cutting/foundation/service-call-framework.md` |
| `Configuration-Architecture.md` | contributor | 4-layer model + R1-R5 invariants; configKey catalog | `docs/developers/architecture/cross-cutting/foundation/configuration-architecture.md`, `packages/shared/schemas/configkey/` |
| `Storage-Cache-MQ-Stack.md` | contributor | Postgres + Valkey + NATS JetStream; what each holds; no Redis pub/sub | `docs/developers/architecture/cross-cutting/storage/cache-multi-tier-architecture.md`, `docs/developers/architecture/cross-cutting/foundation/mq-architecture.md` |
| `Spillstore.md` | contributor / operator | Why large bodies spill; S3 vs local FS; presigned URL flow | `docs/developers/architecture/cross-cutting/storage/spillstore-architecture.md` |
| `Credentials-Storage.md` | security reviewer | AES-256-GCM; key sourcing (env-only binding); rotation propagation | `docs/developers/architecture/cross-cutting/safety/credentials-architecture.md` |
| `Observability-Stack.md` | operator | Prometheus naming, OTEL spans, trace_id propagation, runtime introspection | `docs/developers/architecture/cross-cutting/observability/prometheus-naming-architecture.md`, `docs/developers/architecture/cross-cutting/observability/otel-pipeline-architecture.md`, `docs/developers/architecture/cross-cutting/observability/trace-id-propagation-architecture.md`, `docs/developers/architecture/cross-cutting/observability/runtime-introspection-architecture.md` |
| `Emergency-Passthrough.md` | operator | 3-tier safety; ResolvedRequest L4; IAM; Hub 60s reconcile | `docs/developers/architecture/cross-cutting/safety/emergency-passthrough-architecture.md` |
| `Kill-Switch.md` | operator | Hub shadow Category A; immediate vs reconcile semantics; IAM gating | `docs/developers/architecture/cross-cutting/safety/kill-switch-architecture.md`, `docs/users/features/flows/kill-switch-and-passthrough.md` |

---

### ⚙️ TECHNICAL — Deployment (8 pages)

| File | Audience | Job | Sources |
|---|---|---|---|
| `Deployment-Models.md` | operator | Single-node baseline → multi-node roadmap; air-gapped status | `docs/users/product/deployment-models.md`, `docs/operators/ops/deployment.md` |
| `Deployment-Single-Node-Production.md` | operator | EC2 recipe walkthrough — link to canonical runbook; summary of steps + gotchas | `docs/operators/ops/ec2-single-node.md`, `docs/operators/ops/deployment.md` |
| `Deployment-Hardware-Sizing.md` | operator | Per-service CPU/RAM/disk math; provider concurrency; cache sizing | `docs/operators/ops/deployment.md`, `docs/operators/ops/runbooks/perf-2026-05-20-nexus-traffic-event.md` |
| `Deployment-Environment-Variables.md` | operator | env-only secrets binding; [MUST MATCH] catalog; link to .env.example | `.env.example`, `docs/developers/workflow/local-dev-debugging.md` "Environment variables" |
| `Deployment-TLS-Certificates.md` | operator | Public TLS for admin/API; internal mTLS PKI; cert rotation | `docs/operators/ops/pki-and-certs.md` |
| `Deployment-Database-Migrations.md` | operator | Prisma flow; baseline + in-flight; production-side scripts | `docs/developers/architecture/cross-cutting/storage/db-migration-mechanics-architecture.md`, `docs/operators/ops/runbooks/prod-deploy-data-changes.md` |
| `Deployment-Cache-MQ.md` | operator | Valkey 8 / Redis 7; NATS JetStream 2+; capacity sizing | `docs/operators/ops/redis-setup.md`, `docs/operators/ops/runbooks/e61-valkey-migration.md`, `docs/developers/architecture/cross-cutting/foundation/mq-architecture.md` |
| `Deployment-Spillstore-Setup.md` | operator | S3 or local FS; presigned-URL flow; agent presign | `docs/developers/architecture/cross-cutting/storage/spillstore-architecture.md`, `docs/users/api/openapi/admin/e37-s2-agent-presigned-spill.yaml` |

---

### ⚙️ TECHNICAL — Operations (8 pages)

| File | Audience | Job | Sources |
|---|---|---|---|
| `Operations-Runbook-Index.md` | operator | Map of runbooks under `docs/operators/ops/runbooks/` with one-line summaries | `docs/operators/ops/runbooks/*.md` |
| `Operations-Day-2-Cheatsheet.md` | operator | Common ops: restart service, check logs, drain traffic, view metrics | `docs/developers/workflow/local-dev-debugging.md`, `docs/operators/ops/monitoring.md` |
| `Operations-Backup-Restore.md` | operator | Postgres + Valkey + spillstore backup; restore procedure | `docs/operators/ops/backup-dr.md` |
| `Operations-Credential-Rotation.md` | operator | Provider keys, encryption keys, internal-service tokens, mTLS — rotation flows | `docs/developers/architecture/cross-cutting/safety/credentials-architecture.md` §rotation, `docs/users/features/flows/credential-rotation.md` |
| `Operations-Migrations-On-Prod.md` | operator | Applying migrations safely to running prod | `docs/operators/ops/runbooks/prod-deploy-data-changes.md`, `.claude/skills/prod-deploy/` |
| `Operations-Capacity-Performance.md` | operator | Tuning levers; provider concurrency; rate limit headers; cache hit-rate | `docs/operators/ops/runbooks/perf-2026-05-20-nexus-traffic-event.md`, `docs/operators/ops/runbooks/perf-2026-05-20-nexus-vs-bifrost.md` |
| `Operations-Logs-Metrics-Traces.md` | operator | Log paths, Prometheus endpoints, OTEL spans, diag event triage | `docs/developers/workflow/local-dev-debugging.md`, `docs/developers/architecture/cross-cutting/observability/diag-event-triage-architecture.md`, `docs/operators/ops/monitoring.md` |
| `Operations-FAQ.md` | operator | 10-15 day-2 questions: how do I…? what does X log mean? | aggregated from `docs/operators/ops/**` |

---

### ⚙️ TECHNICAL — API Reference (6 pages)

| File | Audience | Job | Sources |
|---|---|---|---|
| `API-Overview.md` | integrator | Surface map: admin / VK / hub / agent-cmd; auth strategy per surface | `docs/users/api/openapi/`, `docs/developers/architecture/overview.md` §API |
| `API-AI-Gateway.md` | integrator | /v1/chat /v1/messages /v1/responses /v1/embeddings /v1/models — when each is used, capability matrix | `docs/users/api/openapi/ai-gateway/ai-gateway-v1.yaml`, `docs/users/api/openapi/ai-gateway/e56-s1-responses.yaml`, `docs/users/api/openapi/ai-gateway/e62-s2-embeddings.yaml` |
| `API-Admin.md` | integrator | Admin API map per resource; link to per-epic OpenAPI specs | `docs/users/api/openapi/admin/*.yaml` |
| `API-Hub.md` | integrator | Hub HTTP + WebSocket protocol; envelope; auth | `docs/users/api/openapi/hub/e3-hub-api.yaml`, `docs/developers/architecture/services/hub/nexus-hub-internals-architecture.md` |
| `API-Authentication.md` | integrator | VK bearer, admin OAuth, internal-service token | `docs/users/api/openapi/auth/*.yaml`, `docs/developers/architecture/cross-cutting/safety/credentials-architecture.md` |
| `API-OpenAPI-Index.md` | integrator | Catalog of every OpenAPI YAML, by epic-story | `docs/users/api/openapi/**/*.yaml` |

---

### ⚙️ TECHNICAL — Security (8 pages)

| File | Audience | Job | Sources |
|---|---|---|---|
| `Security-Reporting-A-Vulnerability.md` | reporter | How to report; what we promise | `SECURITY.md` |
| `Security-Threat-Model.md` | security reviewer | Assets, adversaries, controls; trust boundaries | `docs/developers/architecture/overview.md` §10-11, CLAUDE.md security-relevant bindings |
| `Security-Credential-Storage.md` | security reviewer | AES-256-GCM; encryption-key sourcing; rotation propagation | `docs/developers/architecture/cross-cutting/safety/credentials-architecture.md` |
| `Security-Secrets-Handling.md` | security reviewer | env-only binding rationale; [MUST MATCH] cross-service shared secrets | CLAUDE.md "Secrets are env-only", `.env.example` |
| `Security-Network-Safety.md` | security reviewer | NE fail-open 5 invariants; emergency passthrough; kill switch | CLAUDE.md "NE fail-open" binding, `docs/developers/architecture/services/agent/agent-ne-fail-open-architecture.md` |
| `Security-Audit-Forensics.md` | compliance lead | What's logged, retention, immutability, SIEM forwarding, spillstore | `docs/developers/architecture/cross-cutting/observability/audit-pipeline-architecture.md`, `docs/developers/architecture/cross-cutting/storage/data-retention-purge-architecture.md` |
| `Security-Supply-Chain.md` | security reviewer | License posture; Ed25519 agent update signing; go.work pinning; Valkey BSD | `LICENSE`, `docs/developers/architecture/services/agent/agent-autoupdater-architecture.md`, `docs/developers/workflow/conventions.md` |
| `Security-Compliance-Posture.md` | compliance lead | SOC2/ISO/HIPAA readiness — current state (not promise) | `docs/operators/ops/compliance.md` |

---

### 🧪 DEVELOPMENT — Core (8 pages)

| File | Audience | Job | Sources |
|---|---|---|---|
| `Contributing.md` | first contributor | Where to start; 3-doc pre-edit rule; SDD pipeline overview; PR review pointer | `CONTRIBUTING.md`, `CLAUDE.md`, `docs/developers/workflow/ai-workflow.md` |
| `Dev-Repo-Structure.md` | first contributor | Top-level tour; `packages/`, `tools/`, `docs/`, `examples/`, `scripts/`, `.claude/`, `.cursor/` | `docs/developers/architecture/project-structure.md`, `docs/README.md` |
| `Dev-Local-Development.md` | contributor | Daily workflow; log paths; kill/restart; cp_login / cp_curl helpers | `docs/developers/workflow/local-dev-debugging.md` |
| `Dev-SDD-Pipeline.md` | contributor | Plan→Todo→Arch→Req→Story→OpenAPI→Code→Test→Verify — each step explained | CLAUDE.md "Mandatory Development Workflow", `docs/developers/workflow/ai-workflow.md`, `.cursor/rules/sdd-workflow.mdc` |
| `Dev-Code-Doc-Lockstep.md` | contributor | Why & how; lockstep config; per-doc trigger table | CLAUDE.md "Code / doc lockstep", `scripts/doc-lockstep.config.mjs`, `.cursor/rules/code-doc-lockstep.mdc` |
| `Dev-Testing-Coverage.md` | contributor | 95% rule + allowlist categories; what counts as observable-behavior assertion | CLAUDE.md "Unit test coverage ≥95%", `scripts/check-go-coverage.sh`, `.cursor/rules/unit-test-coverage-95.mdc`, `docs/developers/workflow/testing.md`, `docs/developers/workflow/coverage-allowlist-methodology.md` |
| `Dev-Code-Review-Checklist.md` | reviewer | The CI bindings + PR review questions; how to use the project-review skill | `docs/developers/workflow/conventions.md` §11, `.claude/skills/project-review/` |
| `Dev-Release-Process.md` | maintainer | prod-YYYYMMDD tagging; deploy skill; migration sequencing | `.claude/skills/prod-deploy/`, `docs/operators/ops/runbooks/prod-deploy-data-changes.md` |

---

### 🧪 DEVELOPMENT — Recipes (8 pages)

Each Recipe is a step-by-step "how do I add X" how-to. Recipes link OUT to the
canonical architecture doc for "why" — the recipe is "how".

| File | Audience | Job | Sources |
|---|---|---|---|
| `Recipe-Index.md` | new contributor | Catalog: when to use which recipe | (this file) |
| `Recipe-Adding-A-Provider-Adapter.md` | contributor | Codec + stream session + error normalizer + hub_ingress; `/adapter-conformance-check` | `docs/developers/architecture/services/ai-gateway/provider-adapter-architecture.md`, `.claude/skills/add-provider-adapter/` |
| `Recipe-Adding-A-Hook.md` | contributor | New hook stage / new hook type; HookConfig schema; rollout flow | `docs/developers/architecture/services/ai-gateway/hook-architecture.md`, `docs/users/features/flows/hook-rollout.md` |
| `Recipe-Adding-A-Thing-Type.md` | contributor | New Thing type; shadow blob; configKey catalog; configKey registration | `docs/developers/architecture/cross-cutting/foundation/thing-model.md`, `docs/developers/architecture/cross-cutting/foundation/configuration-architecture.md`, `.claude/skills/add-shadow-key/` |
| `Recipe-Adding-A-CP-UI-Section.md` | contributor | New section in admin UI; useApi queryKey domain prefix; allowedActions wiring | `.claude/skills/add-cp-ui-section/`, `docs/developers/workflow/conventions.md` §TypeScript |
| `Recipe-Adding-A-Runbook.md` | contributor / operator | runbook template; lockstep entry; cross-link to architecture doc | `docs/operators/ops/runbooks/*.md` (as exemplars) |
| `Recipe-Adding-An-Audit-Event.md` | contributor | New audit event type; pipeline wiring; CP UI surface | `docs/developers/architecture/cross-cutting/observability/audit-pipeline-architecture.md` |
| `Recipe-Adding-An-IAM-Action.md` | contributor | New IAM action; resource/NRN registration; UI allowedActions; iam-impact-review skill | `.cursor/rules/iam-impact-review.mdc`, `.claude/skills/iam-impact-review/`, `docs/developers/architecture/services/control-plane/iam-identity-architecture.md` |

---

### 🧪 DEVELOPMENT — AI Vibe-Coding Workbench (6 pages — DIFFERENTIATOR)

This group exists because the workbench methodology is a project differentiator
the OSS community asks about. The wiki gives it top-level visibility instead of
burying it under Contributing.

| File | Audience | Job | Sources |
|---|---|---|---|
| `Workbench-Overview.md` | fork-adopter / contributor | What the workbench is; how the pieces fit (CLAUDE.md + cursor rules + skills + lint suite); evidence of effect | `README.md` §AI vibe-coding workbench, `CLAUDE.md`, `docs/developers/workflow/ai-workflow.md` |
| `Workbench-CLAUDE-md-Anatomy.md` | fork-adopter | Section-by-section breakdown of CLAUDE.md; what each binding does; why each exists | `CLAUDE.md` |
| `Workbench-Cursor-Rules.md` | fork-adopter | `.cursor/rules/*.mdc` catalog; alwaysApply vs targeted; how rules trigger | `.cursor/rules/*.mdc` |
| `Workbench-Claude-Code-Skills.md` | fork-adopter | `.claude/skills/*` catalog; how to invoke; how to author a new skill | `docs/developers/workflow/ai-skill-catalog.md`, `.claude/skills/README.md`, `.claude/skills/*/` |
| `Workbench-Forking-Guide.md` | fork-adopter | Step-by-step: extract the workbench, adapt CLAUDE.md to your repo, pick cursor rules, port skills | `docs/developers/workflow/ai-workflow.md` "Forking and adopting" section if present, otherwise synthesize |
| `Workbench-Lessons-Learned.md` | fork-adopter | What patterns proved load-bearing (e.g. 95% coverage binding, code-doc lockstep, 3-doc pre-edit rule, fail-open); failure modes prevented | CLAUDE.md feedback memories, `docs/developers/workflow/ai-workflow.md` |

---

### 👥 COMMUNITY (5 pages)

| File | Audience | Job | Sources |
|---|---|---|---|
| `Community-Support-Channels.md` | new user | Where to ask questions: GitHub Issues, Discussions, Security email | `README.md`, `SECURITY.md` |
| `Community-Code-Of-Conduct.md` | community | CoC text | `CODE_OF_CONDUCT.md` if present, else Contributor Covenant boilerplate (with maintainer review) |
| `Community-Governance.md` | community | How decisions get made; maintainer authority; PR acceptance criteria | `CONTRIBUTING.md` §governance if present, else draft from CLAUDE.md workflow rules |
| `Community-Maintainers.md` | community | Current maintainers + areas of ownership | `MAINTAINERS.md` if present, else compile from git log + main contributors |
| `License.md` | anyone | Apache-2.0 explanation + practical implications (commercial use, attribution) | `LICENSE`, `NOTICE` if present |

---

## Auxiliary files

| File | Purpose |
|---|---|
| `_Sidebar.md` | Hierarchical sidebar rendered on every wiki page. Updated last (after all pages exist). |
| `_Footer.md` | Short footer: link to repo, license, security email. Updated last. |
| `outline.md` (this file, under docs/handoffs/E76-wiki-expansion/) | THIS file. The locked IA + per-page contract. |
| `decisions.md` | Decision log — DEC-001..010 from original 8-page program; DEC-011..NNN for the v2 expansion. |
| `page-template.md` | Canonical page shape every subagent follows. |
| `style-guide.md` | Voice, Mermaid usage, link conventions, terminology. |
| `cleanup-candidates.md` | Subagent writeback file for dead code / docs spotted during research. Opus reviews each candidate centrally before any deletion. |

## Phase plan

| Phase | Groups | Page count | Dispatch pattern |
|---|---|---|---|
| **P0** | Lock IA v2 + write coordination infra | 0 wiki pages; 5 infra files | Opus only |
| **P1** | Home + Product Overview + Concepts + Getting Started + FAQ + Roadmap | 28 pages | 4 Sonnet subagents parallel |
| **P2** | AI Gateway + Compliance Proxy + Desktop Agent + Cross-Cutting | 37 pages | 4-6 Sonnet subagents parallel |
| **P3** | Control Plane + Deployment + Operations + API Reference + Security | 40 pages | 5 Sonnet subagents parallel |
| **P4** | Features + Development + Recipes + Workbench + Community | 37 pages | 5-6 Sonnet subagents parallel; Opus drafts `Workbench-Overview.md` directly |

Total: ~142 page slots in the table above; many subsystem overviews are
counted once in the totals (28 + 37 + 40 + 37 ≈ 142 minus ~47 cross-counted
overviews/indexes = ~95 unique pages). The total `find docs/_wiki -name '*.md'`
target after P4 = ~95.

## Brainstorm escalation triggers

Opus runs `/brainstorm` (internal) when any of the following surfaces from a
subagent or during review:

1. **IA conflict** — a page topic doesn't fit cleanly in its group; split / merge / move.
2. **Doc/code mismatch** — research surfaces a doc that disagrees with current code (log as cleanup candidate + decision).
3. **Sidebar overload** — a group grows past 13 pages or shrinks below 3; rebalance.
4. **Workbench positioning** — any decision about how to frame the methodology to the public.
5. **Cleanup candidate** — any candidate that is non-trivially dead (referenced elsewhere even tangentially).

Every brainstorm outcome lands in `decisions.md` as a new DEC-NNN entry with
the rejected alternatives.

## Out of scope (still)

- No translated wiki pages. English only per CLAUDE.md.
- No API YAML inlined in the wiki. API pages reference OpenAPI specs by path.
- No SDD epic/story texts inlined. Roadmap pages summarize and link.
- No `docs/_wiki/` in code-doc lockstep map (E76-DEC-005 stays).
