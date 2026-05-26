-- E68 negative-feedback bug fix (2026-05-21).
--
-- The L2 semantic-cache poison list is keyed on the Redis HASH key of the
-- cached entry, not on traffic_event.id. Before this migration the audit
-- drawer's "Mark as bad cache hit" thumbs-down posted traffic_event.id as
-- entryKey, which never matched the Reader's IsPoisoned check — the
-- negative-feedback action was a silent no-op in production.
--
-- Fix: stamp the Redis HASH key on every gateway_cache_kind='semantic' row
-- so the admin UI can post the real key the gateway will check on its next
-- FT.SEARCH hit. Column is nullable + only populated when a semantic L2 hit
-- actually served the response; NULL on extract-cache hits, MISS, SKIPPED,
-- and on rows from data-planes that don't run L2 (compliance-proxy, agent).
--
-- Format: "<redis_index_name>:<sha256(EmbeddingInput)[:16]>" — see
-- packages/ai-gateway/internal/cache/semantic/client.go entryKey().

ALTER TABLE "traffic_event"
ADD COLUMN "gateway_cache_l2_entry_key" TEXT;
