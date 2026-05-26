# AI Skill Catalog

The `.claude/skills/` directory holds 26 invocable procedures the AI
agent can fire as `/skill-name` in Claude Code. Each is a self-contained
runbook with trigger keywords, pre-conditions, steps, and a verification
gate. This catalog is the index + the adaptation guide for forks.

The pattern: anything you do 3+ times manually becomes a skill. Skills
externalise tacit operational knowledge ("which order to restart
services after a deploy") into something an agent can execute reliably.

---

## Catalog

### Deployment + operations

| Skill | Purpose | OSS-portable? |
|---|---|---|
| [`prod-deploy`](../../../.claude/skills/prod-deploy/) | Full prod release: git tag → build all services → upload → kill-then-start in order → verify online → smoke check. Mandatory post-deploy smoke binding. | **No** — hardcodes `taskforce10x.com` EC2 instance. See *Adapting for forks* below. |
| [`prod-login`](../../../.claude/skills/prod-login/) | OAuth + PKCE login against prod Control Plane; caches token; exposes `cp_login` / `cp_curl` / `cp_curl_code` helpers for subsequent admin API calls. | **No** — see adaptation. |
| [`prod-debug`](../../../.claude/skills/prod-debug/) | Diagnose prod issues: service logs, DB queries, Redis state, NATS streams, config/shadow inspection, metrics, known failure-pattern fixes. | **No** — see adaptation. |
| [`build-agent`](../../../.claude/skills/build-agent/) | macOS Agent build / sign / notarize / package via the official path. Single source of truth for signing identities, provisioning, notarytool keychain profile, macOS 26 launch-constraint reqs. | Partial — adapt code-signing identities. |
| [`add-shadow-key`](../../../.claude/skills/add-shadow-key/) | Add a new Thing config key end-to-end across schema, types, UI, Hub publisher, service receiver. | **Yes** — generic Thing-model operation. |
| [`run-local`](../../../.claude/skills/run-local/) | First-run bootstrap: bring the full local stack up from a clean clone (Postgres + Valkey + NATS via docker-compose, the four Go services, the Control Plane UI). Encodes every gotcha a first-time OSS contributor hits. | **Yes** — generic local bring-up. |

### End-to-end testing

| Skill | Purpose | OSS-portable? |
|---|---|---|
| [`test-all`](../../../.claude/skills/test-all/) | Full regression program: preflight + L1 smoke + L1 Go integration + L2 protocol + L3 AI-judge + L4 Playwright UI. ~$0.10/run. Single "did my change break anything" entry point. | Partial — depends on `tests/.env.local` infra. |
| [`smoke-gateway`](../../../.claude/skills/smoke-gateway/) | Full-surface AI Gateway + Control Plane smoke: every model in catalog (non-stream + SSE + 2-turn cache), routing-rule auto-management, DB cross-check, Prometheus diff, auto-fix loop. | Partial — needs VKs + provider credentials. |
| [`test-compliance-proxy`](../../../.claude/skills/test-compliance-proxy/) | Compliance proxy MITM end-to-end: verifies HTTPS provider traffic interception on `:3128`, compliance pipeline runs, matching `traffic_event` rows + Prometheus counters. | **Yes** — local-runnable. |
| [`test-cursor-adapter`](../../../.claude/skills/test-cursor-adapter/) | Synthetic protobuf test against Cursor IDE Tier-1 normalizer. Sends hand-rolled `GetChatRequest` through compliance proxy; verifies `traffic_event_normalized` row. | Partial — needs compliance proxy + DB. |
| [`test-geminiweb-adapter`](../../../.claude/skills/test-geminiweb-adapter/) | Synthetic batchexecute test against Gemini Web Tier-1 normalizer. | Partial — same as above. |
| [`test-openai-responses`](../../../.claude/skills/test-openai-responses/) | Synthetic test for the OpenAI Responses-API ingress (E56). 5 hand-rolled requests covering text non-stream / SSE / function-call SSE / structured outputs / reasoning-effort. | Partial — needs local AI Gateway running. |

### Architecture + design

| Skill | Purpose | OSS-portable? |
|---|---|---|
| [`spec-writing`](../../../.claude/skills/spec-writing/) | SDD discipline: comprehensive Requirements → SDD → OpenAPI flow with templates and approval gates. | **Yes** — generic SDD workflow. |
| [`add-provider-adapter`](../../../.claude/skills/add-provider-adapter/) | 9-step procedure for adding an LLM provider adapter; maps to the 8 architectural rules in `docs/developers/architecture/services/ai-gateway/provider-adapter-architecture.md` (Rule 8 — decode via `shared/normalize` — was added in E58-S0). | **Yes** — adapt to your provider model. |
| [`adapter-conformance-check`](../../../.claude/skills/adapter-conformance-check/) | Audits existing adapter codecs against the 8 architectural rules (Rules 1-7 + Rule 8 from E58-S0); surfaces per-adapter logic leaking into the generic dispatcher. | **Yes** — adapt patterns. |
| [`add-cp-ui-section`](../../../.claude/skills/add-cp-ui-section/) | Add a new admin section: route, sidebar entry, breadcrumb, page scaffold, i18n keys, IAM action wiring. | **Yes** — adapt to your UI. |
| [`project-review`](../../../.claude/skills/project-review/) | Multi-role review prompt (architect / SRE / security / product) for an existing program or PR. | **Yes** — generic. |

### Debug + audit

| Skill | Purpose | OSS-portable? |
|---|---|---|
| [`frontend-bug-trace`](../../../.claude/skills/frontend-bug-trace/) | Page → API → backend → DB tracing for UI bugs. Fixes mismatches, verifies via login + DB queries. | **Yes** — generic web-app pattern. |
| [`i18n-gap-check`](../../../.claude/skills/i18n-gap-check/) | Cross-bundle (control-plane-ui + agent-ui) i18n audit: missing EN keys, ES/ZH gaps, orphan keys, dynamic templates, hardcoded English. | **Yes** — generic 3-locale i18n check. |
| [`iam-impact-review`](../../../.claude/skills/iam-impact-review/) | When a change adds/moves/renames an admin endpoint or sidebar entry: audits IAM allowedActions ↔ handler middleware ↔ catalog ↔ managed policy fixture ↔ seed parity. | **Yes** — adapt your IAM model. |
| [`ne-fail-open-audit`](../../../.claude/skills/ne-fail-open-audit/) | macOS NetworkExtension safety-critical audit: synchronous decisions, fail-open timeouts, no hardcoded enforcement lists, system-DNS allowlist. | Partial — macOS-NE-specific. |
| [`gap-review`](../../../.claude/skills/gap-review/) | Audits gaps between SDD docs (source of truth) and architecture / requirements / OpenAPI / code / tests. Creates a punch list. | **Yes** — generic SDD-parity audit. |
| [`pre-edit-reader`](../../../.claude/skills/pre-edit-reader/) | Pre-edit 3-doc enforcement: open architecture doc + feature doc + conventions doc before code touches disk. | **Yes** — generic discipline. |
| [`arch-doc-trigger-check`](../../../.claude/skills/arch-doc-trigger-check/) | Lockstep: every `docs/developers/architecture/**/*-architecture.md` ↔ trigger map row. | **Yes** — generic doc-code parity. |
| [`frontend-arch-review`](../../../.claude/skills/frontend-arch-review/) | Tailwind v4 + shadcn surface audit: design-token compliance, theme/mode safety, hex/rgba leak detection in `*.module.css` and `style={{}}`. | **Yes** — adapt token taxonomy. |

---

## How a skill is structured

Every skill folder contains a `SKILL.md` (or single-file equivalent) with:

1. **Description + trigger keywords.** What invokes it (`/test-all`,
   "run full regression", "smoke everything").
2. **Pre-conditions.** What the environment must look like before
   firing (running services, env vars, credentials).
3. **Step-by-step procedure.** Numbered. Each step is concrete and
   verifiable.
4. **Binding rules.** What hard contracts the skill enforces (e.g.
   `prod-deploy` Step 7 mandates a post-deploy smoke run).
5. **Output / verification.** What "done" looks like and where the
   report lands.
6. **Recovery / known failure modes.** What to do when the skill fails
   mid-execution.

The longer skills (`prod-deploy` 518 lines, `spec-writing` 486 lines,
`build-agent` 426 lines, `project-review` 469 lines) read like
operational books — they encode hard-won failure modes.

---

## Adapting for forks

5 skills are tightly coupled to this repo's prod infra
(`taskforce10x.com` EC2, the specific OAuth client, the specific DB
schema). The rest are portable. To fork:

### 1. Replace prod endpoints

Search-and-replace `taskforce10x.com` with your domain across:

```bash
grep -rln 'taskforce10x.com' .claude/skills/
grep -rln 'nexus.taskforce10x.com' .claude/skills/
```

The skills primarily affected: `prod-deploy`, `prod-login`, `prod-debug`,
`smoke-gateway`, `build-agent`. Each centralises the URL near the top.

### 2. Replace admin credentials

`prod-login` seeds a super-admin login at `admin@nexus.ai / admin123`.
Change to your seeded super-admin, or switch the skill to read
credentials from a `.env.prod` file instead of inlining them.

### 3. Remove hardcoded EC2 ops

`prod-deploy` SSHes into a specific EC2 instance, swaps binaries in
`/srv/nexus/bin/`, runs `systemctl restart`. Adapt to your hosting:
container registry push + rolling deploy, GitHub Actions release, etc.
Keep the **mandatory post-deploy smoke binding** — it's the most
load-bearing rule the skill carries.

### 4. Re-anchor incident references

Several skills cite specific past incidents (`2026-05-13 NRN mismatch`,
`2026-05-15 macOS network bricked`, etc.). Forks should remove these or
replace with their own incidents as they accumulate. Memory anchors
(`[[feedback_name]]`) reference the maintainer's local memory store
and won't resolve in a fresh fork; either reroot to your own memory
entries or delete.

### 5. Keep the structure, swap the substance

The *shape* of a skill — pre-conditions / step-by-step / binding /
verification / recovery — is generic. The *substance* (which URL, which
DB column, which incident motivated the binding) is yours to fill in.

---

## When to write a new skill

- You did the procedure 3+ times manually.
- The procedure has 5+ steps where order matters.
- A wrong step causes pain (lost data, prod incident, etc.).
- There's a verification gate that's hard to do from memory.

When *not* to:

- One-off task.
- Already covered by an existing skill (extend it instead of forking).
- The "skill" is really just one command — alias it, don't ceremonialise.

---

## Related

- [`docs/developers/workflow/ai-workflow.md`](./ai-workflow.md) — the workflow these skills slot into.
- [`CLAUDE.md`](../../../CLAUDE.md) — canonical binding rules cited by skills.
- [`.claude/skills/README.md`](../../../.claude/skills/README.md) — pointer at this catalog from the skill directory itself.
