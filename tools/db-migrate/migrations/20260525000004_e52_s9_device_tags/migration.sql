-- E52-S9: free-form tags on devices.
--
-- Ad-hoc labels like `contractor` / `byod` / `executive` that
-- compose into smart-group predicates and into UI filters. Tags are
-- string-only — no key/value pairs (that's what `metadata` is for).
--
-- A GIN index on the array column enables fast "any device tagged X"
-- queries — used by smart-group predicate evaluation and any future
-- UI tag-filter chips.

ALTER TABLE thing ADD COLUMN IF NOT EXISTS tags TEXT[] NOT NULL DEFAULT ARRAY[]::TEXT[];

-- GIN index supports `WHERE tags @> ARRAY['contractor']` / `tags && ARRAY['a','b']`
-- in O(log N) instead of full scans.
CREATE INDEX IF NOT EXISTS thing_tags_gin_idx ON thing USING GIN (tags);
