# Docs-Product Branch Tracking

**Branch:** `feature/docs-product`
**Worktree:** `worktrees/docs-product/`
**Started:** 2026-05-26
**Goal:** Write user-facing feature + product docs from scratch, aggregated by section/domain, anchored to current code (not to existing stubs). Each doc passes `/doc-write` 9-step protocol + `/doc-review` per-claim audit + full-Chinese-translation user approval.
**Status:** Phase 1 complete (product anchor — overview + features). Phase 2 (CP-UI) starting. Inventory expanded 20 → 25 docs after CP-UI split (D7).

This file is the single source of truth for what's planned, what's done, and what's deleted on this branch. Update on every status change.

---

## Workflow contract

For every doc in the inventory:

| Status | Meaning |
|---|---|
| `[ ]` | Pending — not started |
| `[~]` | In progress — claimed, no parallel work on other docs |
| `[D]` | Drafted — English doc landed on disk, passed `/doc-write` 9-step + `/doc-review` per-claim audit |
| `[Z]` | Chinese summary shown — summary presented in chat for user review |
| `[A]` | Approved — user has approved; doc is complete |
| `[X]` | Cancelled — removed from scope (record reason in Decisions log) |

**Rule:** never advance to next doc until current is `[A]`. Repo content stays English. Chinese is chat-only.

---

## Decisions log

- **2026-05-26 D1** — Inventory is aggregated by **section/domain**, not by menu leaf. One doc per CP-UI sidebar section (`nav.json` header) and per Agent-UI Shell nav item (`Shell.tsx`).
- **2026-05-26 D2** — Existing 32 stub files in `docs/users/{features,product}/` are NOT authoritative. Inventory is rederived from code surface: `packages/control-plane-ui/src/routes/shellRouteConfig.tsx` + `packages/control-plane-ui/src/i18n/locales/en/nav.json` + `packages/agent/ui/frontend/src/layout/Shell.tsx` + `packages/agent/ui/frontend/src/app/App.tsx`.
- **2026-05-26 D3** — Renames: `cp-ui/setup-status-system.md` → `cp-ui/system.md`; `agent-ui/health-diagnostics.md` → `agent-ui/diagnostics.md`; `agent-ui/install-and-enrol.md` → `agent-ui/onboarding.md`. Reason: match section header / route path / code page name.
- **2026-05-26 D4** — Deletes: `agent-ui/dashboard.md` (no `/dashboard` route — Agent Shell only has `/overview`); `product/competitive-landscape.md` (marketing content, out of feature/product description scope).
- **2026-05-26 D5** — Flows deferred entirely. All 11 `docs/users/features/flows/*.md` stubs (incl. `README.md`) deleted in cleanup. Future flow docs will be written from a **persona angle** — "what does each role (admin / fleet operator / dev-user / compliance officer) care about when they walk in?" — not as feature lifecycles. Persona-driven flow set will be designed in a later branch after section docs land.
- **2026-05-26 D6** — Doc target = describe **features and product**, not tutorials or marketing. Each section doc covers: what the section is for, every menu leaf inside it (purpose, key fields, key actions), and how the section's objects relate to other sections. Persona-driven framing is reserved for Phase-7 flows (D5), not for this branch.
- **2026-05-26 D7** — CP-UI section docs split 8 → 13: large sections (AI Gateway 9 leaves, Compliance 11 leaves, Infrastructure 13 leaves) sub-split by sub-domain so each doc stays at one reviewable concept boundary (200-500 lines). Small sections (Overview / Alerts / Devices / IAM / System) remain one doc each. Total branch scope 20 → 25 docs.

---

## Per-doc tracking

### Phase 1 — Product anchor (write first, frames terminology for all downstream docs)

| # | Status | Doc | Code surface anchor | Note |
|---|---|---|---|---|
| 1 | `[A]` | `docs/users/product/overview.md` | — | What Nexus is, who for, key value props |
| 2 | `[A]` | `docs/users/product/features.md` | `nav.json`; full feature surface | Capability matrix |

### Phase 2 — CP-UI section docs (13 docs after D7 split)

