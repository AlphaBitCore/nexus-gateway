# Deploy Runbook — Schema & Model-Catalog Changes

**Use this whenever a change adds/renames a Prisma column, or edits `tools/db-migrate/model-catalog.json`** (which regenerates `seed/fixtures/Model.json`). Worked example throughout: **AP-2** — four new nullable `Model` columns (`temperatureMin`, `temperatureMax`, `minOutputTokens`, `family`) + a catalog backfill.

This repo uses **`prisma db push`** (no migration files) and a **trunk → main** flow: feature branches merge to trunk (dev/stg), trunk merges to main (prod).

---

## Principles (read once)

1. **Additive + nullable = safe, zero-downtime.** New nullable columns do not affect the running (old) binary — it never selects them. AP-2's four columns are all nullable. This is what lets you apply schema *ahead of* the code.
2. **Ordering is the whole game: schema must land BEFORE the new binary.** The new columns are read in `GetModelByCode` **and** `ResolveModelCandidates` (the routing hot path). A new binary against a DB that hasn't been pushed → `column does not exist` on **every** request, not just AP-2. Never deploy the binary first.
3. **`prisma db push`, never `prisma migrate dev`.** There are no migration files here. `migrate dev` creates them and can **reset** the DB. Always `db push`.
4. **`db push` applies your ENTIRE local schema, not just your columns.** If your working tree's `schema/*.prisma` has any other divergence, you push that too. Always dry-run the diff first (below).
5. **Schema ≠ data.** `db push` adds *empty* columns. Populating values is a **separate** step. Dev/stg may full-seed; **prod must never full-seed** — use a scoped backfill.
6. **Provisional data warning (AP-2).** The backfilled `temperature`/`family` values are provider-default heuristics + `family ≈ code`, i.e. scaffolding. They are fine for internal browsing but **must be curated with real per-model values before any user-facing (OpenRouter-style) feature ships them.**

---

## Situation 1 — Merge + deploy to dev / stg

### 1A. Before merging to trunk (on the PR)

