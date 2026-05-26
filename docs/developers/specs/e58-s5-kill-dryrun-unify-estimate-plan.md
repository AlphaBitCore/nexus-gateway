# E58-S5 — Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use `superpowers:subagent-driven-development` (recommended) or `superpowers:executing-plans` to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Delete the `nexus.dry_run` request-extension flag and every entangled code path so that `POST /v1/estimate` is the AI Gateway's sole cost-preview surface, fully independent of routing engine, hook pipeline, cache, traffic_event audit, and quota.

**Architecture:** Deletion-only refactor across five subsystems (AI Gateway proxy, shared Go libs, Control Plane analytics/store, DB schema, Control Plane UI). One Prisma migration drops three TrafficEvent columns + one VirtualKey column. No new features. `/v1/estimate` (E58-S4) is **not** modified — it stays exactly as-shipped.

**Tech Stack:** Go 1.25+, Prisma migrations + pgx, React + TypeScript + Vite, i18next, Python smoke harness.

**Source of truth:** `docs/developers/specs/e58-s5-kill-dryrun-unify-estimate.md` — read it before starting. This plan is execution order; the spec carries the line-number references, DO-NOT-TOUCH list (§4.7.1), and acceptance criteria (§6).

---

## Pre-flight

### Task 0: Create isolated worktree

Per CLAUDE.md "Worktree per session (binding)" — this implementation is non-trivial and must land via PR, not direct-to-develop.

**Files:**
- New worktree: `./worktrees/e58-s5-kill-dryrun/`
- New branch: `feature/e58-s5-kill-dryrun` (from `develop`)

- [ ] **Step 1: Verify spec is committed on develop**

```bash
git log --oneline -1 -- docs/developers/specs/e58-s5-kill-dryrun-unify-estimate.md
```

Expected: shows commit `5a0f69e1d docs(specs): E58-S5 — kill nexus.dry_run flag, unify on /v1/estimate`.

- [ ] **Step 2: Create the worktree from develop**

```bash
git fetch origin develop
git worktree add ./worktrees/e58-s5-kill-dryrun -b feature/e58-s5-kill-dryrun origin/develop
```

Expected: `Preparing worktree (new branch 'feature/e58-s5-kill-dryrun')` + `HEAD is now at <sha> ...`.

- [ ] **Step 3: cd into worktree and verify clean**

```bash
cd ./worktrees/e58-s5-kill-dryrun
git status --short
```

Expected: empty output (clean tree).

- [ ] **Step 4: Verify spec is reachable from the worktree**

```bash
test -f docs/developers/specs/e58-s5-kill-dryrun-unify-estimate.md && echo OK
```

Expected: `OK`.

All subsequent tasks operate in `./worktrees/e58-s5-kill-dryrun/`.

---

## Phase 1 — Backend Go deletion

### Task 1: Strip dry-run from AI Gateway proxy + dry_run.go + canonicalext

**Why:** `proxy.go`, `dry_run.go`, and `canonicalext.IsDryRun` form one tightly-coupled triangle. Build will break unless changed atomically.

**Files:**
- Delete: `packages/ai-gateway/internal/ingress/proxy/dry_run.go`
- Modify: `packages/ai-gateway/internal/ingress/proxy/proxy.go` (per spec §4.1.2 — lines 445, 498–510, 695, 740, 942, 967–969, 1047–1058, 1412–1430, 1718, 1775)
- Modify: `packages/shared/canonicalext/` (per spec §4.1.3 — remove `nexus.dry_run` parse + `IsDryRun` function)

- [ ] **Step 1: Locate the canonicalext file holding `IsDryRun`**

```bash
grep -rn "func IsDryRun\|nexus.dry_run\|nexus_dry_run" packages/shared/canonicalext/ packages/shared/canonical/ 2>/dev/null
```

Expected: 1–3 hits in a single file.

- [ ] **Step 2: Delete `dry_run.go` entirely**

```bash
git rm packages/ai-gateway/internal/ingress/proxy/dry_run.go
```

- [ ] **Step 3: Strip `isDryRun` branches from `proxy.go`**

Open `packages/ai-gateway/internal/ingress/proxy/proxy.go`. Apply these edits (line numbers per spec; verify with `grep -n` first if drift suspected):

```
Line 445:  Remove:  isDryRun := canonicalext.IsDryRun(body)
Line 942:  Remove:  if !isDryRun { isDryRun = canonicalext.IsDryRun(prepReq.Body) }
Lines 498–510:  Collapse `if isDryRun { checkDryRunRateLimit } else { checkRateLimit }`
                into a single `h.checkRateLimit(w, vkMeta)` call.
Line 695:  Remove:  if !isDryRun { ... } wrapper around h.checkQuota
                Quota check becomes unconditional.
Line 740:  Change:  h.runRequestHooks(..., isDryRun, logger)
            To:     h.runRequestHooks(..., logger)
Lines 967–969:  Remove the cache-HIT branch dry-run dispatch.
Lines 1047–1058:  Remove the cache-MISS branch dry-run dispatch.
Lines 1412–1430:  Remove dryRunRateLimitDefault const + checkDryRunRateLimit function.
Line 1718:  Change runRequestHooks signature — drop the `isDryRun bool` parameter.
Line 1775:  Change `pipeline.SetAllowModify(!isDryRun)` to `pipeline.SetAllowModify(true)`.
```

- [ ] **Step 4: Strip `IsDryRun` + `nexus.dry_run` from canonicalext**

In the file identified in Step 1, delete:
- The `IsDryRun` function (and any `IsDryRunFromRaw` variant).
- Any `nexus.dry_run` JSON-path parsing helper.
- Any const naming the field (e.g. `extKeyDryRun = "dry_run"`).

- [ ] **Step 5: Verify build**

```bash
go build ./packages/ai-gateway/... ./packages/shared/...
```

Expected: clean build, no errors. If unresolved references to `IsDryRun` or `isDryRun` appear, locate via `grep -rn` and remove.

- [ ] **Step 6: Verify unit tests still compile**

