# Remaining Architecture Refactors — Session Handoff (2026-05-17)

> Written per CLAUDE.md → Mandatory rules → "Handoff at context-full". The
> next session should pre-load this file, then resume from §4 "Per-task
> execution plan".

## 1. Program goal + current phase

**Goal.** Finish the 8-item architecture refactor program brainstormed
2026-05-17. Five items shipped that session:

- ✅ R1 — shared/ 36 → 7 buckets (commit `66bd0ade7`)
- ✅ R2 — agent/core/ 34 → 7 buckets (commit `99aa5b549`)
- ✅ R5 — httpclient bypass — 2 fixes (commit `fd96ce5e1`)
- ✅ R5b — forbidigo lint refresh (commit `4d6cdd33e`)
- ✅ R7 — naming-duplicate consolidation (commit `3a6f3c68d`)
- ✅ R4 — investigation only; no work needed (all 3 services already use `shared/compliance`)
- ✅ R8 — hub + ai-gw second-pass bucket reorg (commit `c7413702d`)

The three deferred items were partially landed in the 2026-05-17
follow-on session (the user explicitly authorized "all three together"
with maintainer override of the §4-retirement decision):

- ✅ **R3 — FULLY DONE.** `shared/transport/configloader` shipped
  with `Loader` + `Handler[V]` + Cat B `WithPuller`/`NeedsPull`/
  `RefreshPullKeys` extensions, 100% coverage. Four services
  refactored, each in its own commit: compliance-proxy `c63a996c3`,
  control-plane `c151ba389`, ai-gateway `318238e68`, agent
  `c2d9bfe67` (also retired `configsync.Manager` — the old
  hand-rolled dispatcher). The configloader API extension PR was
  `f451b6c9c`. nexus-hub is N/A — uses internal/selfshadow.Manager
  (PostgreSQL LISTEN-based; already Register-based pattern).
- ❌ **R7-gomod — RETRACTED.** The schemas canonical first split
  (commit `2fa1134ce`) was reverted in the same day's later half by
  maintainer decision: shared stays one umbrella module. The
  retraction commit reverts go.work + 6 go.mod files + deletes
  `packages/shared/schemas/go.mod` + restores
  `shared-package-architecture.md` §4 to "umbrella is the chosen shape".
  The 2026-05-16 retirement of the split is the binding decision again.
  Re-opening requires a concrete trigger (binary-size budget,
  supply-chain SBOM constraint, contributor friction) — the OSS
  contributor-surface argument used as trigger in the 2026-05-17
  brainstorm did not survive maintainer review.
- ✅ **R6 (design + runbook + first domain)** — runbook
  `docs/dev/_internal/r6-handler-decomp-runbook.md` + Tier 2 arch doc
  updated (commit `9c934b05d`). First domain `handler/settings/`
  extracted as canonical pattern (commit `57f919d31`). Remaining 14
  domains follow the runbook §5 order (alerts/ next).

The architectural decisions standing as of the retraction:
- `shared-package-architecture.md` §4 — split policy RETIRED (umbrella
  is the chosen shape; 2026-05-16 decision restored after the
  2026-05-17 experiment was retracted).
- `control-plane-internals-architecture.md` "When you change one of
  these" — 100-file flat heuristic retired; runbook in motion.

## 2. Load-bearing facts the next session needs

### 2.1 Current shared/ + agent/core/ layout (post-R1/R2/R7/R8)

```
packages/shared/                         packages/agent/core/
├── audit/         (4)                   ├── compliance/   (4)
├── compliance/    (15)                  ├── platform/    (21)
├── traffic/       (19 + adapters/)      ├── control/      (4)
├── runtime/       (7)                   ├── network/      (5)
├── transport/     (9)                   ├── rules/        (3)
├── storage/       (5)                   ├── security/     (5)
├── security/      (3)                   ├── sync/         (5)
├── policy/        (5)                   ├── observability/(7)
└── schemas/       (4)                   └── host/         (3)

packages/nexus-hub/internal/             packages/ai-gateway/internal/
(11 top-level dirs)                      (13 top-level dirs)
├── alerts/        (R7)                  ├── cache/        (R7)
├── jobs/          (R7)                  ├── credentials/  (R7)
├── self/          (R7)                  ├── providers/    (R7)
├── identity/      (R8)                  ├── pipeline/     (R8)
├── storage/       (R8)                  ├── execution/    (R8)
├── observability/ (R8)                  ├── observability/(R8)
├── config, handler, ws, jwks, thingmgr  ├── config, handler, middleware,
                                         │   router, runtimeapi, store, streaming
```

