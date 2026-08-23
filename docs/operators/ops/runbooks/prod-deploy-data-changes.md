# Prod deploy data changes

Per-branch checklist of out-of-band DB changes that ship alongside a deploy. Each entry names the branch / merge that requires the change, the exact tables + JSON keys touched, the value semantics (preserve vs flip), and the safe ordering relative to the binary cutover.

Treat this as the operator's hand-off note from each PR to the deploy step. If a branch touches DB state shape and does NOT land an entry here, the deploy will silently drift.

## Format

For each branch, record:

- **Scope:** what changed at the wire / schema level.
- **Tables + paths:** the rows + JSON paths to flip.
- **Value rule:** preserve, flip per type, or recompute.
- **Order:** flip-before-deploy / deploy-then-flip / atomic.
- **Smoke after deploy:** the one-liner the operator runs to confirm the chain is healthy.
- **Rollback:** what to do if the deploy reverts.

## Branch entries

### `feature/docs-backfill` — PR-B kill-switch wire rename `enabled` → `engaged`

**Scope.** The shared `interception.Killswitch` JSON shape was renamed from `{enabled: bool}` to `{engaged: bool}`. The agent-side internal store also flipped semantic so wire and runtime agree on "engaged=true means engaged" (previously the agent receiver stored `enabled=true` meaning "bump allowed = NOT engaged" and the bridge ran an inversion wrapper). Compliance-proxy was already canonical (`enabled=true` always meant engaged); only the JSON key changes on that side.

**Tables + JSON paths.**

| Table | Path | Producer / consumer | Notes |
|---|---|---|---|
| `thing_config_template` | `state` JSON, where `config_key='killswitch'` | Hub UPSERT from CP admin API; receivers read on shadow tick | Both `compliance-proxy` and `agent` type rows |
| `thing` | `desired` JSON → `killswitch.*` | Hub aggregated desired state; pushed to Things | Same rule per Thing type |
| `thing` | `reported` JSON → `killswitch.*` | Receiver Snapshot uploads | Same rule per Thing type |
| `config_change_event` | `desired_state` / `reported_state` JSON, where `config_key='killswitch'` | History rows for the kill-switch toggle history page | Same rule per Thing type |

**Value rule.**

- **compliance-proxy / control-plane rows** (`type='compliance-proxy'` or `type='control-plane'`): rename JSON key only — `{"enabled": X}` → `{"engaged": X}`. Value is preserved because the field always meant "engaged" on that side.
- **agent rows** (`type='agent'`): rename AND flip the value — under the old semantic, `enabled=true` meant "bump allowed = kill switch NOT engaged"; under the new canonical semantic, that maps to `engaged=false`. So `{"enabled": true}` → `{"engaged": false}`, and `{"enabled": false}` → `{"engaged": true}`.

**SQL one-liner (run by operator with `psql` against prod):**

```sql
-- compliance-proxy + control-plane rows: rename key, preserve value.
UPDATE thing_config_template
   SET state = jsonb_set(state - 'enabled', '{engaged}', (state->'enabled')::jsonb)
 WHERE config_key = 'killswitch'
   AND type IN ('compliance-proxy', 'control-plane')
   AND state ? 'enabled';

-- agent rows: rename key AND flip the value.
UPDATE thing_config_template
   SET state = jsonb_set(state - 'enabled', '{engaged}', to_jsonb(NOT (state->>'enabled')::boolean))
 WHERE config_key = 'killswitch'
   AND type = 'agent'
   AND state ? 'enabled';

-- Repeat for thing.desired and thing.reported (whole document; killswitch is one of many keys).
UPDATE thing
   SET desired = jsonb_set(desired #- '{killswitch,enabled}',
                           '{killswitch,engaged}',
                           (desired #> '{killswitch,enabled}')::jsonb)
 WHERE type IN ('compliance-proxy', 'control-plane')
   AND desired #> '{killswitch,enabled}' IS NOT NULL;

UPDATE thing
   SET desired = jsonb_set(desired #- '{killswitch,enabled}',
                           '{killswitch,engaged}',
                           to_jsonb(NOT (desired #>> '{killswitch,enabled}')::boolean))
 WHERE type = 'agent'
   AND desired #> '{killswitch,enabled}' IS NOT NULL;

-- Same two statements with `reported` replacing `desired`.

-- config_change_event history rows: rename key in desired_state / reported_state;
-- history rows are immutable by design, but the audit query page surfaces the wire
-- shape, so the rename keeps the history readable. Do this LAST so the production
-- toggle audit trail remains queryable mid-flight.
UPDATE config_change_event
   SET desired_state = jsonb_set(desired_state - 'enabled', '{engaged}', (desired_state->'enabled')::jsonb)
 WHERE config_key = 'killswitch'
   AND desired_state ? 'enabled';
```

