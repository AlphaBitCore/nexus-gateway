-- E58-S3 T5.4 + E58-S4 T6.3: per-VK dry-run + compare-endpoint rate limit caps.
--
-- Nullable Int columns; runtime falls back to per-feature defaults
-- (dryRun: 60/min, compare: 30/min) when NULL. No backfill needed —
-- existing VKs inherit defaults.

ALTER TABLE "VirtualKey"
    ADD COLUMN IF NOT EXISTS "dryRunRateLimitRpm" INTEGER,
    ADD COLUMN IF NOT EXISTS "compareEndpointRateLimitRpm" INTEGER;
