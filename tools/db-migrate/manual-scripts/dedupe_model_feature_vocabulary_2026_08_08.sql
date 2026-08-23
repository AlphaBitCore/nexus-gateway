-- Model.features carried two names for one capability and one name that added
-- nothing.
--
-- `thinking` and `reasoning` describe the same thing — a model that reasons
-- before answering — and the vendor sets were DISJOINT: Anthropic and Gemini
-- rows said `thinking`, thirteen other providers said `reasoning`, and no row
-- said both. So a routing rule, a picker, or an operator's query keyed on
-- either one saw a partial answer and nothing said so. The canonical layer had
-- already settled the question: the response content type is `reasoning` and
-- the token counter is `ReasoningTokens`. The catalogue never followed.
--
-- `tool_use` was on four Cohere rows, every one of which also carried
-- `function_calling`. It distinguished nothing.
--
-- `features` is published verbatim by GET /v1/models, so this changes a shipped
-- contract. Approved as such; the CHANGELOG carries the migration note.
--
-- Idempotent by construction: replacing a value that is absent, or removing one
-- that is absent, changes nothing. Safe to re-run.

BEGIN;

-- Guard: every tool_use row must also carry function_calling, or dropping the
-- value loses information rather than de-duplicating it. Verified on prod
-- (0 rows) before writing this; asserted here so a different database with a
-- different shape stops instead of silently losing a capability.
DO $$
DECLARE
    orphaned int;
BEGIN
    SELECT count(*) INTO orphaned
      FROM "Model"
     WHERE 'tool_use' = ANY(features)
       AND NOT ('function_calling' = ANY(features));
    IF orphaned > 0 THEN
        RAISE EXCEPTION 'refusing to drop tool_use: % row(s) carry it WITHOUT '
            'function_calling, so the value is the only record that the model '
            'takes tools. Add function_calling to those rows first.', orphaned;
    END IF;
END $$;

-- thinking → reasoning. array_remove first so a row that somehow carries both
-- ends with one, not two.
UPDATE "Model"
   SET features = array_append(array_remove(features, 'thinking'), 'reasoning')
 WHERE 'thinking' = ANY(features)
   AND NOT ('reasoning' = ANY(features));

-- A row carrying both loses only the duplicate.
UPDATE "Model"
   SET features = array_remove(features, 'thinking')
 WHERE 'thinking' = ANY(features);

-- tool_use adds nothing beside function_calling.
UPDATE "Model"
   SET features = array_remove(features, 'tool_use')
 WHERE 'tool_use' = ANY(features);

COMMIT;

-- What the operator sees rather than a row count: the vocabulary as it now
-- stands, so a leftover is visible instead of inferred.
SELECT unnest(features) AS feature, count(*) AS models
  FROM "Model"
 GROUP BY 1
 ORDER BY 2 DESC, 1;