```bash
go test -count=1 -run=NONE ./packages/ai-gateway/... ./packages/shared/...
```

Expected: all packages compile. Tests asserting on removed paths will be addressed in Task 9 (Go unit-test sweep) — they may fail when run; capture the failing-test list as you go.

- [ ] **Step 7: Commit**

```bash
git add packages/ai-gateway/internal/ingress/proxy/ packages/shared/canonicalext/ packages/shared/canonical*/
git status --short
git commit -m "$(cat <<'EOF'
refactor(ai-gateway): strip nexus.dry_run flag from proxy + canonicalext

Delete the per-request dry-run code path: dry_run.go file, all isDryRun
branches in proxy.go (rate limit, quota bypass, dispatch sites, hook
modify gate), and canonicalext.IsDryRun parser. Real-call path now
carries zero estimate logic. POST /v1/estimate (E58-S4) is the sole
cost-preview surface.

Part of E58-S5. Spec: docs/developers/specs/e58-s5-kill-dryrun-unify-estimate.md

Co-Authored-By: Claude Opus 4.7 (1M context) <noreply@anthropic.com>
EOF
)"
```

---

### Task 2: Strip `DryRunRateLimitRpm` from vkauth + admin VK surface

**Why:** Coupled across (Go vkauth struct, admin VK CRUD handler, admin API schemas, SQL queries). Build breaks if partial.

**Files:**
- Modify: `packages/ai-gateway/internal/auth/vkauth/` (locate via grep)
- Modify: Control Plane admin VK handler (locate via grep)
- Modify: VK schema definitions in admin API
- (DB column drop deferred to Task 4 — code stops referencing the column first.)

- [ ] **Step 1: Locate all `DryRunRateLimitRpm` references**

```bash
grep -rn "DryRunRateLimitRpm\|dryRunRateLimitRpm\|dry_run_rate_limit" packages/ 2>/dev/null
```

Expected: hits in 4–8 files spanning vkauth + control-plane admin + possibly Prisma-generated types.

- [ ] **Step 2: Delete the field from `VKMeta` struct in vkauth**

Open the vkauth file holding `VKMeta`. Remove the `DryRunRateLimitRpm *int` field and any default-value handling.

- [ ] **Step 3: Strip the DB column from the VK loader SQL**

In the vkauth loader, remove `dry_run_rate_limit_rpm` from the `SELECT` column list and the corresponding `row.Scan(...)` slot.

- [ ] **Step 4: Strip from Control Plane admin VK CRUD**

Locate via grep — likely in `packages/control-plane/internal/admin/` or `iam/`. Remove:
- `dryRunRateLimitRpm` from request/response JSON struct tags
- The field from any `INSERT` / `UPDATE` SQL strings
- Any validator that bounds it

- [ ] **Step 5: Verify build**

```bash
go build ./packages/...
```

Expected: clean.

- [ ] **Step 6: Verify the grep is now empty (Go side)**

```bash
grep -rn "DryRunRateLimitRpm\|dryRunRateLimitRpm" packages/ --include='*.go' 2>/dev/null
```

Expected: no hits.

- [ ] **Step 7: Commit**

```bash
git add packages/
git commit -m "$(cat <<'EOF'
refactor(vkauth,cp): remove DryRunRateLimitRpm from VK surface

Drop the per-VK dry-run rate-limit bucket — no longer reachable after
nexus.dry_run flag was removed in the previous commit. Strips field
from vkauth.VKMeta, the VK loader SELECT, Control Plane admin CRUD
schemas, and the VK INSERT/UPDATE SQL. DB column drop ships with the
schema migration later in this PR.

Part of E58-S5.

Co-Authored-By: Claude Opus 4.7 (1M context) <noreply@anthropic.com>
EOF
)"
```

---

### Task 3: Strip dry-run from audit.Record + mq messages + shared/traffic markers

**Why:** `audit.Record` is read/written by ai-gateway audit pipeline + Hub traffic_event writer (via mq message). Atomic.

**Files:**
- Modify: `packages/ai-gateway/internal/platform/audit/audit.go` (lines 586, 590, 664, 665–668, 751, 1303, 1304, 1323 per spec §4.5)
- Modify: `packages/shared/transport/mq/messages.go` (lines 32–36 — `IsDryRun bool` + `DryRunAssumptions []string`)
- Modify: `packages/shared/traffic/markers.go` + `markers_test.go` (strip `x-nexus-estimate` + `x-nexus-dry-run` marker constants)
- Modify: Any Hub-side consumer of `mq.AuditUpload.IsDryRun` (likely `packages/nexus-hub/internal/...` — locate via grep)

- [ ] **Step 1: Delete fields from `audit.Record`**

Open `packages/ai-gateway/internal/platform/audit/audit.go`:

```
Line 586:  Delete:  IsDryRun bool
Line 590:  Delete:  DryRunAssumptions []string
Lines 664–668:  Delete:  EstimatedCostUsd float64 + its multi-line doc
Line 751:  Delete:  "EstimatedCostUsd is 0 for these requests..." comment
Line 1303:  Delete:  IsDryRun:           rec.IsDryRun,
Line 1304:  Delete:  DryRunAssumptions:  rec.DryRunAssumptions,
Line 1323:  Delete:  EstimatedCostUsd:   rec.EstimatedCostUsd,
```

- [ ] **Step 2: Delete `IsDryRun` from `mq.AuditUpload`**

Open `packages/shared/transport/mq/messages.go`:

```
Lines 32–33:  Delete the `IsDryRun bool` field + its comment
Line 36:     Delete:  DryRunAssumptions []string `json:"dryRunAssumptions,omitempty"`
```

- [ ] **Step 3: Strip marker constants from `shared/traffic/markers.go`**

```bash
grep -n "x-nexus-estimate\|x-nexus-dry-run\|HeaderEstimate\|HeaderDryRun" packages/shared/traffic/markers.go
```

Delete the matching `const` lines and the corresponding lines in `markers_test.go`.

- [ ] **Step 4: Locate + strip Hub-side consumers**