- [ ] CI green: `go build ./...`, `go test -race -count=1 ./...` (touched packages ≥95% coverage).
- [ ] `npm run check:model-catalog` passes (fixture regenerated from `model-catalog.json`; never hand-edit `Model.json`).
- [ ] `npm run check:doc-lockstep` + `npm run check:terminology` pass.
- [ ] Diff is scoped — no stray reformatted schema files. (`prisma format` reformats **all** `schema/*.prisma`; revert any file you didn't intend to change: `git checkout HEAD -- tools/db-migrate/schema/<unrelated>.prisma`.)
- [ ] PR description calls out the schema change + the post-pull step for teammates (below).

### 1B. Tell every developer (the local-dev footgun)

Anyone who pulls trunk and runs the gateway against an already-running local DB must first:

```bash
cd tools/db-migrate && npm run db:push && npm run seed
```

Otherwise their gateway 500s on routing. (`scripts/dev-start.sh` already runs `db push` on stack bring-up, so this only bites people who skip it. Consider adding the Tier-1 post-merge hook — see the "Prevention" section.)

### 1C. Deploying to the dev / stg environment (ordered)

1. [ ] **Apply schema first:**
   ```bash
   cd tools/db-migrate && prisma db push
   ```
   (Additive/nullable → the currently-running old binary keeps working.)
2. [ ] **Populate the columns.** Dev/stg may reference-seed:
   ```bash
   npm run seed        # dev (reference + demo)
   # or: npm run seed:prod   # stg-like (reference only, no demo data)
   ```
3. [ ] **Deploy the new binary** (after steps 1–2).

### 1D. Verify (dev / stg)

```bash
curl -s -w '\n%{http_code} %{time_total}\n' http://<host>:3050/api/v1/open/models/gpt-4o
```
- [ ] `200`, `time_total` < `0.100`, body has `pricing_detail`, `parameter_constraints.max_tokens.{min,max}`, `parameter_constraints.temperature.{min,max}`, `family`.
- [ ] `curl .../api/v1/open/models/does-not-exist` → `404` + `{"error":{"message":"model not found: does-not-exist"}}`.
- [ ] **Routing sanity** (the hot-path risk): send a normal chat/completions request through a routing rule and confirm it still resolves — proves the extended `ResolveModelCandidates` SELECT works against the pushed schema.

---

## Situation 2 — Before merging trunk → main (prod)

Prod carries live data. The safe sequence applies the **schema to prod first** (safe because additive), then merges + deploys the binary, then backfills values with a **scoped** update. Use the canonical tooling: `Skill('prod-deploy')` + [prod-deploy-data-changes.md](prod-deploy-data-changes.md).

### 2A. Pre-flight (before the main PR)

- [ ] Trunk is already deployed + verified on stg (Situation 1 done and green).
- [ ] Confirm your local `schema/*.prisma` = main + only the intended change (no other divergence — Principle 4).
- [ ] Point at prod via the prod env file only (`tests/.env.prod`); never inline/commit the prod `DATABASE_URL`.
- [ ] **Dry-run the exact SQL that will hit prod:**
  ```bash
  cd tools/db-migrate && prisma migrate diff \
    --from-schema-datasource schema --to-schema-datamodel schema --script
  ```
  - [ ] Output is **only** the intended `ADD COLUMN ... NULL` statements — nothing dropped, altered, or renamed.
  - [ ] If `db push` ever asks for `--accept-data-loss` against prod → **STOP.** Additive nullable columns never trigger it; if it appears, your schema diverged.

### 2B. Apply schema to prod (ahead of the binary — safe)

1. [ ] `prisma db push` against prod (via the prod env / prod-deploy skill). Prod DB now ahead of the running old binary — fine, additive.
2. [ ] Confirm columns exist:
   ```sql
   SELECT column_name, data_type, is_nullable FROM information_schema.columns
   WHERE table_name = 'Model'
     AND column_name IN ('minOutputTokens','temperatureMin','temperatureMax','family');
   ```

### 2C. Merge trunk → main + deploy the binary

3. [ ] Merge to main; build + deploy the new gateway binary via `Skill('prod-deploy')` (schema is already pushed, so ordering is satisfied).

### 2D. Backfill prod values — SCOPED, never a full seed

4. [ ] Populate ONLY the four new columns on existing rows — a targeted `UPDATE` (or a reference-only upsert if prod's catalog tracks `model-catalog.json`). Do **not** run `npm run seed` on prod (it can reset live/reference rows). Draft the scoped UPDATE from the same values in `model-catalog.json`. Follow [prod-deploy-data-changes.md](prod-deploy-data-changes.md) for the change-without-touching-other-data pattern.

### 2E. Verify (prod)

- [ ] `curl` the detail endpoint (same checks as 1D) against prod.
- [ ] `< 100 ms` latency holds.
- [ ] Routing sanity: a live request routes normally (extended SELECT works).
- [ ] Spot-check a couple of models return populated `temperature`/`family` (not null) if you ran the backfill.

### 2F. Rollback

- **Binary:** roll back to the previous build — safe. Old code ignores the four columns.
- **Schema:** no action needed. Additive nullable columns are inert to the old binary; leave them. (Only drop columns in a deliberate, separately-reviewed change.)

---

## Prevention (make the footgun loud, not silent)

- **Tier 1 — local dev (post-pull warning).** Add a `post-merge`/`post-checkout` hook via the existing `scripts/setup-git-hooks.mjs` that prints a banner when the pull touched `tools/db-migrate/schema/` or `model-catalog.json`: *"Prisma schema/catalog changed — run `cd tools/db-migrate && npm run db:push && npm run seed`."* Git-only check (no DB needed). Warn, don't auto-mutate.
- **Tier 2 — CI + pre-deploy gate.** A `check:db-schema` script wrapping `prisma migrate diff --from-schema-datasource schema --to-schema-datamodel schema --exit-code` (non-zero when DB ≠ schema). In CI it blocks a drifted PR; as a pre-deploy step against the stg/prod DB it fails the pipeline if the binary would land ahead of its schema. It **detects**; the fix is still `db push` in the right order.

---

## Quick reference

| | Dev | Stg | Prod |
|---|---|---|---|
| Apply schema | `dev-start.sh` (auto) or `prisma db push` | `prisma db push` | `prisma db push` (dry-run diff first, prod env) |
| Populate values | `npm run seed` | `npm run seed:prod` | **scoped UPDATE only** (never full seed) |
| Order | schema → binary | schema → binary | schema → merge/deploy binary → backfill |
| Forgot-guard | Tier 1 hook | Tier 2 in CI | Tier 2 pre-deploy gate |
| Rollback | reseed | reseed | binary rollback; leave columns |

**Golden rule:** schema before binary, always. Additive nullable columns make that ordering free of downtime — use it.
