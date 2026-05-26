# Prod Deploy — DB & Data Changes Checklist

This runbook is the **single source of truth** for every database change that
must be applied to production at the next `/prod-deploy`. The `prod-deploy`
skill (`.claude/skills/prod-deploy/SKILL.md`) reads this file before applying
binaries — work through each section in order and tick the items off.

**Format rule (binding):** every release that adds schema changes, seed/data
fixes, or historical-data repairs MUST update this file in the same PR. After
the next prod deploy completes, the file is reset (release-applied items
deleted; new release items added in their place).

- **Last updated:** 2026-05-21 — cost-compute correctness pass. Two Prisma migrations + two manual SQL scripts pending; binary swap of `ai-gateway` + `nexus-hub` + `control-plane`. Drops the `provider_pricing` table (now dead code) and makes `Model` row the single source of truth for all 4 prices (input / output / cached-read / cached-write). Also fixes a 2.25× Anthropic double-count bug in `proxy.go.computeCacheCosts`. UI rewires drawer Costs breakdown to read 4 prices directly from Model JOIN — section 1 subtotal now closes exactly to `estimated_cost_usd`. Historical traffic_event rows + every rollup tier (5m/1h/1d/1mo × fleet + thing) need a one-shot recompute (Section §3.2 below). See [[project_cost_compute_correctness_2026_05_21]] for the full chain.
- **Author:** nexus
- **Last prod tag deployed:** `prod-20260520` (`8f67f5c1` — Responses-API input + instructions cost-estimator fix).
- **Target prod tag for this cycle:** `prod-20260521-cost-recompute` (to be set on deploy).

**(Previous: `prod-20260519@2babdf78` — 2nd deploy of 2026-05-19 — latency fix + observability.latencyDetail yaml. See [[project_prod_releases]] for full chain.)**

**(Historical entries below — superseded.)**

- **Previous: `prod-20260515-e55` (`a7cf4808`)**
  — 40 commits in window. Major lift: extracted `packages/shared/transport/tlsbump/`
  (~4400 LOC) from compliance-proxy/internal/proxy/ — both cp and macOS
  agent now dispatch through `tlsbump.HandleConnection`, deleting the
  agent's 965-LOC homebrew MITMRelay. New Hub Cat B loader for
  `agent.streaming_compliance` (was returning 4-byte empty payload).
  Agent local SQLite `audit_events` table gains `method`+`path` columns
  via idempotent ALTER TABLE migration in `audit/queue.go`.
  `writer_adapter.go` now copies RequestBody/ResponseBody InlineBytes.
  TrafficEventDetail HookList accepts JSON Array directly. CP UI
  agent-setup page rewritten with 3-platform tabs + per-platform FAQ +
  removal of the token fallback Card. Build-tag refactor in cmd/agent
  (`wire_bridge_{darwin,other}.go`) lets linux/windows cross-compile.
  Pre-deploy pg_dump 200 MB at
  `${NEXUS_HOME}/db-backups/nexus_gateway-prod-20260515-e55-20260515T152318Z.dump.gz`.
- **Target prod commit (HEAD at write time):** `a7cf4808`.
- **Window covered:** commits `69a6b764..a7cf4808` — fully shipped in
  this deploy. No migrations in this window.

**(Historical entry retained for reference — superseded above.)**

- **Previous: `prod-20260515-config-sync` (`72d070fe`)**
  — Binary release. Bundles 2 commits: `5b10d473` (6 admin handler
  fixes — RulePack/InterceptionDomain agent push/RenewVK/Project CRUD/
  personal VK/DeviceGroup member changes) + `72d070fe` (Hub stamps
  `source='agent'` on AuditUpload to satisfy chk_traffic_event_source
  CHECK constraint — recovered from a 32-min audit pipeline stall
  observed at 7d verify time). One DB change tracked: migration
  `20260526000000_e53_reasoning_tokens_column` recorded in
  _prisma_migrations (column was already present in prod from a
  parallel session's earlier ALTER; idempotent INSERT skipped the
  duplicate row insert). 174 MB pg_dump backup pre-deploy at
  `${NEXUS_HOME}/db-backups/nexus_gateway-prod-20260515b-20260514T175442Z.dump.gz`.
