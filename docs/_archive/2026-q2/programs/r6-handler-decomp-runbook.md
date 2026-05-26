# R6 — AdminHandler decomposition runbook (per-domain)

> **Audience:** the next session that picks up `R6` from
> `r6-r3-shared-gomod-handoff.md` §4.1. This file is the concrete
> recipe — open it alongside `control-plane-internals-architecture.md`
> (Tier 2) and `iam-identity-architecture.md` before you start.

The handoff's §4.1 describes the strategy at a high level; this
runbook captures what the 2026-05-17 design pass actually found and
the recipe a single PR can follow per domain. R6 is multi-PR by
design — do **one** domain per PR.

## 1. The God object today (2026-05-17 snapshot)

- `packages/control-plane/internal/handler/` has **414** unique
  methods on `*AdminHandler` across **76** `.go` source files
  (`grep -hE "^func \(h \*AdminHandler\) [A-Z]" ... | sort -u | wc -l`).
- The struct is defined in `helpers.go` and carries 20+ fields
  shared across every domain.
- The route-wiring entry point is `admin_routes.go` which calls
  `h.Register<Domain>Routes(g, iamMW)` for each domain.

## 2. Domain groupings (verified)

Run this command to refresh the group counts before you start:

```bash
grep -hE "^func \(h \*AdminHandler\) [A-Z]" \
  packages/control-plane/internal/handler/*.go \
  | grep -v _test.go \
  | awk '{print $4}' | sed 's/(.*//' | sort -u
```

The 2026-05-17 grouping (after de-noising verb prefixes):

| Domain | Sample methods | Approx count | Notes |
|---|---|---|---|
| `iam` | List/Get/Create/Update/Delete IAMPolicy + Group + Member; Attach/DetachPolicy; SimulateIAM | 21 (in `admin_iam.go`) | Cross-uses `revokeUserScope` (shared with `idp`). See §4. |
| `virtualkey` | ListVirtualKey, CreateVirtualKey, ApproveVirtualKey, RevokeVirtualKey, RegenerateVirtualKey | ~25 | |
| `routing` | ListRoutingRule, CreateRoutingRule, RoutingSimulate, ListProviderModel | ~30 | |
| `providers` | ListProvider, CreateProvider, AddProviderModel, TestProvider, ListCredential | ~30 | |
| `analytics` | AnalyticsByProvider, AnalyticsByUser, AnalyticsCacheROI, AnalyticsCost, AnalyticsLatencyPhases | ~25 | |
| `traffic` | ComplianceAudit, ComplianceTrinity, ProxyComplianceExport, ListTrafficEvent | ~20 | |
| `compliance` | ApproveComplianceExemption, ApproveExemption, RejectExemption, ListExemptionRequest | ~25 | |
| `agent` | ListAgentDevice, AgentFleetHealth, BulkRotateCertGroup, BulkForceRefreshGroup | ~30 | |
| `cache` | CacheGet*, CachePut*, CacheList*, CacheFlush, CachePreview | ~20 | |
| `alerts` | AckAlert, ListAlert, CreateAlertRule | ~10 | Small, good second-domain candidate. |
| `hooks` | ListHook, CreateHook, TestHook | ~15 | |
| `infra` | AdminResyncNode, Jobs*, ConfigSync*, KillSwitch | ~25 | |
| `auth` | Login, Logout, OAuth*, SSOEnroll | ~20 | Cross-uses `revokeUserScope`. |
| `settings` | ListSetting, UpdateSetting | ~10 | Smallest, good first-domain candidate if `iam` is too entangled. |
| `dsar` + extras | Misc small surfaces | ~15 | |

**Total ≈ 320; the gap to 414 is multi-method files (`admin_iam.go`
has 21; many big files have 10+ methods).**

## 3. The recipe — one domain per PR

For domain `<d>` (e.g. `iam`):

```bash
# Inside the repo root
mkdir -p packages/control-plane/internal/handler/<d>
```

### 3.1 Build the per-domain `Handler` struct

Create `packages/control-plane/internal/handler/<d>/handler.go`:

```go
package <d>

import (
    "log/slog"
    "github.com/ai-nexus-platform/nexus-gateway/packages/control-plane/internal/audit"
    "github.com/ai-nexus-platform/nexus-gateway/packages/control-plane/internal/store"
    // ... only the deps this domain's methods actually use
)

// Deps is the construction-time arg shape. main.go assembles the
// AdminHandler-equivalent value and passes the smaller Deps subset
// the domain handler needs.
type Deps struct {
    DB     *store.DB
    Audit  *audit.Writer
    Logger *slog.Logger
    // ... domain-specific fields (e.g. IAM engine, Revocation service)
}

type Handler struct {
    db     *store.DB
    audit  *audit.Writer
    logger *slog.Logger
    // ... mirror Deps
}

func New(d Deps) *Handler {
    return &Handler{
        db: d.DB,
        audit: d.Audit,
        logger: d.Logger,
        // ...
    }
}
```

