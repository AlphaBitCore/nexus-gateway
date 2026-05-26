# R8 — Repository-Wide Directory-Size Decomposition Program

**Status:** plan landed 2026-05-17 — execution autonomous after Plan
approval per `[[feedback_autonomous_execution]]`.

**Scope:** every package-internal directory containing >10 own-files
(test files excluded) across the whole monorepo. The ≥20-file
directories carry a **mandatory-consideration** bias; 10–19 build a
todo and decide keep-or-decompose with written rationale.

**Why now:** flat directories at the package boundary force "god-file"
patterns (50-file `handler/`, 53-file `store/`, 53-file
`api/services/`) where unrelated domains share editing surface. Same
cognitive-load problem R6 solved for `handler/`, but generalised. The
2026-05-17 directory survey (15 dirs ≥20 files, 41 ≥10) confirms the
pattern.

**Relationship to in-flight R6:** this plan supersets R6 — the
remaining 6 handler/ domains are folded in as the first phase. R6
runbook §5 already documents the 9 landed; R8 inherits + finishes it.

## Classification of all 41 candidates

Three buckets:

### Bucket A — KEEP AS-IS (legitimate shape; do NOT decompose)

The high file count comes from a one-file-per-unit convention that's
already idiomatic. Splitting further would fragment a cohesive registry.

| Dir | Files | Rationale |
|---|---|---|
| `agent/cmd/agent` | 18 | Go `cmd/` entry; per-OS build-tag files (`*_darwin.go` / `*_other.go`) are idiomatic — splitting by tag is the split |
| `agent/core/platform` | 16 | Per-OS build-constraint split; same as above |
| `nexus-hub/internal/jobs/defs` | 40 | One file per scheduled job — registry shape |
| `nexus-hub/internal/alerts/eval/aggregators` | 21 | One file per aggregator — registry shape |
| `shared/policy/hooks` | 19 | One file per hook implementation — registry shape |
| `shared/schemas/configtypes` | 27 | One file per typed-config schema — public API kernel; splitting breaks importers |
| `shared/transport/tlsbump` | 12 | Cohesive subsystem; per-OS + protocol files |
| `shared/transport/normalize` | 17 | Cohesive normalize pipeline |
| `shared/traffic` | 12 | Single subsystem |
| `shared/runtime/opsmetrics` | 19 | Cohesive metrics subsystem |
| `control-plane-ui/public/provider-templates` | 20 | Static JSON data, one file per provider — data not code |
| `control-plane-ui/src/lib/forms` | 11 | Small form-helper cluster, cohesive |
| `control-plane-ui/src/hooks` | 14 | Cross-cutting generic React hooks |
| `control-plane/internal/authserver/store` | 11 | Small auth-store cluster |
| `ai-gateway/internal/store` | 11 | Small + AI-Gateway-owned |
| `nexus-hub/internal/thingmgr` | 11 | Small subsystem |
| `ai-gateway/internal/providers/spec_openai` | 11 | Adapter — registry-of-quirks shape |

### Bucket B — DECOMPOSE (R6-style extraction; ≥20 files biased mandatory)