- **Target prod commit (HEAD at write time):** `72d070fe`.
- **Window covered:** commits `ec29fd5b..72d070fe` — fully shipped in
  this deploy. One migration in window (E53 reasoning_tokens, already
  applied; only tracking-row added during deploy).

---

## Section 0 — Pre-flight (mandatory before any DB op)

Open an SSH session and run the baseline counters before applying any
migration or one-shot SQL listed in Sections 1–3 below. Capture the output
so Section 5 can diff against it.

Required env vars (set via maintainer's local `.env`; see `.env.example`):
`PROD_SSH_TARGET` (e.g. `ec2-user@<your-prod-ip>`). The DB password is
read from `/etc/nexus-gateway/env` on the prod host so it never lives
locally.

```bash
HOST=${PROD_SSH_TARGET}
ssh -o StrictHostKeyChecking=no $HOST '
  PGPASSWORD=$(grep ^DATABASE_URL /etc/nexus-gateway/env | sed "s|.*://[^:]*:||;s|@.*||") \
  psql -h localhost -U nexus -d nexus_gateway -c \
  "SELECT id, type, status, version FROM thing WHERE id LIKE \"%-ip-172%\" ORDER BY type;"
'
```

Capture three baseline values (write them down — Section 5 uses them):

```sql
SELECT COUNT(*) AS thing_count            FROM thing;
SELECT COUNT(*) AS template_count         FROM thing_config_template;
SELECT COUNT(*) AS credential_count       FROM "Credential";
SELECT MAX(desired_ver) AS max_ver_aigw   FROM thing WHERE type = 'ai-gateway';
SELECT MAX(desired_ver) AS max_ver_cp     FROM thing WHERE type = 'compliance-proxy';
SELECT MAX(desired_ver) AS max_ver_agent  FROM thing WHERE type = 'agent';
```

Also list which migrations prod has already applied so you can spot any drift:

```sql
SELECT migration_name FROM _prisma_migrations ORDER BY migration_name DESC LIMIT 10;
```

Expected most-recent rows on prod **before** this cycle's changes (per
`memory/project_prod_releases.md`, last applied 2026-05-13):

```
20260516000000_e46_traffic_event_normalized
20260515000000_e43_canonicalize_iam_policy_actions
20260514110000_e42_pr3_hot_swap_keys
20260514100000_e42_pr2_hot_swap_keys
20260514000000_e42_config_template_audit
20260513130000_e38_s13_prompt_cache_3tier_cutover
20260513120000_e38_s13_prompt_cache_3tier
20260513100000_e41_v2_credential_state_v2
20260512180358_e44_iam_group_scim
20260512100000_e41_credential_state_pipeline
```

If any of those are missing, **stop and reconcile manually** before touching
this release's migrations.

---

## Section 1 — Schema migrations to apply

Two new migrations for this cycle. Apply via `npx prisma migrate deploy`
in `tools/db-migrate/`. Both run in a single Prisma transaction; rollback
strategy is to restore from the pg_dump taken in Section 0.

### §1.1 — `20260608000000_model_cache_pricing_backfill`

**What.** Backfills `Model.cachedInputReadPricePerMillion` +
`Model.cachedInputWritePricePerMillion` on every chat-model row whose
provider matches one of the 5 in-use adapter types. Uses the
publicly-documented provider ratios:

| Provider | cache_read | cache_write |
|---|---|---|
| Anthropic claude-* | 0.10× input | 1.25× input |
| OpenAI gpt-*/o-* | 0.50× input | 0 (no surcharge) |
| Google Gemini | 0.25× input | 0 |
| DeepSeek | 0.10× input | 0 |
| Moonshot | leaves NULL (per-SKU; admin fills via CP) |

Only rows with `cachedInput*PricePerMillion IS NULL` are touched
(`COALESCE` preserves any admin-set value). Migration runs in <1s
even on prod (target table has ~35 enabled chat models).

**Expected effect on local dev DB** (verified): UPDATE 6 + UPDATE 12 +
UPDATE 3 + UPDATE 2 = 23 chat-model rows; 6 Moonshot SKUs left NULL.

### §1.2 — `20260608000001_drop_provider_pricing_table`

