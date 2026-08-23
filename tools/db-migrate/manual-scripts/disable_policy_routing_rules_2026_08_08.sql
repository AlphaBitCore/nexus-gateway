-- disable_policy_routing_rules_2026_08_08.sql
--
-- WHY: `policy` was an accepted strategyType with no implementation. A rule
--   carrying it was persisted, broadcast fleet-wide, and then produced no
--   target on every gateway — while the admin UI listed it as enabled. Worse,
--   because it was not a `fallback` rule it competed for the primary slot and
--   returned zero targets WITHOUT an error, so every lower-priority rule was
--   locked out and nothing was surfaced.
--
--   The resolver now yields the slot and records why, and the admin API refuses
--   the value on write. Stored rows are the remaining half: left enabled they
--   still occupy a rule slot for no reason, and left editable they cannot be
--   saved (the write path refuses the type they carry).
--
-- WHAT: disables every `policy` rule and appends the reason to its description,
--   so the admin sees the rule, learns it never fired, and can delete it. The
--   row and its config are NOT deleted and NOT rewritten to another type —
--   silently reinterpreting an admin's configuration is what this program
--   exists to stop, and a `policy` node carries no provider/model to convert.
--
-- TARGET: every environment, unconditionally. The gateway ships as OSS, so the
--   set of databases carrying such a row cannot be surveyed — this runs whether
--   or not any row matches. (Checked at authoring time: 0 rows on prod, 0 in
--   the seed fixtures.)
--
-- IDEMPOTENT: yes. Only rules still enabled with strategyType='policy' are
--   touched, and the marker is appended only when it is not already present, so
--   re-running changes nothing and does not duplicate the note.

BEGIN;

UPDATE "RoutingRule"
   SET "enabled"     = false,
       "description" = COALESCE("description", '')
                     || CASE WHEN COALESCE("description", '') = '' THEN '' ELSE ' ' END
                     || '[disabled by upgrade: the `policy` strategy was never implemented. '
                     || 'This rule produced no targets and, at stage 1, prevented every '
                     || 'lower-priority rule from serving. Its configuration is preserved '
                     || 'above; delete the rule or re-author it as a strategy the gateway '
                     || 'can dispatch.]',
       "updatedAt"   = NOW()
 WHERE "strategyType" = 'policy'
   AND "enabled"
   AND COALESCE("description", '') NOT LIKE '%[disabled by upgrade: the `policy` strategy%';

-- Report what was touched, so the operator running this sees it rather than
-- inferring it from a row count scrolling past.
SELECT id, name, "strategyType", "enabled"
  FROM "RoutingRule"
 WHERE "strategyType" = 'policy';

COMMIT;