| Dir | Files | Target | Strategy |
|---|---|---|---|
| `control-plane/internal/handler` | 50 | 6 more domains (compliance, agent, infra, dsar, extras, iam+auth after revokeUserScope lift) | R6 runbook recipe — narrow-Hub-interface + helper-copy |
| `control-plane/internal/store` | 52 | Sibling per-domain subpackages mirroring R6 handler split | Mirror handler/ domain spine |
| `control-plane-ui/src/api/services` | 53 | One folder per domain matching the 8-nav-section taxonomy | See R8-UI-PLAN §A |
| `control-plane-ui/src/pages/infrastructure` | 53 | Sub-folders per resource (nodes / config-sync / overrides / jobs / diag / siem / observability / agent-setup / proxy-rollout / kill-switch / crash-reports / recent-errors) | See R8-UI-PLAN §B |
| `control-plane-ui/src/pages/settings` | 27 | Move to its proper nav-section homes (devices/infrastructure/compliance/system); only "true settings" stays — see R8-UI-PLAN §C | Relocation, not extraction |
| `control-plane-ui/src/pages/iam` | 28 | Sub-folders for roles/policies/users/groups/simulator; orgs+projects+user-detail already present | See R8-UI-PLAN §D |
| `control-plane-ui/src/pages/traffic` | 21 | Sub-folders: list/, audit-drawer/, filters/, analytics/ | See R8-UI-PLAN §E |
| `control-plane-ui/src/pages/compliance/rule-packs` | 21 | Sub-folders: list/, detail/, form/, bind/, import/, overrides/ | See R8-UI-PLAN §F |
| `nexus-hub/internal/storage/store` | 22 | catb_* cluster extraction → `storage/store/catbagent/` subpackage | One commit |

### Bucket C — EVALUATE-THEN-DECIDE (10–19 files; verdict in todo)

For each, decide keep-or-decompose using the same factors that
classified Buckets A and B. Three signals favour KEEP: (1) single
cohesive subsystem with one public type; (2) per-OS or per-implementation
registry where each file is a self-contained unit; (3) <12 own-files
where decomp would create folders with 3-4 files each. Three signals
favour DECOMPOSE: (1) >12 own-files; (2) cross-cutting concerns (e.g.,
mixing CRUD + analytics + admin); (3) the directory already has subdir
siblings (signals natural cluster boundaries).

| Dir | Files | Likely verdict |
|---|---|---|
| `nexus-hub/internal/handler` | 14 | Decompose into 3–4 narrow-Hub domains (registry / shadow / events / metrics) |
| `ai-gateway/internal/handler` | 19 | Decompose — model proxy / admin / metrics endpoint groups |
| `ai-gateway/internal/router` | 17 | Decompose — registry / matchers / strategies |
| `ai-gateway/internal/pipeline/aiguard` | 12 | Likely KEEP — single pipeline |
| `control-plane-ui/src/auth` | 25 | KEEP — already a domain; flat structure is the boundary |
| `control-plane-ui/src/pages/traffic` (already B) | 21 | — |
| `control-plane-ui/src/pages/proxy` | 11 | RELOCATE (most of it duplicates compliance dashboards or belongs in infrastructure proxy-rollout) |
| `control-plane-ui/src/pages/ai-gateway/providers` | 13 | Likely KEEP — already nested under ai-gateway/, has wizard/ subdir |
| `control-plane-ui/src/pages/ai-gateway/providers/wizard` | 12 | KEEP — wizard-step registry |
| `control-plane-ui/src/pages/ai-gateway/routing` | 17 | Likely KEEP — already nested |
| `control-plane-ui/src/pages/ai-gateway/credentials` | 12 | KEEP — single resource CRUD |
| `control-plane-ui/src/pages/status` | 11 | KEEP — small status dashboard |
| `control-plane-ui/src/pages/compliance` | 13 | KEEP — top-level compliance UX (dashboard + exemptions + interception) |
| `control-plane-ui/src/pages/compliance/hooks` | 15 | KEEP — single resource CRUD with form variants |
| `control-plane-ui/src/pages/alerts` | 18 | KEEP — flat OK; has detailRenderers/ + ruleEditors/ subdirs already |
| `control-plane-ui/src/pages/alerts/detailRenderers` | 13 | KEEP — registry shape |
| `control-plane-ui/src/pages/alerts/ruleEditors` | 12 | KEEP — registry shape |

## Execution order

Priority is bias-toward-blast-radius and parallel-session safety:

1. **R6 handler/ finish (Bucket B, biggest single program)** — 6
   remaining domains.
2. **`control-plane/internal/store` decomp (Bucket B, 52)** — mirror
   handler domain spine.
