# Workbench Claude Code Skills

*Audience: contributors and fork-adopters who want to understand, invoke, or author skills.*

The `.claude/skills/` directory holds 25 invocable procedures that an AI agent fires as `/skill-name` in Claude Code. Each skill is a self-contained runbook: pre-conditions, numbered steps, binding rules the skill enforces, verification gate, and recovery notes for failure mid-execution. Skills encode tacit operational knowledge — the "which order to restart services", "which five stamp sites to update for a new token field", "which exact audit to run before merging an endpoint change" — into something an agent can execute reliably. The pattern: any procedure done three or more times manually, with five or more steps where order matters and a wrong step causes pain, becomes a skill.

---

## Deployment and operations skills

These skills encode production operations. Most are tightly coupled to this repo's production infrastructure and require adaptation when forking.

| Skill | Job | Fork-portable? |
|---|---|---|
| `prod-deploy` | Full production release: git tag, build all 4 Go services and UI, upload to EC2, replace binaries, restart services in correct order, verify nodes online, run post-deploy smoke. Includes mandatory smoke binding — skipping requires explicit user approval. | No — hardcodes `taskforce10x.com`. Adapt by replacing EC2 SSH target, binary paths, and restart sequence. Keep the mandatory post-deploy smoke binding. |
| `prod-login` | OAuth + PKCE login against the production Control Plane. Caches the bearer token and exposes `cp_login`, `cp_curl`, `cp_curl_code`, and `cp_curl_full` helpers for subsequent admin API calls in the session. | No — hardcodes the production OAuth endpoint. Adapt by replacing the URL and seeded admin credentials. |
| `prod-debug` | Diagnose production issues: service logs, DB queries for traffic, analytics, cache, credentials, nodes, Redis inspection, NATS stream status, config and shadow inspection, metrics, and common failure patterns with known fixes. | No — hardcodes `taskforce10x.com` DB and service paths. Adapt by replacing connection strings and service paths. The failure-pattern catalog is highly portable. |
| `build-agent` | macOS Agent build, sign, notarize, package via the official path. Single source of truth for signing identities, provisioning profiles, notarytool keychain profile, macOS 26 launch-constraint requirements, and the install/uninstall sequence. | Partial — adapt code-signing identities, provisioning profile names, and bundle IDs. The sequencing discipline (sign before notarize before package) is universal. |
| `run-local` | First-run bootstrap: bring the full local stack up from a clean clone. Starts PostgreSQL, Valkey, and NATS via docker-compose, then the four Go services (Hub, Control Plane, AI Gateway, Compliance Proxy) and the Control Plane UI (Vite). Encodes every gotcha a first-time OSS contributor hits (missing `-config` flag, `go.work` not in build context, Prisma migration ordering). | Yes — generic local bring-up; adapts to any Go + React stack with minor port/config adjustments. |
| `add-shadow-key` | Add a new Thing config key end-to-end: schema update, configtypes constants, `configkey` registration (`ValidByThingType`, `TypedRegistry`), Hub publisher wiring, service receiver wiring, CP UI surface. | Yes — generic Thing-model operation following the 4-layer configuration architecture. |

---

## End-to-end testing skills

These skills run smoke and integration tests against a running stack.

| Skill | Job | Fork-portable? |
|---|---|---|
| `test-all` | Full regression program: preflight → L1 smoke → L1 Go integration → L2 protocol → L3 AI-judge → L4 Playwright UI. Single "did my change break anything" entry point. ~$0.10/run. | Partial — depends on `tests/.env.local` configured with running services and provider credentials. |
| `smoke-gateway` | Full-surface AI Gateway + Control Plane smoke: every model in the catalog (non-stream + SSE + 2-turn cache), routing-rule auto-management, `traffic_event` DB cross-check, Prometheus counter delta, auto-fix loop on failures. Covers 29 models × 4 ingresses. | Partial — needs virtual keys, provider API credentials, and a running AI Gateway. The auto-fix loop (investigate → fix code → rebuild → restart → rerun) is a portable pattern. |
| `test-compliance-proxy` | Compliance proxy MITM end-to-end: verifies HTTPS provider traffic interception on `:3128`, checks the compliance pipeline runs, verifies matching `traffic_event` rows and Prometheus counters. | Yes — local-runnable with just the compliance proxy and a running PostgreSQL. |
| `test-cursor-adapter` | Synthetic protobuf test against the Cursor IDE Tier-1 normalizer. Sends a hand-rolled `GetChatRequest` through the compliance proxy and verifies the `traffic_event_normalized` row is correct. | Partial — needs the compliance proxy and a seeded DB. The protobuf construction logic is repo-specific. |
| `test-geminiweb-adapter` | Synthetic `batchexecute` test against the Gemini Web Tier-1 normalizer. Same pattern as `test-cursor-adapter`. | Partial — same as above; replace batchexecute payload format for a different provider. |
| `test-openai-responses` | Synthetic test for the OpenAI Responses-API ingress. Five hand-rolled requests covering text non-stream, SSE, function-call SSE, structured outputs, and reasoning-effort. | Partial — needs a running AI Gateway and a virtual key with OpenAI credentials. |

---

## Architecture and design skills

These skills enforce or assist with the architectural discipline.

