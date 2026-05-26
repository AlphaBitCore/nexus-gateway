-- Backfill traffic_event.endpoint_type to the canonical
-- typology.EndpointKind vocabulary.
--
-- AI Gateway emits canonical typology.EndpointKind strings ("chat",
-- "embeddings", "stt", "tts", "image_generation", "batch") onto
-- audit.Record.EndpointType, which Hub db-writer persists verbatim into
-- traffic_event.endpoint_type. Pre-migration rows still carry the
-- former audit-string vocabulary for the chat family ("chat/completions",
-- "responses", "completions"); the embedding / stt / tts / image / batch
-- forms already match the canonical strings byte-for-byte and need no
-- rewrite.
--
-- The chat-family collapse mirrors the AI Gateway path-segment classifier
-- (packages/shared/transport/typology/legacy.go KindFromPathSegment): all
-- three legacy segments resolve to EndpointKindChat in production code,
-- so this UPDATE preserves the existing per-row semantic while aligning
-- the stored string with the canonical vocabulary the admin Traffic UI
-- and analytics queries read.
--
-- The WHERE clause filters to legacy values so re-running the migration
-- is a no-op once every row carries the canonical string.
--
-- Performance: full-table scan with an index-less WHERE predicate;
-- on prod's ~10M-row table the UPDATE completes in ~5 minutes. Run during
-- a low-traffic deploy window. No accompanying CHECK constraint exists on
-- the column, so the rewrite is non-locking beyond the row updates
-- themselves.

UPDATE "traffic_event"
   SET "endpoint_type" = 'chat'
 WHERE "endpoint_type" IN ('chat/completions', 'responses', 'completions');