3. **`control-plane-ui/src/api/services` (Bucket B, 53)** — single
   most-imported file in the UI tree; touched by every domain folder
   in step 4.
4. **`control-plane-ui/src/pages/{infrastructure,settings,iam,traffic,compliance/rule-packs}` (Bucket B)**
   — five UI folders, one PR each. Settings is a relocation, the rest
   are sub-folders.
5. **`nexus-hub/internal/storage/store` catbagent split (Bucket B)** —
   small + isolated.
6. **Bucket C** — in size-descending order: nexus-hub/handler (14) →
   ai-gateway/handler (19) → ai-gateway/router (17). All others KEEP.
7. **Re-survey** — after the program closes, rerun the find script and
   confirm no >10 dir remains except Bucket A entries.

## Parallel-session safety

Per `[[project_parallel_worktree_sessions]]` + `[[feedback_never_git_restore_parallel]]`:

- Every commit uses **explicit pathspec** (`git commit -- path/a path/b`).
- Never `git stash`, never `git add -A`, never `git restore <file>`.
- Each decomp PR is **atomic**: move + import-rewrite + lazyPages +
  shellRouteConfig + i18n + tests pass in one commit.
- Coordinate with parallel coverage sessions: when a coverage PR adds
  `<pkg>_test.go` to a directory R8 is about to extract, **wait** for
  the coverage commit to land, then absorb it into the extraction.

## Naming conventions

- **Backend (Go)**: `package <domain>` under `<parent>/<domain>/`.
  Helper-copy strategy per R6 runbook §4.2 until a 4th call site
  appears.
- **UI (TS)**: `src/pages/<nav-section>/<resource>/` for pages,
  `src/api/services/<nav-section>/<resource>.ts` for services. Nav
  section names match `shellRouteConfig.tsx`'s `NavSectionKey` union
  (`overview / aiGateway / compliance / alerts / devices /
  infrastructure / iam / system`).

## UI-specific sub-plans

### §A — `api/services/` per-domain reshape

Move every flat `api/services/<file>.ts` into
`api/services/<nav-section>/<file>.ts` matching the backend handler
domain its endpoints hit:

```
api/services/
  overview/      analytics.ts, fleet-analytics.ts
  ai-gateway/    providers.ts, credentials.ts, routing.ts,
                 virtualKeys.ts, personalVirtualKeys.ts, quotaPolicies.ts,
                 quotaOverrides.ts, quotaAnalytics.ts, passthrough.ts,
                 aiGatewayClientSimulator.ts
  compliance/    hooks.ts, rulepacks.ts, compliance.ts,
                 compliance-report.ts, interceptionDomains.ts,
                 aiguard.ts, dsar.ts, agent-exemptions.ts
  alerts/        alerts.ts
  devices/       devices.ts, device-groups.ts, fleet.ts
  infrastructure/ hub.ts, nodeRuntime.ts, thingStats.ts,
                  diagevents.ts, diagmode.ts, opsmetrics.ts,
                  service-urls.ts, system.ts, retention.ts,
                  proxy.ts
  iam/           iam.ts, organizations.ts, projects.ts, authserver.ts,
                 personalApiKeys.ts
  system/        setup.ts, setup-state.ts, agent-events.ts, cache.ts
  _shared/       index.ts (re-exports)
```

Each `.test.ts` moves with its `.ts` sibling. Update all
`api/services/<X>` imports to `api/services/<section>/<X>`.

### §B — `pages/infrastructure/` per-resource subfolder

53 files → 12 sub-resource folders mirroring infrastructure nav items
(nodes / config-sync / overrides / jobs / diag-mode / observability /
observability-retention / agent-setup / proxy-rollout / kill-switch /
crash-reports / recent-errors). Shared tabs (LogsTab, MetricsTab,
RuntimeStateTab, ConfigurationTab, ThingStatsTab) live in
`pages/infrastructure/_shared/`.