**Order.**

Deploy-then-flip is acceptable; the new binary tolerates an absent `engaged` key (decodes to `Engaged=false` = disengaged — the fail-safe baseline). But the deploy window leaves any currently-engaged compliance-proxy fleet in `engaged=false` state until the SQL runs, which means a kill switch that was on at deploy time will silently disengage. **Flip-before-deploy** is the safer order:

1. Take a backup: `pg_dump` of the four affected tables.
2. Run the SQL above.
3. Deploy the new binaries (Hub, CP, agent, ai-gateway, compliance-proxy).
4. Verify with the smoke command below.

Atomic (a single transaction wrapping data flip + binary swap) is not feasible with multi-host k8s deploys; flip-then-deploy is the next-best.

**Smoke after deploy.**

```bash
psql -c "SELECT type, state FROM thing_config_template WHERE config_key='killswitch';"
# Expect: every row has state matching '{"engaged": <bool>}'. Zero rows with 'enabled'.

curl -fsSL -H "Authorization: Bearer $CP_TOKEN" https://control-plane.internal/api/admin/compliance/killswitch
# Expect: {"desired":{"engaged":false},"version":N} — the engage flag is now the wire key.
```

**Rollback.**

The SQL is symmetric — swap `enabled` ↔ `engaged` in each statement to revert. The binaries also tolerate the old `enabled` key on rollback for a one-deploy grace window (struct tag does NOT — rollback would re-introduce the inversion bug). Safer rollback: `git revert` the PR-B commit, redeploy, then run the inverse SQL.

### `feature/docs-backfill` — PR-C AI-Guard reconcile producer

**Scope.** No DB shape change. The reconcile fires entirely in-process inside `WebhookForward.Execute` and lands its output on the existing `traffic_event.request_hook_reason_code` column.

**Tables + JSON paths.** None.

**Value rule.** N/A.

**Order.** Plain deploy.

**Smoke after deploy.**

Send a request that triggers an admin policy with `onMatch.inflightAction = "block-hard"` and an AI-Guard-style webhook returning `decision: "approve"`. The traffic_event row should land with `request_hook_decision = "REJECT_HARD"` (policy ceiling wins) and `request_hook_reason_code = "AIGUARD_SUGGESTED_VS_POLICY"`. The CP-UI audit drawer chip should render the locale-translated explanation.

**Rollback.** Plain `git revert`. Stamps revert to producer-side raw verbatim and the reason code stops being stamped — no DB cleanup required.

### `fix/gw-json-parse` — Model catalog: corrected caps, floating Claude codes, new models

**Scope.** No schema change. `Model` **row** changes only, and they must reach an existing deployment or the fixes do not apply: the seed fixture governs fresh installs, while a live deployment's `Model` rows were created through the provider wizard and drift freely from the catalog.

Two of the corrections fix a live 400, so this is not cosmetic:

- **Output caps that exceed the vendor's real ceiling.** The gateway fills `max_tokens` from `Model.maxOutputTokens` whenever a caller omits the field, so a too-high stored cap makes the provider reject every such request. Anthropic's real ceilings are: `claude-fable-5` / `claude-sonnet-5` / `claude-opus-4-8` / `-4-7` / `-4-6` / `claude-sonnet-4-6` = **128000**; `claude-opus-4-5` / `claude-sonnet-4-5` / `claude-haiku-4-5` = **64000**; `claude-opus-4-1` = **32000**. Any row storing 131072 or 65536 is over its ceiling and is 400ing now.
- **Claude codes moved from dated ids to floating aliases** (`claude-haiku-4-5-20251001` → `claude-haiku-4-5`), with the dated id preserved in `aliases` so callers still sending it keep resolving.