```bash
grep -rn "IsDryRun\|DryRunAssumptions" packages/nexus-hub/ 2>/dev/null
```

For each hit: remove the field from the SQL `INSERT INTO traffic_event ...` column list and the corresponding `$N` argument slot, OR remove the assignment that reads from the mq message into a local var.

- [ ] **Step 5: Verify build**

```bash
go build ./packages/...
```

Expected: clean.

- [ ] **Step 6: Commit**

```bash
git add packages/
git commit -m "$(cat <<'EOF'
refactor(shared,hub): drop IsDryRun from audit Record + mq message + traffic markers

Remove the dry-run fields from audit.Record (struct + writer mapper),
mq.AuditUpload (IsDryRun + DryRunAssumptions), shared/traffic/markers.go
(x-nexus-estimate + x-nexus-dry-run header constants + tests), and the
Hub-side traffic_event writer that consumed the mq fields. Aligns the
internal traffic_event payload with the schema columns being dropped
later in this PR.

Part of E58-S5.

Co-Authored-By: Claude Opus 4.7 (1M context) <noreply@anthropic.com>
EOF
)"
```

---

### Task 4: Strip TrafficStore + Analytics rollup `is_dry_run` filters

**Why:** `trafficstore.TrafficEvent` fields + 9 rollup SQL sites + the list-query filter all reference the column. Must change together before migration drops the column.

**Files (per spec §4.4 + §4.5):**
- Modify: `packages/control-plane/internal/traffic/store/trafficstore/traffic_event.go` (lines 76, 78, 277, 373, 428, 632–634)
- Modify: `packages/control-plane/internal/traffic/analytics/handler/analytics_rollup.go` (lines 262, 371)
- Modify: `packages/control-plane/internal/traffic/analytics/handler/analytics_latency.go` (line 141)
- Modify: `packages/control-plane/internal/traffic/analytics/handler/cost_summary.go` (lines 138, 151, 189, 228)
- Modify: `packages/control-plane/internal/traffic/analytics/handler/cache_roi.go` (lines 244, 355, 406)

- [ ] **Step 1: Strip TrafficStore struct fields**

In `traffic_event.go`:

```
Line 76:   Delete the comment about "is_dry_run=false by default"
Line 78:   Delete:  IsDryRun bool `json:"isDryRun,omitempty"`
Line ~78:  Delete:  DryRunAssumptions ... (the adjacent line)
Line 277:  Delete:  a.is_dry_run, a.dry_run_assumptions,
Line 373:  Delete:  &a.IsDryRun, &a.DryRunAssumptions,
Line 428:  Delete:  &a.IsDryRun, &a.DryRunAssumptions,
Lines 632–634:  Delete the entire `is_dry_run = true / false` filter clause.
```

If the SELECT column list and the row scanner had a comma-separated form, **decrement the column count** that pgx will return; mismatched SELECT-vs-Scan is a runtime panic.

- [ ] **Step 2: Strip `WHERE is_dry_run = false` from each rollup site**

For each of the 9 lines above, open the file and delete the `AND is_dry_run = false` fragment from the SQL string literal. The surrounding SQL must remain syntactically valid — usually means removing exactly one `AND` + the predicate.

- [ ] **Step 3: Verify build**

```bash
go build ./packages/control-plane/...
```

Expected: clean.

- [ ] **Step 4: Verify no remaining `is_dry_run` reference in production Go**

```bash
grep -rn "is_dry_run\|IsDryRun" packages/control-plane/ packages/ai-gateway/ packages/shared/ packages/nexus-hub/ --include='*.go' 2>/dev/null | grep -v _test
```

Expected: no hits.

- [ ] **Step 5: Run analytics handler tests (filter changes are SQL-shape sensitive)**

```bash
go test -race -count=1 ./packages/control-plane/internal/traffic/...
```

Expected: PASS (some tests that asserted on dry-run filter clauses will need updating — addressed in Task 9).

- [ ] **Step 6: Commit**

```bash
git add packages/control-plane/internal/traffic/
git commit -m "$(cat <<'EOF'
refactor(cp/traffic): drop is_dry_run from store + analytics rollup SQL

Remove IsDryRun / DryRunAssumptions from trafficstore.TrafficEvent
(struct, SELECT list, row scanners, list-query filter). Remove the
WHERE is_dry_run = false clause from all 9 rollup callsites:
analytics_rollup (cost summary + by-provider phase percentiles),
analytics_latency, cost_summary (4 sites), cache_roi (3 sites).

After this commit, no Go code in any service references the column.
DB schema drop follows in the migration commit.

Part of E58-S5.

Co-Authored-By: Claude Opus 4.7 (1M context) <noreply@anthropic.com>
EOF
)"
```

---

### Task 5: DB schema migration — drop the 4 columns

**Why:** Columns must be dropped only after all reading/writing code is removed (Tasks 1–4). Per CLAUDE.md `feedback_migration_timestamp_unique`, the new migration's `YYYYMMDDHHMMSS_` prefix must not collide with any other in-flight migration.

**Files:**
- Modify: `tools/db-migrate/schema.prisma`
- Create: `tools/db-migrate/migrations/<timestamp>_drop_dryrun_columns/migration.sql`

- [ ] **Step 1: Check for prefix collisions**

```bash
ls tools/db-migrate/migrations/ | cut -c1-14 | sort | uniq -d
```

Expected: empty output. If a duplicate appears, this is a pre-existing bug; surface to user before proceeding.

- [ ] **Step 2: Edit `tools/db-migrate/schema.prisma`**

Locate the `model TrafficEvent` block. Delete the three lines:

```prisma
  is_dry_run            Boolean   @default(false)
  dry_run_assumptions   Json?
  estimated_cost_usd    Decimal?  @db.Decimal(12, 6)
```

Locate the `model VirtualKey` block. Delete:

```prisma
  dryRunRateLimitRpm    Int?
```

(Field names may vary slightly — match against the actual `schema.prisma` content.)

- [ ] **Step 3: Generate the migration**