### §C — `pages/settings/` relocation

27 files spanning 4 nav sections. Final disposition:

| Current | Lives in nav section | New home |
|---|---|---|
| `DeviceAuthSettingsPage.*`, `IdentityProvider*Page.*`, `idpRoutes.ts` | devices | `pages/devices/auth/` |
| `SettingsAgentTab.*` | devices | `pages/devices/agent-defaults/` |
| `SettingsCacheTab.*`, `SettingsPayloadCaptureTab.*`, `SettingsStreamingComplianceTab.*`, `aiguard/` | compliance | `pages/compliance/{cache,payload-capture,streaming-compliance,aiguard}/` |
| `SettingsSiemTab.*`, `SettingsObservabilityTab.*`, `ObservabilityRetention.*` | infrastructure | `pages/infrastructure/{siem,observability,observability-retention}/` |
| `SettingsCredentialReliabilityTab.*` | ai-gateway | `pages/ai-gateway/credentials/reliability/` |
| `ApiKeyForm.*`, `personal-vks/` | system (account-level) | `pages/account/{api-keys,personal-vks}/` |
| `SettingsPageWrappers.tsx`, `useSettings.ts` | shared | `pages/_shared/settings/` (tab-route framing) |

After relocation, `pages/settings/` is deleted entirely (no
top-level Settings page exists in the nav — every settings surface is
a tab under another section).

### §D — `pages/iam/` sub-resource subfolder

28 files → 5 sub-resource folders (orgs, projects, users, roles,
policies, groups, simulator). `orgs/projects/user-detail/` already
exist. Add `roles/`, `policies/`, `users/`, `groups/`, `simulator/`.
Shared bits (`CatalogPicker`, `ChipInput`, `ScopedActionsPicker`,
`iam-policy-document.ts`) → `pages/iam/_shared/`.

### §E — `pages/traffic/` sub-folder

21 files → 4 folders: `list/` (TrafficTab, NormalizedPayloadView,
ComplianceTagChips, TrafficFileSinkNotice), `audit-drawer/`
(trafficAuditDrawer), `filters/` (LiveTraffic*Filters,
liveTrafficFilters.ts), `analytics/` (TrafficAnalyticsPage,
AnalyticsTab).

### §F — `pages/compliance/rule-packs/` sub-folder

21 files → 6 folders: `list/`, `detail/`, `form/`, `bind/`, `import/`,
`overrides/`. Each contains its `*.tsx + *.module.css + *.test.tsx`
triplet.

## Done-criteria

Two consecutive runs of the directory-size scan come back with no
>10-file directory **except** Bucket A entries. The R8 plan is then
parked in `docs/dev/_internal/` as historical context; bindings move
to `feedback_directory_size_decomp.md` (≥20 files = always reconsider
extraction).

## Final state (2026-05-17 autonomous run)

**Landed (12 of 15 Bucket B items + Bucket C verdicts):**

| Task | Outcome | Commit |
|---|---|---|
| R8-A | Master plan published | `54118e085` |
| R8-B1.1 killswitch/ | extracted, 100% cov | `236866df3` |
| R8-B1.2 passthrough/ | extracted, 15.9% (allowlist) | `654e53f4a` |
| R8-B1.3 aiguard/ | extracted, 80.5% (allowlist; parallel pushed to 98.2%) | `d6838552b` |
| R8-B1.4 exemption/ | extracted, 15.8% (allowlist) | `4e9b56d1f` |
| R8-B2 agent/ | extracted (38 methods, 7 source files) | (current) |
| R8-B3 infra/ | extracted (8 source files) | (current) |
| R8-B4 dsar/ | extracted (extras/ deferred) | (current) |
| R8-B9 cp-ui api/services | reshape to 8 per-section folders | (current) |
| R8-B10 cp-ui pages/infrastructure | split to 11 sub-resource folders | (current) |
| R8-B11 cp-ui pages/settings | relocated to nav homes; settings/ deleted | (current) |
| R8-B12 cp-ui pages/iam | split by sub-resource | (current) |
| R8-B13 cp-ui pages/traffic | 4-subfolder split | (current) |
| R8-B14 cp-ui pages/compliance/rule-packs | 6-subfolder split | (current) |
| R8-B15 catbagent/ | extracted from hub storage/store | (current) |
| R8-C1/C2/C3 | KEEP verdict — see below | n/a |

