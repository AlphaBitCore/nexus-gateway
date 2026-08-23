-- Post-push schema extras: PostgreSQL-native DDL that `prisma db push` cannot
-- express from schema.prisma's model graph. Apply AFTER `prisma db push` and
-- BEFORE seeding. Re-runnable (DROP ... IF EXISTS + CREATE ... IF NOT EXISTS),
-- but note the metric_ops_raw block is destructive to that table's rows — ops
-- telemetry is disposable, so re-applying on an existing DB is acceptable.
--
-- Currently the only entry is the metric_ops_raw RANGE partitioning: schema.prisma
-- declares MetricOpsRaw as a plain table (Prisma has no partition representation);
-- the Hub `ops-raw-partition` job requires the table to be RANGE-partitioned on
-- sampled_at or it fails every cycle ("metric_ops_raw is not partitioned").

DROP TABLE IF EXISTS metric_ops_raw CASCADE;

CREATE TABLE metric_ops_raw (
    id            uuid                        NOT NULL,
    sampled_at    timestamp(6) with time zone NOT NULL,
    thing_id      text                        NOT NULL,
    thing_type    text                        NOT NULL,
    metric_name   text                        NOT NULL,
    metric_kind   text                        NOT NULL,
    dimension_key text DEFAULT ''::text       NOT NULL,
    value         double precision,
    metadata      jsonb,
    PRIMARY KEY (sampled_at, id)
) PARTITION BY RANGE (sampled_at);

CREATE UNIQUE INDEX metric_ops_raw_sampled_at_thing_id_metric_name_dimension_ke_key
    ON metric_ops_raw (sampled_at, thing_id, metric_name, dimension_key);
CREATE INDEX metric_ops_raw_metric_name_sampled_at_idx
    ON metric_ops_raw (metric_name, sampled_at DESC);
CREATE INDEX metric_ops_raw_thing_id_sampled_at_idx
    ON metric_ops_raw (thing_id, sampled_at DESC);
CREATE INDEX metric_ops_raw_thing_type_sampled_at_idx
    ON metric_ops_raw (thing_type, sampled_at DESC);

ALTER TABLE metric_ops_raw
    ADD CONSTRAINT metric_ops_raw_thing_id_fkey FOREIGN KEY (thing_id)
    REFERENCES thing(id) ON UPDATE CASCADE ON DELETE CASCADE;

-- Pre-create daily partitions spanning [today-30, today+2] so inserts land
-- immediately; the Hub partition job extends the window on its own cadence.
DO $$
DECLARE
    d0 date := (now() AT TIME ZONE 'UTC')::date;
    d  date;
BEGIN
    FOR d IN SELECT generate_series(d0 - 30, d0 + 2, interval '1 day')::date LOOP
        EXECUTE format(
            'CREATE TABLE IF NOT EXISTS %I PARTITION OF metric_ops_raw FOR VALUES FROM (%L) TO (%L)',
            'metric_ops_raw_p' || to_char(d, 'YYYYMMDD'),
            to_char(d, 'YYYY-MM-DD') || ' 00:00:00+00',
            to_char(d + 1, 'YYYY-MM-DD') || ' 00:00:00+00'
        );
    END LOOP;
END $$;

-- ── Function: cache_key_source ────────────────────────────────────────────
-- Resolves which config tier supplies a given cache key. No Prisma representation.
-- Tier 1 (cache_global_config) is retired: the orphaned table is never joined,
-- so an ops query cannot surface its stale switch state as "effective".
CREATE OR REPLACE FUNCTION public.cache_key_source(p_provider_id text, p_key text) RETURNS text
    LANGUAGE sql STABLE
    AS $$
  SELECT CASE
    WHEN o."config" ? p_key THEN 'provider-override'
    WHEN a."config" ? p_key THEN 'adapter-default'
    ELSE 'code-default'
  END
  FROM "Provider" p
  LEFT JOIN "cache_adapter_config"  a ON a."adapter_type" = p."adapter_type"
  LEFT JOIN "cache_provider_config" o ON o."provider_id" = p."id"
  WHERE p."id" = p_provider_id;