**Tables + JSON paths.** `Model` — `code`, `aliases`, `maxOutputTokens`, `maxContextTokens`, the four price columns, `description`, `features`, `status` / `deprecationDate` / `replacedBy` (on `claude-opus-4-1`, which the vendor deprecated). No JSON paths.

**Value rule.** Recompute from the catalog: run the reference seed. It reconciles each fixture row against any live row matching its `id`, its `code`, **or one of its aliases**, then updates in place — so a wizard-created row under the old dated name is adopted and corrected while keeping the id that `traffic_event.model_id` and `VirtualKey.allowedModels[].modelId` reference. Rows the catalog does not carry are left untouched; nothing is deleted.

**Order.** Seed-then-deploy or deploy-then-seed both work — the row values and the binary are independent. Seeding first closes the live 400 sooner.

Before seeding, check for a half-applied rename, which is the one shape the seed refuses to guess at:

```sql
SELECT id, code, "maxOutputTokens" FROM "Model"
 WHERE code LIKE 'claude-%' ORDER BY code;
```

If BOTH a floating code and its dated alias exist as separate rows, the seed stops and names both ids rather than corrupting either. Merge them by hand first: keep the row whose id `traffic_event` references, delete the other.

**Smoke after deploy.**

```sql
-- Expect zero rows. Any hit is a cap above the vendor ceiling, i.e. a live 400.
SELECT code, "maxOutputTokens" FROM "Model"
 WHERE (code LIKE 'claude-opus-4-1%'  AND "maxOutputTokens" > 32000)
    OR (code LIKE 'claude-haiku-4-5%' AND "maxOutputTokens" > 64000)
    OR (code LIKE 'claude-sonnet-4-5%' AND "maxOutputTokens" > 64000)
    OR (code LIKE 'claude-opus-4-5%'  AND "maxOutputTokens" > 64000)
    OR (code IN ('claude-fable-5','claude-sonnet-5','claude-opus-4-8','claude-opus-4-7',
                 'claude-opus-4-6','claude-sonnet-4-6') AND "maxOutputTokens" > 128000);
```

Then confirm the 400 is actually gone, from the gateway, with `max_tokens` deliberately omitted so the catalog ceiling is what goes on the wire:

```bash
curl -s -o /dev/null -w '%{http_code}\n' "$GW/v1/chat/completions" \
  -H "Authorization: Bearer $VK" -H 'content-type: application/json' \
  -d '{"model":"claude-opus-4-8","messages":[{"role":"user","content":"ok"}]}'   # expect 200
```

**Rollback.** `git revert` restores the previous fixture, and re-seeding rewrites the rows back. Note the revert reinstates the over-ceiling caps and with them the 400 — prefer fixing forward. The renamed `code` values do not need reverting either way: the dated ids stay resolvable through `aliases`.

**Dated follow-up — `claude-sonnet-5` prices change on 2026-09-01.** The stored rates are the vendor's introductory ones, which expire **2026-08-31**: input / output go `2` / `10` → **`3` / `15`**, and cache write / read go `2.5` / `0.2` → **`3.75` / `0.3`**. Nothing in this repo watches the clock — there is no scheduled pricing-refresh job — so from Sep 1 the catalog silently under-bills `claude-sonnet-5` by 1.5× until someone edits it. On or after that date, update the four price columns in `tools/db-migrate/model-catalog.json`, regenerate, and re-seed by the value rule above. Rates only: no cap, `code`, or alias moves with it, and already-stamped `traffic_event` rows are historical and are not recomputed.

### `fix/gw-json-parse` — Rule pack: phone PII enforces redaction

**Scope.** No schema change. One `rule` **row** value: `pii-con-002` (phone) moves `severity` `warn` → `soft`. `severityEnforces()` enforces on `hard|soft` and observes on `warn`, so at `warn` a phone number is tagged but never masked, where email (`pii-con-001`) and SSN (`pii-gov-002`) are masked.