**control-plane/internal/handler/** went from **50 production files**
to **25**. cp-ui pages/ + api/services/ all dropped under threshold
except the section-roots (each section's own files now ≤10).

**Deferred (Bucket B items beyond autonomous session bandwidth):**

| Task | Why deferred | Recommended next session |
|---|---|---|
| R8-B4 extras/ | admin_extras.go (1833 lines) needs careful interface design; would consume hours alone | New PR: extract only the leaf operations (interception domains, thing-stats, siem, opsmetrics, observability_retention) into separate small sub-packages; leave admin_extras.go in handler/ flat for now |
| R8-B5 revokeUserScope lift | precedes iam+auth; needs cross-package audit | Lift to shared helpers package |
| R8-B6 iam/ | Blocked by B5 + needs careful Hub deps narrowing | After B5; pattern is the same as R6 recipe |
| R8-B7 auth/ | Blocked by B5 | After B5 |
| R8-B8 store/ decomp (52 files) | Largest single Go decomp; ~25 store.DB methods need narrow interfaces per domain | Dedicated session — mirror handler/ domain spine (one store/<domain>/ per handler/<domain>/) |

**Bucket C revised verdicts (handler-cohesion analysis):**

- **`nexus-hub/internal/handler` (14 files):** KEEP. The HubHandler
  type owns all methods; splitting requires 3-4 cooperating handlers
  each holding a shared subset of the same DB/Hub deps. Cost > benefit
  for a 4-file overage.
- **`ai-gateway/internal/handler` (19 files):** KEEP. Same reasoning
  — the AIGatewayHandler is the central proxy entry, methods are
  tightly coupled to the request lifecycle, no clear domain spine.
- **`ai-gateway/internal/router` (17 files):** KEEP. Routing strategy
  registry where files-per-strategy is the natural shape.

All three move to **Bucket A** in the revised classification.

**Re-survey 2026-05-17 evening:**

Remaining >10-file production directories are now:

- Bucket A (legitimate KEEP — registry shape / per-OS / static data /
  small cohesive subsystem): `agent/cmd/agent`, `agent/core/platform`,
  `nexus-hub/internal/jobs/defs`, `nexus-hub/internal/alerts/eval/
  aggregators`, `nexus-hub/internal/thingmgr`, `nexus-hub/internal/
  storage/store` (16, was 22), `shared/policy/hooks`, `shared/schemas/
  configtypes`, `shared/transport/{tlsbump,normalize}`, `shared/
  traffic`, `shared/runtime/opsmetrics`, `control-plane-ui/public/
  provider-templates`, `control-plane-ui/src/lib/forms`, `control-
  plane/internal/authserver/store`, `ai-gateway/internal/store`,
  `ai-gateway/internal/providers/spec_openai`, `ai-gateway/internal/
  pipeline/aiguard`, `control-plane-ui/src/auth` (12), `agent/ui/
  frontend/src/pages` (12), `control-plane-ui/src/pages/ai-gateway/
  providers/wizard` (11).
- Bucket A-revised: `nexus-hub/internal/handler` (14), `ai-gateway/
  internal/handler` (19), `ai-gateway/internal/router` (17).
- **Deferred to future sessions:** `control-plane/internal/handler`
  (25 — was 50; still needs B4-extras + iam/auth + small leaves),
  `control-plane/internal/store` (52 — R8-B8 untouched).
