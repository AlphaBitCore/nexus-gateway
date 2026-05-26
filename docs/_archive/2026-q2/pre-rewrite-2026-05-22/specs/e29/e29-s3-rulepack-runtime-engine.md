# E29 S3 — Rule Pack Runtime Engine

## Story

As a compliance operator I need the Rule Pack rows I import via
E27 to actually participate in the runtime hook pipeline, so that
`content-safety`, `keyword-filter`, and `pii-detector` stop
describing their own patterns in bespoke `HookConfig` blobs and
instead evaluate against the installed Rule Packs.

## Scope

- `packages/shared/policy/hooks/rulepack_engine.go` (new) — implements
  `hooks.Hook` and is registered under `implementationId`
  `rulepack-engine` in `packages/shared/policy/hooks/registry.go`.
- `packages/shared/policy/rulepack/evaluator.go` (existing) — exports the
  regex matcher used by the engine.
- `packages/shared/policy/rulepack/store.go` (existing) — adds a
  `ListEffectiveRuleSetsForHook(ctx, hookId)` helper returning a
  precompiled snapshot (install metadata + override-applied rules +
  compiled `*regexp.Regexp` per rule).
- `packages/shared/policy/hooks/content_safety.go`,
  `packages/shared/policy/hooks/keyword_filter.go`,
  `packages/shared/policy/hooks/pii_detector.go` — re-implement each hook
  as a thin delegate that constructs a `rulepack-engine` with a
  curated default pack (category filter: `safety`, `keyword`,
  `pii`). The three factories remain so that existing `HookConfig`
  rows keep constructing, but their runtime behaviour funnels
  through the shared evaluator.
- `packages/shared/compliance/policy.go` and
  `packages/shared/compliance/config_cache.go` — extend the
  resolver so the runtime engine can fetch the effective rule set
  for the hook being built without reaching back into an HTTP API.
- `packages/ai-gateway/internal/observability/audit/record.go` and
  `packages/compliance-proxy/internal/audit/record.go` — add a
  `BlockingRule` field serialized as JSON
  `{pack, packVersion, ruleId, category, severity, labels}`.
- `packages/shared/audit/schema.sql` (or the Prisma migration
  equivalent) — add a `blocking_rule JSONB NULL` column to the
  traffic audit table.

## Tasks

1. Precompile effective rules per install when the hook config
   cache refreshes; cache compiled regexes in a `sync.Map` keyed
   by `(installId, packVersion)`.
2. Implement `rulepack-engine.Execute`:
   - Read the precompiled rules attached to the hook via
     `HookConfig.ID`.
   - Evaluate every rule against each `ContentBlock.Text`.
   - Collapse matches into a decision by picking the highest-severity
     match: `hard` → `RejectHard`, `soft` → `RejectSoft`, `info` →
     tag-only `Approve`.
   - Populate `HookResult.BlockingRule` with
     `(pack, packVersion, ruleId, category, severity, labels)` when
     the decision is a reject.
3. Rewire `content-safety`, `keyword-filter`, `pii-detector`
   factories to construct a `rulepackEngine` under the hood. The
   legacy bespoke config keys (`patternDefinitions`, `patterns`,
   `categories`) log a deprecation warning at construction time and
   are otherwise ignored.
4. Emit the `BlockingRule` through the audit record in ai-gateway
   and compliance-proxy. Audit writer serializes it as JSON on the
   `blocking_rule` column.
5. Runtime policy resolver wires `ListEffectiveRuleSetsForHook` into
   the snapshot it hands to the pipeline builder.

## Acceptance criteria

- New Go test `TestRulePackEngine_Execute_BlockingRule` covers the
  decision matrix (`hard` / `soft` / `info`, multiple matches,
  override disables, severity override downgrades).
- Parity test `TestContentSafety_ViaRulePackEngine` reproduces the
  previous "violence category rejects soft" scenario against a
  seeded `safety-default` pack.
- Audit row includes `blocking_rule` JSON on a rejected request;
  unmatched traffic leaves the column `NULL`.
- `thingclient.OnConfigChanged` triggered invalidation still
  refreshes the engine's snapshot without a restart.
