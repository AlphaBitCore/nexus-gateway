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

## How to add a new entry

When a branch lands DB-shape changes (schema migration in `tools/db-migrate/migrations/`, or in-place JSON-key flip, or computed-column re-population), append a new entry to this file with the same Scope / Tables / Value rule / Order / Smoke / Rollback shape. Commit the entry as part of the same PR. If the PR lands without an entry here, the operator running the deploy will not know what to flip and the runtime will drift from the binary's expectations.

## References

- `tools/db-migrate/seed/data/seed-baseline.sql` — current canonical seed for fresh installs (matches the post-deploy shape).
- `packages/shared/schemas/configtypes/interception/killswitch.go` — wire schema.
- `packages/control-plane/internal/governance/killswitch/handler/handler.go` — admin API surface.
- `packages/compliance-proxy/internal/runtime/killswitch/killswitch.go` — receiver.
- `packages/agent/internal/lifecycle/killswitch/killswitch.go` — receiver.
- `docs/developers/architecture/cross-cutting/safety/kill-switch-architecture.md` — architectural reference.
- `docs/developers/architecture/cross-cutting/safety/pii-redaction-policy-architecture.md` — AI-Guard reconcile architectural reference.