The fixtures are generated from `tools/db-migrate/seed/rule-packs/*.yaml`. The YAML already said `soft`; only the generated fixture was stale, so this ships as a regenerated fixture with no YAML change.

**Blast radius — read before prioritising this.** The severity only decides anything where the `nexus/pii` pack's bound hooks are **enabled**. Both ship disabled (`pii-scanner` stage=request, `pii-outbound-scanner` stage=response, `enabled: false` in `HookConfig.json`), and the hook loader selects `WHERE enabled = true`, so on a deployment that has not turned them on the pack never executes and this row changes nothing observable. That is the state of production today — no `HookConfig` row is enabled there, so **nothing** is being masked, phone included. This entry is therefore latent: it corrects the value that would be wrong the moment an operator enables PII scanning. Do not report it as a live leak being closed.

**Tables + JSON paths.** `rule` — `severity`, on the single row `ruleId = 'pii-con-002'`. No JSON paths.

**Value rule.** Run the reference seed. `rule` reconciles on the composite key `(packId, ruleId)` and updates in place.

**Order.** **Seed, then reload config — in that order, and the reload is not optional** on any deployment where the hooks ARE enabled. The engine does not read `rule.severity` per request: `packages/shared/policy/rulepack/enricher.go` materialises the rules into the hook config blob (`Config["_rulePackInstalls"]`) at config-load time. A service that loaded its config before the seed keeps enforcing the old severity, so the DB can read `soft` while the pipeline still only tags. The standard deploy order (restart, then seed) leaves exactly that state — restart again after seeding, or push a config change.

**Smoke after deploy.** The DB value is all that can be checked on a deployment with the hooks off:

```sql
-- Expect soft for all three.
SELECT "ruleId", severity FROM rule WHERE "ruleId" IN ('pii-con-001','pii-con-002','pii-gov-002');
```

Where the hooks ARE enabled, the DB value is necessary but not sufficient — only behaviour proves the config reloaded. Confirm the hook is actually live first, or the check passes for the wrong reason:

```sql
-- If this returns nothing, the pack does not run and the curl below proves nothing.
SELECT name, stage, enabled FROM "HookConfig"
 WHERE id IN ('20b82564-5ce3-4d0b-a102-da5f3bf3f29b','b2f2d960-54a5-4f01-abf7-47877f73bce3')
   AND enabled = true;
```

```bash
# Only meaningful once the request-stage hook is enabled: the number must not
# reach the upstream. Read it off the traffic_event row's stored content, not
# the model's reply — a model that simply did not repeat the number would make
# a response-body grep pass with the redaction still broken.
```

**Rollback.** `git revert` restores the stale fixture and re-seeding writes `warn` back. Fix forward.

### `fix/gw-json-parse` — Model catalog: `omni-moderation-latest` withdrawn

**Scope.** No schema change. `omni-moderation-latest` is removed from `model-catalog.json`, so fresh installs never receive it. The model is alive at OpenAI but the gateway has **no moderation endpoint**; it was seeded `type = 'chat'`, so every call reached `/v1/chat/completions` and was rejected with `This is not a chat model`. It could not work in any configuration, and `inTemplate: true` was actively offering it in the provider wizard. Moderation as a capability belongs to the guardrail surface, not to a chat-typed catalog row.

**Tables + JSON paths.** `Model` — the single row `code = 'omni-moderation-latest'`. No JSON paths.

**Value rule.** **The seed will NOT remove it.** `reconcileRows` upserts; a row the catalog no longer carries is left untouched (see the value rule of the catalog entry above). An existing deployment therefore keeps serving it from `/v1/models` and the smoke keeps driving it — the 8 red arms do not clear on their own. Flip it by hand:

```sql
UPDATE "Model" SET enabled = false WHERE code = 'omni-moderation-latest';
```