```bash
cd tools/db-migrate
TS=$(date -u +%Y%m%d%H%M%S)
mkdir -p migrations/${TS}_drop_dryrun_columns
cat > migrations/${TS}_drop_dryrun_columns/migration.sql <<'SQL'
-- E58-S5: drop dry-run schema after Go code stopped referencing these columns
ALTER TABLE "TrafficEvent"
  DROP COLUMN IF EXISTS "is_dry_run",
  DROP COLUMN IF EXISTS "dry_run_assumptions",
  DROP COLUMN IF EXISTS "estimated_cost_usd";

ALTER TABLE "VirtualKey"
  DROP COLUMN IF EXISTS "dryRunRateLimitRpm";
SQL
echo "Created migrations/${TS}_drop_dryrun_columns/migration.sql"
cd -
```

- [ ] **Step 4: Run the migration locally**

```bash
cd tools/db-migrate
npx prisma migrate dev --name drop_dryrun_columns --create-only
npx prisma migrate dev
cd -
```

Expected: `Database is now in sync with the migration schema.`

- [ ] **Step 5: Verify the column drops landed in the DB**

```bash
cd tools/db-migrate
echo '\d "TrafficEvent"' | npx prisma db execute --stdin 2>/dev/null | grep -E "is_dry_run|dry_run_assumptions|estimated_cost_usd|dryRunRateLimitRpm"
cd -
```

Expected: empty output.

- [ ] **Step 6: Verify all services still boot locally**

```bash
./scripts/dev-start.sh
sleep 10
curl -sS http://localhost:3001/health http://localhost:3050/health http://localhost:3040/health http://localhost:3060/health
```

Expected: 4 × `{"status":"ok"}` or equivalent.

- [ ] **Step 7: Commit**

```bash
git add tools/db-migrate/schema.prisma tools/db-migrate/migrations/
git commit -m "$(cat <<'EOF'
chore(db): drop dry-run columns from TrafficEvent + VirtualKey

ALTER TABLE drops: TrafficEvent.is_dry_run, .dry_run_assumptions,
.estimated_cost_usd; VirtualKey.dryRunRateLimitRpm. All Go code
already stopped referencing these columns in the preceding commits;
this is the schema-level garbage collection.

Migration is dev-phase per CLAUDE.md "no backward compatibility";
no rollback path beyond git revert + re-create columns.

Part of E58-S5.

Co-Authored-By: Claude Opus 4.7 (1M context) <noreply@anthropic.com>
EOF
)"
```

---

## Phase 2 — UI deletion

### Task 6: Strip dry-run surfaces from `trafficAuditDrawer.tsx`

**Why:** Three independent surfaces in the same file (Surface A: dedicated DRY RUN block; Surface B: AiProvider's Estimated-cost row; Surface C: Costs-breakdown panel's `estimatedCostUsd` fallback). Per spec §4.6.

**Files:**
- Modify: `packages/control-plane-ui/src/pages/traffic/audit-drawer/trafficAuditDrawer.tsx`

- [ ] **Step 1: Surface A — delete the dry-run JSX block**

Open the file, locate the `{e.isDryRun && (...)}` block starting around line 701 and extending through ~line 815 (closing JSX + `dryRunAssumptions` map). Delete the entire block including its outer parentheses + the trailing newline.

- [ ] **Step 2: Surface B — delete the Estimated-cost row in AiProvider section**

Around line 1099, find and delete:

```typescript
{ label: t('pages:traffic.detail.aiProvider.estimatedCost'), value: e.estimatedCostUsd != null ? fmtCost(e.estimatedCostUsd) : null },
```

- [ ] **Step 3: Surface C — clean up Costs-breakdown panel**

Around line 1123, find:

```typescript
const primary = e.estimatedCostUsd ?? 0;
```

Change to use real-call fields. Verify by reading lines 1109–1160 — the panel reads `cost_usd` / `costUsd` etc. for real rows. Replace `e.estimatedCostUsd ?? 0` with the real-cost field used elsewhere (likely `e.costUsd ?? 0`).

- [ ] **Step 4: Final grep on the file**

```bash
grep -n "isDryRun\|estimatedCost\|dryRunAssumptions" packages/control-plane-ui/src/pages/traffic/audit-drawer/trafficAuditDrawer.tsx
```

Expected: 0 hits.

- [ ] **Step 5: Verify TypeScript compiles**

```bash
cd packages/control-plane-ui
npm run typecheck
cd -
```

Expected: 0 errors.

- [ ] **Step 6: Commit**

```bash
git add packages/control-plane-ui/src/pages/traffic/audit-drawer/
git commit -m "$(cat <<'EOF'
refactor(cp-ui): strip dry-run surfaces from trafficAuditDrawer

Delete three surfaces: (A) the dedicated DRY RUN explanation block
(~lines 701-815) — title, estimate table, formula explainer,
assumptions list; (B) the AiProvider section's Estimated-cost row
(~line 1099); (C) the Costs-breakdown panel's estimatedCostUsd
fallback (~line 1123) — switched to real cost_usd. After this PR
every traffic_event row is a real call.

Part of E58-S5.

Co-Authored-By: Claude Opus 4.7 (1M context) <noreply@anthropic.com>
EOF
)"
```

---

### Task 7: Strip dry-run filter + badge from traffic list

**Why:** The `dryRunMode` 3-option select (real / include / only) and the warning Badge on dry-run rows are now meaningless — after Task 5 no row can have `isDryRun = true`.

**Files (per spec §4.6):**
- Modify: `packages/control-plane-ui/src/pages/traffic/filters/LiveTrafficBasicFilters.tsx` (lines 274, 311–313)
- Modify: `packages/control-plane-ui/src/pages/traffic/list/TrafficTab.tsx` (lines 135–137)

- [ ] **Step 1: Delete the filter UI in `LiveTrafficBasicFilters.tsx`**

Open the file. Around line 274, delete the comment paragraph. Around lines 311–313, delete the `<select>` with `dryRunMode` options. Also remove any related state hook (likely `const [dryRunMode, setDryRunMode] = useState(...)`) and any query-param wiring (URL param, `useApi` queryKey member, body builder field).