### 2.2 Binding rules to respect

From CLAUDE.md:
- **English only** for all committed text.
- **Plan first + Todo non-waivable** — write the plan, capture as `TaskCreate` before edits.
- **Parallel-session safety** — always commit with `--` pathspec; never
  `git stash`; never `git restore <file>`; wait for `.git/index.lock`.
- **Pre-edit reading 3-doc rule** — for R6 read
  `docs/dev/architecture/iam-identity-architecture.md`,
  `docs/dev/architecture/oauth-pkce-admin-auth-architecture.md`,
  `docs/dev/workflow/conventions.md`.
- **Coverage ≥95% per package** — every new handler subpackage must
  hit 95% OR get an allowlist entry with category rationale.
- **IAM impact review** mandatory when admin endpoint moves —
  `iamMW(action)` strings must match `allowedActions` in the UI.

### 2.3 Memory anchors that apply

- `[[feedback_autonomous_retry_brainstorm]]` — 5min retry on transient
  errors; internal brainstorm for blockers; best architecture wins.
- `[[feedback_autonomous_execution]]` — self-driven after plan approval.
- `[[project_parallel_worktree_sessions]]` — explicit pathspec on every
  commit; the parallel test-coverage session may interleave.
- `[[project_repo_reorg_program]]` — preceding doc/dev reorg context.

## 3. Why these three are not "do now"

| | R6 (AdminHandler) | R3 (configloader) | R7-gomod (shared/ split) |
|---|---|---|---|
| Scope | 414 methods × ~10-15 domains | 23 files across 4 services | 7+ new go.mod files |
| Design needed | Per-domain DI contract, helper extraction strategy, route-registration refactor | Generic interface for parse/apply/version-track, per-key adapter registration | Driver-scoped dep grouping, replace-directive chain across 5 service go.mods |
| Risk if rushed | Break 100+ test files, IAM action drift, route 404s | Hot-path config apply breakage, missed Cat-B keys | Build breakage across 5 services; OSS readiness setback |
| Right cadence | Multi-week, ~10 PRs (one per domain) | 1-2 day spec + 2-3 day impl | 1 day execution after spec |

The session that just landed R1/R2/R5/R7/R8 has done 5 reorgs of
moderate complexity in one go. Adding any of these three on top would
exceed the responsible "blast radius" budget for a single session.

## 4. Per-task execution plan

### 4.1 R6 — AdminHandler decomposition (10-15 PRs)

**Approach: extract one domain per PR, smallest first.**

**Step 1 (this fresh session, before code): design pass.**

Open `packages/control-plane/internal/handler/helpers.go` and read the
`AdminHandler` struct. Document every field with its type + which
domains use it. The struct is the God-object surface; splitting it
means each domain handler gets only the fields its methods actually
touch.

**Step 2: produce the domain grouping.**

Run:
```bash
grep -hE "^func \(h \*AdminHandler\) [A-Z]" \
    packages/control-plane/internal/handler/*.go \
  | grep -v _test.go \
  | awk '{print $4}' | sed 's/(.*//' | sort -u
```

That gives 414 methods. Group by domain noun (after the HTTP-verb
prefix). Verb prefixes are List/Get/Update/Delete/Create/Register;
the noun is the domain. Suggested initial groupings (refine during
design):

