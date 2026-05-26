# Release History

Nexus Gateway uses `prod-YYYYMMDD` annotated git tags to mark each production deployment. Each tag marks a commit on `main` that was deployed to the production host. This page lists the known production release tags with a summary of what changed in each. The canonical source for ongoing release tracking is the ["Recently shipped" section of `docs/developers/roadmap.md`](https://github.com/AlphaBitCore/nexus-gateway/blob/main/docs/developers/roadmap.md).

Two `prod-YYYYMMDD` tags currently exist. Earlier deployments predate the formal tagging convention and are tracked by commit SHA only.

---

## Release log

### prod-20260520 — 2026-05-20

**Deployed**: 2026-05-20T15:48:14+08:00

**Primary changes**: Reasoning-token surfacing fixes across three providers, plus a Responses-API cost-estimator correction discovered during the 2026-05-20 production validation smoke.

**Reasoning-token fixes** — Three independent providers were not surfacing reasoning token counts correctly:

- *OpenAI SSE final-chunk* — the `openAIStreamEncoder` built the final SSE usage block with only `prompt_tokens`, `completion_tokens`, and `total_tokens`, silently dropping `cache_read_tokens` and `reasoning_tokens` from the canonical chunk usage. Symptom: `gemini-2.5-pro` called via `/v1/chat/completions` in stream mode returned a final chunk with no `completion_tokens_details.reasoning_tokens`, even though Gemini's `thoughtsTokenCount` was correctly extracted upstream. Fixed: both `completion_tokens_details` and `prompt_tokens_details` are now projected when present, matching the non-stream projector behavior.
- *Moonshot/kimi-k2.x* — `kimi-k2.x` ships reasoning text as `message.reasoning_content` (non-stream) or `delta.reasoning_content` (stream) but does not populate `usage.completion_tokens_details.reasoning_tokens`. Fixed: derives a token count from `reasoning_content` char-length heuristic (`chars * 2 / 7`). Verified: DB rows now show `reasoning_tokens=48`, `216`, etc.
- *Anthropic extended thinking* — Anthropic API counts thinking tokens as part of `output_tokens` but does not break down how many are thinking vs. visible text. Fixed: sums char-length of every `ContentReasoning` block and applies the same heuristic. Note: a prompt that does not trigger thinking blocks (e.g., a trivial "reply with one word" prompt) correctly shows zero reasoning tokens — the fix does not fabricate a count.

**Responses-API cost estimator fix** — `countCanonicalInputChars` only scanned `messages[].content` and Gemini `contents[].parts[].text`. The Responses-API native request shape uses a top-level `input` field (string, structured array, or tool-output items) plus an optional `instructions` field. Dry-run estimates on OpenAI Responses-API native models (`gpt-5.4-mini`, `gpt-5.x` families) returned `prompt_tokens=0`. Fixed to handle all three `input` shapes and treats `instructions` as the Responses-API analogue of Anthropic's `system` field.

**Also included** between `prod-20260519` and `prod-20260520`: README vision-first rewrite (Trinity consistency framing, real performance numbers, workbench reveal, Chinese version), two-phase documentation reorg (audience-first top-level, per-epic specs, archive partitioning), OSS AI vibe-coding workbench documentation surface, lint tooling additions (binding-rule enforcement for stale placeholder comments and yaml secrets), `thingclient` write-frame context-cancel fix, audit MQ flush-race fix, smoke-gateway worker-pool parallelization, AI Gateway server/upstream timeout bumps for long-reasoning models (60s→360s server, 120s→300s upstream), Valkey connection pool tuning (`poolSize` 10→200), NATS `NEXUS_EVENTS` stream limit correction (MaxBytes 2G→8G), and ingress proxy unit-test coverage close (94.3%→95.4%, removed from allowlist).

---

### prod-20260519 — 2026-05-19

**Deployed**: 2026-05-19T01:07:36+08:00

**Summary**: Initial formal production release tag. Marked the state of production when the `prod-YYYYMMDD` tagging convention was first adopted. The full 5-service architecture was already deployed and serving real traffic before this tag; this commit captures the production baseline including the initial reasoning-token surface fixes.

At this point production was running:

- **AI Gateway** — 5 providers (OpenAI, Anthropic, Gemini, Moonshot, DeepSeek) × 35 models; 4 ingress shapes (`/v1/chat/completions`, `/v1/responses`, `/v1/messages`, `:generateContent`); canonical bus, cross-format routing, prompt-cache integration, per-VK quota, audit pipeline.
- **Compliance Proxy** — MITM TLS bump on `:3128` with admin-trusted CA; 5 web adapters (chatgptweb, claudeweb, geminiweb, cursor) and 5 API adapters verified end-to-end; hook pipeline running per Hub-pushed `HookConfig`.
- **Nexus Hub** — Thing Registry, Device Shadow (Cat A/B/C config taxonomy), agent CA + Ed25519 attestation, audit chain pipeline with body capture + spillstore (S3), scheduled jobs (retention purge, drift checker, cert rotation), Prometheus metrics rollup.
- **Control Plane admin API** — full IAM (resource catalog, NRN permissions, super-admin policy), virtual-key CRUD with budgets + quotas, provider/model catalog, routing rules (priority + weighting + LLM-dispatch smart routing), analytics (cost/savings/latency/cache-ROI), hook-config, kill switch, SIEM bridge, IdP/SSO (OIDC + SAML JIT provisioning), JWT verifier (multi-issuer).
- **Control Plane UI** — admin dashboard, theme system, i18n (EN/ZH/ES), design-token framework (light/dark themes), traffic audit drawer with NormalizedPayload viewer, observability surfaces.

---

## What gets deployed in a Nexus release

Each production deployment packages and replaces:

1. **Four Go service binaries** — `nexus-hub`, `control-plane`, `ai-gateway`, and
   `compliance-proxy`. Each is a standalone binary built from its `cmd/<svc>/` entry
   point. The Desktop Agent has a separate release cadence (`.pkg` for macOS, `.deb`
   and `.rpm` for Linux, `.exe` installer for Windows) coordinated through the
   `build-agent` skill.

2. **Control Plane UI** — The React + TypeScript + Vite admin dashboard, built as a
   static site and served by nginx. UI releases are bundled with the server-side
   service deployments.

3. **Database migrations** — Prisma migrations under `tools/db-migrate/prisma/migrations/`
   applied in timestamp order. Each migration has a unique 14-character timestamp
   prefix (enforced by pre-commit hook) so two migrations cannot share a prefix
   and silently skip one.

4. **Seed data updates** — When new built-in data (alert rules, hook configs, provider
   catalog entries) needs to land in production, it is applied via a separate SQL
   script under `tools/db-migrate/manual-scripts/` rather than in the migration files,
   to preserve idempotency.

The deploy skill (`.claude/skills/prod-deploy/`) encodes the full sequence: build →
upload → apply migration → kill services in dependency order → start services →
verify nodes online → run smoke check.

---

## Versioning philosophy

Nexus Gateway does not use semantic versioning (major.minor.patch). Deployments are
tagged by calendar date (`prod-YYYYMMDD`) because:

- The codebase is pre-GA; semantic version guarantees (backward compatibility windows,
  deprecation cycles) do not apply at this stage.
- The date tag gives operators an unambiguous point-in-time reference for support
  requests, incident timelines, and migration ordering.
- "What's running in prod?" is answered by `git describe --tags` returning
  `prod-20260520`, not by decoding a semver string.

Once the project reaches GA, a semver convention will be adopted. The roadmap's
"Maintenance checklist" section in `docs/developers/roadmap.md` will be updated at
that point to reflect the new tagging convention.

---

## Release conventions

Production deployments follow the `prod-YYYYMMDD` tag convention on the `main` branch. When multiple deployments occur on the same calendar day, they are distinguished by appending a short suffix describing the fix (e.g., `prod-20260513-iam-fix`, `prod-20260513-e48`). For such suffixed tags, the memory anchor `[[project_prod_releases]]` carries the annotated history from earlier deployments predating the formal tagging convention.

The deploy skill (`.claude/skills/prod-deploy/`) is the canonical automated path for building all 4 Go services, uploading binaries, and restarting services in the correct order on the production host. Deployments that include database migrations follow the sequencing in [`docs/operators/ops/runbooks/prod-deploy-data-changes.md`](https://github.com/AlphaBitCore/nexus-gateway/blob/main/docs/operators/ops/runbooks/prod-deploy-data-changes.md).

The `main` branch is the target for production; `develop` carries ongoing optimization and pre-merge fixes. The standard workflow for a new epic is a feature branch → PR to `main`; prod tag is cut after smoke validation.

---

## Canonical docs

- [`docs/developers/roadmap.md`](https://github.com/AlphaBitCore/nexus-gateway/blob/main/docs/developers/roadmap.md) — "Recently shipped this cycle" section with commit SHAs and PR references for all epics
- [`docs/operators/ops/runbooks/prod-deploy-data-changes.md`](https://github.com/AlphaBitCore/nexus-gateway/blob/main/docs/operators/ops/runbooks/prod-deploy-data-changes.md) — migration sequencing, backup/restore steps, rollback procedure for production deployments

**Adjacent wiki pages**: [Roadmap Active](Roadmap-Active) · [Roadmap Queued](Roadmap-Queued) · [Production State](Production-State) · [Dev Release Process](Dev-Release-Process) · [Operations Day 2 Cheatsheet](Operations-Day-2-Cheatsheet)