**Disable, do not `DELETE`.** `traffic_event.model_id` is a plain column with **no FK**, and calls against this model recorded rows before it was withdrawn (16 on production at the time of writing). Deleting the row orphans that history and the analytics join loses the model name. `enabled = false` reaches the same user-visible end state — `ListEnabledModels` backs `/v1/models`, so a disabled row is gone from the catalog listing, unroutable, and skipped by the smoke — while leaving the history joinable.

**Order.** Independent of the binary; flip whenever. Flipping before the smoke keeps the 8 arms from reporting.

**Smoke after deploy.**

```sql
-- Expect enabled = f (or zero rows on a fresh install).
SELECT code, enabled FROM "Model" WHERE code = 'omni-moderation-latest';
```

```bash
# Expect the id to be absent from the catalog listing.
curl -s "$GW/v1/models" -H "Authorization: Bearer $VK" | grep -c omni-moderation   # expect 0
```

**Rollback.** `UPDATE "Model" SET enabled = true WHERE code = 'omni-moderation-latest';` restores the row — and with it the 400 on every call. There is nothing to roll back to that works; the row only ever produced errors.

### `fix/image-ref-metadata` — `traffic_event_normalized` is dropped at deploy

**Scope.** Schema change, and the only **irreversible** one on this branch. The `traffic_event_normalized` model is gone from `tools/db-migrate/schema/traffic.prisma`, along with the `traffic_event` back-reference. `prisma db push` makes the database match the schema, so the pre-binary schema step **drops the table and every row in it**. Production carried 15 rows at the time of writing (newest 2026-06-26).

The table was a second stored copy of the captured text; the normalized projection is now recomputed at view time from the captured body. The erasure path is the reason: a second copy is a second thing a subject-erasure request can miss, and one surface is the stronger position. `normalizedScrubbed` stays in the DSAR response pinned at `0` — it ships in the admin API contract, so it is deprecated in `dsar.yaml` with a removal window rather than vanishing.

**Tables + paths.** `traffic_event_normalized` — the whole table. `traffic_event` — no column change; only the Prisma-side relation is removed.

**Value rule.** Destroyed, not migrated. Nothing reads the table after step 2 of the drop, and the projection it held is derivable from `traffic_event_payload.inline_request_body`, which is retained.

**Order.** **Deploy the binaries FIRST, then push the schema.** The gateway and Hub stop writing the table in step 1 and stop reading it in step 2; a schema push against binaries that still read it would take the table out from under them. If the schema step runs first by accident, redeploy the binaries before serving traffic.

**Before you push the schema**, if those 15 rows have any retention value, take them — there is no second chance:

```bash
pg_dump -h localhost -U "$PGUSER" -d "$PGDB" -t traffic_event_normalized \
  > "traffic_event_normalized-$(date +%F).sql"
```

**Smoke after deploy.**

```sql
-- Expect NULL: the table is gone.
SELECT to_regclass('public.traffic_event_normalized');
```

```bash
# The normalized projection still answers, recomputed at view time.
# Expect a body with normalized text, not a 500.
curl -s "$CP_URL/api/admin/traffic/$SOME_EVENT_ID" -H "authorization: Bearer $TOKEN" \
  | python3 -c "import json,sys; d=json.load(sys.stdin); print(bool(d.get('requestBody')))"
```

**Rollback.** Re-creating the table restores the schema shape but not the rows, and nothing writes it any more — a restored table stays empty and inert. If the deploy reverts to a binary that still READS the sidecar, that read must find the table: re-create it from the dump above, or from the pre-drop schema, before serving traffic.

### `fix/image-ref-metadata` — routing rules the gateway can no longer dispatch

**Scope.** Two strategy shapes stopped being dispatchable, and the admin API now
refuses both on write. Stored rows carrying them are not refused — they are
inert, and left enabled they occupy a rule slot forever while showing green in
the UI.

**Tables.** `"RoutingRule"` — `enabled`, `description`. Config is never rewritten.

**Value rule.**

- `strategyType = 'policy'` — the strategy never had an implementation. Disable
  and append the reason.
- a config whose entries are ALL nested strategies — an entry inside a strategy
  is resolved as a provider+model leaf, so such a rule produces no target at
  all. Disable and append the reason.

  A PARTIALLY nested rule is deliberately left alone: a fallback chain with four
  leaf links and one nested link still serves the four, and disabling it would
  take working routing offline. Its dead entry is already visible — the resolver
  records the skip on the routing trace and simulate marks the entry
  unreachable.

