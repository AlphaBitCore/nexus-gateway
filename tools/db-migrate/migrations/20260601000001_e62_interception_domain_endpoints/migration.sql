-- E62-S6 FR-9.6: Add applicableEndpoints to interception_domain.
-- Empty array (the default) means "all endpoints" — fully backward compatible
-- with existing interception_domain rows that have no endpoint filter.

ALTER TABLE "interception_domain"
    ADD COLUMN "applicable_endpoints" TEXT[] NOT NULL DEFAULT ARRAY[]::TEXT[];

-- E62-S6: Cohere embedding interception_path additions.
-- The cohere-public interception_domain exists from the original seed but its
-- interception_path rows do not cover /v1/embed or /v2/embed. Insert them now.
INSERT INTO public.interception_path (
    id, domain_id, path_pattern, match_type, action,
    priority, description, enabled, created_at, updated_at
)
SELECT
    gen_random_uuid(),
    d.id,
    ARRAY['/v1/embed', '/v2/embed'],
    'GLOB',
    'PROCESS',
    25,
    'Cohere embed v1/v2 — E62',
    true,
    NOW(),
    NOW()
FROM public.interception_domain d
WHERE d.name = 'cohere-public'
  AND NOT EXISTS (
      SELECT 1
      FROM public.interception_path p
      WHERE p.domain_id = d.id
        AND '/v1/embed' = ANY(p.path_pattern)
  );