### 3.2 Move the source files

For each `admin_<d>*.go`:

1. Move to `packages/control-plane/internal/handler/<d>/<short>.go`
   (e.g. `admin_iam.go` → `handler/iam/iam.go`; later batches may
   split further: `policies.go`, `groups.go`, `principals.go`).
2. Change `package handler` → `package <d>`.
3. Replace receiver `(h *AdminHandler)` → `(h *Handler)`.
4. Replace field accesses:
   - `h.DB` → `h.db`
   - `h.Audit` → `h.audit`
   - `h.Logger` → `h.logger`
   - (etc., matching the new struct)

### 3.3 Wire route registration

Add to `handler/<d>/handler.go`:

```go
import (
    "github.com/labstack/echo/v4"
    "github.com/ai-nexus-platform/nexus-gateway/packages/shared/security/iam"
)

func (h *Handler) RegisterRoutes(g *echo.Group, iamMW func(action string) echo.MiddlewareFunc) {
    g.GET("/iam/policies", h.ListIAMPolicies, iamMW(iam.ResourceIamPolicy.Action(iam.VerbRead)))
    // ... copy the rest of the iamMW-gated registrations from the
    // pre-extraction Register<D>Routes body verbatim
}
```

### 3.4 Update the central wiring

In `packages/control-plane/internal/handler/admin_routes.go`:

```go
// Before:
h.RegisterIAMRoutes(g, iamMW)

// After:
iamHandler := iam.New(iam.Deps{
    DB: h.DB, Audit: h.Audit, Logger: h.Logger,
    Revocation: h.Revocation, AuthRefreshTTL: h.AuthRefreshTTL,
    // ... whatever the domain needs
})
iamHandler.RegisterRoutes(g, iamMW)
```

The pre-extraction `RegisterIAMRoutes` method on AdminHandler is
deleted.

### 3.5 Move + update tests

For each `admin_<d>*_test.go`:

1. Move to `handler/<d>/<short>_test.go`.
2. Change package to `<d>` (or `<d>_test` for black-box style).
3. Replace `*AdminHandler` construction with `*Handler` construction.

If the test stitches together features from multiple domains, leave
it in the parent `handler` package and update it to drive both
`*AdminHandler` and the extracted `*Handler` side-by-side.

### 3.6 Verify

```bash
# Unit tests for the moved domain.
go test -race -count=1 ./packages/control-plane/internal/handler/<d>/...

# Full CP build.
go build ./packages/control-plane/...

# IAM action lockstep — every `iamMW(...)` action string must still
# match `allowedActions` in shellRouteConfig.tsx.
# (Manual grep until npm run check:iam-actions exists.)
grep -rn 'iamMW(' packages/control-plane/internal/handler/<d>/ | sort -u
grep -rn 'allowedActions:' packages/control-plane-ui/src/routes/shellRouteConfig.tsx | sort -u
```

Confirm UI `allowedActions` and the iamMW strings are still in
lockstep; otherwise a non-super-admin user just lost access to the
moved domain.

### 3.7 Commit

```
refactor(cp): extract <d> admin into handler/<d>/ subpackage

- Move admin_<d>*.go to handler/<d>/, change receiver to *Handler.
- New Handler struct takes only the Deps that <d> methods touch.
- admin_routes.go wires <d>.New(...) + RegisterRoutes(...).
- Tests moved + receiver swapped.
- IAM action strings verified against shellRouteConfig.tsx allowedActions.

R6 from r6-r3-shared-gomod-handoff.md, domain <N> of ~15.
```

## 4. Cross-domain entanglements found during the 2026-05-17 design pass

These are the snags the next session will hit. None block R6 entirely,
but each demands a deliberate decision before the iam/ split lands.

### 4.1 `revokeUserScope` is shared between iam + idp

`func (h *AdminHandler) revokeUserScope(ctx, userID, reason)` lives
in `auth_sessions.go` and is called from `admin_iam.go` (7 sites) +
`admin_identity_provider.go` (2 sites). After the iam extraction it
must stay reachable from both worlds.

**Options (in order of preference):**