| Domain | Sample methods | Approx count |
|---|---|---|
| `iam/` | List/Get/Create/Update/Delete IAMPolicy, IAMGroup, IAMUser, IAMPrincipal, Attach/DetachPolicy | ~40 |
| `virtualkey/` | ListVirtualKey, CreateVirtualKey, ApproveVirtualKey, RevokeVirtualKey, RegenerateVirtualKey | ~25 |
| `routing/` | ListRoutingRule, CreateRoutingRule, RoutingSimulate, ListProviderModel | ~30 |
| `providers/` | ListProvider, CreateProvider, ListProviderModel, AddProviderModel, TestProvider, ListCredential | ~30 |
| `analytics/` | AnalyticsByProvider, AnalyticsByUser, AnalyticsCacheROI, AnalyticsCost, AnalyticsLatencyPhases, etc. | ~25 |
| `traffic/` | ComplianceAudit, ComplianceTrinity, ProxyComplianceExport, ListTrafficEvent | ~20 |
| `compliance/` | ApproveComplianceExemption, ApproveExemption, RejectExemption, ListExemptionRequest | ~25 |
| `agent/` | ListAgentDevice, AgentFleetHealth, BulkRotateCertGroup, BulkForceRefreshGroup | ~30 |
| `cache/` | CacheGet*, CachePut*, CacheList*, CacheFlush, CachePreview | ~20 |
| `alerts/` | AckAlert, ListAlert, CreateAlertRule | ~10 |
| `hooks/` | ListHook, CreateHook, TestHook | ~15 |
| `infra/` | AdminResyncNode, Jobs*, ConfigSync*, KillSwitch | ~25 |
| `auth/` | Login, Logout, OAuth*, SSOEnroll | ~20 |
| `settings/` | ListSetting, UpdateSetting | ~10 |
| `dsar/` + extras | Misc small surfaces | ~15 |

Adjust based on `grep` output. Aim for 10-15 domains, each ~20-40
methods.

**Step 3: execute one domain extraction as the canonical pattern.**

Pick `iam/` (well-bounded, no test infra surprises). Steps:

1. `mkdir packages/control-plane/internal/handler/iam`
2. Create `packages/control-plane/internal/handler/iam/handler.go`
   with:
   ```go
   package iam

   type Handler struct {
       Store store.Store
       Logger *slog.Logger
       // Add ONLY what IAM methods actually use
   }

   func New(deps Deps) *Handler { ... }
   func (h *Handler) RegisterRoutes(g *echo.Group, iamMW func(action string) echo.MiddlewareFunc) { ... }
   ```
3. Move each `admin_iam_*.go` file:
   - Rename to plain `policies.go`, `groups.go`, etc.
   - Change package decl from `handler` to `iam`
   - Change method receivers from `(h *AdminHandler)` to `(h *Handler)`
   - Replace `h.DB` with `h.Store`, etc.
4. Move IAM-specific helpers from `admin_iam.go`'s package-level helpers
   to `iam/helpers.go`
5. Update `packages/control-plane/cmd/control-plane/main.go` to wire
   `iam.New(...)` and call `iam.RegisterRoutes(...)`
6. Update + move IAM test files
7. Verify `go test ./packages/control-plane/internal/handler/iam/...`
8. Verify `iamMW` action strings still match `allowedActions` in CP UI
   (run `npm run check:iam-actions` if it exists, or grep manually)
9. Commit `refactor(cp): extract IAM admin into handler/iam/ subpackage`

**Step 4: repeat for each remaining domain (~10-14 more PRs).**

Each PR follows the same shape. Order by smallest-domain-first so the
pattern stabilises before tackling the big ones (analytics, agent).

**Step 5: collapse AdminHandler when empty.**

When the last method moves out, delete `helpers.go` and any remaining
`admin_*.go` stubs. Update `main.go` to remove the AdminHandler
wiring.

### 4.2 R3 — shared/configloader abstraction

**Approach: design + implement in one session (1-2 days).**

**Design contract (`packages/shared/configloader/configloader.go`):**

```go
package configloader

// KeyHandler is what each service registers for a single Cat-B key.
type KeyHandler[V any] struct {
    Key    string                       // Cat-B key name (e.g. "killswitch")
    Parse  func(raw []byte) (V, error)
    Apply  func(ctx context.Context, v V) error
}

// Loader subscribes to thingclient shadow deltas, dispatches each
// incoming delta to the matching KeyHandler, tracks per-key
// reportedVer, and acks back to Hub.
type Loader struct {
    thingClient *thingclient.Client
    handlers    map[string]anyHandler
    logger      *slog.Logger
}

func New(tc *thingclient.Client, logger *slog.Logger) *Loader { ... }
func Register[V any](l *Loader, h KeyHandler[V]) { ... }
func (l *Loader) Run(ctx context.Context) error { ... }
```

**Per-service refactor:**

- `packages/compliance-proxy/internal/configloader/` (11 files) →
  collapses to ~3 files: one per Cat-B key (`allowlists.go`,
  `hooks.go`, etc.) where each calls `configloader.Register(...)`.
