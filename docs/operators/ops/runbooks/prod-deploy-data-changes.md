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