| # | Status | Doc | Section + leaves covered |
|---|---|---|---|
| 3 | `[A]` | `docs/users/features/cp-ui/overview.md` | OVERVIEW: Dashboard · Traffic · Analytics & Metrics · Quota Usage · Cache ROI |
| 4 | `[A]` | `docs/users/features/cp-ui/ai-gateway-routing.md` *(new)* | AI GATEWAY/A: Providers & Models · Routing Rules · Virtual Keys · Credentials · Credential Reliability |
| 5 | `[A]` | `docs/users/features/cp-ui/ai-gateway-cost-cache.md` *(new)* | AI GATEWAY/B: Quota Policies · Quota Overrides · Cache · Emergency Passthrough |
| 6 | `[A]` | `docs/users/features/cp-ui/compliance-hooks.md` *(new)* | COMPLIANCE/A: Overview · Hooks & Policies · Rule Packs · Exemptions |
| 7 | `[A]` | `docs/users/features/cp-ui/compliance-network.md` *(new)* | COMPLIANCE/B: Interception Domains · AI Guard Backend · Streaming Compliance · Payload Capture |
| 8 | `[A]` | `docs/users/features/cp-ui/compliance-records.md` *(new)* | COMPLIANCE/C: Operation Logs · Data Subject Requests · Compliance Report |
| 9 | `[A]` | `docs/users/features/cp-ui/iam.md` | IAM: Organizations · Projects · Users · Roles · Policies · Simulator · Identity Providers |
| 10 | `[A]` | `docs/users/features/cp-ui/devices.md` | DEVICES: Devices · Device Groups · Device Auth · Device Defaults |
| 11 | `[A]` | `docs/users/features/cp-ui/alerts.md` | ALERTS: Inbox · Rules · Channels |
| 12 | `[ ]` | `docs/users/features/cp-ui/infrastructure-nodes.md` *(new)* | INFRASTRUCTURE/A: Nodes · Overrides · Config Sync · Scheduled Jobs |
| 13 | `[ ]` | `docs/users/features/cp-ui/infrastructure-ops.md` *(new)* | INFRASTRUCTURE/B: Kill Switch · Recent Errors · Crash Reports · Diag Mode · Proxy Rollout · Agent Setup |
| 14 | `[ ]` | `docs/users/features/cp-ui/infrastructure-observability.md` *(new)* | INFRASTRUCTURE/C: Observability · Observability Retention · SIEM |
| 15 | `[ ]` | `docs/users/features/cp-ui/system.md` *(rename from `setup-status-system.md`)* | SYSTEM: AI Gateway Simulator · Status & Health · Setup Wizard |

### Phase 3 — Agent-UI section docs (8 docs)

| # | Status | Doc | Code surface anchor | Route |
|---|---|---|---|---|
| 16 | `[ ]` | `docs/users/features/agent-ui/onboarding.md` *(rename from `install-and-enrol.md`)* | `pages/onboarding/Onboarding.tsx` | (first-install flow) |
| 17 | `[ ]` | `docs/users/features/agent-ui/overview.md` | `pages/overview/Overview.tsx` | `/overview` |
| 18 | `[ ]` | `docs/users/features/agent-ui/activity.md` | `pages/activity/Activity.tsx` | `/activity` |
| 19 | `[ ]` | `docs/users/features/agent-ui/traffic.md` *(new)* | `pages/traffic/Traffic.tsx` | `/traffic` |
| 20 | `[ ]` | `docs/users/features/agent-ui/stats.md` *(new)* | `pages/activity/Stats.tsx` | `/stats` |
| 21 | `[ ]` | `docs/users/features/agent-ui/policies.md` | `pages/policies/*` (Overview, Domains, Hooks, Exemptions, RulePacks) | `/policies` + subpages |
| 22 | `[ ]` | `docs/users/features/agent-ui/diagnostics.md` *(rename from `health-diagnostics.md`)* | `pages/diagnostics/*` | `/diagnostics` |
| 23 | `[ ]` | `docs/users/features/agent-ui/settings.md` | `pages/settings/Settings.tsx` | `/settings` |

### Phase 4 — Product positioning

| # | Status | Doc | Code surface anchor | Note |
|---|---|---|---|---|
| 24 | `[ ]` | `docs/users/product/architecture.md` | translate `developers/architecture/overview.md` for non-dev readers | User-facing high-level architecture |
| 25 | `[ ]` | `docs/users/product/deployment-models.md` | `scripts/dev-start.sh`, `prod-deploy` skill, `docs/operators/` | SaaS / self-host / air-gap |

---

## Cleanup operations (Phase 6, before merging branch)

