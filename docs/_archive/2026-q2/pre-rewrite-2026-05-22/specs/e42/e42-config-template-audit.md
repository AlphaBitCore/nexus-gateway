# E42 — Configuration Template Audit & Hub Self-Shadow

## Background

The Control Plane UI exposes a Configuration tab on each Node detail page
(`/infrastructure/nodes/:id` → "Configuration"). That tab renders one row
per templated `config_key` registered in the `thing_config_template` table
and shows a 4-column merged view (template default / target / applied /
override). It is the operator's primary entry point for "what runtime
state is Hub pushing to this node, what has the node actually applied, and
where can I override or force a resync?"

A 2026-05 audit surfaced four gaps that make this tab misleading or empty
for most node types:

1. **The template registry is severely under-populated.** The
   `thing_config_template` table contains 2 rows total:
   `ai-gateway/gemini_cache` and `compliance-proxy/onboarding`. The four
   services that consume shadow updates (ai-gateway, compliance-proxy,
   agent, control-plane) actually subscribe to **32 distinct
   `config_key` values** across their `OnConfigChanged` switches and
   `configsync.ShadowApplier` tables. Operators visiting Node Detail for
   any agent see "No configuration templates for this node type"; for
   ai-gateway and compliance-proxy they see only the single registered
   key, while routing rules, credentials, hooks, observability, etc.
   are invisible.