$$;

-- ── View: cache_provider_effective ────────────────────────────────────────
-- Merges the two cache-config tiers (adapter + provider) into one effective
-- row per provider. Tier 1 is retired — the orphaned cache_global_config is
-- deliberately not joined so its stale contents can never look "effective".
-- CREATE OR REPLACE cannot drop columns, so re-provisioned databases must
-- drop the old 4-column view first (idempotent).
DROP VIEW IF EXISTS public.cache_provider_effective;
CREATE VIEW public.cache_provider_effective AS
 SELECT p.id AS provider_id,
    p.name AS provider_name,
    p.adapter_type,
    (COALESCE(a.config, '{}'::jsonb) || COALESCE(o.config, '{}'::jsonb)) AS effective_config,
    COALESCE(a.config, '{}'::jsonb) AS adapter_config,
    COALESCE(o.config, '{}'::jsonb) AS override_config,
    o.updated_at AS override_updated_at,
    o.updated_by AS override_updated_by
   FROM ((public."Provider" p
     LEFT JOIN public.cache_adapter_config a ON ((a.adapter_type = p.adapter_type)))
     LEFT JOIN public.cache_provider_config o ON ((o.provider_id = p.id)));

-- ── Partial / expression / GIN indexes (Prisma @@index cannot express these) ─
-- Correctness-bearing uniques: thing_type_physical_id_uniq (agent thing.id
-- stability across reinstalls), DeviceAssignment_deviceId_active_uidx (one active
-- assignment per device), exemption_request_pending_dedup_uniq, uq_ops_rollup_*
-- (COALESCE expression-unique, dedups rollups). Remainder are hot-path partial
-- indexes. All idempotent (IF NOT EXISTS).
CREATE INDEX IF NOT EXISTS "AdminApiKey_owner_active_partial_idx" ON public."AdminApiKey" USING btree ("ownerUserId") WHERE (status = 'active'::text);
CREATE INDEX IF NOT EXISTS "RevokedToken_targetDeviceId_idx" ON public."RevokedToken" USING btree ("targetDeviceId") WHERE ("targetDeviceId" IS NOT NULL);
CREATE INDEX IF NOT EXISTS "RevokedToken_targetSessionId_idx" ON public."RevokedToken" USING btree ("targetSessionId") WHERE ("targetSessionId" IS NOT NULL);
CREATE INDEX IF NOT EXISTS "RevokedToken_targetUserId_idx" ON public."RevokedToken" USING btree ("targetUserId") WHERE ("targetUserId" IS NOT NULL);
CREATE INDEX IF NOT EXISTS alert_rule_group_filter_idx ON public."AlertRule" USING btree (group_id_filter) WHERE (group_id_filter IS NOT NULL);
CREATE INDEX IF NOT EXISTS device_group_membership_expires_idx ON public."DeviceGroupMembership" USING btree (expires_at) WHERE (expires_at IS NOT NULL);
CREATE INDEX IF NOT EXISTS diag_silence_active_idx ON public.diag_silence USING btree (expires_at) WHERE ((expires_at IS NULL) OR (expires_at > '2000-01-01 00:00:00+00'::timestamp with time zone));
CREATE INDEX IF NOT EXISTS iam_policy_attachment_expires_idx ON public."IamPolicyAttachment" USING btree (expires_at) WHERE (expires_at IS NOT NULL);
CREATE INDEX IF NOT EXISTS idx_diag_crash_cohort ON public.thing_diag_event USING btree (agent_version, ((os_info ->> 'os'::text)), occurred_at DESC) WHERE (event_type = 'crash'::text);
CREATE INDEX IF NOT EXISTS idx_ops_1d_fleet_time ON public.metric_ops_rollup_1d USING btree (thing_type, bucket_start DESC) WHERE (thing_id IS NULL);
CREATE INDEX IF NOT EXISTS idx_ops_1d_thing_time ON public.metric_ops_rollup_1d USING btree (thing_id, bucket_start DESC) WHERE (thing_id IS NOT NULL);
CREATE INDEX IF NOT EXISTS idx_ops_1h_fleet_time ON public.metric_ops_rollup_1h USING btree (thing_type, bucket_start DESC) WHERE (thing_id IS NULL);
CREATE INDEX IF NOT EXISTS idx_ops_1h_thing_time ON public.metric_ops_rollup_1h USING btree (thing_id, bucket_start DESC) WHERE (thing_id IS NOT NULL);
CREATE INDEX IF NOT EXISTS idx_ops_1mo_fleet_time ON public.metric_ops_rollup_1mo USING btree (thing_type, bucket_start DESC) WHERE (thing_id IS NULL);
CREATE INDEX IF NOT EXISTS idx_ops_1mo_thing_time ON public.metric_ops_rollup_1mo USING btree (thing_id, bucket_start DESC) WHERE (thing_id IS NOT NULL);
CREATE INDEX IF NOT EXISTS idx_ops_5m_fleet_time ON public.metric_ops_rollup_5m USING btree (thing_type, bucket_start DESC) WHERE (thing_id IS NULL);
CREATE INDEX IF NOT EXISTS idx_ops_5m_thing_time ON public.metric_ops_rollup_5m USING btree (thing_id, bucket_start DESC) WHERE (thing_id IS NOT NULL);
CREATE INDEX IF NOT EXISTS idx_tco_expires ON public.thing_config_override USING btree (expires_at) WHERE (expires_at IS NOT NULL);
CREATE INDEX IF NOT EXISTS idx_traffic_event_api_key_fingerprint_timestamp ON public.traffic_event USING btree (api_key_fingerprint, "timestamp") WHERE (api_key_fingerprint IS NOT NULL);
CREATE INDEX IF NOT EXISTS idx_traffic_event_attestation_verified ON public.traffic_event USING btree (created_at DESC) WHERE (attestation_verified = true);
-- The traffic drawer's trace pivot and the admin list's requestId filter both
-- key on trace_id: it is the value the gateway hands back in the
-- X-Nexus-Request-Id response header, so it is the only id an operator or a
-- support ticket ever holds. Without this the filter is a sequential scan over
-- the whole retention window, run twice per page (COUNT + rows), on a table the
-- UI queries with no default time bound. Partial: compliance-proxy passthrough
-- rows carry no trace, and there is no reason to index their NULLs.
CREATE INDEX IF NOT EXISTS idx_traffic_event_trace_id ON public.traffic_event USING btree (trace_id) WHERE (trace_id IS NOT NULL);
CREATE INDEX IF NOT EXISTS thing_primary_ip_idx ON public.thing USING btree (primary_ip) WHERE (primary_ip IS NOT NULL);
CREATE INDEX IF NOT EXISTS thing_tags_gin_idx ON public.thing USING gin (tags);
CREATE INDEX IF NOT EXISTS traffic_event_credential_health_rollup_idx ON public.traffic_event USING btree ("timestamp" DESC, credential_id) WHERE ((source = 'ai-gateway'::text) AND (credential_id IS NOT NULL));
CREATE INDEX IF NOT EXISTS traffic_event_embedding_model_id_idx ON public.traffic_event USING btree (embedding_model_id) WHERE (embedding_model_id IS NOT NULL);
CREATE INDEX IF NOT EXISTS traffic_event_passthrough_active_idx ON public.traffic_event USING btree ("timestamp" DESC) WHERE (passthrough_flags IS NOT NULL);
-- Provider-detail analytics attribute usage off routed_provider_id (the provider
-- that actually served the call, which can differ from the requested provider_id
-- under smart routing). Every provider-detail sub-query filters
-- source='ai-gateway' AND routed_provider_id = $1 [+ a timestamp range]; the
-- requested-side provider_id/provider_name indexes do not cover that predicate,
-- so without this partial the page seq-scans the whole table per sub-query.
CREATE INDEX IF NOT EXISTS traffic_event_routed_provider_ts_idx ON public.traffic_event USING btree (routed_provider_id, "timestamp" DESC) WHERE ((source = 'ai-gateway'::text) AND (routed_provider_id IS NOT NULL));
CREATE UNIQUE INDEX IF NOT EXISTS "DeviceAssignment_deviceId_active_uidx" ON public."DeviceAssignment" USING btree ("deviceId") WHERE ("releasedAt" IS NULL);
-- Virtual-key name uniqueness is scoped by vkType: application keys share one
-- fleet-wide admin namespace, while personal keys are self-service and only
-- collide within their owner (user A's "test" must not block user B's).
-- Handlers map violations of these two indexes to 409 duplicate_name.
CREATE UNIQUE INDEX IF NOT EXISTS "VirtualKey_application_name_uniq" ON public."VirtualKey" USING btree (name) WHERE ("vkType" = 'application'::text);
CREATE UNIQUE INDEX IF NOT EXISTS "VirtualKey_personal_owner_name_uniq" ON public."VirtualKey" USING btree ("ownerId", name) WHERE ("vkType" = 'personal'::text);
CREATE UNIQUE INDEX IF NOT EXISTS exemption_request_pending_dedup_uniq ON public.exemption_request USING btree (target_host, requested_by) WHERE (status = 'PENDING'::public."ExemptionRequestStatus");
CREATE UNIQUE INDEX IF NOT EXISTS thing_type_physical_id_uniq ON public.thing USING btree (type, physical_id) WHERE ((type = 'agent'::text) AND (physical_id IS NOT NULL));
CREATE UNIQUE INDEX IF NOT EXISTS uq_ops_rollup_1d ON public.metric_ops_rollup_1d USING btree (bucket_start, COALESCE(thing_id, ''::text), metric_name, dimension_key);
CREATE UNIQUE INDEX IF NOT EXISTS uq_ops_rollup_1h ON public.metric_ops_rollup_1h USING btree (bucket_start, COALESCE(thing_id, ''::text), metric_name, dimension_key);
CREATE UNIQUE INDEX IF NOT EXISTS uq_ops_rollup_1mo ON public.metric_ops_rollup_1mo USING btree (bucket_start, COALESCE(thing_id, ''::text), metric_name, dimension_key);
CREATE UNIQUE INDEX IF NOT EXISTS uq_ops_rollup_5m ON public.metric_ops_rollup_5m USING btree (bucket_start, COALESCE(thing_id, ''::text), metric_name, dimension_key);
-- Error-governance aggregation (GET /api/admin/traffic/errors/groups) filters
-- every query on status_code >= 400 over a timestamp window. No full index
-- covers status_code, so without this partial the group + bucket passes
-- heap-fetch every row in the window and discard the ~95-99% that succeeded.
-- Errors-only partial keeps both passes proportional to failed traffic and
-- adds write cost only on error-row inserts.
CREATE INDEX IF NOT EXISTS traffic_event_error_ts_idx ON public.traffic_event USING btree ("timestamp") WHERE (status_code >= 400);
-- The rollup-first error-governance read enriches each displayed error class
-- with a LIMIT-1 sample reason and a distinct end-user count, both scoped to
-- one class via the exact COALESCE key expressions below. The timestamp-only
-- partial above leaves those class predicates as heap filters (every
-- enrichment query would re-visit every error row in the window); this
-- expression partial makes both point queries walk only the class's own rows
-- (equality on the three class expressions + trailing timestamp keeps
-- ORDER BY timestamp DESC LIMIT 1 an index walk). Expressions must stay
-- byte-identical to errorClassWhere in the Control Plane trafficstore.
CREATE INDEX IF NOT EXISTS traffic_event_error_class_idx ON public.traffic_event USING btree
  ((COALESCE(error_code, ''::text)),
   (COALESCE(routed_provider_name, provider_name, ''::text)),
   (COALESCE(routed_model_name, model_name, ''::text)),
   "timestamp")
  WHERE (status_code >= 400);