Runs after all 20 docs are `[A]`. Doc-lockstep config (`scripts/doc-lockstep.config.mjs`) must be checked and updated in the same commit if any deleted/renamed file is referenced there.

| File | Action | Reason |
|---|---|---|
| `docs/users/features/cp-ui/ai-gateway.md` | `git rm` | Replaced by `ai-gateway-routing.md` + `ai-gateway-cost-cache.md` (D7 split) |
| `docs/users/features/cp-ui/compliance.md` | `git rm` | Replaced by `compliance-hooks.md` + `compliance-network.md` + `compliance-records.md` (D7 split) |
| `docs/users/features/cp-ui/infrastructure.md` | `git rm` | Replaced by `infrastructure-nodes.md` + `infrastructure-ops.md` + `infrastructure-observability.md` (D7 split) |
| `docs/users/features/cp-ui/setup-status-system.md` | `git mv` → `system.md` | Section header is "SYSTEM"; match section-name pattern |
| `docs/users/features/agent-ui/health-diagnostics.md` | `git mv` → `diagnostics.md` | Route is `/diagnostics`; match route-name pattern |
| `docs/users/features/agent-ui/install-and-enrol.md` | `git mv` → `onboarding.md` | Code page is `Onboarding.tsx`; match code-name pattern |
| `docs/users/features/agent-ui/dashboard.md` | `git rm` | No `/dashboard` route in Agent Shell; redundant with `overview.md` |
| `docs/users/product/competitive-landscape.md` | `git rm` | Marketing content; out of scope (D6) |
| `docs/users/features/flows/README.md` | `git rm` | Flows deferred (D5) |
| `docs/users/features/flows/agent-enrollment.md` | `git rm` | Flows deferred (D5) |
| `docs/users/features/flows/alert-evaluation.md` | `git rm` | Flows deferred (D5) |
| `docs/users/features/flows/credential-rotation.md` | `git rm` | Flows deferred (D5) |
| `docs/users/features/flows/dry-run.md` | `git rm` | Flows deferred (D5) |
| `docs/users/features/flows/hook-rollout.md` | `git rm` | Flows deferred (D5) |
| `docs/users/features/flows/idp-federation.md` | `git rm` | Flows deferred (D5) |
| `docs/users/features/flows/kill-switch-and-passthrough.md` | `git rm` | Flows deferred (D5) |
| `docs/users/features/flows/routing-rule-lifecycle.md` | `git rm` | Flows deferred (D5) |
| `docs/users/features/flows/traffic-event-lifecycle.md` | `git rm` | Flows deferred (D5) |
| `docs/users/features/flows/v1-estimate-compare.md` | `git rm` | Flows deferred (D5) |
| `docs/users/features/flows/vk-lifecycle.md` | `git rm` | Flows deferred (D5) |

---

## Open questions

- None.

---

## Change history (most recent first)

- 2026-05-26 — Doc #3 `cp-ui/overview.md` approved (full Chinese review). Covers all 5 OVERVIEW nav leaves + a "Data freshness and rollups" section (5-min finest bucket, cascade 5m→1h→1d→1mo, ~5-6 min lag, daily correction, Cache-ROI-only direct banner). Surfaced + removed 2 dead surfaces in a prior commit: `AnalyticsTab.tsx` (orphan) + `/metrics` route (orphan). `/doc-review` CLEAN: 50 page claims + 11 rollup claims verified.
- 2026-05-26 — D7 CP-UI split: 8 → 13 docs (large sections AI Gateway / Compliance / Infrastructure sub-split). Total branch scope 20 → 25 docs. Phase 2 row numbers renumbered.
- 2026-05-26 — Doc #2 `product/features.md` approved (full Chinese review). 61 capability rows across 7 sections; `/doc-review` Round-2 CLEAN on all named enumerations (20 codecs, 7 strategies, 11 built-in hooks, 4 channel types, 4 rollup windows, 3 bypass flags). Phase 1 (product anchor) complete.
- 2026-05-26 — Doc #1 `product/overview.md` approved (full Chinese review). Surfaced upstream drift: README.md + CLAUDE.md both cite Compliance Proxy as port 3040; actual listener is `:3128`, port 3040 is the Prometheus metrics endpoint. Fixed in same branch as separate commit.
- 2026-05-26 — Tracking file created; inventory locked at 20 docs; flows deferred (D5); 3 renames + 13 deletes queued for Phase 6 cleanup.