| Skill | Job | Fork-portable? |
|---|---|---|
| `spec-writing` | SDD discipline end-to-end: comprehensive Requirements → SDD → OpenAPI flow with templates, review gates, and the 4-question completion self-audit. Produces `docs/developers/specs/e<N>-<name>.md` and the corresponding SDD and OpenAPI files. | Yes — generic SDD workflow; swap the file naming pattern for your own epic/story scheme. |
| `add-provider-adapter` | 9-step procedure for adding a new LLM provider adapter. Maps to the 7 architectural rules in `provider-adapter-architecture.md`: codec (request + response), stream session (chunk accumulator), error normalizer, `hub_ingress` wiring, smoke verification. | Yes — adapt to your provider adapter model. The codec/stream/error trio pattern is broadly applicable to any protocol translation layer. |
| `adapter-conformance-check` | Audits all existing provider adapters against the 7 `provider-adapter-architecture.md` §3a rules. Surfaces per-adapter logic that has leaked into the generic dispatcher, canonical-format violations, and missing streaming parity. | Yes — adapt the 7 rules to your own adapter architecture. |
| `add-cp-ui-section` | Add a new Control Plane admin section: route registration, sidebar entry, breadcrumb, page scaffold, i18n keys for all 3 locales, IAM action wiring (with the 5-step IAM audit). | Yes — adapt to your React + routing + IAM model. The i18n + IAM audit discipline is portable. |
| `project-review` | Multi-role review: architect perspective (component coupling, blast radius, missing architecture docs), SRE perspective (failure modes, observability, runbook gaps), security perspective (IAM drift, credential handling, NE safety), and product perspective (user value, complexity cost). | Yes — generic; adapt the review axes to your domain. |

---

## Debug and audit skills

These skills diagnose problems and audit code/doc compliance.

| Skill | Job | Fork-portable? |
|---|---|---|
| `frontend-bug-trace` | Page → API → backend → DB tracing for Control Plane UI bugs. Fixes mismatches between UI state, API responses, and DB rows. Verifies via admin login and DB query. | Yes — generic web-app debugging pattern. |
| `i18n-gap-check` | Cross-bundle i18n audit: missing EN keys (highest priority — renders raw key strings), ES/ZH translation gaps, orphan keys in ES/ZH not in EN, EN keys unused in source, dynamic template literals (manual review list), hardcoded English in `.tsx` that bypasses `t()`. Covers control-plane-ui and agent-ui with the shared `packages/ui-shared` namespace. | Yes — generic 3-locale i18n check; adapt namespace and bundle paths. |
| `iam-impact-review` | When a PR adds, moves, renames, or removes an admin API endpoint or sidebar entry: audits 5 layers in lockstep — UI `allowedActions`, handler `iamMW(...)`, IAM action catalog, managed-policy fixture, and seed data. Surfaces any layer that is out of sync. | Yes — adapt to your own IAM model; the 5-layer parity check pattern applies to any RBAC system. |
| `ne-fail-open-audit` | macOS NetworkExtension safety-critical audit: synchronous decision enforcement, fail-open timeout verification, absence of hardcoded enforcement lists, `isLikelyXyz = true` pattern scan, system-DNS process allowlist check. | Partial — macOS NE specific, but the fail-open pattern (every async path must have a timeout that defaults to passthrough) applies to any intercepting proxy. |
| `gap-review` | Audits gaps between SDD docs (source of truth) and actual code: architecture/requirements/OpenAPI parity, acceptance criteria coverage in tests, missing runbook entries, stale feature docs. Produces a prioritized punch list. | Yes — generic SDD-parity audit; adapt the doc tree structure. |
| `pre-edit-reader` | The 3-doc pre-edit reading enforcement: opens architecture doc, feature doc, and conventions doc for the area being changed before code touches disk. Returns a summary confirming the three reads are done. | Yes — generic discipline; swap the three doc paths for your own. |
| `arch-doc-trigger-check` | Checks that every `docs/developers/architecture/**/*-architecture.md` file has a corresponding row in the trigger map (`docs/developers/architecture/README.md`). New architecture docs without trigger entries are invisible to the pre-edit reading rule. | Yes — adapt the trigger map path. |
| `frontend-arch-review` | Design-token compliance audit for the UI: hex/rgba leak detection in `*.module.css` and `style={{}}`, theme/mode safety check, token layer violations. | Yes — adapt the token taxonomy and glob patterns. |

---

## How to author a new skill

A skill warrants creation when: the procedure is done three or more times manually, it has five or more steps where order matters, a wrong step causes a real problem (lost data, production incident, signed binary that won't install), and there is a verification gate that is hard to remember. One-off tasks, commands that are already an alias, and procedures already covered by an existing skill (extend instead of fork) do not warrant a new skill.

Every skill folder contains a `SKILL.md` with:

1. Description and trigger keywords — what invokes it.
2. Pre-conditions — what the environment must look like before firing.
3. Step-by-step procedure — numbered, concrete, verifiable steps.
4. Binding rules — hard contracts the skill enforces (e.g., the post-deploy smoke in `prod-deploy`).
5. Output and verification — what "done" looks like and where reports land.
6. Recovery and known failure modes — what to do when the skill fails mid-execution.

The longer skills (`prod-deploy` 518 lines, `spec-writing` 486 lines, `build-agent` 426 lines, `project-review` 469 lines) read like operational books — they encode hard-won failure modes that would otherwise live only in institutional memory.

---

## Canonical docs

- [`ai-skill-catalog.md`](https://github.com/AlphaBitCore/nexus-gateway/blob/main/docs/developers/workflow/ai-skill-catalog.md) — the full skill catalog with portability ratings and fork-adaptation guidance
- [`ai-workflow.md`](https://github.com/AlphaBitCore/nexus-gateway/blob/main/docs/developers/workflow/ai-workflow.md) — the four artifact layers and where skills fit

**Adjacent wiki pages**: [Workbench Overview](Workbench-Overview) · [Workbench CLAUDE md Anatomy](Workbench-CLAUDE-md-Anatomy) · [Workbench Cursor Rules](Workbench-Cursor-Rules) · [Workbench Forking Guide](Workbench-Forking-Guide) · [Workbench Lessons Learned](Workbench-Lessons-Learned)
