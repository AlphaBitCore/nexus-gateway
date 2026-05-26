-- E50 — Traffic event latency phase breakdown columns.
--
-- Splits the historical single `latency_ms` value into a closed-set
-- phase taxonomy captured by ai-gateway, compliance-proxy, and agent.
-- See docs/users/product/architecture.md "Latency Phase Taxonomy (E50)" and
-- docs/developers/specs/e50/e50-s1-schema-and-shared.md.
--
-- Column semantics:
--   upstream_ttfb_ms   — TTFB from the destination this row's service
--                        forwarded to. Streaming: first chunk arrival.
--   upstream_total_ms  — Full upstream round-trip (header + body / stream close).
--   request_hooks_ms   — Aggregate of per-hook latency in request_hooks_pipeline.
--   response_hooks_ms  — Aggregate of per-hook latency in response_hooks_pipeline.
--   latency_breakdown  — JSONB long-tail per-source phases. Closed key set
--                        per `source`; producers must not invent ad-hoc keys.
--
-- our_overhead_ms is NOT stored — derived as
--   GREATEST(0, latency_ms - upstream_total_ms)
-- at read time in admin API responses and UI rendering.

ALTER TABLE "traffic_event"
  ADD COLUMN "upstream_ttfb_ms"   INTEGER,
  ADD COLUMN "upstream_total_ms"  INTEGER,
  ADD COLUMN "request_hooks_ms"   INTEGER,
  ADD COLUMN "response_hooks_ms"  INTEGER,
  ADD COLUMN "latency_breakdown"  JSONB;

-- Non-negative duration constraints. CHECK is permissive for NULL so
-- backfill / pre-E50 rows pass.
ALTER TABLE "traffic_event"
  ADD CONSTRAINT "chk_traffic_event_upstream_ttfb_nonneg"
    CHECK ("upstream_ttfb_ms" IS NULL OR "upstream_ttfb_ms" >= 0),
  ADD CONSTRAINT "chk_traffic_event_upstream_total_nonneg"
    CHECK ("upstream_total_ms" IS NULL OR "upstream_total_ms" >= 0),
  ADD CONSTRAINT "chk_traffic_event_request_hooks_nonneg"
    CHECK ("request_hooks_ms" IS NULL OR "request_hooks_ms" >= 0),
  ADD CONSTRAINT "chk_traffic_event_response_hooks_nonneg"
    CHECK ("response_hooks_ms" IS NULL OR "response_hooks_ms" >= 0),
  ADD CONSTRAINT "chk_traffic_event_ttfb_le_total"
    CHECK ("upstream_ttfb_ms" IS NULL
        OR "upstream_total_ms" IS NULL
        OR "upstream_ttfb_ms" <= "upstream_total_ms");