1. **Lift to a package-neutral helper.** Move the body to
   `internal/authserver/revocation` as a free function `RevokeUserScope(ctx, deps, userID, reason)` taking
   `(*revocation.Service, *store.DB, *slog.Logger, time.Duration)`.
   Both call sites use it directly. `revocationRevoke` is the test
   seam — keep it as a `var = ...` in the new helper file.
2. **Duplicate.** Copy the body into the iam subpackage. Worst on
   maintenance.
3. **Inject as a callback.** `iam.Deps.RevokeUserScope func(ctx,
   userID, reason)` set by `admin_routes.go` from
   `h.revokeUserScope`. Decent if revocation isn't ready to be a
   shared helper yet.

The 2026-05-17 design pass recommends option 1; do it BEFORE the iam
move (separate PR) so the move stays mechanical.

### 4.2 Private helpers shared across files

`parsePagination`, `errJSON`, `pickInt`, etc. live at package level
in `handler/` and are called from many `admin_*.go`. The extraction
needs them in `handler/<d>/` too. **Options:**

1. **Copy.** Each subpackage gets its own copy. ~50 lines duplicated
   per domain — acceptable for a 15-domain split.
2. **Move to a small shared subpackage** `handler/internal/util/` or
   `handler/util/`. Cleaner but adds a PR.
