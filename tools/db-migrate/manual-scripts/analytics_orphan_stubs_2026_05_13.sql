-- Stub parents for orphan analytics IDs (prod, 2026-05-13).
--
-- ┌─────────────────────────────────────────────────────────────────────────┐
-- │ Background: metric_rollup_5m/1h/1d/1mo retain data for longer than      │
-- │ traffic_event, so dimensionKey values like organization=<id> /          │
-- │ project=<id> / virtual_key=<id> can outlive their parent rows. The      │
-- │ analytics handler joins those IDs to Organization / Project /           │
-- │ VirtualKey at query time; when the parent is gone, the UI displays the  │
-- │ raw UUID instead of a name.                                             │
-- │                                                                         │
-- │ This script inserts placeholder parent rows for the 3 orphans we found  │
-- │ on prod so the analytics page renders names again. All three rows are   │
-- │ marked enabled=false / status='revoked'|'archived' to keep them out of  │
-- │ pickers and active-set queries.                                         │
-- │                                                                         │
-- │ ID provenance:                                                          │
-- │   * vk  92a4cf4e — name recovered from AdminAuditLog (delete event)    │
-- │     and traffic_event.identity.credential. Original VK was revoked on  │
-- │     2026-05-08 16:38.                                                  │
-- │   * proj eb45d2f3 — name recovered from traffic_event.identity.project. │
-- │     Belongs to org 5fabaad6 (confirmed by 555/555 rollup bucket         │
-- │     co-occurrence between the two dim keys).                            │
-- │   * org 5fabaad6 — name NOT recoverable; AdminAuditLog does not track  │
-- │     Org/Project create/delete. Name "Apex Research" chosen to pair      │
-- │     with the Research Lab project and to match prod's existing          │
-- │     Apex-prefixed org naming pattern.                                   │
-- └─────────────────────────────────────────────────────────────────────────┘
--
-- USAGE
--   1. Back up affected tables:
--        pg_dump … --table='public."Organization"' --table='public."Project"' \
--                  --table='public."VirtualKey"' > orphan_stubs_backup.sql
--   2. Run this script:
--        psql … -v ON_ERROR_STOP=1 -f analytics_orphan_stubs_2026_05_13.sql
--   3. Hit /analytics in the UI and confirm the IDs now show names.
--
-- Idempotency: every INSERT uses ON CONFLICT (id) DO NOTHING, so re-running
-- after partial application is safe.

BEGIN;

-- Tier 1: Organization (must exist before Project FK).
INSERT INTO "Organization" (
  id, name, code, "parentId",
  description, "contactName", "contactEmail", "contactPhone",
  enabled, "createdAt", "updatedAt", timezone, path, source
)
VALUES (
  '5fabaad6-0441-45f4-b3b0-cac754a3a4a0',
  'Apex Research',
  'APX-RESEARCH-5fabaad6',
  NULL,
  'Reconstructed parent row for an organization whose record was lost before analytics retention drained. Name chosen to pair with the Research Lab project under this org. Restores label lookup for 555 historical rollup buckets.',
  NULL, NULL, NULL,
  FALSE,
  NOW(), NOW(),
  'UTC',
  '/5fabaad6-0441-45f4-b3b0-cac754a3a4a0/',
  'local'
)
ON CONFLICT (id) DO NOTHING;

-- Tier 2: Project under the stub org.
INSERT INTO "Project" (
  id, name, code, "organizationId",
  description, "contactName", "contactEmail",
  status, "createdAt", "updatedAt"
)
VALUES (
  'eb45d2f3-dcb9-4c9f-9c57-3b0e0133cd52',
  'Research Lab',
  'research-lab-archived-eb45d2f3',
  '5fabaad6-0441-45f4-b3b0-cac754a3a4a0',
  'Placeholder for a project that was deleted; name recovered from traffic_event.identity.project. Owned 1396 historical events / 555 rollup buckets.',
  NULL, NULL,
  'archived',
  NOW(), NOW()
)
ON CONFLICT (id) DO NOTHING;

-- Tier 3: VirtualKey. keyHash NULL means no real key derives to this row, so
-- it cannot authenticate any request — purely a label-lookup placeholder.
INSERT INTO "VirtualKey" (
  id, name, "keyHash", "keyPrefix", "projectId", "sourceApp",
  enabled, "expiresAt", "rateLimitRpm", "budgetLimitUsd",
  "allowedModels", "ownerId", "createdBy",
  "createdAt", "updatedAt",
  "vkType", "vkStatus"
)
VALUES (
  '92a4cf4e-38c7-4d62-a588-1099eb941fb8',
  'prod-smoke-test',
  NULL, NULL, NULL, NULL,
  FALSE,
  NULL, NULL, NULL,
  '[]'::jsonb,
  NULL, NULL,
  NOW(), NOW(),
  'personal',
  'revoked'
)
ON CONFLICT (id) DO NOTHING;

-- Verification — should print one row per stub with the names above.
SELECT 'Organization' AS tbl, id, name FROM "Organization" WHERE id='5fabaad6-0441-45f4-b3b0-cac754a3a4a0'
UNION ALL
SELECT 'Project', id, name FROM "Project" WHERE id='eb45d2f3-dcb9-4c9f-9c57-3b0e0133cd52'
UNION ALL
SELECT 'VirtualKey', id, name FROM "VirtualKey" WHERE id='92a4cf4e-38c7-4d62-a588-1099eb941fb8';

COMMIT;
