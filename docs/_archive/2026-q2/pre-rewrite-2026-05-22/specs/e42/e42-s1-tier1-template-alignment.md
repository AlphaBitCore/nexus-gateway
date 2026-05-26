# E42-S1 — Tier 1 Template Registry Alignment (SDD)

## User story

As an operator, when I open the Configuration tab on any Node detail page
for ai-gateway, compliance-proxy, agent, or control-plane, I see a row
for every shadow `config_key` the running service actually consumes — so
I can inspect Hub-pushed target state, see what the node has applied,
force a per-key resync, and add overrides without bouncing between the
UI and the source code.

## Tasks

1. **Inventory the ground truth.** For each of the four consuming services,
   list every `case` in the OnConfigChanged switch (or for Agent, every
   `ShadowApplier` entry) and capture the service's compiled-in default
   value for that key. Source files:
   - `packages/ai-gateway/cmd/ai-gateway/main.go` (lines ~543–635)
   - `packages/compliance-proxy/cmd/compliance-proxy/main.go` (lines ~462–584)
   - `packages/agent/core/sync/configsync/manager.go` (lines ~113–134)
   - `packages/control-plane/cmd/control-plane/main.go` (lines ~334–348)
2. **Write the migration** at `tools/db-migrate/migrations/<ts>_e42_config_template_audit/migration.sql`.
   The migration must:
   - `INSERT ... ON CONFLICT DO NOTHING` 31 new template rows
     (ai-gateway: 14 new; compliance-proxy: 7 new; agent: 8 new;
     control-plane: 1 new; nexus-hub: 1 new — observability).
     `(ai-gateway, gemini_cache)` and `(compliance-proxy, onboarding)`
     already exist and are skipped.
   - For each affected `type`, run a single `UPDATE thing SET desired =
     desired || <new keys merged via CASE-when-absent>, desired_ver =
     MAX(desired_ver)+1` so the bump happens once per type. Keys
     already present in `desired` MUST be left untouched (operator may
     have customised them via override).
3. **Default state JSON for each new key.** Defaults are derived from the
   service's static fallback so a freshly enrolled node behaves the same
   before and after the operator first edits the UI. Examples:
   - `observability` → `{"enabled": false, "endpoint": "", "serviceName": "", "samplingRate": 1.0}`
   - `killswitch` → `{"engaged": false}`
   - `payload_capture` → `{"enabled": false, "sample_rate": 0, "max_body_bytes": 0}`
   - Category B pointer keys (`routing_rules`, `policy_rules`, `interception_domains`,
     `credentials`, `virtual_keys`, `providers`, `models`, etc.) → `{}` (the
     applier pulls the authoritative payload from its own table on first sync).
4. **Do not edit handler code.** The four services already dispatch every
   listed key; this story is migration-only. Any drift detected during
   inventory (a key dispatched but with a typo / wrong fallback) is fixed in
   a separate commit so the migration stays a pure data change.
5. **Update IoT-policy notes in `docs/developers/architecture/cross-cutting/foundation/thing-model.md` Section 5.** Add the
   1:1 registration rule and the bootstrap boundary list. (Architecture
   step already done in commit 1 of the PR.)

## Out of scope

- Adding new `OnConfigChanged` subscriptions to promote YAML-only knobs
  (covered by PR2 / PR3 of E42).
- Replacing the JSON `<pre>` view with a schema-driven editor.
- Hub self-shadow code (covered by E42-S2). This story only seeds the
  Hub `observability` row; the runtime wiring lands in S2.
- Any UI changes beyond the rows that automatically appear once
  `ListTemplatesByType` returns more entries.

## Acceptance criteria

- [ ] `SELECT type, COUNT(*) FROM thing_config_template GROUP BY type ORDER BY type;`
      returns: `agent=8, ai-gateway=15, compliance-proxy=8, control-plane=1, nexus-hub=1`.
- [ ] For every existing `thing` row of an affected type, every newly
      registered `config_key` is present in `thing.desired` after the
      migration runs. Verification SQL: `SELECT id, type, jsonb_object_keys(desired) FROM thing WHERE type IN (...)`.
- [ ] `thing.desired_ver` for at least one node of each affected type has
      advanced (the per-type bump). Existing per-Thing override rows are
      not touched.
- [ ] Visiting `/infrastructure/nodes/<id>` Configuration tab for any
      ai-gateway / compliance-proxy / agent / control-plane node renders
      one row per registered template key with the default JSON in the
      "Template default" column.
- [ ] No regression on existing rows: `(ai-gateway, gemini_cache).state`
      and `(compliance-proxy, onboarding).state` are byte-identical to
      their pre-migration values.