- `packages/agent/core/sync/configsync/` (7 files) → collapses similarly.
- `packages/ai-gateway/internal/config/` (3 files) → minor changes.
- `packages/control-plane/internal/config/` (2 files) → minor changes.

**Verification:** smoke each service config-reload path; confirm
Cat-B `killswitch` apply still works end-to-end on the local stack.

### 4.3 R7-gomod — shared/ go.mod split

**Approach: design + execute in one focused session (1 day).**

**Target modules:**

```
packages/shared-core/        # only stdlib + slog + pgx + prometheus
packages/shared-transport/   # adds golang.org/x/net, coder/websocket, klauspost/compress
packages/shared-mq/          # adds nats.go
packages/shared-storage/     # adds aws-sdk-go-v2 (S3), redis/go-redis
packages/shared-security/    # adds golang-jwt
packages/shared-policy/      # adds bloom/v3
packages/shared-schemas/     # only stdlib
```

`packages/shared/audit`, `compliance`, `traffic` stay where they are
(they import across several of the above) — they become the umbrella
"shared-app" module that depends on all the split modules.

**Per-service updates:**

Each consumer service's `go.mod` adds `require` lines for ONLY the
shared modules it actually uses, with matching `replace` directives.
Run `go mod tidy` per service after the split to prune unused require
lines.

**Verification:** `npm run check:workspace-replace` passes; every
service builds; binary size drops noticeably for services that no
longer pull in NATS or AWS SDK transitively.

## 5. Open questions for the maintainer

None blocking — the design choices above are concrete enough to execute.
Surface anything that conflicts with the maintainer's hidden constraints
before starting Step 3 of any of the three plans.

## 6. After all three land

- Delete this handoff file (`docs/dev/_internal/r6-r3-shared-gomod-handoff.md`).
- Update `[[project_repo_reorg_program]]` memory entry to reflect DONE-DONE.
- Run `npm run check:all` once more to confirm CI green.
- The Nexus Gateway architecture refactor program is then truly closed.

## 7. Session 2 outcomes (2026-05-17 cont'd) — partial landings

After this handoff was written, the user revisited the same session
budget question and explicitly authorized "all three together" with
maintainer override of any conflicting Tier 1 arch-doc decisions. The
follow-on session landed the canonical slice of each item — not the
full execution. Remaining work for fresh sessions:

### 7.1 R3 partial landing (commit `c63a996c3`)

**FULLY DONE.** `packages/shared/transport/configloader/` shipped
with generic `Loader` + `Handler[V]` + Cat B
`WithPuller`/`NeedsPull`/`RefreshPullKeys` extensions, 100% unit-
test coverage, continue-on-error semantics, OutcomeTracker
integration. All four services migrated:

- compliance-proxy `c63a996c3` — 11 keys, 245 → 32-line wrapper +
  290-line configdispatch.go (main.go: 1221 → 1022).
- control-plane `c151ba389` — 2 keys (observability, log_level),
  35-line switch → 28-line dispatch helpers.
- ai-gateway `318238e68` — 18 keys across 16 case labels, 292 →
  ~30-line wrapper + 441-line configdispatch.go (main.go: 1779 → 1506).
  Uses a getter-func pattern for the late-bound aiguardConfigCache
  singleton; see commit message for the cross-pattern notes.
- configloader Cat B API extension `f451b6c9c` — Puller +
  NeedsPull + RefreshPullKeys (additive; 100% coverage maintained).
- agent `c2d9bfe67` — 13 keys + 2 acknowledged-external no-ops,
  shadowMgr → cfgLoader with Cat B HTTP-pull semantics; deleted
  configsync.Manager + manager_test.go (the old hand-rolled
  dispatcher); main.go: 3496 → 3388. configsync pkg retained types
  still consumed elsewhere (ShadowApplier, AdapterFunc, ConfigState,
  ConfigSnapshot, InterceptionDomainDTO).

`packages/nexus-hub/cmd/nexus-hub/main.go` is **N/A** — Hub uses its
own `internal/selfshadow.Manager` (PostgreSQL LISTEN-based, not
thingclient.OnConfigChanged) which is already a Register-based
dispatcher pattern. No refactor needed.

R3 closeout: every Thing that consumes
thingclient.OnConfigChanged now uses the shared
configloader.Loader. Adding a new shadow key to any service is a
one-line change in that service's configdispatch.go.