- [ ] **Step 2: Delete the badge in `TrafficTab.tsx`**

Around lines 135–137, delete:

```typescript
{r.isDryRun && (
  <Badge variant="warning" title={t('pages:traffic.detail.dryRun.badgeTip', 'Estimated cost only — no upstream call was made')}>
    {t('pages:traffic.detail.dryRun.badge', 'DRY RUN')}
  </Badge>
)}
```

- [ ] **Step 3: Verify the row type doesn't still declare `isDryRun`**

Locate the TypeScript type used for `r` in `TrafficTab.tsx`. If it has `isDryRun?: boolean`, delete that field too (cascade ripple: re-run typecheck after Step 4).

- [ ] **Step 4: Verify TypeScript compiles**

```bash
cd packages/control-plane-ui
npm run typecheck
cd -
```

Expected: 0 errors.

- [ ] **Step 5: Commit**

```bash
git add packages/control-plane-ui/src/pages/traffic/
git commit -m "$(cat <<'EOF'
refactor(cp-ui): strip dry-run filter + DRY RUN badge from traffic list

Remove the dryRunMode 3-option select from LiveTrafficBasicFilters
and any associated query-param plumbing. Remove the warning Badge
on dry-run rows in TrafficTab — no row can be a dry-run after
E58-S5 schema cleanup.

Part of E58-S5.

Co-Authored-By: Claude Opus 4.7 (1M context) <noreply@anthropic.com>
EOF
)"
```

---

### Task 8: Strip dry-run i18n keys (en / es / zh)

**Why:** UI no longer references these keys; orphan keys would fail `npm run check:i18n`. Per spec §4.6, this is **cost-preview scope only** — `pages:hooks.dryRun*` (rule-pack testing feature) is NOT in scope.

**Files:**
- Modify: `packages/control-plane-ui/src/i18n/locales/en/pages.json`
- Modify: `packages/control-plane-ui/src/i18n/locales/es/pages.json`
- Modify: `packages/control-plane-ui/src/i18n/locales/zh/pages.json`

- [ ] **Step 1: Inventory keys to delete (per spec §4.6 table)**

For each of the three locales, delete:

```
pages.traffic.detail.dryRun        (entire sub-tree)
pages.traffic.detail.aiProvider.estimatedCost
pages.traffic.dryRunExact
pages.traffic.dryRunEst
pages.traffic.dryRunExactTip
pages.traffic.dryRunEstTip
pages.traffic.tipDryRun
pages.traffic.dryRunMode           (entire sub-tree: real / include / only)
```

**KEEP** (different feature, not cost preview):

```
pages.hooks.dryRunTitle
pages.hooks.dryRunSubtitle
pages.hooks.dryRunContent
pages.hooks.dryRunPlaceholder
pages.hooks.dryRunRun
pages.hooks.dryRunEmpty
```

- [ ] **Step 2: Apply deletions to each locale**

For each of `en/pages.json`, `es/pages.json`, `zh/pages.json`, open the file and delete each key listed in Step 1. Preserve JSON structure (no trailing commas; closing braces intact).

- [ ] **Step 3: Verify all three locales parse**

```bash
for loc in en es zh; do
  jq . packages/control-plane-ui/src/i18n/locales/$loc/pages.json > /dev/null && echo "$loc OK"
done
```

Expected: `en OK`, `es OK`, `zh OK`.

- [ ] **Step 4: Run i18n parity check**

```bash
cd packages/control-plane-ui
npm run check:i18n
cd -
```

Expected: no errors. (Common failure: an `es` or `zh` key remained after the `en` counterpart was deleted, or vice versa — fix by re-deleting in the lagging locale.)

- [ ] **Step 5: Verify the UI still renders the Traffic page**

```bash
cd packages/control-plane-ui
npm run build
cd -
```

Expected: build success.

- [ ] **Step 6: Commit**

```bash
git add packages/control-plane-ui/src/i18n/locales/
git commit -m "$(cat <<'EOF'
chore(cp-ui/i18n): delete cost-preview dryRun keys from en/es/zh

Delete the cost-preview dry-run i18n keys after UI surfaces were
removed in preceding commits. Preserves the unrelated rule-pack
testing keys at pages.hooks.dryRun* (different feature, same name).

Part of E58-S5.

Co-Authored-By: Claude Opus 4.7 (1M context) <noreply@anthropic.com>
EOF
)"
```

---

## Phase 3 — Test sweep

### Task 9: Sweep Go unit tests for dry-run references

**Why:** Code deletion in Phases 1 leaves tests that assert on now-deleted functions / fields. Per spec §4.8.1.

**Files (per spec §4.8.1):**
- Modify or delete: `packages/ai-gateway/internal/ingress/proxy/coverage_extra_test.go`
- Modify or delete: `packages/ai-gateway/internal/ingress/proxy/coverage_gaps_test.go`, `coverage_boost_test.go`, `proxy_residuals_test.go`, `proxy_cost_test.go`
- Modify: `packages/shared/traffic/markers_test.go`
- Modify: `packages/shared/transport/mq/*_test.go`
- Any other file matched by sweep grep.

- [ ] **Step 1: Inventory failing tests**

```bash
go test -count=1 ./packages/... 2>&1 | grep -E "FAIL|^---" | head -40
```

Note which packages/tests fail.

- [ ] **Step 2: For each failing test, decide: delete OR keep-and-update**

Rule of thumb:
- **Delete** if the test's stated purpose is "assert dry-run path X".
- **Keep + update** if the test exercises a real-call code path that incidentally referenced `IsDryRun` (e.g. an audit-record builder that always set `IsDryRun: false`).

- [ ] **Step 3: Apply edits per file**

For each test file in the inventory, open and either:
- Delete dry-run-specific `t.Run(...)` subtests entirely.
- Strip `IsDryRun: false` / `IsDryRun: true` from struct literals in shared test helpers.

Use `grep -n "IsDryRun\|isDryRun\|dryRun\|dry_run" <file>` first.

- [ ] **Step 4: Re-run full Go test suite**

