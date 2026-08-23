-- disable_nested_routing_configs_2026_08_08.sql
--
-- WHY: a routing rule's ENTRIES — a fallback chain's links, a load balancer's
--   weighted entries, a conditional's branches — now name a provider and a
--   model directly and are resolved as leaves. The gateway no longer evaluates
--   one strategy inside another.
--
--   The admin surface only ever wrote the leaf shape, and the write path now
--   refuses anything else. What remains is a row stored while the API still
--   accepted nesting: it parses, it is broadcast fleet-wide, and it resolves to
--   no target on every gateway. The rule shows enabled in the UI and never
--   fires, and simulate reported a distribution over branches no live request
--   could take (that walker now marks them unreachable instead).
--
-- WHAT: disables only the rules that route NOTHING, and appends the reason.
--
--   The narrowness is the point. A nested entry is skipped, not fatal: a
--   fallback chain with four leaf links and one nested link still serves the
--   four, and a conditional whose branches are leaves still routes even if its
--   `default` is nested. Disabling those would take working routing offline —
--   a migration causing the outage it was written to prevent. So the predicate
--   requires at least one nested entry AND no resolvable leaf anywhere: such a
--   rule produces no target on any request, which is the only case where
--   disabling loses nothing.
--
--   A partially-nested rule is left enabled and serving. Its dead branch is
--   already visible: the resolver records the skip on the routing trace, and
--   simulate marks the entry unreachable.
--
--   The config is NOT rewritten. Flattening someone tree is a guess about what
--   they meant, and the branch they lose might be the one that mattered.
--
-- TARGET: every environment, unconditionally. The gateway ships as OSS, so the
--   databases carrying such a row cannot be surveyed. (Checked at authoring
--   time: 0 rows on prod — every rule there is leaf-shaped — and 0 in the seed.)
--
-- IDEMPOTENT: yes. Only enabled rules that carry a nested entry AND no leaf are
--   touched, and the marker is appended only when absent.

BEGIN;

UPDATE "RoutingRule"
   SET "enabled"     = false,
       "description" = COALESCE("description", '')
                     || CASE WHEN COALESCE("description", '') = '' THEN '' ELSE ' ' END
                     || '[disabled by upgrade: this rule nests one strategy inside another. '
                     || 'The gateway resolves an entry inside a strategy as a provider+model '
                     || 'leaf and does not evaluate a nested strategy, so this rule routed '
                     || 'nothing. Its configuration is preserved above; re-author each entry '
                     || 'as a provider+model, or split the nested part into its own rule.]',
       "updatedAt"   = NOW()
 WHERE "enabled"
   -- carries at least one nested entry ...
   AND (
        jsonb_path_exists(config, '$.targets[*].type ? (@ != "single")')
     OR jsonb_path_exists(config, '$.weightedTargets[*].node.type ? (@ != "single")')
     OR jsonb_path_exists(config, '$.conditions[*].then.type ? (@ != "single")')
     OR jsonb_path_exists(config, '$.default.type ? (@ != "single")')
   )
   -- ... and NOT ONE resolvable leaf, so the rule routes nothing at all.
   AND NOT (
        jsonb_path_exists(config, '$.targets[*].type ? (@ == "single")')
     OR jsonb_path_exists(config, '$.weightedTargets[*].node.type ? (@ == "single")')
     OR jsonb_path_exists(config, '$.conditions[*].then.type ? (@ == "single")')
     OR jsonb_path_exists(config, '$.default.type ? (@ == "single")')
   )
   AND COALESCE("description", '') NOT LIKE '%[disabled by upgrade: this rule nests one strategy%';

SELECT id, name, "strategyType", "enabled"
  FROM "RoutingRule"
 WHERE jsonb_path_exists(config, '$.targets[*].type ? (@ != "single")')
    OR jsonb_path_exists(config, '$.weightedTargets[*].node.type ? (@ != "single")')
    OR jsonb_path_exists(config, '$.conditions[*].then.type ? (@ != "single")')
    OR jsonb_path_exists(config, '$.default.type ? (@ != "single")');

COMMIT;