2. **`nexus-hub` never participates in shadow config.** Hub is the
   broker that pushes shadows to other Things and historically does not
   consume them. The Configuration tab shows a Hub-specific empty-state
   ("Hub manages templates for other nodes; it does not consume managed
   templates itself"). But Hub has significant runtime tunables
   (OTEL endpoint, scheduler intervals, retention windows, log level)
   that today require a YAML edit + process restart.
3. **`Thing.metadata` is not displayed.** The `thing.metadata` JSONB
   column carries useful operator information (host info written by
   selfreg, enrollment provenance, custom labels). The Node Detail
   Overview tab renders a fixed set of InfoRow fields and never surfaces
   `metadata`.
4. **The template-registration policy is not written down.** The relationship
   between `OnConfigChanged` switch keys and `thing_config_template` rows is
   implicit and was not enforced; both prior migrations
   (`20260511000001_fix_thing_config_template_gaps` and
   `20260511000002_remove_cp_redundant_template_keys`) had to chase the
   drift retroactively.

E42 closes all four gaps. The work is scoped into three SDD stories
delivered as PR1; PR2 and PR3 extend Hub self-shadow coverage and
promote additional YAML-only knobs to shadow-mutable, but are explicitly
out of scope for PR1.

## Glossary

- **Shadow key** — A logical configuration namespace pushed from Hub to a
  Thing inside `thing.desired` and reported back inside `thing.reported`.
  Each key is dispatched in the consuming service's
  `thingclient.Options.OnConfigChanged` switch (or for Agent, the
  `configsync.ShadowApplier` table).
- **Template** — A row of `thing_config_template` keyed by
  `(type, config_key)`. Holds the default `state` JSON used when a Thing
  has not yet received an explicit override and acts as the registry of
  "this key is meaningful for this type".
- **Override** — A per-Thing+key entry in `thing_config_override` that
  shadows the template default for one specific node (e.g. set the
  killswitch to engaged on `compliance-proxy-canary-02` only).
- **Self-shadow** — Hub's mechanism for consuming its own shadow row via
  PostgreSQL `LISTEN`/`NOTIFY`, in place of a `thingclient` WebSocket
  loop pointed at itself.
- **Bootstrap config** — Fields that must come from `*.dev.yaml`/env
  because they are required to construct the shadow channel itself
  (DB DSN, Redis URL, MQ broker URL, internal-service tokens, TLS
  cert/key paths, server listen address, identity).

## Personas

- **Operator / SRE** — Wants to see every runtime-mutable config a node
  type consumes, see what state Hub is pushing right now, compare against
  what the node has applied, and force a resync or set a per-node
  override without restarting the binary or editing YAML.
- **Platform engineer** — Needs the template registry to stay aligned
  with the consuming service's `OnConfigChanged` switch, with the alignment
  rule written down so future shadow keys are added in one place that
  ships them everywhere (handler + template + UI).
- **Compliance reviewer** — Wants the existing audit chain (template
  version bumps + override actor/timestamp) to continue working
  unchanged.

## Functional Requirements (MoSCoW)

### Must

- **F1 — Template registry alignment (Tier 1).** For every shadow `config_key`
  dispatched by ai-gateway, compliance-proxy, agent, or control-plane,
  there MUST be a matching row in `thing_config_template` with the
  service's compiled-in default as the `state` JSON. The four target
  services subscribe to 32 keys in total (ai-gateway 15, compliance-proxy 8,
  agent 8, control-plane 1); the existing migration leaves 2 keys covered
  (ai-gateway/gemini_cache + compliance-proxy/onboarding), so the gap-fill
  migration MUST INSERT/UPSERT 30 new rows (plus 1 row for the first Hub
  self-shadow key, observability).
- **F2 — Backfill desired state for existing Things.** Every existing
  `thing` row of the affected types MUST receive a desired-state merge
  for the newly added keys (key absent → key inserted with the default;
  key already present → unchanged). `desired_ver` MUST be bumped to
  `MAX(desired_ver)+1` for each affected type so connected nodes fire
  `OnConfigChanged` on next reconnect.
- **F3 — Hub self-shadow for `observability`.** `nexus-hub` MUST gain a
  `thing_config_template` row for `observability` with the same default
  shape as other services. The Hub process MUST run a self-shadow manager
  that LISTENs on `config_changed`, filters notifications by `hub.id`,
  re-reads `thing.desired`, and dispatches an in-process callback that
  reconfigures the OTEL exporter without restart. The manager MUST be
  multi-instance safe (every Hub instance LISTENs and reacts
  independently).
- **F4 — `NOTIFY config_changed` emission.** Every Hub code path that
  writes `thing.desired` (`thingmgr.UpdateDesiredForType`, admin
  override apply/clear, future cluster fan-outs) MUST emit
  `NOTIFY config_changed, '<thing_id>'` inside the same DB transaction
  so the listener only observes committed state.
- **F5 — Node Detail metadata display.** The Overview tab of
  `InfraNodeDetailPage` MUST render `thing.metadata` when non-null.
  Common keys (`hostname`, `os`, `osVersion`, `enrolledBy`, `role`,
  `metricsUrl`, `schedulerEnabled`, `source_ip`, `pid`) MUST be highlighted
  at the top of the section with friendly labels; the raw JSON MUST be
  available below in a default-collapsed code block. All copy MUST exist
  in `en`, `zh`, and `es` locale files.

### Should

- **S1 — Template-registration policy in `docs/developers/architecture/cross-cutting/foundation/thing-model.md`.** The
  rule "every `OnConfigChanged` case key has exactly one matching
  template row; no orphans in either direction" SHOULD be added to
  Section 5 of the thing-model doc with rationale. Future PRs referencing
  this policy can link to it instead of restating it.
- **S2 — Bootstrap boundary in `docs/developers/architecture/cross-cutting/foundation/thing-model.md`.** The list of
  fields excluded from shadow management (DB DSN, secrets, identity,
  cert paths, listen address) SHOULD be enumerated in the same section
  so reviewers have a clear test for "could this knob be promoted?".

### Could

- **C1 — Cluster fan-out helper for Hub.** A helper that mirrors a
  shadow-write across every Thing of `type='nexus-hub'` COULD ship in
  PR2 to support multi-instance Hubs in production. Not required for
  PR1 because dev / staging run a single Hub.
- **C2 — Force Resync All button on Hub.** Already implemented globally
  in the Configuration tab; works for Hub via the same NOTIFY path once
  F3 lands.

### Won't (this epic)

- **W1 — Promoting additional YAML-only knobs to shadow-mutable.** The
  per-service audit identified ~28 candidates (upstream timeouts,
  cache TTL, log levels, scheduler intervals, retention windows,
  compliance streaming mode, etc.). These are deferred to PR2 / PR3 in
  the same epic.
- **W2 — Per-instance vs cluster-wide config layering for Hub.** PR1
  ships per-instance Hub shadow rows only. Cluster-wide config (one row
  that fans out to all Hub instances) is a PR2 design.
- **W3 — Replacing JSON cells with schema-driven editors.** The
  Configuration tab continues to render JSON in `<pre>` blocks; per-key
  React forms are a separate UX project.

## Non-Functional Requirements

- **N1 — No hot-path regression.** The selfshadow manager runs in a
  dedicated goroutine; reload callbacks must be O(1) in the number of
  active requests and must not block the LISTEN connection.
- **N2 — Crash safety.** A panic inside a reload callback must be
  recovered by the selfshadow manager and logged at `slog.LevelError`
  without crashing the Hub process. The callback dispatcher retries on
  subsequent NOTIFY events; it does not loop on the failing payload.
- **N3 — Multi-instance correctness.** With three Hub replicas, a config
  change made via admin UI on Replica A must reach Replicas B and C
  within 1 second over `NOTIFY` (PostgreSQL guarantees commit-time
  delivery).
- **N4 — Operator-facing copy stays English-only.** Per repo policy, all
  new i18n keys, log messages, and doc text are English. `zh` and `es`
  locale files receive English-source translations; technical tokens
  (config keys, thing types) remain literal.

## Constraints & Assumptions

- The `thing_config_template`, `thing.desired`, and
  `thing_config_override` schemas remain unchanged. E42 is pure migration
  data + new in-process code + frontend additions.
- The `appliedConfigStore.ListTemplatesByType` query is the
  authoritative read path for the Configuration tab and remains
  unchanged.
- `selfreg.SelfRegistrar` already writes a `nexus-hub` row for the
  running Hub instance using `hub.id`; selfshadow reuses that row and
  does not create a parallel registration mechanism.
- Existing `thing.desired_ver` bumps inside the migration are the same
  mechanism used by `20260511000001`; downstream services already accept
  this through their reconnect path.
- PostgreSQL `LISTEN`/`NOTIFY` payloads are 8 KB max — we send only
  `thing_id` (a UUID at worst), well under the limit.

## Acceptance Criteria

- AC1 — Visiting `/infrastructure/nodes/hub-dev` shows the Configuration
  tab populated with `observability` template + applied state. The empty-state
  copy `noTemplatesHub` no longer renders for Hub.
- AC2 — Visiting `/infrastructure/nodes/<any-agent-id>` shows the
  Configuration tab populated with 8 template rows
  (`auth`, `exemptions`, `hook_config`, `interception_domains`,
  `killswitch`, `observability`, `payload_capture`, `policy_rules`).
- AC3 — Visiting `/infrastructure/nodes/<any-ai-gateway-id>` shows the
  Configuration tab populated with 15 template rows.
- AC4 — Visiting `/infrastructure/nodes/<any-compliance-proxy-id>` shows
  the Configuration tab populated with 8 template rows.
- AC5 — Visiting `/infrastructure/nodes/<any-control-plane-id>` shows
  the Configuration tab populated with `observability`.
- AC6 — Visiting Node Detail Overview tab for any node, the Metadata
  section is visible. Common keys are surfaced with friendly labels;
  raw JSON is available in a collapsed block.
- AC7 — Modifying the Hub's `observability` template (or adding an
  override on the Hub node) reconfigures the running Hub's OTEL exporter
  within 2 seconds — no Hub restart required.
- AC8 — `go test ./packages/nexus-hub/internal/self/shadow/...` and
  `go test ./packages/control-plane/internal/handler/...` pass.
- AC9 — `docs/developers/architecture/cross-cutting/foundation/thing-model.md` Section 5 documents both the 1:1
  registration policy and the bootstrap boundary; reviewers can point
  future PRs at the rule.