3. **Keep in handler/** and have `handler/<d>/` import the parent.
   Architectural wart — sub-package importing parent. Avoid.

The 2026-05-17 design pass recommends starting with option 1, then
collapsing to option 2 when the third or fourth domain reveals the
duplication tax.

### 4.3 Tests that drive multiple domains

`admin_iam_action_catalog_test.go` lives in `handler/` and is named
after iam but actually calls `h.GetActionCatalog` (in
`admin_extras.go`). When the iam package moves, this test stays in
`handler/` (driving `*AdminHandler`); it does not move with iam/.
Look for this pattern in other domains too — the file-name does not
always reflect the receiver.

### 4.4 `admin_routes.go` is the single integration point

Every per-domain `Register<D>Routes` call lives in `admin_routes.go`.
After every domain moves, `admin_routes.go` becomes a small wiring
file that constructs each domain Handler and calls
`<d>.RegisterRoutes`. Plan to land `admin_routes.go` cleanups
incrementally — every domain PR touches it.

## 5. Order of extraction (recommended)

1. ✅ **`settings`** (7 methods, no cross-domain helpers). Smallest
   blast radius. Recipe confirmed by commit `57f919d31` — see commit
   message for the canonical layout + helper-copy strategy used.
2. ✅ **`alerts`** (14 methods). Self-contained. Recipe extended with a
   narrow `HubBaseURLToken` interface to decouple from the parent
   `HubNotifier` (9-method god-object) — commit `cfe039ee4`. Pattern
   should generalise to other Hub-forwarding domains (cache, hooks,
   compliance) when they extract.
3. ✅ **`hooks`** (7 admin methods + registry + classification helpers).
   `HubInvalidator` narrow interface — commit `efa61eb16`. Recipe
   handles cross-domain registry test (admin_hooks_registry_test.go
   stays in handler/ flat, references hooks.HookImplRegistry +
   hooks.ValidateHookEnums via package import alias — runbook §3.5
   guidance).
4. ✅ **`cache`** (10 CRUD methods + propagateCacheConfig + validators
   + effective/overrides views). `HubConfigChanger` narrow interface —
   commit `4429938ea`. CachePreview + AnalyticsCacheROI deferred to
   their natural domains (extras/ + analytics/).
5. ✅ **`virtualkey`** (15 methods: 6 admin CRUD + 4 approval +
   5 user surface). Two register groups + one unified Handler.
   `HubVKInvalidator` (NotifyConfigChange + InvalidateConfig) — commit
   `d09f2a43a`. user_virtual_keys.go co-located in virtualkey/user.go
   because it shares the same helpers (generateVirtualKey,
   notifyVKInvalidate).
6. ✅ **`providers`** (28 methods: providers + models + credentials +
   reliability + key-rotation, 5 source files, 1675 lines). Largest
   single extraction. `HubInvalidator` narrow interface — commit
   `e53b69775`. Cross-pkg ref (admin_extras.go) needed providers.IsValidAdapterType
   exported.
7. ✅ **`routing`** (7 methods + RoutingSimulate). 3 test files ported
   with full test-helper copy (hubSpy, auditSpy, echoContext,
   assertErrorEnvelope, newAdminHandlerWithHubSpy) — commit `ef8c16c84`.
8. ✅ **`analytics`** (17 methods, 7 source files, 2202 lines).
   Read-only domain: no Hub, no Audit. Includes cache_roi +
   cost_summary (originally deferred from cache/). Cross-pkg refs:
   queryMetricsOrFallback + parseTimeRange kept in handler/ flat as
   local copies for admin_extras/admin_proxy_rollup callers — commit
   `e023aa16d`.
9. ✅ **`traffic`** (~20 methods, 8 source files + 4 tests, 1500 lines).
   Includes admin_traffic + admin_proxy + admin_proxy_rollup +
   admin_traffic_adapters + admin_compliance (compliance reports are
   traffic-event analyses; the compliance kill-switch + exemption
   admin stays for the next compliance/ extraction) — commit `4fe1f14cf`.

⏳ **Remaining (6 domains).** See "Order of extraction" §5 below for
priorities. Settings/, alerts/, hooks/, cache/, virtualkey/,
providers/, routing/, analytics/, traffic/ — the easy + middle-bound
slices — are all DONE. What's left tends to need pre-work:
  - **compliance/** — exemption grants + projection + requests +
    killswitch + passthrough + aiguard (~25 methods, 2158 lines).
    Aborted on first try due to scope creep: passthrough has its
    own HubNotifier-derived interface, aiguard has h.AIGuard field
    dependency, exemptions has its own complianceExemptionDataLayer
    interface seam already declared. Restart with all 4 sub-clusters
    as separate compliance/{exemption,killswitch,passthrough,aiguard}.go
    files; expand Deps to carry the AIGuard subsystem too.
  - **agent/** — agent device + group management (~30 methods).
  - **iam/** — needs the revokeUserScope helper-lift PR first (§4.1).
  - **auth/** — same revokeUserScope dependency.
  - **infra/** — node runtime + jobs + config-sync + organization
    routes.
  - **dsar/ + extras/** — small remainder.

Order revised vs runbook §5 based on actual extraction experience:
the hardest entanglements are in compliance/ (4 sub-clusters with
different Hub interfaces) and iam/auth/ (revokeUserScope sharing).
Save iam/auth/ for last; compliance/ can be split into 4 separate
PRs for safety.
3. **`hooks`** (15 methods). Self-contained.
4. **`cache`** (20 methods). Self-contained.
5. **`virtualkey`** (25 methods). First non-trivial domain.
6. **`iam`** (21 methods). Requires the §4.1 revokeUserScope
   refactor PR landed first.
7. **`auth`** (20 methods). Same revokeUserScope dependency.
8. … remaining domains in any order …
15. **`analytics`** (25 methods). Big, but most independent.

The handoff §4.1 step 3 explicitly recommends iam/ as the canonical
first; the 2026-05-17 design pass downgrades that recommendation —
iam touches revokeUserScope and the audit.Writer surface area is
larger than other domains. settings/ is the safer canonical first.

## 6. When to update the Tier 2 arch doc

`control-plane-internals-architecture.md` carries the
"Currently keeping flat for grep-friendliness" guidance. **Update
this doc in the same PR as the first domain extraction (most likely
`settings/`).** The new wording should:

- Drop the 100-file threshold (it was a useful heuristic but the
  refactor program now supersedes it).
- Point to this runbook for the per-domain extraction recipe.
- List which domains are extracted (growing list as work lands).

The first extraction PR adds a new arch-doc-triggers row if a domain
gets its own dedicated arch doc; existing rows for `handler/**` keep
pointing at the parent doc.

## 7. Verification checklist for every PR

- [ ] `go build ./packages/control-plane/...` clean.
- [ ] `go test -race -count=1 ./packages/control-plane/internal/handler/<d>/...` passing with ≥95% coverage.
- [ ] IAM action strings match `shellRouteConfig.tsx` `allowedActions` (manual grep until lint exists).
- [ ] `npm run check:coverage` (existing pre-commit) green.
- [ ] No `*AdminHandler` receiver left in moved files.
- [ ] Old `Register<D>Routes` on `*AdminHandler` deleted.
- [ ] `admin_routes.go` calls `<d>.New(...).RegisterRoutes(...)`.
- [ ] Commit message follows the `refactor(cp): extract <d> admin into handler/<d>/ subpackage` shape so the program is greppable post-hoc.

## 8. When all domains have landed

- Delete `helpers.go`'s `AdminHandler` struct (or shrink it to a
  shell-of-its-former-self for the routes that genuinely cross all
  domains, if any survive).
- Update `control-plane-internals-architecture.md` to reflect the
  new shape (list of subpackages + their owners + the cross-domain
  helpers that survived in handler/).
- Close out R6 in the program tracker:
  `[[project_repo_reorg_program]]` memory entry → R6 DONE.
- Delete this runbook (it lives in `docs/dev/_internal/`, so it can
  go without an arch-doc-triggers update).