```bash
go test -race -count=1 ./packages/...
```

Expected: all PASS.

- [ ] **Step 5: Verify coverage gate**

```bash
./scripts/check-go-coverage.sh
```

Expected: no regression. (Deletion can only raise coverage; if it reports a drop, a non-test source file lost coverage — investigate via `go test -cover` on the affected package.)

- [ ] **Step 6: Commit**

```bash
git add packages/
git commit -m "$(cat <<'EOF'
test(ai-gateway,shared): sweep dry-run cases from Go unit tests

Delete dry-run-specific subtests from proxy coverage tests, marker
tests, and mq message tests. Strip IsDryRun fields from shared
audit-record builder helpers. All deleted tests targeted the
nexus.dry_run code path that was removed in this PR's earlier
commits.

scripts/check-go-coverage.sh: green.

Part of E58-S5.

Co-Authored-By: Claude Opus 4.7 (1M context) <noreply@anthropic.com>
EOF
)"
```

---

### Task 10: Rewrite or delete the L5 scenario

**Why:** `tests/scenarios/dry_run_estimate_test.go` exercises the deleted `nexus.dry_run` flag. Per spec §4.8.2 — either delete or rewrite for `/v1/estimate`. Per CLAUDE.md "L5 scenario landing rule (binding)", any new/edited scenario must show live PASS output in the PR description.

**Files:**
- Delete or replace: `tests/scenarios/dry_run_estimate_test.go`

- [ ] **Step 1: Read the existing scenario to understand its coverage**

```bash
wc -l tests/scenarios/dry_run_estimate_test.go
head -50 tests/scenarios/dry_run_estimate_test.go
```

- [ ] **Step 2: Decide — delete vs rewrite**

Rewrite if the scenario's scaffold (auth + DB cross-check pattern) is reusable for `/v1/estimate` coverage. Delete if the scaffold is dry-run-specific.

**Recommendation:** rewrite into `tests/scenarios/estimate_endpoint_test.go` covering:
- Single-target estimate (1 element in `compareTargets`)
- Multi-target compare (3 elements, verify cheapest selection)
- VK `allowedModels` per-target enforcement (one allowed, one not — verify per-target error)
- Rate-limit bucket (exceed `compareEndpointRateLimitRpm`, expect 429)
- Estimator assumptions surfacing in response
- `reasoningEffort` variation (low / medium / high) on a reasoning-capable model

- [ ] **Step 3: Rewrite (or `git rm` if deleting)**

If rewriting, build the new file following the existing scenario harness pattern. If deleting:

```bash
git rm tests/scenarios/dry_run_estimate_test.go
```

- [ ] **Step 4: Run the scenario live**

Per CLAUDE.md binding, compile-clean is not enough.

```bash
cd tests/scenarios
NEXUS_TEST_TARGET=local GOWORK=off go test -run ^TestE58S5Estimate -count=1 -v
cd -
```

(Adjust `^Test...` to match the new test name.) Expected: `--- PASS` per case, OR `--- SKIP` with a documented architectural reason.

- [ ] **Step 5: Capture the live output for the PR**

Save the output to paste into the PR description per CLAUDE.md L5 binding.

```bash
cd tests/scenarios
NEXUS_TEST_TARGET=local GOWORK=off go test -run ^TestE58S5Estimate -count=1 -v 2>&1 | tee /tmp/e58-s5-scenario-live-run.log
cd -
```

- [ ] **Step 6: Commit**

```bash
git add tests/scenarios/
git commit -m "$(cat <<'EOF'
test(scenarios): replace dry-run scenario with /v1/estimate coverage

Drop tests/scenarios/dry_run_estimate_test.go and add
estimate_endpoint_test.go covering single-target estimate,
multi-target compare, VK allowedModels enforcement, rate-limit
bucket, estimator assumptions, and reasoningEffort variations.

Live run: see /tmp/e58-s5-scenario-live-run.log
(per CLAUDE.md L5 binding — compile-clean is not sufficient).

Part of E58-S5.

Co-Authored-By: Claude Opus 4.7 (1M context) <noreply@anthropic.com>
EOF
)"
```

---

### Task 11: Strip dry-run from Python smoke

**Why:** Per spec §4.8.3, smoke harness has multiple dry-run touchpoints that will either reference deleted constants or query a dropped DB column.

**Files:**
- Modify: `tests/scripts/smoke-gateway.py`

- [ ] **Step 1: Apply deletions per spec §4.8.3**

Open `tests/scripts/smoke-gateway.py`:

```
Line 18:        Delete the "Cache and dry-run arms explicitly skipped" comment.
Lines 859–860:  Delete x-nexus-dry-run header-capture comment + the
                corresponding `headers.get('x-nexus-dry-run')` line.
                KEEP `x-nexus-routed-model` capture if still used.
Line 1197:      Verify the "Used by the dry-run smoke arm" helper is only
                used by dry-run — search for callers. If yes, delete.
Lines 1285–1316:  Delete the cross-check section that uses
                `WHERE te.is_dry_run = false` (now an invalid column).
Lines 1695–1793:  Delete the entire "Per-ingress dry-run callable
                wrappers (E58-S3 smoke arm D)" comment block.
Lines 1698–1699:  Delete  call_dry_run_non_stream / call_dry_run_stream
                  type fields.
Line 1795:      Delete  _DRY_RUN_EXTRA = {"nexus": {"dry_run": True}}.
Line 1798:      Delete  _chat_call_dry_run + its sibling per-ingress
                wrappers (anthropic / responses / gemini).

KEEP, unchanged: line 1083 `def estimate(self, body, timeout=30)` —
this is the /v1/estimate caller and is the post-S5 cost-preview surface.
```

- [ ] **Step 2: Verify Python parses cleanly**

```bash
python3 -m py_compile tests/scripts/smoke-gateway.py && echo OK
```

Expected: `OK`.

- [ ] **Step 3: Smoke help still shows /v1/estimate (sanity)**

```bash
python3 tests/scripts/smoke-gateway.py --help 2>&1 | grep -iE "dry|estimate"
```