**§7.1.a Agent slice — ALL STEPS DONE.**

Agent has `core/sync/configsync/Manager` which is ALREADY a per-key
applier-table dispatcher (the pattern configloader generalises). The
agent refactor must replace Manager's hand-rolled dispatch table with
`configloader.Register*` calls AND add Cat B HTTP-pull semantics
(Manager pulls from Hub for `needsPull` keys; configloader needed a
matching API).

**Step 1 — configloader API extension (DONE, commit `f451b6c9c`).**
The shared package now exposes:
  - `Puller func(ctx, key) ([]byte, error)` + `WithPuller(p) Option`
  - `Handler[V].NeedsPull bool` + `RegisterRawPull()` variant
  - `(*Loader).RefreshPullKeys(ctx) → (applied, failed int)` for boot
  - All additive; 100% test coverage maintained.

**Step 2-4 — agent migration (DONE, commit `c2d9bfe67`).** The
historical Manager-specific code (NewManager, ManagerConfig,
dispatchTable, ApplyDesired, RefreshPullKeys, pullConfig,
acknowledgedExternalKeys) was all retired. ShadowApplier +
AdapterFunc + ConfigState moved to a new types.go because other
agent subsystems still implement them. The runbook table below is
preserved as the historical record of the migration.

**Step 2 — write `packages/agent/cmd/agent/configdispatch.go`.**
Mirror the compliance-proxy / ai-gateway pattern. 13 keys to
register; the existing inline closures from `cmd/agent/main.go`
ManagerConfig (lines 565-764) translate roughly 1:1 into
RegisterRaw / RegisterRawPull calls:

| Key                    | Cat | Wrap                                            |
|------------------------|-----|-------------------------------------------------|
| exemptions             | A   | `teeCatB("exemptions", exemptionStore)`         |
| killswitch             | A   | `killSwitch` (direct ApplyShadowState)          |
| observability          | A   | inline `tp.Reconfigure` closure                 |
| timing_intervals       | A   | inline heartbeat + drain swap closure           |
| log_level              | A   | `logging.SetLevel` closure                      |
| agent_settings         | A   | inline config-merge + QUIC bundles closure      |
| policy_rules           | B   | `teeCatB("policy_rules", policyEngine)`         |
| interception_domains   | B   | `teeCatB("interception_domains", pipeline)`     |
| hook_config            | B   | `teeCatB("hook_config", pipeline)`              |
| payload_capture        | B   | `teeCatB("payload_capture", payloadCaptureStore)` |
| streaming_compliance   | B   | `teeCatB("streaming_compliance", streamingPolicyStore)` |
| installed_rule_packs   | B   | `teeCatB(..., pipeline.ApplyRulePacksShadowState)` |
| user_context           | B   | `teeCatB("user_context", no-op)` (view-only)    |

`teeCatB` (defined inline in main.go) wraps the applier so a
successful apply also records bytes into `policiesCache`. Keep this
helper inline OR migrate to a `policies.TeeApplier` constructor that
the configdispatch.go calls directly.

`auth`, `diag_mode` are processed by OTHER subsystems
(`acknowledgedExternalKeys` in configsync/manager.go); register them
as no-op handlers in the loader so the Loader's WARN-on-unknown
doesn't fire.

The Puller closure copies the body of `Manager.pullConfig`:
HTTP-GET `<hubHTTPURL>/api/internal/things/config/<key>?type=agent`
with Bearer auth + `X-Thing-Id` header.

**Step 3 — switch `cmd/agent/main.go` from shadowMgr to cfgLoader.**
Three call sites:

  - line 811 `shadowMgr.ApplyDesired(ctx, csDesired)` →
    `cfgLoader.Apply(ctx, desired)` (no ConfigState conversion;
    same type).
  - line 1600 `shadowMgrLocal.RefreshPullKeys(refreshCtx)` →
    `cfgLoader.RefreshPullKeys(refreshCtx)` (ignore return values
    via `_, _ = ` since old API was void).
  - line 1847 `shadowMgr.RefreshPullKeys(ctx)` → same.

The `tc.OnConfigChanged` wrapper (lines 790-873) shrinks
substantially: the manual outcomes.Record loop and the
csReported / csOutcomes conversion both go away (Loader handles
them). Preserve the `auth` routing branch and the configKeyRecorder
calls (if any — agent may not have an introspect recorder).

