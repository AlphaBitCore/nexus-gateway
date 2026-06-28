-- migrate_hook_onmatch_action_2026_06_22.sql
--
-- WHY: The hook onMatch contract collapsed its two orthogonal fields
--   (inflightAction × storageAction) into a single `action` (approve | redact |
--   block). The runtime reader still maps the legacy keys for a deprecation
--   window, but stored HookConfig.config rows should be rewritten to the new
--   shape so the legacy keys can be dropped from the schema after the window.
--
-- TARGET: any environment whose "HookConfig" rows predate the action merge
--   (dev / staging / prod). Fresh databases seeded after this change already
--   carry the new shape (tools/db-migrate/seed/fixtures/HookConfig.json).
--
-- IDEMPOTENT: yes. Only rows that still carry a legacy inflightAction/
--   storageAction key AND do not yet carry `action` are rewritten; re-running
--   is a no-op. The CASE mirrors decision.ActionFromLegacy (the new action
--   follows the inflight axis; an approve paired with a redacting storage
--   policy upgrades to redact — the compliance-safe direction).
--
-- MAPPING:
--   inflightAction block-hard | block-soft           -> block
--   inflightAction redact                            -> redact
--   inflightAction approve + storage redact|drop     -> redact
--   inflightAction approve + storage keep|absent     -> approve
--   inflightAction absent (legacy match default)     -> block

UPDATE "HookConfig"
SET config = jsonb_set(
        config #- '{onMatch,inflightAction}' #- '{onMatch,storageAction}',
        '{onMatch,action}',
        to_jsonb(
            CASE
                WHEN config->'onMatch'->>'inflightAction' IN ('block-hard', 'block-soft') THEN 'block'
                WHEN config->'onMatch'->>'inflightAction' = 'redact' THEN 'redact'
                WHEN config->'onMatch'->>'inflightAction' = 'approve'
                     AND config->'onMatch'->>'storageAction' IN ('redact', 'drop-content') THEN 'redact'
                WHEN config->'onMatch'->>'inflightAction' = 'approve' THEN 'approve'
                ELSE 'block'
            END
        )
    )
WHERE config ? 'onMatch'
  AND (config->'onMatch' ? 'inflightAction' OR config->'onMatch' ? 'storageAction')
  AND NOT (config->'onMatch' ? 'action');