**Order. Binaries FIRST, then the scripts.**

Running the scripts against the OLD binary disables rules that were routing
correctly, because the old evaluator followed nested entries. Running the new
binary before the scripts leaves the inert rules enabled for that window: they
yield the primary slot and record why, so the rules below them serve the
request — degraded, explained, and not an outage. That asymmetry is why the
order is binaries first.

**Run BOTH, in this order.** They overlap by design: the policy script matches
only a top-level `strategyType='policy'`, and a NESTED `{"type":"policy"}` node
is caught only by the nesting script's predicate. Running one alone leaves
enabled, inert rules behind.

```bash
HOST=${NEXUS_SSH_HOST}
for f in disable_policy_routing_rules_2026_08_08.sql \
         disable_nested_routing_configs_2026_08_08.sql; do
  scp -q -o StrictHostKeyChecking=no "tools/db-migrate/manual-scripts/$f" "$HOST:/tmp/$f"
  ssh -o StrictHostKeyChecking=no "$HOST" \
    "PGPASSWORD=$NEXUS_SSH_PGPASSWORD psql -h localhost -U $NEXUS_SSH_PGUSER \
       -d $NEXUS_SSH_PGDB -v ON_ERROR_STOP=1 -f /tmp/$f; rm -f /tmp/$f"
done
```

Both are idempotent: the marker in `description` guards re-runs, so a second
pass changes nothing and does not duplicate the note. Each ends with a SELECT
listing the matching rules, so the operator sees what was touched rather than
inferring it from a row count.

**Smoke.** Every remaining enabled rule carries a dispatchable strategy:

```sql
SELECT id, name, "strategyType"
  FROM "RoutingRule"
 WHERE "enabled"
   AND "strategyType" NOT IN
       ('single','fallback','loadbalance','conditional','ab_split','smart','latency');
-- expect zero rows
```

**Rollback.** Re-enable by hand: `UPDATE "RoutingRule" SET enabled = true WHERE
id = '...'`. The rule will still route nothing — the binary decides that, not
the flag — so a rollback of the DATA is only useful alongside a rollback of the
binaries. The configuration itself was never modified, so nothing is lost.

### `fix/image-ref-metadata` — three matchConditions keys change meaning, no data change

**Scope.** Behaviour only. No migration and no rows to touch; the entry exists
because a rule an operator wrote and has been watching may start or stop
matching after the binaries roll, and finding that out from traffic is the
expensive way.

**Value rules.**

- `requestedModelLiterals` now matches as GLOBS. The admin form has always
  offered `gpt-4-*` as its example while the comparison was exact, so a rule
  written from that example matched nothing. A pattern with no `*` still
  compares exactly, so `auto` — which every smart rule is required to pin — is
  unaffected. A stored value CONTAINING a `*` starts matching after the roll:
  that is the rule finally doing what its author asked, but it is a change.
- `providers` now compares against every provider serving the named model code,
  not the first catalogue row. A rule scoped to one provider previously fired or
  did not according to row order for any code two providers both serve.
- `modelTypes` matches the ENDPOINT the request arrived on rather than the named
  model's catalogue type. A stored `audio` keeps working and reaches the TTS,
  STT and realtime endpoints; nothing is migrated, because splitting one
  deprecated value into three means guessing which the admin meant.

**Order.** Binaries only. Nothing to run before or after.

**Exposure check** — run BEFORE the deploy, on each environment:

```sql
SELECT id, name, "matchConditions"
  FROM "RoutingRule"
 WHERE enabled
   AND ("matchConditions"::text LIKE '%*%'
     OR "matchConditions" ? 'providers'
     OR "matchConditions" ? 'modelTypes');
-- Each row is a rule whose matching may change. Zero rows = nothing to watch.
-- Production carried zero at the time of writing (5 enabled rules, none using
-- any of the three keys).
```

**Smoke.** For each row the check returned, send one request the rule is meant
to catch and confirm `routing_trace` names that rule:

```sql
SELECT timestamp, routed_model_id, routing_trace->'trace'
  FROM traffic_event ORDER BY timestamp DESC LIMIT 1;
```

**Rollback.** Roll the binaries back. There is no data change to undo.

### `fix/image-ref-metadata` — Model.features loses two names for one capability

**Scope.** In-place value change on `"Model".features`, a published array: GET
/v1/models emits it verbatim. Two edits, both de-duplication rather than loss.

`thinking` and `reasoning` describe one capability and the vendor sets were
DISJOINT — Anthropic and Gemini rows said the first, thirteen other providers
said the second, no row said both. So anything keyed on either saw a partial
answer with nothing to say so. The canonical layer had already settled it
(`ContentReasoning`, `Usage.ReasoningTokens`); the catalogue never followed.
`tool_use` sat on four Cohere rows, every one of which also carried
`function_calling`, so it distinguished nothing.

**Tables + paths.** `"Model".features` only. `tools/db-migrate/model-catalog.json`,
`seed/fixtures/Model.json` and the three affected provider templates carry the
new vocabulary, so a fresh install and a migrated one agree.

**Order.** Either side of the binaries. Nothing reads `thinking` or `tool_use`
in code — the admin picker offered `thinking` and now offers `reasoning`, and
`mergeModelFeatureOptions` keeps rendering an unmigrated value so a row stays
editable rather than silently losing it on the next save.

```bash
HOST=${NEXUS_SSH_HOST}
f=dedupe_model_feature_vocabulary_2026_08_08.sql
scp -q -o StrictHostKeyChecking=no "tools/db-migrate/manual-scripts/$f" "$HOST:/tmp/$f"
ssh -o StrictHostKeyChecking=no "$HOST" \
  "PGPASSWORD=$NEXUS_SSH_PGPASSWORD psql -h localhost -U $NEXUS_SSH_PGUSER \
     -d $NEXUS_SSH_PGDB -v ON_ERROR_STOP=1 -f /tmp/$f; rm -f /tmp/$f"
```

Idempotent by construction — replacing an absent value or removing an absent one
changes nothing — and it REFUSES to run if any row carries `tool_use` without
`function_calling`, because then the value is the only record that the model
takes tools. It ends with the whole vocabulary and its counts, so a leftover is
visible rather than inferred.

**Smoke.** No row carries either retired name, and the count moved rather than
vanished:

```sql
SELECT count(*) FILTER (WHERE 'thinking' = ANY(features))  AS thinking,
       count(*) FILTER (WHERE 'tool_use' = ANY(features))  AS tool_use,
       count(*) FILTER (WHERE 'reasoning' = ANY(features)) AS reasoning
  FROM "Model";
-- expect thinking = 0, tool_use = 0, reasoning = (previous reasoning + previous thinking)
```

Proven on prod rows in a temp table under ROLLBACK before shipping: 13 →
`reasoning`, 4 `tool_use` removed, 83 rows total unchanged, and every other
feature's count identical.

**Rollback.** `UPDATE "Model" SET features = array_append(array_remove(features,
'reasoning'), 'thinking') WHERE provider is Anthropic or Gemini` restores the
old spelling for the rows that had it. `tool_use` is not restorable from the
row itself and does not need to be: `function_calling` beside it carried the
same fact.

### `fix/bugfix-20260819` — Model.features gains `structured_outputs`

**Scope.** Additive value change on `"Model".features`, a published array: GET
/v1/models emits it verbatim and `capability_matrix` now derives a key from it.
No column, no table, no row count changes.

The value is what makes a model ELIGIBLE for a request carrying a
`response_format` of type `json_schema` (and its `/v1/responses`,
Anthropic-Messages and Gemini spellings). One catalogued model is the reason it
has to be a routing constraint rather than a hint: `kimi-k2.5` accepts the field
and answers HTTP 200 with `finish_reason: stop` and prose, so nothing downstream
can turn it into an error and the caller's own parse is the first thing that
notices.