**Step 4 — delete `packages/agent/core/sync/configsync/manager.go`.**
Other configsync files (snapshot.go, cache.go, offline.go,
dto_to_domainpolicy.go, the ShadowApplier / AdapterFunc /
ConfigSnapshot / InterceptionDomainDTO types) stay — they're
imported by `agent/core/compliance/pipeline.go` and others.

After deletion, `configsync.NewManager`, `ManagerConfig`,
`ApplyDesired`, `RefreshPullKeys`, `dispatchTable`, `pullConfig`
are gone. `ConfigState` and the `acknowledgedExternalKeys` map can
go with manager.go (only Manager used them).

**Verification per step:**

  - `go build ./packages/agent/...` clean after each step.
  - `go test -race -count=1 ./packages/agent/core/sync/configsync/`
    passes (manager_test.go gets deleted with manager.go).
  - End-to-end: spin up local Hub + agent, push a shadow change
    (kill switch toggle), verify the agent applies and reports.

Lower-priority than the three server services because Manager is
already a Register-based pattern internally — the agent works today.
The refactor is for consistency (every service uses the same
shared/transport/configloader.Loader) and so future Cat B keys can
be added in one place.

### 7.2 R7-gomod — RETRACTED (2026-05-17)

The canonical first split landed in commit `2fa1134ce` (schemas/
nested go.mod) and was retracted in the same day's later half by
maintainer decision. The retraction restored the umbrella shape and
re-applied the 2026-05-16 "umbrella is the chosen shape" Tier 1 §4
stance. The brief security/ second split was reversed before any
commit landed (Edit-revert in working tree only).

**Lessons captured for any future re-opening:**

- The OSS contributor-surface argument ("umbrella's driver-scoped dep
  list obscures per-bucket ownership") used as trigger in the
  2026-05-17 brainstorm did **not** survive maintainer review. The
  §3 dep-tier table in `shared-package-architecture.md` is already
  the single-source-of-truth for per-bucket dep ownership; promoting
  that table to module-boundary level adds churn without proportional
  benefit.
- Nested go.mod approach (chosen over the top-level rename for
  blast-radius reasons) is verified to work for stdlib-only buckets
  (`schemas/` built cleanly). The technique stays catalogued — if a
  future trigger justifies re-opening, this section's recipe was
  proven, not the policy.
- Cross-bucket import audit done during the 2026-05-17 design pass:
  - **0 cross-bucket imports:** `runtime/`, `security/`, `schemas/`.
    Clean leaf-module candidates if ever re-opened.
  - **1 cross-bucket import:** `storage/` (→ `audit`), `policy/`
    (→ `transport/`). Circular-with-umbrella risk; need
    audit/transport lifted first.
  - **7 cross-bucket imports:** `transport/` (→ audit, compliance,
    policy, runtime, schemas, traffic). Worst case; would need
    sequential pre-work.

  This audit stays useful even though the split is retracted; if the
  shape is ever revisited the dep graph is documented.

**Do not re-open without a written concrete trigger**: binary-size
budget, supply-chain SBOM constraint, or contributor friction with a
specific incident citation. The 2026-05-17 retraction is the
canonical record of "this was tried; the umbrella stays".

### 7.3 R6 partial landing (commit `9c934b05d`)

**Done.** Design pass complete: 414 unique `*AdminHandler` methods
across 76 .go files, 15 candidate domains identified. Runbook at
`docs/dev/_internal/r6-handler-decomp-runbook.md` with per-PR recipe
+ cross-domain entanglements found (revokeUserScope shared between
iam + idp; private helpers parsePagination/errJSON/pickInt at
package level; tests named after one domain that drive another).
Tier 2 arch doc `control-plane-internals-architecture.md` updated.

**Remaining.** Execute the runbook one domain per PR. Order revised
vs handoff §4.1: settings → alerts → hooks → cache → virtualkey → iam
→ auth → … (iam dropped from canonical-first slot because of the
revokeUserScope entanglement). 15 domains × ~1 PR = 15 PRs.

The runbook §4.1 calls out a pre-requisite PR: lift `revokeUserScope`
out of `auth_sessions.go` into a package-neutral helper in
`internal/authserver/revocation/` BEFORE the iam or auth extraction
PRs.