Expected: no `dry-run` mentions. May mention `/v1/estimate` if surfaced as a CLI flag.

- [ ] **Step 4: Commit**

```bash
git add tests/scripts/smoke-gateway.py
git commit -m "$(cat <<'EOF'
test(smoke): strip dry-run arms from smoke-gateway.py

Delete the per-ingress dry-run callable wrappers, the _DRY_RUN_EXTRA
extension, the x-nexus-dry-run header-capture, and the cross-check
SQL that filtered on the dropped is_dry_run column. The /v1/estimate
caller at line ~1083 is preserved as the sole cost-preview surface.

Part of E58-S5.

Co-Authored-By: Claude Opus 4.7 (1M context) <noreply@anthropic.com>
EOF
)"
```

---

### Task 12: Strip dry-run UI Vitest cases

**Files (per spec §4.8.4):**
- Modify: `packages/control-plane-ui/src/pages/traffic/audit-drawer/*.test.tsx`
- Modify: `packages/control-plane-ui/src/pages/traffic/list/*.test.tsx`
- Modify: `packages/control-plane-ui/src/pages/traffic/filters/*.test.tsx`

- [ ] **Step 1: Inventory dry-run test files**

```bash
grep -rln "isDryRun\|dryRun\|estimatedCost\|x-nexus-estimate" packages/control-plane-ui/src/ --include="*.test.tsx" --include="*.test.ts" 2>/dev/null
```

Note each match.

- [ ] **Step 2: Delete dry-run-specific `it(...)` / `describe(...)` blocks**

For each test file, open and delete subtests scoped to dry-run rendering. If a test file is dedicated entirely to the dry-run badge / drawer surface, delete the whole file.

- [ ] **Step 3: Run Vitest**

```bash
cd packages/control-plane-ui
npm run test
cd -
```

Expected: all PASS, no skipped dry-run tests.

- [ ] **Step 4: Commit**

```bash
git add packages/control-plane-ui/src/pages/traffic/
git commit -m "$(cat <<'EOF'
test(cp-ui): sweep dry-run cases from Traffic page Vitest

Delete dry-run-specific test subtests from the audit drawer, traffic
list, and filter test files. Costs breakdown panel test coverage for
real-call rows is retained.

Part of E58-S5.

Co-Authored-By: Claude Opus 4.7 (1M context) <noreply@anthropic.com>
EOF
)"
```

---

## Phase 4 — Verification

### Task 13: Run the §4.9 sweep grep checklist

**Why:** Final gate — anything left after Tasks 1–12 indicates a missed touchpoint.

- [ ] **Step 1: Code paths (expect 0 except `wirerewrite`/`cacheconfig` `DryRunAlways`)**

```bash
git grep -E "IsDryRun|isDryRun|dry_run|dryRun" -- 'packages/**/*.go' ':!packages/**/*_test.go'
```

Expected hits **only** in:
- `packages/shared/transport/wirerewrite/config.go` — `DryRunAlways`
- `packages/shared/transport/wirerewrite/engine.go` — `DryRunAlways` plumbing
- `packages/shared/storage/cacheconfig/types.go` — `DryRunAlways`

Any other hit = bug. Go fix Task 1–4.

- [ ] **Step 2: x-nexus-estimate header**

```bash
git grep -E "x-nexus-estimate|X-Nexus-Estimate" -- packages/
```

Expected: 0 hits.

- [ ] **Step 3: Tests**

```bash
git grep -E "IsDryRun|isDryRun|dry_run|dryRun" -- 'packages/**/*_test.go' 'tests/'
```

Expected: 0 hits.

- [ ] **Step 4: UI (expect 0 except `pages.hooks.dryRun*`)**

```bash
git grep -E "isDryRun|dryRun|estimatedCost|x-nexus-estimate" -- packages/control-plane-ui/
```

Expected hits **only** in `packages/control-plane-ui/src/i18n/locales/*/pages.json` under the `hooks.dryRun*` keys.

- [ ] **Step 5: i18n**

```bash
git grep -E "dryRun|estimatedCost" -- packages/control-plane-ui/src/i18n/
```

Expected: only `hooks.dryRun*` keys.

- [ ] **Step 6: Docs**

```bash
git grep -E "dry_run|dryRun|x-nexus-estimate" -- docs/developers/ docs/users/ docs/operators/
```

Expected hits **only** in `docs/developers/specs/e58-s5-kill-dryrun-unify-estimate.md` (this spec) + `docs/developers/specs/e58-s5-kill-dryrun-unify-estimate-plan.md` (this plan). Both describe the deletion; both are fine.

---

### Task 14: Run the full test suite

- [ ] **Step 1: Go**

```bash
go test -race -count=1 ./packages/...
```

Expected: all PASS.

- [ ] **Step 2: Go coverage gate**

```bash
./scripts/check-go-coverage.sh
```

Expected: GREEN. No new packages added to allowlist.

- [ ] **Step 3: UI typecheck + tests + i18n + build**

```bash
cd packages/control-plane-ui
npm run typecheck && npm run test && npm run check:i18n && npm run build
cd -
```

Expected: all green.

- [ ] **Step 4: Workspace-level checks**

```bash
npm run check:terminology
npm run check:doc-lockstep
```

Expected: both pass.

---

### Task 15: Run scoped local smoke (CLAUDE.md binding — user-approved scope-down)

**Why:** Per CLAUDE.md "AI Gateway / traffic_event changes require ai-gateway smoke run before done" — this PR touches `traffic_event` schema, so a smoke run is mandatory. User-approved scope-down (2026-05-23): instead of `--all-ingress` (~30 min, ~100 API calls), pick a few simple cases. The deletion is uniform across all ingresses (one canonicalext flag, one proxy.go path) so a single-ingress scoped smoke is provably representative.

**Scoping decision (call out in PR completion notes):** OpenAI ingress only, 2 models (1 fast `gpt-4o-mini` + 1 reasoning `o3-mini`), non-stream + stream + 1 cache turn each. Skips: Anthropic / Gemini / Responses-API ingresses, the cross-format routing arm, the 29-model fan-out, embeddings (P3E).