**What.** `DROP TABLE IF EXISTS "provider_pricing";` — the regex-index
seam is retired (gateway now reads from Model row's 4 price columns
via `cachelayer.Layer.LookupCachePricing`, which was rewritten to
hit the in-memory models snapshot). 56 baseline rows go away. The
table is currently 56 KB on disk; DROP is metadata-only.

**Sequencing (critical).** This migration MUST run AFTER the new Hub +
ai-gateway binaries are deployed. Old gateway processes will error on
their next snapshot reload if the table is gone before the code change
lands. Per Section 6 of the deploy runbook: binary push → confirm Hub
+ AI-GW healthz → THEN `prisma migrate deploy`.

**Expected migrations row on prod after this cycle:**

```
20260608000001_drop_provider_pricing_table
20260608000000_model_cache_pricing_backfill
20260607000000_e61_internal_ops_cost                 -- (was deployed in prod-20260519 if applicable)
20260606000000_e72_extract_cache_config
20260605000002_semantic_cache_advanced_fleet_tuning
...
```

---

## Section 2 — Required data inserts

_None pending._ Add one-shot inserts (AlertRule rows, managed-policy `document`
patches, seeded enum extensions, etc.) here as they become required.

---

## Section 3 — Historical data fixes

### §3.1 — Backfill rollup `virtual_key` dimension (applied 2026-05-19 in prod-20260519 deploy)

**(Applied 2026-05-19 — backfill SQL ran successfully via prod-20260519 deploy. Result: 542 buckets per rollup table (metric_rollup_5m + thing_metric_rollup_5m). Kept as worked example for future identity-shape backfill jobs.)**

_(Original entry retained below for reference.)_

#### Historical context — §3.1 — Backfill rollup `virtual_key` dimension (2026-05-17, REQUIRED this release)

**Why.** Commit `220319dda` fixed 7 query paths that were reading the
dead JSON key `identity.credential` instead of the actual producer
key `identity.vk`. One of those was the Hub rollup-5m + thing-rollup-5m
jobs: they sourced `virtualKeyID` from the dead key, got "", and the
dim-builder's `if virtualKeyID != "" { dims = append(..., {"virtual_key", virtualKeyID}) }`
guard then **dropped the `virtual_key=<UUID>` dimension entirely** on
every rolled-up row.

**Impact.** Every `metric_rollup_5m` + `thing_metric_rollup_5m` bucket
in prod is missing its per-VK slice. Dashboards keyed by Virtual Key
(per-VK cost / tokens / latency) currently render empty for ALL
historical data. NEW data (post-deploy) rolls up correctly because the
code fix supplies the right identity path.

**Fix.** Run the idempotent backfill SQL — re-aggregates the 6
highest-impact metrics (`request_count`, `prompt_tokens`,
`completion_tokens`, `total_tokens`, `estimated_cost_usd`,
`latency_sum`, `latency_count`) per (5-min bucket × VK) from raw
`traffic_event`. Inserts only the missing `dimensionKey='virtual_key=<UUID>'`
buckets; existing buckets keyed by other dimensions are untouched.

```bash
# Step A — local-staged file is in the repo:
ls -la tools/db-migrate/manual-scripts/backfill_rollup_virtual_key_dimension_2026_05_17.sql

# Step B — copy to prod EC2 and run via psql against prod DB.
HOST=${PROD_SSH_TARGET}
scp tools/db-migrate/manual-scripts/backfill_rollup_virtual_key_dimension_2026_05_17.sql \
    $HOST:/tmp/

ssh $HOST 'PGPASSWORD=$(grep ^DATABASE_URL /etc/nexus-gateway/env | sed "s|.*://[^:]*:||;s|@.*||") \
    psql -h localhost -U nexus -d nexus_gateway \
    -f /tmp/backfill_rollup_virtual_key_dimension_2026_05_17.sql'

# Expected RAISE NOTICE output (numbers depend on prod traffic volume):
#   metric_rollup_5m backfill buckets:        ~ K * 7
#   thing_metric_rollup_5m backfill buckets:  ~ K * 7    (where K = distinct VK×5min-bucket combos)
```

**Idempotent.** The unique index `(bucketStart, metricName, dimensionKey, subDimension)` on both rollup tables means re-running this script is a no-op for already-present buckets. Safe to retry.