**Tables + paths.** `"Model".features` only. `tools/db-migrate/model-catalog.json`,
`seed/fixtures/Model.json` and five `packages/control-plane-ui/public/provider-templates/*.json`
carry the value, so a fresh install and a synced one agree. 48 chat rows:
openai 16, anthropic 10, moonshot 10, gemini 7, cohere 5.

**Order — the sync is the switch, not the binary.** Deploy the binaries first,
then sync. While NO row carries the tag the router's pool-level fail-open skips
the dimension entirely, so the filter is inert; the first sync that lands the
value is what starts enforcing it. Deploying in the other order would enforce a
dimension against a catalogue that cannot satisfy it.

There is no SQL script for this one. The mechanism is the **Sync button on the
CP UI provider-detail → Model tab**, which diffs `"Model"` rows against
`provider-templates/*.json`; `features` is classified `'set'`, so a new value
MERGES into an existing array rather than replacing it. Sync each of the five
providers above.

Prod was synced once at 44 rows on 2026-08-19, before the four moonshot rows
below were measured. **Sync moonshot again** or those four stay excluded:
`moonshot-v1-8k-vision-preview`, `moonshot-v1-32k-vision-preview`,
`moonshot-v1-128k-vision-preview`, `kimi-k2.7-code-highspeed`.

**Smoke.** The count matches the catalogue, and the two models the wire refuses
are still untagged:

```sql
SELECT count(*) FILTER (WHERE 'structured_outputs' = ANY(features)) AS tagged,
       count(*) FILTER (WHERE code IN ('kimi-k2.5','gpt-4-turbo','deepseek-v4-pro',
                                       'deepseek-v4-flash','gpt-audio-1.5','gpt-audio-mini')
                          AND 'structured_outputs' = ANY(features))  AS wrongly_tagged
  FROM "Model" WHERE type = 'chat';
-- expect tagged = 48, wrongly_tagged = 0
```

Then one live request per direction, against a model the router chose rather
than one the caller named:

```bash
# must come back as a JSON object matching the schema
curl -s "$NEXUS_GW_URL/v1/chat/completions" -H "authorization: Bearer $VK" \
  -H 'content-type: application/json' -d '{"model":"auto","messages":[
    {"role":"user","content":"answer the schema"}],"response_format":{"type":"json_schema",
    "json_schema":{"name":"v","strict":true,"schema":{"type":"object",
    "properties":{"respond":{"type":"boolean"}},"required":["respond"],
    "additionalProperties":false}}}}' | jq -r '.choices[0].message.content'
```

**Rollback.** The value is additive and nothing else reads it, so removing it
restores the previous behaviour exactly — every model becomes eligible again and
the dimension fails open on an empty pool:

```sql
UPDATE "Model" SET features = array_remove(features, 'structured_outputs')
 WHERE 'structured_outputs' = ANY(features);
```

A re-sync re-applies it, so the rollback is not durable against the next sync —
roll the binaries back too if the dimension itself is the problem.

## How to add a new entry

When a branch lands DB-shape changes (schema change in `tools/db-migrate/schema/` applied via `prisma db push`, or in-place JSON-key flip, or computed-column re-population), append a new entry to this file with the same Scope / Tables / Value rule / Order / Smoke / Rollback shape. Commit the entry as part of the same PR. If the PR lands without an entry here, the operator running the deploy will not know what to flip and the runtime will drift from the binary's expectations.

## References

- `tools/db-migrate/seed/fixtures/` — the two-tier fixture seed (reference fixtures + a demo tenant under `fixtures/demo/`) for fresh installs (matches the post-deploy shape).
- `packages/shared/schemas/configtypes/interception/killswitch.go` — wire schema.
- `packages/control-plane/internal/governance/killswitch/handler/handler.go` — admin API surface.
- `packages/compliance-proxy/internal/runtime/killswitch/killswitch.go` — receiver.
- `packages/agent/internal/lifecycle/killswitch/killswitch.go` — receiver.
- `docs/developers/architecture/cross-cutting/safety/kill-switch-architecture.md` — architectural reference.
- `docs/developers/architecture/cross-cutting/safety/pii-redaction-policy-architecture.md` — AI-Guard reconcile architectural reference.
