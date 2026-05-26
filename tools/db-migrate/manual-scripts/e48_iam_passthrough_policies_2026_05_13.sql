-- E48 passthrough IAM policy grants — idempotent prod application.
--
-- This script is NOT a Prisma migration. The IAM managed-policy rows live
-- in tools/db-migrate/seed/data/seed-baseline.sql which is loaded by the dev
-- seed pipeline; that file has already been updated to reflect the final
-- state. This script is the corresponding one-shot for prod — it applies
-- the same three changes idempotently against the existing prod IamPolicy
-- rows:
--
--   1. NexusProviderAdminAccess — append `passthrough-read` and
--      `passthrough-write` Statements (allows Provider Admins to read
--      passthrough state and remove overrides during cleanup).
--   2. NexusSecurityAdminAccess — append the same two Statements
--      (security/compliance team needs to inspect + clean up bypass state).
--   3. NexusIncidentResponse — INSERT new managed policy with
--      `passthrough.emergency-enable` plus situational-awareness reads
--      (observability, alerts, traffic-logs, audit-logs, nodes,
--      kill-switch). This is the role that flips the kill-switch during
--      an active incident.
--
-- Run as part of the next prod deploy that ships the E48 admin UI page.
-- Safe to re-run.

BEGIN;

-- ── 1. NexusProviderAdminAccess: append 2 passthrough Statements idempotently
UPDATE public."IamPolicy"
SET document = jsonb_set(
      document,
      '{Statement}',
      (document->'Statement') || jsonb_build_array(
        jsonb_build_object(
          'Sid', 'passthrough-read',
          'Action', jsonb_build_array('admin:passthrough.read'),
          'Effect', 'Allow',
          'Resource', jsonb_build_array('nrn:nexus:gateway:*:passthrough/*')
        ),
        jsonb_build_object(
          'Sid', 'passthrough-write',
          'Action', jsonb_build_array('admin:passthrough.write'),
          'Effect', 'Allow',
          'Resource', jsonb_build_array('nrn:nexus:gateway:*:passthrough/*')
        )
      )
    ),
    "updatedAt" = NOW()
WHERE name = 'NexusProviderAdminAccess'
  -- Idempotency guard: only patch if the Sid is not already present.
  AND NOT EXISTS (
    SELECT 1
    FROM jsonb_array_elements(document->'Statement') s
    WHERE s->>'Sid' = 'passthrough-read'
  );

-- ── 2. NexusSecurityAdminAccess: append the same 2 Statements idempotently
UPDATE public."IamPolicy"
SET document = jsonb_set(
      document,
      '{Statement}',
      (document->'Statement') || jsonb_build_array(
        jsonb_build_object(
          'Sid', 'passthrough-read',
          'Action', jsonb_build_array('admin:passthrough.read'),
          'Effect', 'Allow',
          'Resource', jsonb_build_array('nrn:nexus:gateway:*:passthrough/*')
        ),
        jsonb_build_object(
          'Sid', 'passthrough-write',
          'Action', jsonb_build_array('admin:passthrough.write'),
          'Effect', 'Allow',
          'Resource', jsonb_build_array('nrn:nexus:gateway:*:passthrough/*')
        )
      )
    ),
    "updatedAt" = NOW()
WHERE name = 'NexusSecurityAdminAccess'
  AND NOT EXISTS (
    SELECT 1
    FROM jsonb_array_elements(document->'Statement') s
    WHERE s->>'Sid' = 'passthrough-read'
  );

-- ── 3. NexusIncidentResponse: INSERT new managed policy (idempotent)
INSERT INTO public."IamPolicy" (
  id, name, description, type, document, enabled,
  "createdBy", "createdAt", "updatedAt"
)
VALUES (
  '2f7d4d48-3e7c-4e48-8c48-7a48d3e48e48',
  'NexusIncidentResponse',
  'On-call incident response — operate E48 emergency passthrough kill-switch, force-resync nodes, read traffic & metrics during active incidents',
  'managed',
  '{"Version": "2026-05-13", "Statement": [
    {"Sid": "passthrough-emergency-enable", "Action": ["admin:passthrough.emergency-enable"], "Effect": "Allow", "Resource": ["nrn:nexus:gateway:*:passthrough/*"]},
    {"Sid": "passthrough-read-write", "Action": ["admin:passthrough.read", "admin:passthrough.write"], "Effect": "Allow", "Resource": ["nrn:nexus:gateway:*:passthrough/*"]},
    {"Sid": "observability-incident-response", "Action": ["admin:observability.read"], "Effect": "Allow", "Resource": ["nrn:nexus:platform:*:observability/*"]},
    {"Sid": "alerts-acknowledge", "Action": ["admin:alert.read", "admin:alert.acknowledge"], "Effect": "Allow", "Resource": ["nrn:nexus:platform:*:alert/*"]},
    {"Sid": "traffic-logs-forensic-read", "Action": ["admin:traffic-log.read"], "Effect": "Allow", "Resource": ["nrn:nexus:gateway:*:traffic-log/*"]},
    {"Sid": "audit-logs-read", "Action": ["admin:audit-log.read"], "Effect": "Allow", "Resource": ["nrn:nexus:iam:*:audit-log/*"]},
    {"Sid": "nodes-incident-resync", "Action": ["admin:node.read", "admin:node.force-resync"], "Effect": "Allow", "Resource": ["nrn:nexus:platform:*:node/*"]},
    {"Sid": "kill-switch-toggle", "Action": ["admin:kill-switch.read", "admin:kill-switch.toggle"], "Effect": "Allow", "Resource": ["nrn:nexus:compliance:*:kill-switch/*"]},
    {"Sid": "settings-read-for-nav", "Action": ["admin:settings.read"], "Effect": "Allow", "Resource": ["nrn:nexus:platform:*:settings/*"]}
  ]}'::jsonb,
  true,
  'seed-script',
  NOW(),
  NOW()
)
ON CONFLICT (id) DO NOTHING;

-- Final sanity check: the 3 expected rows are present with passthrough actions.
DO $$
DECLARE
  provider_ok BOOLEAN;
  security_ok BOOLEAN;
  incident_ok BOOLEAN;
BEGIN
  SELECT EXISTS (
    SELECT 1 FROM public."IamPolicy"
    WHERE name = 'NexusProviderAdminAccess'
      AND document @? '$.Statement[*] ? (@.Sid == "passthrough-read")'
  ) INTO provider_ok;
  SELECT EXISTS (
    SELECT 1 FROM public."IamPolicy"
    WHERE name = 'NexusSecurityAdminAccess'
      AND document @? '$.Statement[*] ? (@.Sid == "passthrough-read")'
  ) INTO security_ok;
  SELECT EXISTS (
    SELECT 1 FROM public."IamPolicy"
    WHERE name = 'NexusIncidentResponse'
      AND document @? '$.Statement[*] ? (@.Sid == "passthrough-emergency-enable")'
  ) INTO incident_ok;
  IF NOT (provider_ok AND security_ok AND incident_ok) THEN
    RAISE EXCEPTION 'IAM passthrough policy sanity check failed: provider=%, security=%, incident=%',
      provider_ok, security_ok, incident_ok;
  END IF;
  RAISE NOTICE 'IAM passthrough policy grants applied: provider=%, security=%, incident=%',
    provider_ok, security_ok, incident_ok;
END $$;

COMMIT;