**Ordering.** Run AFTER the binary deploy (Step 6). If you run it
BEFORE the fixed Hub binary lands, the rollup job's next tick may
race the backfill on overlapping buckets — both INSERTs hit
`ON CONFLICT DO NOTHING` so there's no data corruption, but you may
see the backfill emit fewer rows than expected because the live job
already won the race. Running AFTER the binary swap avoids this.

**Scope NOT covered (acceptable gap):**
- Other metrics (cache_*, ttft_*, model_shift_count, routing_*,
  quality_anomaly_count) — historical per-VK slices remain empty for
  the retention window; new buckets are correct. Add lateral VALUES
  rows to the SQL if a specific dashboard or alert needs them.
- 1h / 1d rollups built from 5m rollups — those get re-aggregated by
  the rollup-1h / rollup-1d jobs on their next sweep, picking up the
  newly-inserted 5m buckets. No separate backfill needed.

**(Previous worked example kept for reference — §3.0 below.)**

### §3.2 — Cost recompute + rollup reset (REQUIRED this release, 2026-05-21)

**Why.** Two compounding issues caused every historical `traffic_event`
row with Anthropic cache tokens to have a wrong `estimated_cost_usd`:

1. **Anthropic double-count bug** in `proxy.go.computeCacheCosts`
   (lines 2929-2934 pre-fix). Special-case left `regularInput =
   PromptTokens` for Anthropic, but our normalizer
   (`shared/transport/normalize/codecs/anthropic_messages.go:340-342`)
   normalises Anthropic's `input_tokens` (uncached only) by SUMMING
   with cache_read + cache_creation into the unified `PromptTokens`
   field. Result: cached tokens billed at BOTH the input rate AND the
   cache rate — 2.25× over on a typical claude-opus call (verified
   row `09b83222`: pre-fix $0.247846 vs. correct $0.110235).

2. **Provider pricing source drift** — gateway was using
   `provider_pricing` table (which had real claude $15/$75 ratios)
   while CP UI displayed Model row prices (which had operator-set
   internal tier values, e.g., $5/$25). Plan B refactor moved Model
   row to single source of truth (see §1.2 above); this also means
   historical `estimated_cost_usd` values reflect old `provider_pricing`
   not the current Model row, so re-computing them at current rates
   is the right thing.

**What.** Two SQL scripts in
`tools/db-migrate/manual-scripts/`:

1. `recompute_traffic_event_costs_2026_05_21.sql` — chunked PL/pgSQL
   loop, 5000 rows / chunk, 50 ms sleep between chunks, per-chunk tx
   so WAL stays bounded. Re-computes 5 fields per row:
   `estimated_cost_usd`, `cache_write_cost_usd`,
   `cache_read_savings_usd`, `cache_net_savings_usd`,
   `reasoning_cost_usd`. Drops + recreates progress table
   `recompute_progress_2026_05_21` so re-runs restart cleanly; the
   UPDATE itself is idempotent (uses current Model prices, so
   replays produce the same value).

2. `reset_rollup_after_cost_recompute_2026_05_21.sql` — scopes to
   `MIN(traffic_event.timestamp)` window, DELETEs affected buckets
   in `metric_rollup_5m/1h/1d/1mo` + `thing_metric_rollup_*`
   (8 tables), resets the 6 watermarks (`rollup-5m / merge-1h /
   merge-1d / merge-1mo` × fleet & thing). Whole script runs in seconds
   (no chunking needed; DELETE is bulk and `metric_rollup_5m` is
   typically <500k rows on a year-old prod). Has a
   `HISTORICAL_CUTOFF` comment for operators who want to limit the
   window to e.g. last 30 days.

**Prod timing estimates.**

| Step | Volume | Est. wall-clock |
|---|---|---|
| `recompute_*.sql` | ~300k traffic_event rows | 5-8 min |
| `reset_rollup_*.sql` | ~500k rollup_5m + ~500k thing_5m rows | ~30 s |
| Rollup cron catch-up (rollup-5m) | ~8640 buckets / month at 1 bucket per minute default cadence | ~6 days |
| Rollup cron catch-up (merge-1h / 1d / 1mo) | cascading from corrected 5m | hours-to-day each tier |