- [ ] **Step 1: Verify local stack is up + healthy**

```bash
curl -sS http://localhost:3001/health http://localhost:3050/health http://localhost:3040/health http://localhost:3060/health
```

Expected: 4 × healthy. If any is down, restart via `./scripts/dev-start.sh`.

- [ ] **Step 2: Run scoped smoke**

```bash
python3 tests/scripts/smoke-gateway.py \
  --models gpt-4o-mini,o3-mini \
  --ingress openai 2>&1 | tee /tmp/e58-s5-smoke-scoped.log
```

(Adjust flag names to match the harness's actual CLI surface — `--ingress` may be `--ingresses` or implied via `--models`.) Expected: all selected arms PASS.

- [ ] **Step 3: Verify traffic_event integrity (scoped to test runs)**

The smoke harness's own cross-check covers this for the chosen models. If any arm fails the DB cross-check (e.g. token columns NULL where they should be filled, or unexpected `is_dry_run` column-not-found error indicating Task 5 migration didn't land), investigate immediately.

- [ ] **Step 4: Capture report for PR description**

```bash
ls -la /tmp/smoke-gateway-*.md | tail -1
```

The latest file is the markdown report. Link in PR with note: "Scoped smoke per user approval — full --all-ingress run can be added pre-merge if desired."

- [ ] **Step 5: Manual UI sanity (per spec §6 acceptance criterion 8)**

```bash
open http://localhost:3000/traffic
```

In the UI:
- Open the Traffic Event detail drawer on a recent real-call row.
- Confirm:
  - No DRY RUN panel anywhere.
  - No "Estimated cost" line item in AiProvider section.
  - Costs breakdown panel renders with real cost values (uncached / cached / output / total) — no NaN / undefined.
  - No DRY RUN warning badge in the row list.
- Open Traffic filters — verify no "dry run mode" dropdown.

## Phase 1.5 — Checkpoint smoke (NEW, post Phase 1)

Between Phase 1 (backend Go) and Phase 2 (UI), run a 1-minute curl-level sanity check to catch any backend regression early — cheaper than discovering it after the UI is done.

- [ ] **Checkpoint A: Real call still works after Tasks 1–5**

```bash
# Pick any local VK (NEXUS_TEST_VK from tests/.env.local).
VK=$(grep NEXUS_TEST_VK tests/.env.local | cut -d= -f2)
curl -sS -X POST http://localhost:3050/v1/chat/completions \
  -H "Authorization: Bearer $VK" \
  -H "Content-Type: application/json" \
  -d '{"model":"gpt-4o-mini","messages":[{"role":"user","content":"hi"}],"max_tokens":10}' \
  | jq '.choices[0].message.content, .usage.total_tokens'
```

Expected: a string content + a non-zero total_tokens. Any 4xx/5xx = stop and investigate before Phase 2.

- [ ] **Checkpoint B: /v1/estimate still works**

```bash
curl -sS -X POST http://localhost:3050/v1/estimate \
  -H "Authorization: Bearer $VK" \
  -H "Content-Type: application/json" \
  -d '{
    "request": {"model":"gpt-4o-mini","messages":[{"role":"user","content":"hi"}]},
    "compareTargets": [{"providerId":"openai","modelId":"gpt-4o-mini"}]
  }' \
  | jq '.targets[0].cost.expected.total, .summary.cheapestExpectedTotalUsd'
```

Expected: two non-zero numbers. If error like `column is_dry_run does not exist` surfaces here, Task 5 migration didn't apply to local DB.

- [ ] **Checkpoint C: Old dry-run flag returns clean shape (no more synthetic body)**

```bash
curl -sS -X POST http://localhost:3050/v1/chat/completions \
  -H "Authorization: Bearer $VK" \
  -H "Content-Type: application/json" \
  -d '{"model":"gpt-4o-mini","messages":[{"role":"user","content":"hi"}],"max_tokens":10,"nexus":{"dry_run":true}}' \
  | jq '.choices[0].message.content'
```

Expected: a REAL string content (the `nexus.dry_run` flag is now ignored — request gets dispatched as a real call). This proves the flag has lost its short-circuit behavior. If the response is empty `[]` choices with `dryrun-...` id, Task 1 incompletely removed the dispatch branches.

---

### Task 16: Final sanity + PR prep

- [ ] **Step 1: Verify all 16 commits look right**

```bash
git log --oneline origin/develop..HEAD
```

Expected: ~10–12 well-labeled commits, all `Part of E58-S5`.

- [ ] **Step 2: Push branch**

```bash
git push -u origin feature/e58-s5-kill-dryrun
```

- [ ] **Step 3: Open PR**

Use `gh pr create` with title `E58-S5 — kill nexus.dry_run flag, unify on POST /v1/estimate`. Body must include:
- Link to spec: `docs/developers/specs/e58-s5-kill-dryrun-unify-estimate.md`
- The Phase 4 verification artifacts: §4.9 grep output (clean), full Go test PASS, ai-gateway smoke markdown report path, L5 scenario live PASS log.
- The 8-point spec §6 acceptance criteria as a markdown checklist.

- [ ] **Step 4: Worktree cleanup (after PR merges)**

```bash
cd ../..
git worktree remove ./worktrees/e58-s5-kill-dryrun
git branch -d feature/e58-s5-kill-dryrun
```

---

## Appendix — Memory anchors for the implementer

- `[[feedback_migration_timestamp_unique]]` — Task 5 Step 1 guards against the collision bug.
- `[[feedback_cache_mandatory_all_ingress]]` — Task 15 must use `--all-ingress`.
- `[[feedback_tests_only_own_data]]` — Task 10's L5 rewrite must not DELETE/UPDATE rows it didn't create.
- `[[feedback_sync_develop_in_worktree]]` — periodically `git fetch + merge origin/develop` inside the worktree.
- `[[feedback_recheck_staged_after_failed_commit]]` — if any commit hook fails, re-check staged set before retry.