If 6-day catch-up is unacceptable, operator may **temporarily** lower
`scheduler.intervals.rollup5m` in `nexus-hub.yaml` from `1m` to e.g.
`5s` + restart Hub. At 5s/bucket, 8640 buckets ≈ 12 hours. Revert to
`1m` after watermark catches up to live.

**Backup.** Take BOTH:

```bash
HOST=${PROD_SSH_TARGET}
TS=$(date -u +%Y%m%dT%H%M%SZ)
ssh $HOST "
  PGPASSWORD=\$(grep ^DATABASE_URL /etc/nexus-gateway/env | sed 's|.*://[^:]*:||;s|@.*||') \
  pg_dump -h localhost -U nexus -d nexus_gateway -Fc \
    -t traffic_event \
    > /home/ec2-user/db-backups/traffic_event-pre-recompute-${TS}.dump
  PGPASSWORD=\$(grep ^DATABASE_URL /etc/nexus-gateway/env | sed 's|.*://[^:]*:||;s|@.*||') \
  pg_dump -h localhost -U nexus -d nexus_gateway -Fc \
    -t metric_rollup_5m -t metric_rollup_1h -t metric_rollup_1d -t metric_rollup_1mo \
    -t thing_metric_rollup_5m -t thing_metric_rollup_1h -t thing_metric_rollup_1d -t thing_metric_rollup_1mo \
    > /home/ec2-user/db-backups/rollups-pre-recompute-${TS}.dump
"
```

Restore (if needed):

```bash
ssh $HOST "
  PGPASSWORD=... pg_restore -h localhost -U nexus -d nexus_gateway -Fc --clean --if-exists \
    /home/ec2-user/db-backups/traffic_event-pre-recompute-<TS>.dump
"
```

**Run order (binding).**

```bash
HOST=${PROD_SSH_TARGET}

# Step A — copy both scripts to prod EC2.
scp tools/db-migrate/manual-scripts/recompute_traffic_event_costs_2026_05_21.sql \
    tools/db-migrate/manual-scripts/reset_rollup_after_cost_recompute_2026_05_21.sql \
    $HOST:/tmp/

# Step B — run recompute. Capture stdout — every 5000-row chunk emits
# a RAISE NOTICE with progress.
ssh $HOST '
  PGPASSWORD=$(grep ^DATABASE_URL /etc/nexus-gateway/env | sed "s|.*://[^:]*:||;s|@.*||") \
  psql -X -h localhost -U nexus -d nexus_gateway \
       -f /tmp/recompute_traffic_event_costs_2026_05_21.sql \
       2>&1 | tee /tmp/recompute.log
'
# Mid-run progress (open another shell on the same host):
ssh $HOST 'PGPASSWORD=... psql -h localhost -U nexus -d nexus_gateway \
    -c "SELECT chunk_no, rows_updated, last_id, finished_at - started_at AS dur \
        FROM recompute_progress_2026_05_21 ORDER BY chunk_no DESC LIMIT 10;"'

# Step C — run rollup reset (only after Step B's `recompute complete`
# notice prints).
ssh $HOST '
  PGPASSWORD=... psql -X -h localhost -U nexus -d nexus_gateway \
       -f /tmp/reset_rollup_after_cost_recompute_2026_05_21.sql \
       2>&1 | tee /tmp/rollup-reset.log
'

# Step D — verify a known buggy row was corrected. Pick any pre-fix
# Anthropic row with cache_read_tokens > 0; new value should equal:
#   (prompt - cache_read - cache_creation) × Model.inputPricePerMillion
#   + cache_read × Model.cachedInputReadPricePerMillion
#   + cache_creation × Model.cachedInputWritePricePerMillion
#   + completion × Model.outputPricePerMillion
# all / 1e6.

# Step E — confirm rollup cron catches up. Watch the watermark:
ssh $HOST 'while true; do
  PGPASSWORD=... psql -h localhost -U nexus -d nexus_gateway -c \
    "SELECT \"jobName\", watermark AT TIME ZONE '\''UTC'\'' \
     FROM rollup_watermark WHERE \"jobName\" IN ('\''rollup-5m'\'','\''thing-rollup-5m'\'');"
  sleep 60
done'
# Expect: watermark advances 1 bucket / minute by default; reaches
# current time after ~6 days (or faster if intervals.rollup5m lowered).
```

**Cache HIT rows — corrected semantic (2026-05-21 update).**
`estimated_cost_usd` is the PREDICTED spend at the configured Model
prices, independent of cache outcome. HIT rows should carry the
would-have-paid value (= the savings amount), NOT 0. Customer's
actual paid-to-upstream is `estimated - savings` (≈ 0 on full HIT,
= estimated on MISS). Gateway code change ships in this release
(`proxy.go` lines 226-241 + 299-316 — handleStreamHit + handleNonStreamHit).

The recompute script's HIT post-pass:
- For HIT rows with tokens (post-Task-#21 fix HITs, ≥ 2026-05-21 02:04
  UTC on dev): recomputes `estimated_cost_usd` AND
  `gateway_cache_savings_usd` from tokens × Model prices. Both columns
  end up equal (full HIT = 100% savings).
- For HIT rows with NULL tokens (older HITs before the cache writer
  started persisting Usage): leaves them NULL. The cached upstream
  entry didn't carry Usage to recover from; fabricating numbers
  would be worse than honest data loss. Acceptable scope gap.

NEW HIT rows from this binary release onward populate every cost
column correctly. Drawer UI subtracts savings from estimated to show
"net actual paid".

**Idempotent.** Re-running either script is safe:
- `recompute` UPDATE always sets values to the same target (current Model prices).
- `reset_rollup` DELETE-then-watermark-reset converges on the same end state.
- Cache-HIT post-pass touches only rows where `estimated_cost_usd IS
  NULL AND prompt_tokens IS NULL AND gateway_cache_status = 'hit'`;
  re-runs match zero rows.

**Ordering vs binary deploy (binding).** The recompute script uses the
CURRENT `Model.cachedInputRead/WritePricePerMillion` values. Those are
backfilled by migration §1.1. So:

1. **Apply migrations (Section 1)** — backfills Model rows.
2. **Deploy binaries (Section 6 of standard runbook)** — new gateway
   reads cache prices from Model rows.
3. **Run recompute_traffic_event_costs** — uses the now-correct Model
   prices to fix historical rows.
4. **Run reset_rollup_after_cost_recompute** — invalidates stale
   rollup tiers so cron re-aggregates from corrected rows.
5. **Drop provider_pricing table (migration §1.2)** — after gateway
   binary is live and old gateway is no longer reading the table.

If steps 2/3 are out of order, gateway might race the recompute on the
same rows: gateway writes a new row → recompute overwrites it →
nothing breaks (new row's prices match current Model row anyway).

**Rollback scope.**
- Migrations §1.1: NOT reversible (`COALESCE` overwrote NULLs with
  computed defaults; original NULLs are lost). To revert: restore
  Model rows from backup, or `UPDATE "Model" SET cachedInput*PricePerMillion
  = NULL WHERE ...` to undo the backfill.
- Migration §1.2: restore `provider_pricing` table + 56 baseline INSERT
  rows from backup. Code rollback is `git revert` of the cachelayer.go
  + pricing.go changes.
- Recompute SQL: restore traffic_event from `traffic_event-pre-recompute-<TS>.dump`.
- Rollup reset SQL: restore the 8 rollup tables OR re-trigger the
  cron (cheaper — let it re-aggregate).

**(Previous worked example kept for reference — §3.0 below.)**

### §3.0 — Identity backfill (applied 2026-05-15)

**(Applied 2026-05-15 — kept here as a worked example for future
identity-backfill jobs.)**

§3.1 Identity backfill — used the per-row PL/pgSQL EXCEPTION-trap
form below, NOT the straight UPDATE, because pre-existing rows with
`source=""` or `usage_extraction_status=""` trip the row-level CHECK
constraints when ANY column is re-touched (PG re-checks the whole
row on UPDATE). The bulk-UPDATE aborted on the first violation;
the per-row loop skipped them.

```sql
DO $$
DECLARE r RECORD; n INT := 0; skipped INT := 0;
BEGIN
  FOR r IN
    SELECT id FROM traffic_event
     WHERE identity IS NULL
       AND created_at > NOW() - INTERVAL '24 hours'
       AND source IN ('agent','ai-gateway','compliance-proxy')
  LOOP
    BEGIN
      UPDATE traffic_event SET identity = '{"status":"pending"}'::jsonb WHERE id = r.id;
      n := n + 1;
    EXCEPTION WHEN check_violation THEN
      skipped := skipped + 1;
    END;
  END LOOP;
  RAISE NOTICE 'updated=% skipped=%', n, skipped;
END $$;
```

Result on `prod-20260515-identity-fix@63c38dba` deploy:
`updated=10173 skipped=73`. Combined with the 43 rows whose
`source=''` were excluded by the WHERE filter (also CHECK-bound),
116 legacy rows remain NULL — out of scope for this backfill.

---

## Section 4 — Items that need NO action (intentionally documented)

Re-confirm in this section that nothing in the upcoming binary swap **needs**
a DB change. Useful for releases like `prod-20260513d-e46` where the entire
delta is application-layer — keeps reviewers from asking "did you forget a
migration?".

- `prod-20260514-passthrough` (E48 admin UI + IAM seed + E51 stats tables) —
  applied two DB changes pre-binary (see Section 1 + 2 above for the
  applied details — now reset). Also exposed a parallel-session bug:
  the initial CP binary had a stale-snapshot of the diag_silence
  handler (it referenced `iam.VerbUpdate` on the `observability`
  resource, which the catalog only declares `read`+`write` for). The
  CP-only rebuild after the on-disk file was updated to `VerbWrite`
  cleared the panic; ai-gateway/compliance-proxy/hub were not affected.
- `prod-20260513-promptcache` (Prompt Cache rules panel restore +
  diag/analytics/agent fixes) — binary-only. The new endpoint
  `GET /api/admin/cache/adapters` is a read-only wrapper around
  `cache_adapter_config` (table created in earlier E38-S13 cycle); no
  schema move. UI rebuild ships CSS Module + i18n changes only.
- `prod-20260513d-e46` (E46 Phase 5/6/7) — binary-only. `traffic_event_normalized`
  was created in migration `20260516000000_e46_traffic_event_normalized`
  applied during the `prod-20260513-e44s09` cycle; the new binaries read/write
  it without any further schema move.
- `prod-20260513-e47` (E47 routing canonical payload + admin guard) —
  binary-only. The fix moves smart-routing user-message extraction off the
  `RoutingContext.Headers["x-smart-messages"]` brittle plumbing onto a
  typed `RoutingContext.Request *normalize.NormalizedPayload` field
  populated unconditionally at Phase 3.5 via `normalize.Registry.Normalize`.
  No schema migration. The S8 admin-API guard is server-side
  validation only; it operates on the existing `RoutingRule.matchConditions`
  JSONB column. Operator-side audit + remediation of any pre-existing
  dangerous smart rules is documented in
  `docs/operators/ops/runbooks/r-routing-rule-matchconditions-audit.md` and runs
  out-of-band of this deploy.
- `prod-20260513-e48` (E48 emergency passthrough) — 3 schema migrations
  applied: `20260517000000_e48_gateway_passthrough_config_3tier` (3
  tables + view + 4 CHECK constraints + 2 seed rows),
  `20260517000010_e48_traffic_event_passthrough_columns` (2 columns +
  partial index), `20260518000000_e27_reported_outcomes_and_process_started_at`
  (2 columns from parallel session). All ALTER ADD COLUMN are O(1)
  metadata-only (PostgreSQL 11+ const-default rule). No historical
  data backfill required. Post-deploy seed.ts adds idempotent
  ON CONFLICT DO NOTHING inserts for the 2 E48 default rows so
  re-seed of dev DBs against a stale seed-baseline.sql still works.

---

## Section 5 — Post-deploy verification

```sql
-- 1. _prisma_migrations did not regress
SELECT migration_name FROM _prisma_migrations ORDER BY migration_name DESC LIMIT 5;

-- 2. All Things online + at new version
SELECT id, type, status, version FROM thing
WHERE id LIKE '%-ip-172%'
ORDER BY type;

-- 3. Sidecar table healthy (E46 phase-1 onwards)
SELECT request_status, COUNT(*)
FROM traffic_event_normalized
WHERE created_at > NOW() - INTERVAL '1 hour'
GROUP BY request_status;
```

After verification: reset Sections 1/2/3 above to `_None pending._`, bump
`Last prod tag deployed`, and commit the reset in the deploy PR.
