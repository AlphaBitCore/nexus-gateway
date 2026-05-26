--
-- PostgreSQL database dump
--

-- Dumped from database version 15.16
-- Dumped by pg_dump version 15.16
--
-- Cleaned by tools/db-migrate/seed/data/clean-schema.py:
--   * Stripped \restrict / \unrestrict PG15 client directives.
--   * Stripped pg_dump's session-state SETs (notably set_config('search_path', ''))
--     so they don't bleed into Prisma's post-migration bookkeeping connection.
--   * Stripped the _prisma_migrations table — Prisma manages it itself.
--
-- check_function_bodies = off defers SQL-function body validation until call
-- time, so `cache_key_source` (which references "Provider") can be created
-- before "Provider" exists later in this file. Required to preserve pg_dump's
-- "all functions first, then tables" ordering.
SET check_function_bodies = false;

--
-- Name: AgentExemptionSource; Type: TYPE; Schema: public; Owner: -
--

CREATE TYPE public."AgentExemptionSource" AS ENUM (
    'AUTO',
    'ADMIN'
);

--
-- Name: AlertSeverity; Type: TYPE; Schema: public; Owner: -
--

CREATE TYPE public."AlertSeverity" AS ENUM (
    'CRITICAL',
    'HIGH',
    'MEDIUM',
    'LOW',
    'INFO'
);

--
-- Name: AlertState; Type: TYPE; Schema: public; Owner: -
--

CREATE TYPE public."AlertState" AS ENUM (
    'FIRING',
    'ACKNOWLEDGED',
    'RESOLVED'
);

--
-- Name: DSARRequestStatus; Type: TYPE; Schema: public; Owner: -
--

CREATE TYPE public."DSARRequestStatus" AS ENUM (
    'PENDING',
    'IN_PROGRESS',
    'COMPLETED',
    'REJECTED'
);

--
-- Name: DSARRequestType; Type: TYPE; Schema: public; Owner: -
--

CREATE TYPE public."DSARRequestType" AS ENUM (
    'ACCESS',
    'ERASURE'
);

--
-- Name: DefaultPathAction; Type: TYPE; Schema: public; Owner: -
--

CREATE TYPE public."DefaultPathAction" AS ENUM (
    'PROCESS',
    'PASSTHROUGH',
    'BLOCK'
);

--
-- Name: ExemptionRequestStatus; Type: TYPE; Schema: public; Owner: -
--

CREATE TYPE public."ExemptionRequestStatus" AS ENUM (
    'PENDING',
    'APPROVED',
    'REJECTED'
);

--
-- Name: FailureAction; Type: TYPE; Schema: public; Owner: -
--

CREATE TYPE public."FailureAction" AS ENUM (
    'FAIL_OPEN',
    'FAIL_CLOSED'
);

--
-- Name: HostMatchType; Type: TYPE; Schema: public; Owner: -
--

CREATE TYPE public."HostMatchType" AS ENUM (
    'EXACT',
    'PREFIX',
    'GLOB',
    'REGEX'
);

--
-- Name: NetworkZone; Type: TYPE; Schema: public; Owner: -
--

CREATE TYPE public."NetworkZone" AS ENUM (
    'PUBLIC',
    'INTERNAL'
);

--
-- Name: PathAction; Type: TYPE; Schema: public; Owner: -
--

CREATE TYPE public."PathAction" AS ENUM (
    'PROCESS',
    'PASSTHROUGH',
    'BLOCK'
);

--
-- Name: PathMatchType; Type: TYPE; Schema: public; Owner: -
--

CREATE TYPE public."PathMatchType" AS ENUM (
    'EXACT',
    'PREFIX',
    'GLOB',
    'REGEX'
);

--
-- Name: cache_key_source(text, text); Type: FUNCTION; Schema: public; Owner: -
--

CREATE FUNCTION public.cache_key_source(p_provider_id text, p_key text) RETURNS text
    LANGUAGE sql STABLE
    AS $$
  SELECT CASE
    WHEN o."config" ? p_key THEN 'provider-override'
    WHEN a."config" ? p_key THEN 'adapter-default'
    WHEN g."config" ? p_key THEN 'global-default'
    ELSE 'code-default'
  END
  FROM "Provider" p
  LEFT JOIN "cache_global_config"   g ON g."id" = 'singleton'
  LEFT JOIN "cache_adapter_config"  a ON a."adapter_type" = p."adapter_type"
  LEFT JOIN "cache_provider_config" o ON o."provider_id" = p."id"
  WHERE p."id" = p_provider_id;
$$;

SET default_tablespace = '';

SET default_table_access_method = heap;

--
-- Name: AdminApiKey; Type: TABLE; Schema: public; Owner: -
--

CREATE TABLE public."AdminApiKey" (
    id text NOT NULL,
    name text NOT NULL,
    "keyHash" text NOT NULL,
    "keyPrefix" text NOT NULL,
    "createdBy" text NOT NULL,
    enabled boolean DEFAULT true NOT NULL,
    "lastUsedAt" timestamp(3) with time zone,
    "expiresAt" timestamp(3) with time zone,
    "ownerUserId" text,
    "createdAt" timestamp(3) with time zone DEFAULT CURRENT_TIMESTAMP NOT NULL,
    "updatedAt" timestamp(3) with time zone NOT NULL
);

--
-- Name: AdminAuditLog; Type: TABLE; Schema: public; Owner: -
--

CREATE TABLE public."AdminAuditLog" (
    id text NOT NULL,
    "sequenceNumber" integer NOT NULL,
    "timestamp" timestamp(3) with time zone DEFAULT CURRENT_TIMESTAMP NOT NULL,
    "actorId" text NOT NULL,
    "actorLabel" text NOT NULL,
    "actorRole" text,
    "sourceIp" text,
    action text NOT NULL,
    "entityType" text NOT NULL,
    "entityId" text,
    "beforeState" jsonb,
    "afterState" jsonb,
    "nexusRequestId" text,
    "clientRequestId" text,
    "clientUserId" text,
    "clientSessionId" text,
    "previousHash" text,
    "integrityHash" text NOT NULL,
    "hashInput" bytea NOT NULL
);

--
-- Name: AdminAuditLog_sequenceNumber_seq; Type: SEQUENCE; Schema: public; Owner: -
--

CREATE SEQUENCE public."AdminAuditLog_sequenceNumber_seq"
    AS integer
    START WITH 1
    INCREMENT BY 1
    NO MINVALUE
    NO MAXVALUE
    CACHE 1;

--
-- Name: AdminAuditLog_sequenceNumber_seq; Type: SEQUENCE OWNED BY; Schema: public; Owner: -
--

ALTER SEQUENCE public."AdminAuditLog_sequenceNumber_seq" OWNED BY public."AdminAuditLog"."sequenceNumber";

--
-- Name: AgentExemption; Type: TABLE; Schema: public; Owner: -
--

CREATE TABLE public."AgentExemption" (
    id text NOT NULL,
    host text NOT NULL,
    reason text NOT NULL,
    source public."AgentExemptionSource" NOT NULL,
    status text DEFAULT 'pending'::text NOT NULL,
    scope text NOT NULL,
    "deviceId" text,
    "groupId" text,
    "expiresAt" timestamp(3) with time zone,
    denylist boolean DEFAULT false NOT NULL,
    "createdBy" text,
    "createdAt" timestamp(3) with time zone DEFAULT CURRENT_TIMESTAMP NOT NULL,
    "updatedAt" timestamp(3) with time zone NOT NULL
);

--
-- Name: Alert; Type: TABLE; Schema: public; Owner: -
--

CREATE TABLE public."Alert" (
    id text NOT NULL,
    "ruleId" text NOT NULL,
    "sourceType" text NOT NULL,
    "targetKey" text NOT NULL,
    "targetLabel" text NOT NULL,
    severity public."AlertSeverity" NOT NULL,
    state public."AlertState" DEFAULT 'FIRING'::public."AlertState" NOT NULL,
    message text NOT NULL,
    details jsonb NOT NULL,
    "firedAt" timestamp(3) with time zone NOT NULL,
    "lastSeenAt" timestamp(3) with time zone NOT NULL,
    "duplicateCount" integer DEFAULT 1 NOT NULL,
    "acknowledgedBy" text,
    "acknowledgedAt" timestamp(3) with time zone,
    "resolvedAt" timestamp(3) with time zone,
    "resolvedBy" text,
    "resolvedReason" text
);

--
-- Name: AlertChannel; Type: TABLE; Schema: public; Owner: -
--

CREATE TABLE public."AlertChannel" (
    id text NOT NULL,
    name text NOT NULL,
    type text NOT NULL,
    enabled boolean DEFAULT true NOT NULL,
    severities text[],
    "sourceTypes" text[],
    config jsonb NOT NULL,
    "createdAt" timestamp(3) with time zone DEFAULT CURRENT_TIMESTAMP NOT NULL,
    "updatedAt" timestamp(3) with time zone NOT NULL
);

--
-- Name: AlertDispatch; Type: TABLE; Schema: public; Owner: -
--

CREATE TABLE public."AlertDispatch" (
    id text NOT NULL,
    "alertId" text NOT NULL,
    "channelId" text NOT NULL,
    "channelName" text NOT NULL,
    success boolean NOT NULL,
    "statusCode" integer,
    "errorMsg" text,
    "attemptedAt" timestamp(3) with time zone DEFAULT CURRENT_TIMESTAMP NOT NULL
);

--
-- Name: AlertRule; Type: TABLE; Schema: public; Owner: -
--

CREATE TABLE public."AlertRule" (
    id text NOT NULL,
    "displayName" text NOT NULL,
    "sourceType" text NOT NULL,
    "defaultSeverity" public."AlertSeverity" NOT NULL,
    "requiresAck" boolean DEFAULT false NOT NULL,
    enabled boolean DEFAULT true NOT NULL,
    params jsonb NOT NULL,
    "paramsSchema" jsonb NOT NULL,
    "cooldownSec" integer DEFAULT 300 NOT NULL,
    "createdAt" timestamp(3) with time zone DEFAULT CURRENT_TIMESTAMP NOT NULL,
    "updatedAt" timestamp(3) with time zone NOT NULL
);

--
-- Name: Credential; Type: TABLE; Schema: public; Owner: -
--

CREATE TABLE public."Credential" (
    id text NOT NULL,
    name text NOT NULL,
    "providerId" text NOT NULL,
    "encryptedKey" text NOT NULL,
    "encryptionIv" text NOT NULL,
    "encryptionTag" text NOT NULL,
    encryption_key_id text DEFAULT 'v1'::text NOT NULL,
    enabled boolean DEFAULT true NOT NULL,
    "rotationState" text DEFAULT 'none'::text,
    "lastRotatedAt" timestamp(3) with time zone,
    "rotationStartedAt" timestamp(3) with time zone,
    "lastUsedAt" timestamp(3) with time zone,
    "lastSuccessAt" timestamp(3) with time zone,
    "lastFailureAt" timestamp(3) with time zone,
    "lastFailureReason" text,
    "totalUsageCount" integer DEFAULT 0 NOT NULL,
    "createdAt" timestamp(3) with time zone DEFAULT CURRENT_TIMESTAMP NOT NULL,
    "updatedAt" timestamp(3) with time zone NOT NULL,
    "expiresAt" timestamp(3) with time zone,
    "selectionWeight" integer DEFAULT 100 NOT NULL,
    status text DEFAULT 'active'::text NOT NULL,
    "retireAt" timestamp(3) with time zone,
    "circuitState" text DEFAULT 'closed'::text NOT NULL,
    "circuitReason" text,
    "circuitOpenedAt" timestamp(3) with time zone,
    "circuitNextProbeAt" timestamp(3) with time zone,
    "healthStatus" text DEFAULT 'unknown'::text NOT NULL,
    "healthSuccessRate5m" numeric(5,4),
    "healthSamplesObserved" integer DEFAULT 0 NOT NULL,
    "healthCheckedAt" timestamp(3) with time zone,
    "healthSuccessRate1h" numeric(5,4),
    "healthDominantError" text,
    "healthTrend" text,
    "healthStatusChangedAt" timestamp(3) with time zone,
    "reliabilityOverrides" jsonb
);

--
-- Name: DeviceAssignment; Type: TABLE; Schema: public; Owner: -
--

CREATE TABLE public."DeviceAssignment" (
    id text NOT NULL,
    "deviceId" text NOT NULL,
    "userId" text,
    "assignedAt" timestamp(3) with time zone DEFAULT CURRENT_TIMESTAMP NOT NULL,
    "releasedAt" timestamp(3) with time zone,
    source text DEFAULT 'heartbeat'::text NOT NULL,
    ip_address text,
    login_method text,
    token_jti text
);

--
-- Name: DeviceGroup; Type: TABLE; Schema: public; Owner: -
--

CREATE TABLE public."DeviceGroup" (
    id text NOT NULL,
    name text NOT NULL,
    description text,
    "createdBy" text,
    "createdAt" timestamp(3) with time zone DEFAULT CURRENT_TIMESTAMP NOT NULL,
    "updatedAt" timestamp(3) with time zone NOT NULL
);

--
-- Name: DeviceGroupMembership; Type: TABLE; Schema: public; Owner: -
--

CREATE TABLE public."DeviceGroupMembership" (
    id text NOT NULL,
    "groupId" text NOT NULL,
    "deviceId" text NOT NULL,
    "createdAt" timestamp(3) with time zone DEFAULT CURRENT_TIMESTAMP NOT NULL
);

--
-- Name: DeviceGroupPolicyRule; Type: TABLE; Schema: public; Owner: -
--

CREATE TABLE public."DeviceGroupPolicyRule" (
    id text NOT NULL,
    "groupId" text NOT NULL,
    domain text NOT NULL,
    action text NOT NULL,
    priority integer DEFAULT 100 NOT NULL,
    "createdAt" timestamp(3) with time zone DEFAULT CURRENT_TIMESTAMP NOT NULL
);

--
-- Name: HookConfig; Type: TABLE; Schema: public; Owner: -
--

CREATE TABLE public."HookConfig" (
    id text NOT NULL,
    name text NOT NULL,
    type text NOT NULL,
    "implementationId" text DEFAULT 'noop'::text NOT NULL,
    stage text DEFAULT 'request'::text NOT NULL,
    category text,
    endpoint text,
    script text,
    config jsonb,
    priority integer DEFAULT 0 NOT NULL,
    "timeoutMs" integer DEFAULT 5000 NOT NULL,
    "failBehavior" text DEFAULT 'fail-open'::text NOT NULL,
    enabled boolean DEFAULT true NOT NULL,
    "applicableIngress" text[] DEFAULT ARRAY['ALL'::text],
    "createdAt" timestamp(3) with time zone DEFAULT CURRENT_TIMESTAMP NOT NULL,
    "updatedAt" timestamp(3) with time zone NOT NULL
);

--
-- Name: IamGroup; Type: TABLE; Schema: public; Owner: -
--

CREATE TABLE public."IamGroup" (
    id text NOT NULL,
    name text NOT NULL,
    description text,
    "createdBy" text,
    "createdAt" timestamp(3) with time zone DEFAULT CURRENT_TIMESTAMP NOT NULL,
    "updatedAt" timestamp(3) with time zone NOT NULL,
    idp_group_name text,
    identity_provider_id uuid,
    source text DEFAULT 'local'::text NOT NULL
);

--
-- Name: IamGroupMembership; Type: TABLE; Schema: public; Owner: -
--

CREATE TABLE public."IamGroupMembership" (
    id text NOT NULL,
    "groupId" text NOT NULL,
    "principalType" text NOT NULL,
    "principalId" text NOT NULL,
    "createdAt" timestamp(3) with time zone DEFAULT CURRENT_TIMESTAMP NOT NULL
);

--
-- Name: IamGroupPolicyAttachment; Type: TABLE; Schema: public; Owner: -
--

CREATE TABLE public."IamGroupPolicyAttachment" (
    id text NOT NULL,
    "groupId" text NOT NULL,
    "policyId" text NOT NULL,
    "createdAt" timestamp(3) with time zone DEFAULT CURRENT_TIMESTAMP NOT NULL
);

--
-- Name: IamPolicy; Type: TABLE; Schema: public; Owner: -
--

CREATE TABLE public."IamPolicy" (
    id text NOT NULL,
    name text NOT NULL,
    description text,
    type text DEFAULT 'custom'::text NOT NULL,
    document jsonb NOT NULL,
    enabled boolean DEFAULT true NOT NULL,
    "createdBy" text,
    "createdAt" timestamp(3) with time zone DEFAULT CURRENT_TIMESTAMP NOT NULL,
    "updatedAt" timestamp(3) with time zone NOT NULL
);

--
-- Name: IamPolicyAttachment; Type: TABLE; Schema: public; Owner: -
--

CREATE TABLE public."IamPolicyAttachment" (
    id text NOT NULL,
    "principalType" text NOT NULL,
    "principalId" text NOT NULL,
    "policyId" text NOT NULL,
    "createdAt" timestamp(3) with time zone DEFAULT CURRENT_TIMESTAMP NOT NULL
);

--
-- Name: IdentityProvider; Type: TABLE; Schema: public; Owner: -
--

CREATE TABLE public."IdentityProvider" (
    id uuid NOT NULL,
    type text NOT NULL,
    name text NOT NULL,
    enabled boolean DEFAULT true NOT NULL,
    config jsonb DEFAULT '{}'::jsonb NOT NULL,
    "roleMapping" jsonb DEFAULT '[]'::jsonb NOT NULL,
    "defaultRole" text DEFAULT 'developer'::text NOT NULL,
    "jitEnabled" boolean DEFAULT true NOT NULL,
    "createdAt" timestamp(3) with time zone DEFAULT CURRENT_TIMESTAMP NOT NULL,
    "updatedAt" timestamp(3) with time zone NOT NULL,
    CONSTRAINT "IdentityProvider_type_check" CHECK ((type = ANY (ARRAY['local'::text, 'oidc'::text, 'saml'::text])))
);

--
-- Name: IdpGroupMapping; Type: TABLE; Schema: public; Owner: -
--

CREATE TABLE public."IdpGroupMapping" (
    id uuid NOT NULL,
    "identityProviderId" uuid NOT NULL,
    "externalGroupId" text NOT NULL,
    "externalGroupName" text,
    "iamGroupId" text NOT NULL,
    "createdAt" timestamp(3) with time zone DEFAULT CURRENT_TIMESTAMP NOT NULL
);

--
-- Name: MetricRollup; Type: TABLE; Schema: public; Owner: -
--

CREATE TABLE public."MetricRollup" (
    id text NOT NULL,
    "bucketStart" timestamp(3) with time zone NOT NULL,
    "metricName" text NOT NULL,
    "dimensionKey" text DEFAULT ''::text NOT NULL,
    dimensions jsonb DEFAULT '{}'::jsonb NOT NULL,
    value numeric(24,6) DEFAULT 0 NOT NULL,
    "createdAt" timestamp(3) with time zone DEFAULT CURRENT_TIMESTAMP NOT NULL,
    "updatedAt" timestamp(3) with time zone NOT NULL
);

--
-- Name: Model; Type: TABLE; Schema: public; Owner: -
--

CREATE TABLE public."Model" (
    id text NOT NULL,
    name text NOT NULL,
    description text,
    "providerId" text NOT NULL,
    "providerModelId" text NOT NULL,
    type text NOT NULL,
    features text[],
    "inputPricePerMillion" numeric(65,30),
    "outputPricePerMillion" numeric(65,30),
    "maxContextTokens" integer,
    "maxOutputTokens" integer,
    status text DEFAULT 'active'::text NOT NULL,
    "deprecationDate" timestamp(3) with time zone,
    "replacedBy" text,
    aliases text[],
    enabled boolean DEFAULT true NOT NULL,
    "createdAt" timestamp(3) with time zone DEFAULT CURRENT_TIMESTAMP NOT NULL,
    "updatedAt" timestamp(3) with time zone NOT NULL,
    code text NOT NULL
);

--
-- Name: ModelPricing; Type: TABLE; Schema: public; Owner: -
--

CREATE TABLE public."ModelPricing" (
    id text NOT NULL,
    "modelId" text NOT NULL,
    "inputPricePerMillion" numeric(65,30) NOT NULL,
    "outputPricePerMillion" numeric(65,30) NOT NULL,
    "effectiveDate" timestamp(3) with time zone DEFAULT CURRENT_TIMESTAMP NOT NULL,
    "updatedBy" text,
    "createdAt" timestamp(3) with time zone DEFAULT CURRENT_TIMESTAMP NOT NULL,
    "updatedAt" timestamp(3) with time zone NOT NULL
);

--
-- Name: NexusUser; Type: TABLE; Schema: public; Owner: -
--

CREATE TABLE public."NexusUser" (
    id text NOT NULL,
    "organizationId" text DEFAULT 'default'::text NOT NULL,
    "displayName" text NOT NULL,
    email text,
    status text DEFAULT 'active'::text NOT NULL,
    "canAccessControlPlane" boolean DEFAULT false NOT NULL,
    "osUsername" text,
    "osDomain" text,
    "passwordHash" text,
    "lastLoginAt" timestamp(3) with time zone,
    "createdBy" text,
    "createdAt" timestamp(3) with time zone DEFAULT CURRENT_TIMESTAMP NOT NULL,
    "updatedAt" timestamp(3) with time zone NOT NULL,
    "passwordUpdatedAt" timestamp(3) with time zone,
    "breakGlass" boolean DEFAULT false NOT NULL,
    "disabledReason" text,
    "disabledAt" timestamp(3) with time zone,
    "preferredTimezone" text,
    source text DEFAULT 'local'::text NOT NULL
);

--
-- Name: OAuthClient; Type: TABLE; Schema: public; Owner: -
--

CREATE TABLE public."OAuthClient" (
    id text NOT NULL,
    name text NOT NULL,
    type text NOT NULL,
    "redirectUris" text[],
    "allowedScopes" text[],
    "requirePkce" boolean DEFAULT true NOT NULL,
    "accessTtlSeconds" integer DEFAULT 3600 NOT NULL,
    "refreshTtlSeconds" integer DEFAULT 86400 NOT NULL,
    "clientSecretHash" text,
    "createdAt" timestamp(3) with time zone DEFAULT CURRENT_TIMESTAMP NOT NULL,
    "updatedAt" timestamp(3) with time zone NOT NULL,
    CONSTRAINT "OAuthClient_type_check" CHECK ((type = ANY (ARRAY['public'::text, 'confidential'::text])))
);

--
-- Name: Organization; Type: TABLE; Schema: public; Owner: -
--

CREATE TABLE public."Organization" (
    id text NOT NULL,
    name text NOT NULL,
    code text NOT NULL,
    "parentId" text,
    description text,
    "contactName" text,
    "contactEmail" text,
    "contactPhone" text,
    enabled boolean DEFAULT true NOT NULL,
    "createdAt" timestamp(3) with time zone DEFAULT CURRENT_TIMESTAMP NOT NULL,
    "updatedAt" timestamp(3) with time zone NOT NULL,
    timezone text DEFAULT 'UTC'::text NOT NULL,
    path text NOT NULL,
    "externalGroupId" text,
    source text DEFAULT 'local'::text NOT NULL
);

--
-- Name: Project; Type: TABLE; Schema: public; Owner: -
--

CREATE TABLE public."Project" (
    id text NOT NULL,
    name text NOT NULL,
    code text NOT NULL,
    "organizationId" text NOT NULL,
    description text,
    "contactName" text,
    "contactEmail" text,
    status text DEFAULT 'active'::text NOT NULL,
    "createdAt" timestamp(3) with time zone DEFAULT CURRENT_TIMESTAMP NOT NULL,
    "updatedAt" timestamp(3) with time zone NOT NULL
);

--
-- Name: Provider; Type: TABLE; Schema: public; Owner: -
--

CREATE TABLE public."Provider" (
    id text NOT NULL,
    name text NOT NULL,
    "displayName" text,
    description text,
    "baseUrl" text NOT NULL,
    "pathPrefix" text NOT NULL,
    "apiVersion" text,
    enabled boolean DEFAULT true NOT NULL,
    headers jsonb,
    "createdAt" timestamp(3) with time zone DEFAULT CURRENT_TIMESTAMP NOT NULL,
    "updatedAt" timestamp(3) with time zone NOT NULL,
    region text,
    adapter_type text NOT NULL,
    streaming_mode text,
    streaming_chunk_bytes integer,
    streaming_hook_timeout_ms integer,
    streaming_max_buffer_bytes integer,
    streaming_fail_behavior text,
    capture_request_body boolean,
    capture_response_body boolean,
    raw_body_spill_enabled boolean
);

--
-- Name: ProviderHealth; Type: TABLE; Schema: public; Owner: -
--

CREATE TABLE public."ProviderHealth" (
    id text NOT NULL,
    "providerId" text NOT NULL,
    provider text NOT NULL,
    status text DEFAULT 'healthy'::text NOT NULL,
    "rollingErrorRate" double precision DEFAULT 0 NOT NULL,
    "avgLatencyMs" integer DEFAULT 0 NOT NULL,
    "lastRequestAt" timestamp(3) with time zone,
    "lastErrorAt" timestamp(3) with time zone,
    "windowStart" timestamp(3) with time zone DEFAULT CURRENT_TIMESTAMP NOT NULL,
    "sampleCount" integer DEFAULT 0 NOT NULL,
    "createdAt" timestamp(3) with time zone DEFAULT CURRENT_TIMESTAMP NOT NULL,
    "updatedAt" timestamp(3) with time zone NOT NULL
);

--
-- Name: QuotaOverride; Type: TABLE; Schema: public; Owner: -
--

CREATE TABLE public."QuotaOverride" (
    id text NOT NULL,
    "targetType" text NOT NULL,
    "targetId" text NOT NULL,
    reason text,
    "costLimitUsd" numeric(24,6),
    "tokenLimit" bigint,
    "enforcementMode" text,
    "periodType" text,
    "createdBy" text,
    "createdAt" timestamp(3) with time zone DEFAULT CURRENT_TIMESTAMP NOT NULL,
    "updatedAt" timestamp(3) with time zone NOT NULL
);

--
-- Name: QuotaPolicy; Type: TABLE; Schema: public; Owner: -
--

CREATE TABLE public."QuotaPolicy" (
    id text NOT NULL,
    name text NOT NULL,
    description text,
    scope text NOT NULL,
    "organizationId" text,
    "vkType" text,
    "periodType" text NOT NULL,
    "costLimitUsd" numeric(24,6),
    "tokenLimit" bigint,
    "enforcementMode" text DEFAULT 'reject'::text NOT NULL,
    "alertThresholds" jsonb DEFAULT '[80, 90]'::jsonb NOT NULL,
    priority integer DEFAULT 0 NOT NULL,
    enabled boolean DEFAULT true NOT NULL,
    "createdBy" text,
    "createdAt" timestamp(3) with time zone DEFAULT CURRENT_TIMESTAMP NOT NULL,
    "updatedAt" timestamp(3) with time zone NOT NULL
);

--
-- Name: RefreshToken; Type: TABLE; Schema: public; Owner: -
--

CREATE TABLE public."RefreshToken" (
    jti uuid NOT NULL,
    "sessionId" uuid NOT NULL,
    "parentJti" uuid,
    "userId" text NOT NULL,
    "clientId" text NOT NULL,
    "deviceId" text,
    "tokenHash" bytea NOT NULL,
    "usedAt" timestamp(3) with time zone,
    "expiresAt" timestamp(3) with time zone NOT NULL,
    "createdAt" timestamp(3) with time zone DEFAULT CURRENT_TIMESTAMP NOT NULL
);

--
-- Name: RevokedToken; Type: TABLE; Schema: public; Owner: -
--

CREATE TABLE public."RevokedToken" (
    id bigint NOT NULL,
    scope text NOT NULL,
    "targetJti" uuid,
    "targetUserId" text,
    "targetDeviceId" text,
    "targetSessionId" uuid,
    "revokedAt" timestamp(3) with time zone DEFAULT CURRENT_TIMESTAMP NOT NULL,
    "expiresAt" timestamp(3) with time zone NOT NULL,
    reason text NOT NULL,
    actor text,
    CONSTRAINT "RevokedToken_scope_check" CHECK ((scope = ANY (ARRAY['jti'::text, 'user'::text, 'device'::text, 'session'::text])))
);

--
-- Name: RevokedToken_id_seq; Type: SEQUENCE; Schema: public; Owner: -
--

CREATE SEQUENCE public."RevokedToken_id_seq"
    START WITH 1
    INCREMENT BY 1
    NO MINVALUE
    NO MAXVALUE
    CACHE 1;

--
-- Name: RevokedToken_id_seq; Type: SEQUENCE OWNED BY; Schema: public; Owner: -
--

ALTER SEQUENCE public."RevokedToken_id_seq" OWNED BY public."RevokedToken".id;

--
-- Name: RoutingRule; Type: TABLE; Schema: public; Owner: -
--

CREATE TABLE public."RoutingRule" (
    id text NOT NULL,
    name text NOT NULL,
    description text,
    "strategyType" text NOT NULL,
    config jsonb NOT NULL,
    "matchConditions" jsonb,
    priority integer DEFAULT 0 NOT NULL,
    "pipelineStage" integer DEFAULT 1 NOT NULL,
    "fallbackChain" jsonb,
    enabled boolean DEFAULT true NOT NULL,
    "createdAt" timestamp(3) with time zone DEFAULT CURRENT_TIMESTAMP NOT NULL,
    "updatedAt" timestamp(3) with time zone NOT NULL,
    "retryPolicy" jsonb
);

--
-- Name: ScimToken; Type: TABLE; Schema: public; Owner: -
--

CREATE TABLE public."ScimToken" (
    id uuid NOT NULL,
    name text NOT NULL,
    "tokenHash" text NOT NULL,
    "tokenPrefix" text NOT NULL,
    "identityProviderId" uuid,
    "createdBy" text NOT NULL,
    "createdAt" timestamp(3) with time zone DEFAULT CURRENT_TIMESTAMP NOT NULL,
    "lastUsedAt" timestamp(3) with time zone,
    "revokedAt" timestamp(3) with time zone
);

--
-- Name: UserFederatedIdentity; Type: TABLE; Schema: public; Owner: -
--

CREATE TABLE public."UserFederatedIdentity" (
    id uuid NOT NULL,
    "userId" text NOT NULL,
    "idpId" uuid NOT NULL,
    "externalSubject" text NOT NULL,
    "externalEmail" text,
    "rawClaims" jsonb,
    "linkedAt" timestamp(3) with time zone DEFAULT CURRENT_TIMESTAMP NOT NULL,
    "lastLoginAt" timestamp(3) with time zone
);

--
-- Name: VirtualKey; Type: TABLE; Schema: public; Owner: -
--

CREATE TABLE public."VirtualKey" (
    id text NOT NULL,
    name text NOT NULL,
    "keyHash" text,
    "keyPrefix" text,
    "projectId" text,
    "sourceApp" text,
    enabled boolean DEFAULT true NOT NULL,
    "expiresAt" timestamp(3) with time zone,
    "rateLimitRpm" integer,
    "budgetLimitUsd" numeric(65,30),
    "allowedModels" jsonb DEFAULT '[]'::jsonb NOT NULL,
    "ownerId" text,
    "createdBy" text,
    "createdAt" timestamp(3) with time zone DEFAULT CURRENT_TIMESTAMP NOT NULL,
    "updatedAt" timestamp(3) with time zone NOT NULL,
    "vkType" text DEFAULT 'personal'::text NOT NULL,
    "vkStatus" text DEFAULT 'active'::text NOT NULL,
    "approvedBy" text,
    "approvedAt" timestamp(3) with time zone,
    "rejectedBy" text,
    "rejectedAt" timestamp(3) with time zone,
    "rejectReason" text
);

--
-- Name: ai_guard_config; Type: TABLE; Schema: public; Owner: -
--

CREATE TABLE public.ai_guard_config (
    id text DEFAULT 'singleton'::text NOT NULL,
    backend_mode text DEFAULT 'configured_provider'::text NOT NULL,
    provider_id text,
    model_id text,
    external_url text,
    external_credential_id text,
    custom_headers jsonb,
    prompt_template text NOT NULL,
    timeout_ms integer DEFAULT 30000 NOT NULL,
    cache_ttl_seconds integer DEFAULT 600 NOT NULL,
    backend_fingerprint text DEFAULT ''::text NOT NULL,
    updated_at timestamp(3) with time zone DEFAULT CURRENT_TIMESTAMP NOT NULL
);

--
-- Name: cache_adapter_config; Type: TABLE; Schema: public; Owner: -
--

CREATE TABLE public.cache_adapter_config (
    adapter_type text NOT NULL,
    config jsonb DEFAULT '{}'::jsonb NOT NULL,
    updated_at timestamp(3) with time zone DEFAULT CURRENT_TIMESTAMP NOT NULL,
    updated_by text
);

--
-- Name: cache_global_config; Type: TABLE; Schema: public; Owner: -
--

CREATE TABLE public.cache_global_config (
    id text DEFAULT 'singleton'::text NOT NULL,
    config jsonb DEFAULT '{}'::jsonb NOT NULL,
    updated_at timestamp(3) with time zone DEFAULT CURRENT_TIMESTAMP NOT NULL,
    updated_by text,
    CONSTRAINT cache_global_config_singleton_check CHECK ((id = 'singleton'::text))
);

--
-- Name: cache_provider_config; Type: TABLE; Schema: public; Owner: -
--

CREATE TABLE public.cache_provider_config (
    provider_id text NOT NULL,
    config jsonb DEFAULT '{}'::jsonb NOT NULL,
    updated_at timestamp(3) with time zone DEFAULT CURRENT_TIMESTAMP NOT NULL,
    updated_by text
);

--
-- Name: cache_provider_effective; Type: VIEW; Schema: public; Owner: -
--

CREATE VIEW public.cache_provider_effective AS
 SELECT p.id AS provider_id,
    p.name AS provider_name,
    p.adapter_type,
    ((COALESCE(g.config, '{}'::jsonb) || COALESCE(a.config, '{}'::jsonb)) || COALESCE(o.config, '{}'::jsonb)) AS effective_config,
    COALESCE(g.config, '{}'::jsonb) AS global_config,
    COALESCE(a.config, '{}'::jsonb) AS adapter_config,
    COALESCE(o.config, '{}'::jsonb) AS override_config,
    o.updated_at AS override_updated_at,
    o.updated_by AS override_updated_by
   FROM (((public."Provider" p
     LEFT JOIN public.cache_global_config g ON ((g.id = 'singleton'::text)))
     LEFT JOIN public.cache_adapter_config a ON ((a.adapter_type = p.adapter_type)))
     LEFT JOIN public.cache_provider_config o ON ((o.provider_id = p.id)));

--
-- Name: compliance_exemption_grant; Type: TABLE; Schema: public; Owner: -
--

CREATE TABLE public.compliance_exemption_grant (
    id text NOT NULL,
    exemption_request_id text,
    source_ip text NOT NULL,
    target_host text NOT NULL,
    reason text NOT NULL,
    duration_minutes integer NOT NULL,
    effective_from timestamp(3) with time zone NOT NULL,
    expires_at timestamp(3) with time zone NOT NULL,
    requested_by text,
    approved_by text NOT NULL,
    inactive boolean DEFAULT false NOT NULL,
    activated_at timestamp(3) with time zone,
    created_at timestamp(3) with time zone DEFAULT CURRENT_TIMESTAMP NOT NULL,
    updated_at timestamp(3) with time zone NOT NULL
);

--
-- Name: config_change_event; Type: TABLE; Schema: public; Owner: -
--

CREATE TABLE public.config_change_event (
    id text NOT NULL,
    "timestamp" timestamp(3) with time zone DEFAULT CURRENT_TIMESTAMP NOT NULL,
    thing_type text NOT NULL,
    config_key text NOT NULL,
    action text NOT NULL,
    actor_id text NOT NULL,
    actor_name text NOT NULL,
    new_state jsonb NOT NULL,
    new_version bigint NOT NULL,
    source_ip text,
    emergency_override boolean DEFAULT false NOT NULL
);

--
-- Name: dsar_request; Type: TABLE; Schema: public; Owner: -
--

CREATE TABLE public.dsar_request (
    id text NOT NULL,
    subject_id text NOT NULL,
    contact text,
    type public."DSARRequestType" NOT NULL,
    status public."DSARRequestStatus" DEFAULT 'PENDING'::public."DSARRequestStatus" NOT NULL,
    notes text,
    completed_at timestamp(3) with time zone,
    outcome jsonb,
    "createdAt" timestamp(3) with time zone DEFAULT CURRENT_TIMESTAMP NOT NULL,
    created_by text NOT NULL,
    "updatedAt" timestamp(3) with time zone NOT NULL,
    updated_by text
);

--
-- Name: enrollment_token; Type: TABLE; Schema: public; Owner: -
--

CREATE TABLE public.enrollment_token (
    id text NOT NULL,
    token_hash text NOT NULL,
    thing_type text NOT NULL,
    thing_id text,
    label text DEFAULT ''::text NOT NULL,
    status text DEFAULT 'pending'::text NOT NULL,
    expires_at timestamp(3) with time zone NOT NULL,
    used_at timestamp(3) with time zone,
    metadata jsonb,
    created_by text,
    created_at timestamp(3) with time zone DEFAULT CURRENT_TIMESTAMP NOT NULL
);

--
-- Name: exemption_request; Type: TABLE; Schema: public; Owner: -
--

CREATE TABLE public.exemption_request (
    id text NOT NULL,
    transaction_id text NOT NULL,
    source_ip text NOT NULL,
    target_host text NOT NULL,
    reason text NOT NULL,
    status public."ExemptionRequestStatus" DEFAULT 'PENDING'::public."ExemptionRequestStatus" NOT NULL,
    duration_minutes integer DEFAULT 240 NOT NULL,
    reviewed_by text,
    review_note text,
    reviewed_at timestamp(3) with time zone,
    "createdAt" timestamp(3) with time zone DEFAULT CURRENT_TIMESTAMP NOT NULL,
    requested_by text NOT NULL
);

--
-- Name: interception_domain; Type: TABLE; Schema: public; Owner: -
--

CREATE TABLE public.interception_domain (
    id text NOT NULL,
    name text NOT NULL,
    description text,
    host_pattern text NOT NULL,
    host_match_type public."HostMatchType" DEFAULT 'EXACT'::public."HostMatchType" NOT NULL,
    adapter_id text NOT NULL,
    adapter_config jsonb,
    enabled boolean DEFAULT true NOT NULL,
    priority integer DEFAULT 0 NOT NULL,
    default_path_action public."DefaultPathAction" DEFAULT 'PROCESS'::public."DefaultPathAction" NOT NULL,
    on_adapter_error public."FailureAction" DEFAULT 'FAIL_OPEN'::public."FailureAction" NOT NULL,
    network_zone public."NetworkZone" DEFAULT 'PUBLIC'::public."NetworkZone" NOT NULL,
    source text DEFAULT 'admin'::text NOT NULL,
    created_at timestamp(3) with time zone DEFAULT CURRENT_TIMESTAMP NOT NULL,
    updated_at timestamp(3) with time zone NOT NULL,
    created_by text,
    streaming_mode text,
    streaming_chunk_bytes integer,
    streaming_hook_timeout_ms integer,
    streaming_max_buffer_bytes integer,
    streaming_fail_behavior text,
    capture_request_body boolean,
    capture_response_body boolean,
    raw_body_spill_enabled boolean
);

--
-- Name: interception_path; Type: TABLE; Schema: public; Owner: -
--

CREATE TABLE public.interception_path (
    id text NOT NULL,
    domain_id text NOT NULL,
    path_pattern text[],
    match_type public."PathMatchType" DEFAULT 'PREFIX'::public."PathMatchType" NOT NULL,
    action public."PathAction" NOT NULL,
    priority integer DEFAULT 0 NOT NULL,
    description text,
    enabled boolean DEFAULT true NOT NULL,
    created_at timestamp(3) with time zone DEFAULT CURRENT_TIMESTAMP NOT NULL,
    updated_at timestamp(3) with time zone NOT NULL
);

--
-- Name: job; Type: TABLE; Schema: public; Owner: -
--

CREATE TABLE public.job (
    id text NOT NULL,
    name text NOT NULL,
    description text NOT NULL,
    "intervalSec" integer NOT NULL,
    enabled boolean DEFAULT true NOT NULL,
    "createdAt" timestamp(3) with time zone DEFAULT CURRENT_TIMESTAMP NOT NULL,
    "updatedAt" timestamp(3) with time zone NOT NULL
);

--
-- Name: job_run; Type: TABLE; Schema: public; Owner: -
--

CREATE TABLE public.job_run (
    id text NOT NULL,
    "jobId" text NOT NULL,
    "startedAt" timestamp(3) with time zone NOT NULL,
    "finishedAt" timestamp(3) with time zone,
    "durationMs" integer,
    status text NOT NULL,
    error text,
    "replicaId" text
);

--
-- Name: metric_ops_raw; Type: TABLE; Schema: public; Owner: -
--

CREATE TABLE public.metric_ops_raw (
    id uuid NOT NULL,
    sampled_at timestamp(6) with time zone NOT NULL,
    thing_id text NOT NULL,
    thing_type text NOT NULL,
    metric_name text NOT NULL,
    metric_kind text NOT NULL,
    dimension_key text DEFAULT ''::text NOT NULL,
    value double precision,
    metadata jsonb
);

--
-- Name: metric_ops_retention_config; Type: TABLE; Schema: public; Owner: -
--

CREATE TABLE public.metric_ops_retention_config (
    layer text NOT NULL,
    retention_days integer NOT NULL,
    updated_at timestamp(6) with time zone DEFAULT CURRENT_TIMESTAMP NOT NULL,
    updated_by text
);

--
-- Name: metric_ops_rollup_1d; Type: TABLE; Schema: public; Owner: -
--

CREATE TABLE public.metric_ops_rollup_1d (
    id uuid NOT NULL,
    bucket_start timestamp(6) with time zone NOT NULL,
    thing_id text,
    thing_type text NOT NULL,
    metric_name text NOT NULL,
    metric_kind text NOT NULL,
    dimension_key text DEFAULT ''::text NOT NULL,
    value_avg double precision,
    value_sum double precision,
    value_min double precision,
    value_max double precision,
    sample_count integer NOT NULL,
    metadata jsonb
);

--
-- Name: metric_ops_rollup_1h; Type: TABLE; Schema: public; Owner: -
--

CREATE TABLE public.metric_ops_rollup_1h (
    id uuid NOT NULL,
    bucket_start timestamp(6) with time zone NOT NULL,
    thing_id text,
    thing_type text NOT NULL,
    metric_name text NOT NULL,
    metric_kind text NOT NULL,
    dimension_key text DEFAULT ''::text NOT NULL,
    value_avg double precision,
    value_sum double precision,
    value_min double precision,
    value_max double precision,
    sample_count integer NOT NULL,
    metadata jsonb
);

--
-- Name: metric_ops_rollup_1mo; Type: TABLE; Schema: public; Owner: -
--

CREATE TABLE public.metric_ops_rollup_1mo (
    id uuid NOT NULL,
    bucket_start timestamp(6) with time zone NOT NULL,
    thing_id text,
    thing_type text NOT NULL,
    metric_name text NOT NULL,
    metric_kind text NOT NULL,
    dimension_key text DEFAULT ''::text NOT NULL,
    value_avg double precision,
    value_sum double precision,
    value_min double precision,
    value_max double precision,
    sample_count integer NOT NULL,
    metadata jsonb
);

--
-- Name: metric_rollup_1d; Type: TABLE; Schema: public; Owner: -
--

CREATE TABLE public.metric_rollup_1d (
    id text NOT NULL,
    "bucketStart" timestamp(3) with time zone NOT NULL,
    "metricName" text NOT NULL,
    "dimensionKey" text DEFAULT ''::text NOT NULL,
    "subDimension" text DEFAULT ''::text NOT NULL,
    value numeric(24,6) DEFAULT 0 NOT NULL,
    metadata jsonb,
    "updatedAt" timestamp(3) with time zone DEFAULT CURRENT_TIMESTAMP NOT NULL
);

--
-- Name: metric_rollup_1h; Type: TABLE; Schema: public; Owner: -
--

CREATE TABLE public.metric_rollup_1h (
    id text NOT NULL,
    "bucketStart" timestamp(3) with time zone NOT NULL,
    "metricName" text NOT NULL,
    "dimensionKey" text DEFAULT ''::text NOT NULL,
    "subDimension" text DEFAULT ''::text NOT NULL,
    value numeric(24,6) DEFAULT 0 NOT NULL,
    metadata jsonb,
    "updatedAt" timestamp(3) with time zone DEFAULT CURRENT_TIMESTAMP NOT NULL
);

--
-- Name: metric_rollup_1mo; Type: TABLE; Schema: public; Owner: -
--

CREATE TABLE public.metric_rollup_1mo (
    id text NOT NULL,
    "bucketStart" timestamp(3) with time zone NOT NULL,
    "metricName" text NOT NULL,
    "dimensionKey" text DEFAULT ''::text NOT NULL,
    "subDimension" text DEFAULT ''::text NOT NULL,
    value numeric(24,6) DEFAULT 0 NOT NULL,
    metadata jsonb,
    "updatedAt" timestamp(3) with time zone DEFAULT CURRENT_TIMESTAMP NOT NULL
);

--
-- Name: metric_rollup_5m; Type: TABLE; Schema: public; Owner: -
--

CREATE TABLE public.metric_rollup_5m (
    id text NOT NULL,
    "bucketStart" timestamp(3) with time zone NOT NULL,
    "metricName" text NOT NULL,
    "dimensionKey" text DEFAULT ''::text NOT NULL,
    "subDimension" text DEFAULT ''::text NOT NULL,
    value numeric(24,6) DEFAULT 0 NOT NULL,
    metadata jsonb,
    "updatedAt" timestamp(3) with time zone DEFAULT CURRENT_TIMESTAMP NOT NULL
);

--
-- Name: provider_pricing; Type: TABLE; Schema: public; Owner: -
--

CREATE TABLE public.provider_pricing (
    id text NOT NULL,
    provider_id text,
    model_pattern text NOT NULL,
    adapter_type text NOT NULL,
    input_usd_per_m numeric(12,8) NOT NULL,
    output_usd_per_m numeric(12,8) NOT NULL,
    cache_write_usd_per_m numeric(12,8) NOT NULL,
    cache_read_usd_per_m numeric(12,8) NOT NULL,
    priority integer DEFAULT 0 NOT NULL,
    created_at timestamp(3) with time zone DEFAULT now() NOT NULL
);

--
-- Name: rollup_watermark; Type: TABLE; Schema: public; Owner: -
--

CREATE TABLE public.rollup_watermark (
    "jobName" text NOT NULL,
    watermark timestamp(3) with time zone NOT NULL,
    "updatedAt" timestamp(3) with time zone DEFAULT CURRENT_TIMESTAMP NOT NULL
);

--
-- Name: rule; Type: TABLE; Schema: public; Owner: -
--

CREATE TABLE public.rule (
    id text NOT NULL,
    "packId" text NOT NULL,
    "ruleId" text NOT NULL,
    category text NOT NULL,
    severity text NOT NULL,
    pattern text NOT NULL,
    flags text,
    description text,
    labels text[]
);

--
-- Name: rule_override; Type: TABLE; Schema: public; Owner: -
--

CREATE TABLE public.rule_override (
    id text NOT NULL,
    "installId" text NOT NULL,
    "ruleLocalId" text NOT NULL,
    disabled boolean DEFAULT false NOT NULL,
    "severityOverride" text,
    "updatedAt" timestamp(3) with time zone NOT NULL
);

--
-- Name: rule_pack; Type: TABLE; Schema: public; Owner: -
--

CREATE TABLE public.rule_pack (
    id text NOT NULL,
    name text NOT NULL,
    version text NOT NULL,
    maintainer text NOT NULL,
    description text,
    signature text,
    "createdAt" timestamp(3) with time zone DEFAULT CURRENT_TIMESTAMP NOT NULL
);

--
-- Name: rule_pack_install; Type: TABLE; Schema: public; Owner: -
--

CREATE TABLE public.rule_pack_install (
    id text NOT NULL,
    "packId" text NOT NULL,
    "pinVersion" text NOT NULL,
    "boundHookId" text NOT NULL,
    enabled boolean DEFAULT true NOT NULL,
    "installedAt" timestamp(3) with time zone DEFAULT CURRENT_TIMESTAMP NOT NULL
);

--
-- Name: system_metadata; Type: TABLE; Schema: public; Owner: -
--

CREATE TABLE public.system_metadata (
    key text NOT NULL,
    value jsonb NOT NULL,
    updated_at timestamp(3) with time zone NOT NULL,
    updated_by text
);

--
-- Name: thing; Type: TABLE; Schema: public; Owner: -
--

CREATE TABLE public.thing (
    id text NOT NULL,
    type text NOT NULL,
    name text,
    version text,
    address text,
    enrolled_by text,
    auth_type text DEFAULT 'bearer'::text NOT NULL,
    conn_protocol text DEFAULT 'http'::text NOT NULL,
    status text DEFAULT 'enrolled'::text NOT NULL,
    enrolled_at timestamp(3) with time zone DEFAULT CURRENT_TIMESTAMP NOT NULL,
    last_seen_at timestamp(3) with time zone,
    updated_at timestamp(3) with time zone NOT NULL,
    desired jsonb DEFAULT '{}'::jsonb NOT NULL,
    reported jsonb DEFAULT '{}'::jsonb NOT NULL,
    desired_ver bigint DEFAULT 0 NOT NULL,
    reported_ver bigint DEFAULT 0 NOT NULL,
    metadata jsonb
);

--
-- Name: thing_agent; Type: TABLE; Schema: public; Owner: -
--

CREATE TABLE public.thing_agent (
    thing_id text NOT NULL,
    hostname text NOT NULL,
    os text NOT NULL,
    os_version text NOT NULL,
    cert_serial text,
    cert_expires_at timestamp(3) with time zone,
    previous_cert_serial text,
    cert_renewed_at timestamp(3) with time zone,
    sysinfo jsonb,
    current_assignment_id text,
    trust_level integer DEFAULT 0 NOT NULL
);

--
-- Name: thing_config_override; Type: TABLE; Schema: public; Owner: -
--

CREATE TABLE public.thing_config_override (
    thing_id text NOT NULL,
    config_key text NOT NULL,
    state jsonb NOT NULL,
    template_ver_at_set bigint NOT NULL,
    set_by text NOT NULL,
    set_at timestamp(3) with time zone DEFAULT CURRENT_TIMESTAMP NOT NULL,
    reason character varying(500),
    expires_at timestamp(3) with time zone,
    emergency_override boolean DEFAULT false NOT NULL,
    CONSTRAINT chk_tco_expires_set CHECK (((expires_at IS NULL) OR (expires_at > set_at))),
    CONSTRAINT chk_tco_reason_len CHECK (((reason IS NULL) OR (length((reason)::text) <= 500)))
);

--
-- Name: thing_config_template; Type: TABLE; Schema: public; Owner: -
--

CREATE TABLE public.thing_config_template (
    type text NOT NULL,
    config_key text NOT NULL,
    state jsonb DEFAULT '{}'::jsonb NOT NULL,
    version bigint DEFAULT 1 NOT NULL,
    updated_at timestamp(3) with time zone NOT NULL,
    updated_by text
);

--
-- Name: thing_diag_event; Type: TABLE; Schema: public; Owner: -
--

CREATE TABLE public.thing_diag_event (
    id uuid NOT NULL,
    thing_id text NOT NULL,
    thing_type text NOT NULL,
    occurred_at timestamp(6) with time zone NOT NULL,
    received_at timestamp(6) with time zone DEFAULT CURRENT_TIMESTAMP NOT NULL,
    level text NOT NULL,
    event_type text NOT NULL,
    source text NOT NULL,
    message text NOT NULL,
    message_hash text NOT NULL,
    attrs jsonb,
    stack_trace text,
    repeat_count integer DEFAULT 1 NOT NULL,
    agent_version text,
    os_info jsonb
);

--
-- Name: thing_diag_mode_window; Type: TABLE; Schema: public; Owner: -
--

CREATE TABLE public.thing_diag_mode_window (
    id uuid NOT NULL,
    thing_id text NOT NULL,
    started_at timestamp(6) with time zone NOT NULL,
    ended_at timestamp(6) with time zone NOT NULL,
    set_by text,
    reason text,
    created_at timestamp(6) with time zone DEFAULT CURRENT_TIMESTAMP NOT NULL
);

--
-- Name: thing_service; Type: TABLE; Schema: public; Owner: -
--

CREATE TABLE public.thing_service (
    thing_id text NOT NULL,
    role text DEFAULT 'default'::text,
    metrics_url text,
    management_url text
);

--
-- Name: traffic_event; Type: TABLE; Schema: public; Owner: -
--

CREATE TABLE public.traffic_event (
    id text NOT NULL,
    source text NOT NULL,
    "timestamp" timestamp(3) with time zone DEFAULT CURRENT_TIMESTAMP NOT NULL,
    source_ip text,
    target_host text,
    method text,
    path text,
    status_code integer,
    latency_ms integer,
    created_at timestamp(3) with time zone DEFAULT CURRENT_TIMESTAMP NOT NULL,
    trace_id text,
    external_request_id text,
    entity_type text,
    entity_id text,
    entity_name text,
    org_id text,
    org_name text,
    identity jsonb,
    provider_id text,
    provider_name text,
    model_id text,
    model_name text,
    prompt_tokens integer,
    completion_tokens integer,
    total_tokens integer,
    estimated_cost_usd numeric(12,6),
    routed_provider_id text,
    routed_provider_name text,
    routed_model_id text,
    routed_model_name text,
    routing_rule_id text,
    routing_rule_name text,
    response_hook_decision text,
    bump_status text,
    source_process text,
    action text,
    routing_trace jsonb,
    details jsonb,
    api_key_class text,
    api_key_fingerprint text,
    usage_extraction_status text,
    compliance_tags text[] DEFAULT ARRAY[]::text[] NOT NULL,
    internal_purpose text,
    cache_status text,
    origin_tz text,
    request_hook_decision text,
    request_hook_reason text,
    request_hook_reason_code text,
    request_hooks_pipeline jsonb,
    request_blocking_rule jsonb,
    response_hook_reason text,
    response_hook_reason_code text,
    response_hooks_pipeline jsonb,
    response_blocking_rule jsonb,
    error_code text,
    error_reason text,
    cache_creation_tokens integer,
    cache_read_tokens integer,
    normalized_strip_count integer,
    normalized_strip_bytes integer,
    cache_marker_injected integer,
    cache_write_cost_usd numeric(12,8),
    cache_read_savings_usd numeric(12,8),
    cache_net_savings_usd numeric(12,8),
    gateway_cache_savings_usd numeric(12,8),
    thing_id text,
    thing_name text,
    credential_id text,
    CONSTRAINT chk_traffic_event_source CHECK ((source = ANY (ARRAY['ai-gateway'::text, 'compliance-proxy'::text, 'agent'::text]))),
    CONSTRAINT chk_traffic_event_usage_extraction_status CHECK (((usage_extraction_status IS NULL) OR (usage_extraction_status = ANY (ARRAY['ok'::text, 'streaming_reported'::text, 'streaming_estimated'::text, 'streaming_unavailable'::text, 'parse_failed'::text, 'no_body'::text, 'non_llm'::text]))))
);

--
-- Name: traffic_event_normalized; Type: TABLE; Schema: public; Owner: -
--

CREATE TABLE public.traffic_event_normalized (
    traffic_event_id text NOT NULL,
    request_normalized jsonb,
    response_normalized jsonb,
    request_status text,
    response_status text,
    request_error_reason text,
    response_error_reason text,
    request_redaction_spans jsonb,
    response_redaction_spans jsonb,
    normalize_version text DEFAULT '1'::text NOT NULL,
    created_at timestamp(3) with time zone DEFAULT now() NOT NULL
);

--
-- Name: traffic_event_payload; Type: TABLE; Schema: public; Owner: -
--

CREATE TABLE public.traffic_event_payload (
    traffic_event_id text NOT NULL,
    created_at timestamp(3) with time zone DEFAULT CURRENT_TIMESTAMP NOT NULL,
    inline_request_body jsonb,
    inline_response_body jsonb,
    request_spill_ref jsonb,
    response_spill_ref jsonb,
    request_size_bytes bigint,
    response_size_bytes bigint,
    request_truncated boolean DEFAULT false NOT NULL,
    response_truncated boolean DEFAULT false NOT NULL,
    request_content_type text,
    response_content_type text
);

--
-- Name: AdminAuditLog sequenceNumber; Type: DEFAULT; Schema: public; Owner: -
--

ALTER TABLE ONLY public."AdminAuditLog" ALTER COLUMN "sequenceNumber" SET DEFAULT nextval('public."AdminAuditLog_sequenceNumber_seq"'::regclass);

--
-- Name: RevokedToken id; Type: DEFAULT; Schema: public; Owner: -
--

ALTER TABLE ONLY public."RevokedToken" ALTER COLUMN id SET DEFAULT nextval('public."RevokedToken_id_seq"'::regclass);

--
-- Name: AdminApiKey AdminApiKey_pkey; Type: CONSTRAINT; Schema: public; Owner: -
--

ALTER TABLE ONLY public."AdminApiKey"
    ADD CONSTRAINT "AdminApiKey_pkey" PRIMARY KEY (id);

--
-- Name: AdminAuditLog AdminAuditLog_pkey; Type: CONSTRAINT; Schema: public; Owner: -
--

ALTER TABLE ONLY public."AdminAuditLog"
    ADD CONSTRAINT "AdminAuditLog_pkey" PRIMARY KEY (id);

--
-- Name: AgentExemption AgentExemption_pkey; Type: CONSTRAINT; Schema: public; Owner: -
--

ALTER TABLE ONLY public."AgentExemption"
    ADD CONSTRAINT "AgentExemption_pkey" PRIMARY KEY (id);

--
-- Name: AlertChannel AlertChannel_pkey; Type: CONSTRAINT; Schema: public; Owner: -
--

ALTER TABLE ONLY public."AlertChannel"
    ADD CONSTRAINT "AlertChannel_pkey" PRIMARY KEY (id);

--
-- Name: AlertDispatch AlertDispatch_pkey; Type: CONSTRAINT; Schema: public; Owner: -
--

ALTER TABLE ONLY public."AlertDispatch"
    ADD CONSTRAINT "AlertDispatch_pkey" PRIMARY KEY (id);

--
-- Name: AlertRule AlertRule_pkey; Type: CONSTRAINT; Schema: public; Owner: -
--

ALTER TABLE ONLY public."AlertRule"
    ADD CONSTRAINT "AlertRule_pkey" PRIMARY KEY (id);

--
-- Name: Alert Alert_pkey; Type: CONSTRAINT; Schema: public; Owner: -
--

ALTER TABLE ONLY public."Alert"
    ADD CONSTRAINT "Alert_pkey" PRIMARY KEY (id);

--
-- Name: Credential Credential_pkey; Type: CONSTRAINT; Schema: public; Owner: -
--

ALTER TABLE ONLY public."Credential"
    ADD CONSTRAINT "Credential_pkey" PRIMARY KEY (id);

--
-- Name: DeviceAssignment DeviceAssignment_pkey; Type: CONSTRAINT; Schema: public; Owner: -
--

ALTER TABLE ONLY public."DeviceAssignment"
    ADD CONSTRAINT "DeviceAssignment_pkey" PRIMARY KEY (id);

--
-- Name: DeviceGroupMembership DeviceGroupMembership_pkey; Type: CONSTRAINT; Schema: public; Owner: -
--

ALTER TABLE ONLY public."DeviceGroupMembership"
    ADD CONSTRAINT "DeviceGroupMembership_pkey" PRIMARY KEY (id);

--
-- Name: DeviceGroupPolicyRule DeviceGroupPolicyRule_pkey; Type: CONSTRAINT; Schema: public; Owner: -
--

ALTER TABLE ONLY public."DeviceGroupPolicyRule"
    ADD CONSTRAINT "DeviceGroupPolicyRule_pkey" PRIMARY KEY (id);

--
-- Name: DeviceGroup DeviceGroup_pkey; Type: CONSTRAINT; Schema: public; Owner: -
--

ALTER TABLE ONLY public."DeviceGroup"
    ADD CONSTRAINT "DeviceGroup_pkey" PRIMARY KEY (id);

--
-- Name: HookConfig HookConfig_pkey; Type: CONSTRAINT; Schema: public; Owner: -
--

ALTER TABLE ONLY public."HookConfig"
    ADD CONSTRAINT "HookConfig_pkey" PRIMARY KEY (id);

--
-- Name: IamGroupMembership IamGroupMembership_pkey; Type: CONSTRAINT; Schema: public; Owner: -
--

ALTER TABLE ONLY public."IamGroupMembership"
    ADD CONSTRAINT "IamGroupMembership_pkey" PRIMARY KEY (id);

--
-- Name: IamGroupPolicyAttachment IamGroupPolicyAttachment_pkey; Type: CONSTRAINT; Schema: public; Owner: -
--

ALTER TABLE ONLY public."IamGroupPolicyAttachment"
    ADD CONSTRAINT "IamGroupPolicyAttachment_pkey" PRIMARY KEY (id);

--
-- Name: IamGroup IamGroup_pkey; Type: CONSTRAINT; Schema: public; Owner: -
--

ALTER TABLE ONLY public."IamGroup"
    ADD CONSTRAINT "IamGroup_pkey" PRIMARY KEY (id);

--
-- Name: IamPolicyAttachment IamPolicyAttachment_pkey; Type: CONSTRAINT; Schema: public; Owner: -
--

ALTER TABLE ONLY public."IamPolicyAttachment"
    ADD CONSTRAINT "IamPolicyAttachment_pkey" PRIMARY KEY (id);

--
-- Name: IamPolicy IamPolicy_pkey; Type: CONSTRAINT; Schema: public; Owner: -
--

ALTER TABLE ONLY public."IamPolicy"
    ADD CONSTRAINT "IamPolicy_pkey" PRIMARY KEY (id);

--
-- Name: IdentityProvider IdentityProvider_pkey; Type: CONSTRAINT; Schema: public; Owner: -
--

ALTER TABLE ONLY public."IdentityProvider"
    ADD CONSTRAINT "IdentityProvider_pkey" PRIMARY KEY (id);

--
-- Name: IdpGroupMapping IdpGroupMapping_pkey; Type: CONSTRAINT; Schema: public; Owner: -
--

ALTER TABLE ONLY public."IdpGroupMapping"
    ADD CONSTRAINT "IdpGroupMapping_pkey" PRIMARY KEY (id);

--
-- Name: MetricRollup MetricRollup_pkey; Type: CONSTRAINT; Schema: public; Owner: -
--

ALTER TABLE ONLY public."MetricRollup"
    ADD CONSTRAINT "MetricRollup_pkey" PRIMARY KEY (id);

--
-- Name: ModelPricing ModelPricing_pkey; Type: CONSTRAINT; Schema: public; Owner: -
--

ALTER TABLE ONLY public."ModelPricing"
    ADD CONSTRAINT "ModelPricing_pkey" PRIMARY KEY (id);

--
-- Name: Model Model_pkey; Type: CONSTRAINT; Schema: public; Owner: -
--

ALTER TABLE ONLY public."Model"
    ADD CONSTRAINT "Model_pkey" PRIMARY KEY (id);

--
-- Name: NexusUser NexusUser_pkey; Type: CONSTRAINT; Schema: public; Owner: -
--

ALTER TABLE ONLY public."NexusUser"
    ADD CONSTRAINT "NexusUser_pkey" PRIMARY KEY (id);

--
-- Name: OAuthClient OAuthClient_pkey; Type: CONSTRAINT; Schema: public; Owner: -
--

ALTER TABLE ONLY public."OAuthClient"
    ADD CONSTRAINT "OAuthClient_pkey" PRIMARY KEY (id);

--
-- Name: Organization Organization_path_key; Type: CONSTRAINT; Schema: public; Owner: -
--

ALTER TABLE ONLY public."Organization"
    ADD CONSTRAINT "Organization_path_key" UNIQUE (path);

--
-- Name: Organization Organization_pkey; Type: CONSTRAINT; Schema: public; Owner: -
--

ALTER TABLE ONLY public."Organization"
    ADD CONSTRAINT "Organization_pkey" PRIMARY KEY (id);

--
-- Name: Project Project_pkey; Type: CONSTRAINT; Schema: public; Owner: -
--

ALTER TABLE ONLY public."Project"
    ADD CONSTRAINT "Project_pkey" PRIMARY KEY (id);

--
-- Name: ProviderHealth ProviderHealth_pkey; Type: CONSTRAINT; Schema: public; Owner: -
--

ALTER TABLE ONLY public."ProviderHealth"
    ADD CONSTRAINT "ProviderHealth_pkey" PRIMARY KEY (id);

--
-- Name: Provider Provider_pkey; Type: CONSTRAINT; Schema: public; Owner: -
--

ALTER TABLE ONLY public."Provider"
    ADD CONSTRAINT "Provider_pkey" PRIMARY KEY (id);

--
-- Name: QuotaOverride QuotaOverride_pkey; Type: CONSTRAINT; Schema: public; Owner: -
--

ALTER TABLE ONLY public."QuotaOverride"
    ADD CONSTRAINT "QuotaOverride_pkey" PRIMARY KEY (id);

--
-- Name: QuotaPolicy QuotaPolicy_pkey; Type: CONSTRAINT; Schema: public; Owner: -
--

ALTER TABLE ONLY public."QuotaPolicy"
    ADD CONSTRAINT "QuotaPolicy_pkey" PRIMARY KEY (id);

--
-- Name: RefreshToken RefreshToken_pkey; Type: CONSTRAINT; Schema: public; Owner: -
--

ALTER TABLE ONLY public."RefreshToken"
    ADD CONSTRAINT "RefreshToken_pkey" PRIMARY KEY (jti);

--
-- Name: RevokedToken RevokedToken_pkey; Type: CONSTRAINT; Schema: public; Owner: -
--

ALTER TABLE ONLY public."RevokedToken"
    ADD CONSTRAINT "RevokedToken_pkey" PRIMARY KEY (id);

--
-- Name: RoutingRule RoutingRule_pkey; Type: CONSTRAINT; Schema: public; Owner: -
--

ALTER TABLE ONLY public."RoutingRule"
    ADD CONSTRAINT "RoutingRule_pkey" PRIMARY KEY (id);

--
-- Name: ScimToken ScimToken_pkey; Type: CONSTRAINT; Schema: public; Owner: -
--

ALTER TABLE ONLY public."ScimToken"
    ADD CONSTRAINT "ScimToken_pkey" PRIMARY KEY (id);

--
-- Name: UserFederatedIdentity UserFederatedIdentity_pkey; Type: CONSTRAINT; Schema: public; Owner: -
--

ALTER TABLE ONLY public."UserFederatedIdentity"
    ADD CONSTRAINT "UserFederatedIdentity_pkey" PRIMARY KEY (id);

--
-- Name: VirtualKey VirtualKey_pkey; Type: CONSTRAINT; Schema: public; Owner: -
--

ALTER TABLE ONLY public."VirtualKey"
    ADD CONSTRAINT "VirtualKey_pkey" PRIMARY KEY (id);

--
-- Name: ai_guard_config ai_guard_config_pkey; Type: CONSTRAINT; Schema: public; Owner: -
--

ALTER TABLE ONLY public.ai_guard_config
    ADD CONSTRAINT ai_guard_config_pkey PRIMARY KEY (id);

--
-- Name: cache_adapter_config cache_adapter_config_pkey; Type: CONSTRAINT; Schema: public; Owner: -
--

ALTER TABLE ONLY public.cache_adapter_config
    ADD CONSTRAINT cache_adapter_config_pkey PRIMARY KEY (adapter_type);

--
-- Name: cache_global_config cache_global_config_pkey; Type: CONSTRAINT; Schema: public; Owner: -
--

ALTER TABLE ONLY public.cache_global_config
    ADD CONSTRAINT cache_global_config_pkey PRIMARY KEY (id);

--
-- Name: cache_provider_config cache_provider_config_pkey; Type: CONSTRAINT; Schema: public; Owner: -
--

ALTER TABLE ONLY public.cache_provider_config
    ADD CONSTRAINT cache_provider_config_pkey PRIMARY KEY (provider_id);

--
-- Name: compliance_exemption_grant compliance_exemption_grant_pkey; Type: CONSTRAINT; Schema: public; Owner: -
--

ALTER TABLE ONLY public.compliance_exemption_grant
    ADD CONSTRAINT compliance_exemption_grant_pkey PRIMARY KEY (id);

--
-- Name: config_change_event config_change_event_pkey; Type: CONSTRAINT; Schema: public; Owner: -
--

ALTER TABLE ONLY public.config_change_event
    ADD CONSTRAINT config_change_event_pkey PRIMARY KEY (id);

--
-- Name: dsar_request dsar_request_pkey; Type: CONSTRAINT; Schema: public; Owner: -
--

ALTER TABLE ONLY public.dsar_request
    ADD CONSTRAINT dsar_request_pkey PRIMARY KEY (id);

--
-- Name: enrollment_token enrollment_token_pkey; Type: CONSTRAINT; Schema: public; Owner: -
--

ALTER TABLE ONLY public.enrollment_token
    ADD CONSTRAINT enrollment_token_pkey PRIMARY KEY (id);

--
-- Name: exemption_request exemption_request_pkey; Type: CONSTRAINT; Schema: public; Owner: -
--

ALTER TABLE ONLY public.exemption_request
    ADD CONSTRAINT exemption_request_pkey PRIMARY KEY (id);

--
-- Name: interception_domain interception_domain_pkey; Type: CONSTRAINT; Schema: public; Owner: -
--

ALTER TABLE ONLY public.interception_domain
    ADD CONSTRAINT interception_domain_pkey PRIMARY KEY (id);

--
-- Name: interception_path interception_path_pkey; Type: CONSTRAINT; Schema: public; Owner: -
--

ALTER TABLE ONLY public.interception_path
    ADD CONSTRAINT interception_path_pkey PRIMARY KEY (id);

--
-- Name: job job_pkey; Type: CONSTRAINT; Schema: public; Owner: -
--

ALTER TABLE ONLY public.job
    ADD CONSTRAINT job_pkey PRIMARY KEY (id);

--
-- Name: job_run job_run_pkey; Type: CONSTRAINT; Schema: public; Owner: -
--

ALTER TABLE ONLY public.job_run
    ADD CONSTRAINT job_run_pkey PRIMARY KEY (id);

--
-- Name: metric_ops_raw metric_ops_raw_pkey; Type: CONSTRAINT; Schema: public; Owner: -
--

ALTER TABLE ONLY public.metric_ops_raw
    ADD CONSTRAINT metric_ops_raw_pkey PRIMARY KEY (id);

--
-- Name: metric_ops_retention_config metric_ops_retention_config_pkey; Type: CONSTRAINT; Schema: public; Owner: -
--

ALTER TABLE ONLY public.metric_ops_retention_config
    ADD CONSTRAINT metric_ops_retention_config_pkey PRIMARY KEY (layer);

--
-- Name: metric_ops_rollup_1d metric_ops_rollup_1d_pkey; Type: CONSTRAINT; Schema: public; Owner: -
--

ALTER TABLE ONLY public.metric_ops_rollup_1d
    ADD CONSTRAINT metric_ops_rollup_1d_pkey PRIMARY KEY (id);

--
-- Name: metric_ops_rollup_1h metric_ops_rollup_1h_pkey; Type: CONSTRAINT; Schema: public; Owner: -
--

ALTER TABLE ONLY public.metric_ops_rollup_1h
    ADD CONSTRAINT metric_ops_rollup_1h_pkey PRIMARY KEY (id);

--
-- Name: metric_ops_rollup_1mo metric_ops_rollup_1mo_pkey; Type: CONSTRAINT; Schema: public; Owner: -
--

ALTER TABLE ONLY public.metric_ops_rollup_1mo
    ADD CONSTRAINT metric_ops_rollup_1mo_pkey PRIMARY KEY (id);

--
-- Name: metric_rollup_1d metric_rollup_1d_pkey; Type: CONSTRAINT; Schema: public; Owner: -
--

ALTER TABLE ONLY public.metric_rollup_1d
    ADD CONSTRAINT metric_rollup_1d_pkey PRIMARY KEY (id);

--
-- Name: metric_rollup_1h metric_rollup_1h_pkey; Type: CONSTRAINT; Schema: public; Owner: -
--

ALTER TABLE ONLY public.metric_rollup_1h
    ADD CONSTRAINT metric_rollup_1h_pkey PRIMARY KEY (id);

--
-- Name: metric_rollup_1mo metric_rollup_1mo_pkey; Type: CONSTRAINT; Schema: public; Owner: -
--

ALTER TABLE ONLY public.metric_rollup_1mo
    ADD CONSTRAINT metric_rollup_1mo_pkey PRIMARY KEY (id);

--
-- Name: metric_rollup_5m metric_rollup_5m_pkey; Type: CONSTRAINT; Schema: public; Owner: -
--

ALTER TABLE ONLY public.metric_rollup_5m
    ADD CONSTRAINT metric_rollup_5m_pkey PRIMARY KEY (id);

--
-- Name: provider_pricing provider_pricing_pkey; Type: CONSTRAINT; Schema: public; Owner: -
--

ALTER TABLE ONLY public.provider_pricing
    ADD CONSTRAINT provider_pricing_pkey PRIMARY KEY (id);

--
-- Name: rollup_watermark rollup_watermark_pkey; Type: CONSTRAINT; Schema: public; Owner: -
--

ALTER TABLE ONLY public.rollup_watermark
    ADD CONSTRAINT rollup_watermark_pkey PRIMARY KEY ("jobName");

--
-- Name: rule_override rule_override_pkey; Type: CONSTRAINT; Schema: public; Owner: -
--

ALTER TABLE ONLY public.rule_override
    ADD CONSTRAINT rule_override_pkey PRIMARY KEY (id);

--
-- Name: rule_pack_install rule_pack_install_pkey; Type: CONSTRAINT; Schema: public; Owner: -
--

ALTER TABLE ONLY public.rule_pack_install
    ADD CONSTRAINT rule_pack_install_pkey PRIMARY KEY (id);

--
-- Name: rule_pack rule_pack_pkey; Type: CONSTRAINT; Schema: public; Owner: -
--

ALTER TABLE ONLY public.rule_pack
    ADD CONSTRAINT rule_pack_pkey PRIMARY KEY (id);

--
-- Name: rule rule_pkey; Type: CONSTRAINT; Schema: public; Owner: -
--

ALTER TABLE ONLY public.rule
    ADD CONSTRAINT rule_pkey PRIMARY KEY (id);

--
-- Name: system_metadata system_metadata_pkey; Type: CONSTRAINT; Schema: public; Owner: -
--

ALTER TABLE ONLY public.system_metadata
    ADD CONSTRAINT system_metadata_pkey PRIMARY KEY (key);

--
-- Name: thing_agent thing_agent_pkey; Type: CONSTRAINT; Schema: public; Owner: -
--

ALTER TABLE ONLY public.thing_agent
    ADD CONSTRAINT thing_agent_pkey PRIMARY KEY (thing_id);

--
-- Name: thing_config_override thing_config_override_pkey; Type: CONSTRAINT; Schema: public; Owner: -
--

ALTER TABLE ONLY public.thing_config_override
    ADD CONSTRAINT thing_config_override_pkey PRIMARY KEY (thing_id, config_key);

--
-- Name: thing_config_template thing_config_template_pkey; Type: CONSTRAINT; Schema: public; Owner: -
--

ALTER TABLE ONLY public.thing_config_template
    ADD CONSTRAINT thing_config_template_pkey PRIMARY KEY (type, config_key);

--
-- Name: thing_diag_event thing_diag_event_pkey; Type: CONSTRAINT; Schema: public; Owner: -
--

ALTER TABLE ONLY public.thing_diag_event
    ADD CONSTRAINT thing_diag_event_pkey PRIMARY KEY (id);

--
-- Name: thing_diag_mode_window thing_diag_mode_window_pkey; Type: CONSTRAINT; Schema: public; Owner: -
--

ALTER TABLE ONLY public.thing_diag_mode_window
    ADD CONSTRAINT thing_diag_mode_window_pkey PRIMARY KEY (id);

--
-- Name: thing thing_pkey; Type: CONSTRAINT; Schema: public; Owner: -
--

ALTER TABLE ONLY public.thing
    ADD CONSTRAINT thing_pkey PRIMARY KEY (id);

--
-- Name: thing_service thing_service_pkey; Type: CONSTRAINT; Schema: public; Owner: -
--

ALTER TABLE ONLY public.thing_service
    ADD CONSTRAINT thing_service_pkey PRIMARY KEY (thing_id);

--
-- Name: traffic_event_normalized traffic_event_normalized_pkey; Type: CONSTRAINT; Schema: public; Owner: -
--

ALTER TABLE ONLY public.traffic_event_normalized
    ADD CONSTRAINT traffic_event_normalized_pkey PRIMARY KEY (traffic_event_id);

--
-- Name: traffic_event_payload traffic_event_payload_pkey; Type: CONSTRAINT; Schema: public; Owner: -
--

ALTER TABLE ONLY public.traffic_event_payload
    ADD CONSTRAINT traffic_event_payload_pkey PRIMARY KEY (traffic_event_id);

--
-- Name: traffic_event traffic_event_pkey; Type: CONSTRAINT; Schema: public; Owner: -
--

ALTER TABLE ONLY public.traffic_event
    ADD CONSTRAINT traffic_event_pkey PRIMARY KEY (id);

--
-- Name: AdminApiKey_keyHash_idx; Type: INDEX; Schema: public; Owner: -
--

CREATE INDEX "AdminApiKey_keyHash_idx" ON public."AdminApiKey" USING btree ("keyHash");

--
-- Name: AdminApiKey_keyHash_key; Type: INDEX; Schema: public; Owner: -
--

CREATE UNIQUE INDEX "AdminApiKey_keyHash_key" ON public."AdminApiKey" USING btree ("keyHash");

--
-- Name: AdminApiKey_ownerUserId_idx; Type: INDEX; Schema: public; Owner: -
--

CREATE INDEX "AdminApiKey_ownerUserId_idx" ON public."AdminApiKey" USING btree ("ownerUserId");

--
-- Name: AdminAuditLog_actorId_idx; Type: INDEX; Schema: public; Owner: -
--

CREATE INDEX "AdminAuditLog_actorId_idx" ON public."AdminAuditLog" USING btree ("actorId");

--
-- Name: AdminAuditLog_clientRequestId_idx; Type: INDEX; Schema: public; Owner: -
--

CREATE INDEX "AdminAuditLog_clientRequestId_idx" ON public."AdminAuditLog" USING btree ("clientRequestId");

--
-- Name: AdminAuditLog_entityType_idx; Type: INDEX; Schema: public; Owner: -
--

CREATE INDEX "AdminAuditLog_entityType_idx" ON public."AdminAuditLog" USING btree ("entityType");

--
-- Name: AdminAuditLog_nexusRequestId_idx; Type: INDEX; Schema: public; Owner: -
--

CREATE INDEX "AdminAuditLog_nexusRequestId_idx" ON public."AdminAuditLog" USING btree ("nexusRequestId");

--
-- Name: AdminAuditLog_sequenceNumber_idx; Type: INDEX; Schema: public; Owner: -
--

CREATE INDEX "AdminAuditLog_sequenceNumber_idx" ON public."AdminAuditLog" USING btree ("sequenceNumber");

--
-- Name: AdminAuditLog_timestamp_idx; Type: INDEX; Schema: public; Owner: -
--

CREATE INDEX "AdminAuditLog_timestamp_idx" ON public."AdminAuditLog" USING btree ("timestamp");

--
-- Name: AgentExemption_deviceId_idx; Type: INDEX; Schema: public; Owner: -
--

CREATE INDEX "AgentExemption_deviceId_idx" ON public."AgentExemption" USING btree ("deviceId");

--
-- Name: AgentExemption_groupId_idx; Type: INDEX; Schema: public; Owner: -
--

CREATE INDEX "AgentExemption_groupId_idx" ON public."AgentExemption" USING btree ("groupId");

--
-- Name: AgentExemption_host_idx; Type: INDEX; Schema: public; Owner: -
--

CREATE INDEX "AgentExemption_host_idx" ON public."AgentExemption" USING btree (host);

--
-- Name: AgentExemption_source_expiresAt_idx; Type: INDEX; Schema: public; Owner: -
--

CREATE INDEX "AgentExemption_source_expiresAt_idx" ON public."AgentExemption" USING btree (source, "expiresAt");

--
-- Name: AlertDispatch_alertId_idx; Type: INDEX; Schema: public; Owner: -
--

CREATE INDEX "AlertDispatch_alertId_idx" ON public."AlertDispatch" USING btree ("alertId");

--
-- Name: AlertDispatch_attemptedAt_idx; Type: INDEX; Schema: public; Owner: -
--

CREATE INDEX "AlertDispatch_attemptedAt_idx" ON public."AlertDispatch" USING btree ("attemptedAt");

--
-- Name: AlertRule_sourceType_enabled_idx; Type: INDEX; Schema: public; Owner: -
--

CREATE INDEX "AlertRule_sourceType_enabled_idx" ON public."AlertRule" USING btree ("sourceType", enabled);

--
-- Name: Alert_firedAt_idx; Type: INDEX; Schema: public; Owner: -
--

CREATE INDEX "Alert_firedAt_idx" ON public."Alert" USING btree ("firedAt");

--
-- Name: Alert_ruleId_targetKey_state_idx; Type: INDEX; Schema: public; Owner: -
--

CREATE INDEX "Alert_ruleId_targetKey_state_idx" ON public."Alert" USING btree ("ruleId", "targetKey", state);

--
-- Name: Alert_state_sourceType_firedAt_idx; Type: INDEX; Schema: public; Owner: -
--

CREATE INDEX "Alert_state_sourceType_firedAt_idx" ON public."Alert" USING btree (state, "sourceType", "firedAt");

--
-- Name: Credential_circuitState_idx; Type: INDEX; Schema: public; Owner: -
--

CREATE INDEX "Credential_circuitState_idx" ON public."Credential" USING btree ("circuitState");

--
-- Name: Credential_expiresAt_idx; Type: INDEX; Schema: public; Owner: -
--

CREATE INDEX "Credential_expiresAt_idx" ON public."Credential" USING btree ("expiresAt");

--
-- Name: Credential_healthStatus_idx; Type: INDEX; Schema: public; Owner: -
--

CREATE INDEX "Credential_healthStatus_idx" ON public."Credential" USING btree ("healthStatus");

--
-- Name: Credential_name_key; Type: INDEX; Schema: public; Owner: -
--

CREATE UNIQUE INDEX "Credential_name_key" ON public."Credential" USING btree (name);

--
-- Name: Credential_providerId_idx; Type: INDEX; Schema: public; Owner: -
--

CREATE INDEX "Credential_providerId_idx" ON public."Credential" USING btree ("providerId");

--
-- Name: Credential_status_idx; Type: INDEX; Schema: public; Owner: -
--

CREATE INDEX "Credential_status_idx" ON public."Credential" USING btree (status);

--
-- Name: DeviceAssignment_deviceId_active_uidx; Type: INDEX; Schema: public; Owner: -
--

CREATE UNIQUE INDEX "DeviceAssignment_deviceId_active_uidx" ON public."DeviceAssignment" USING btree ("deviceId") WHERE ("releasedAt" IS NULL);

--
-- Name: DeviceAssignment_deviceId_releasedAt_idx; Type: INDEX; Schema: public; Owner: -
--

CREATE INDEX "DeviceAssignment_deviceId_releasedAt_idx" ON public."DeviceAssignment" USING btree ("deviceId", "releasedAt");

--
-- Name: DeviceAssignment_ip_address_assignedAt_idx; Type: INDEX; Schema: public; Owner: -
--

CREATE INDEX "DeviceAssignment_ip_address_assignedAt_idx" ON public."DeviceAssignment" USING btree (ip_address, "assignedAt");

--
-- Name: DeviceAssignment_userId_idx; Type: INDEX; Schema: public; Owner: -
--

CREATE INDEX "DeviceAssignment_userId_idx" ON public."DeviceAssignment" USING btree ("userId");

--
-- Name: DeviceGroupMembership_deviceId_idx; Type: INDEX; Schema: public; Owner: -
--

CREATE INDEX "DeviceGroupMembership_deviceId_idx" ON public."DeviceGroupMembership" USING btree ("deviceId");

--
-- Name: DeviceGroupMembership_groupId_deviceId_key; Type: INDEX; Schema: public; Owner: -
--

CREATE UNIQUE INDEX "DeviceGroupMembership_groupId_deviceId_key" ON public."DeviceGroupMembership" USING btree ("groupId", "deviceId");

--
-- Name: DeviceGroupPolicyRule_groupId_idx; Type: INDEX; Schema: public; Owner: -
--

CREATE INDEX "DeviceGroupPolicyRule_groupId_idx" ON public."DeviceGroupPolicyRule" USING btree ("groupId");

--
-- Name: DeviceGroup_name_key; Type: INDEX; Schema: public; Owner: -
--

CREATE UNIQUE INDEX "DeviceGroup_name_key" ON public."DeviceGroup" USING btree (name);

--
-- Name: HookConfig_name_key; Type: INDEX; Schema: public; Owner: -
--

CREATE UNIQUE INDEX "HookConfig_name_key" ON public."HookConfig" USING btree (name);

--
-- Name: IamGroupMembership_groupId_principalType_principalId_key; Type: INDEX; Schema: public; Owner: -
--

CREATE UNIQUE INDEX "IamGroupMembership_groupId_principalType_principalId_key" ON public."IamGroupMembership" USING btree ("groupId", "principalType", "principalId");

--
-- Name: IamGroupMembership_principalType_principalId_idx; Type: INDEX; Schema: public; Owner: -
--

CREATE INDEX "IamGroupMembership_principalType_principalId_idx" ON public."IamGroupMembership" USING btree ("principalType", "principalId");

--
-- Name: IamGroupPolicyAttachment_groupId_policyId_key; Type: INDEX; Schema: public; Owner: -
--

CREATE UNIQUE INDEX "IamGroupPolicyAttachment_groupId_policyId_key" ON public."IamGroupPolicyAttachment" USING btree ("groupId", "policyId");

--
-- Name: IamGroup_name_key; Type: INDEX; Schema: public; Owner: -
--

CREATE UNIQUE INDEX "IamGroup_name_key" ON public."IamGroup" USING btree (name);

--
-- Name: IamGroup_source_identity_provider_id_idx; Type: INDEX; Schema: public; Owner: -
--

CREATE INDEX "IamGroup_source_identity_provider_id_idx" ON public."IamGroup" USING btree (source, identity_provider_id);

--
-- Name: IamPolicyAttachment_principalType_principalId_idx; Type: INDEX; Schema: public; Owner: -
--

CREATE INDEX "IamPolicyAttachment_principalType_principalId_idx" ON public."IamPolicyAttachment" USING btree ("principalType", "principalId");

--
-- Name: IamPolicyAttachment_principalType_principalId_policyId_key; Type: INDEX; Schema: public; Owner: -
--

CREATE UNIQUE INDEX "IamPolicyAttachment_principalType_principalId_policyId_key" ON public."IamPolicyAttachment" USING btree ("principalType", "principalId", "policyId");

--
-- Name: IamPolicy_name_key; Type: INDEX; Schema: public; Owner: -
--

CREATE UNIQUE INDEX "IamPolicy_name_key" ON public."IamPolicy" USING btree (name);

--
-- Name: IamPolicy_type_idx; Type: INDEX; Schema: public; Owner: -
--

CREATE INDEX "IamPolicy_type_idx" ON public."IamPolicy" USING btree (type);

--
-- Name: IdpGroupMapping_iamGroupId_idx; Type: INDEX; Schema: public; Owner: -
--

CREATE INDEX "IdpGroupMapping_iamGroupId_idx" ON public."IdpGroupMapping" USING btree ("iamGroupId");

--
-- Name: IdpGroupMapping_identityProviderId_externalGroupId_key; Type: INDEX; Schema: public; Owner: -
--

CREATE UNIQUE INDEX "IdpGroupMapping_identityProviderId_externalGroupId_key" ON public."IdpGroupMapping" USING btree ("identityProviderId", "externalGroupId");

--
-- Name: MetricRollup_bucketStart_metricName_dimensionKey_key; Type: INDEX; Schema: public; Owner: -
--

CREATE UNIQUE INDEX "MetricRollup_bucketStart_metricName_dimensionKey_key" ON public."MetricRollup" USING btree ("bucketStart", "metricName", "dimensionKey");

--
-- Name: MetricRollup_metricName_bucketStart_idx; Type: INDEX; Schema: public; Owner: -
--

CREATE INDEX "MetricRollup_metricName_bucketStart_idx" ON public."MetricRollup" USING btree ("metricName", "bucketStart");

--
-- Name: ModelPricing_modelId_idx; Type: INDEX; Schema: public; Owner: -
--

CREATE INDEX "ModelPricing_modelId_idx" ON public."ModelPricing" USING btree ("modelId");

--
-- Name: ModelPricing_modelId_key; Type: INDEX; Schema: public; Owner: -
--

CREATE UNIQUE INDEX "ModelPricing_modelId_key" ON public."ModelPricing" USING btree ("modelId");

--
-- Name: Model_code_key; Type: INDEX; Schema: public; Owner: -
--

CREATE UNIQUE INDEX "Model_code_key" ON public."Model" USING btree (code);

--
-- Name: Model_providerId_idx; Type: INDEX; Schema: public; Owner: -
--

CREATE INDEX "Model_providerId_idx" ON public."Model" USING btree ("providerId");

--
-- Name: Model_providerId_providerModelId_key; Type: INDEX; Schema: public; Owner: -
--

CREATE UNIQUE INDEX "Model_providerId_providerModelId_key" ON public."Model" USING btree ("providerId", "providerModelId");

--
-- Name: Model_status_idx; Type: INDEX; Schema: public; Owner: -
--

CREATE INDEX "Model_status_idx" ON public."Model" USING btree (status);

--
-- Name: Model_type_idx; Type: INDEX; Schema: public; Owner: -
--

CREATE INDEX "Model_type_idx" ON public."Model" USING btree (type);

--
-- Name: NexusUser_organizationId_email_key; Type: INDEX; Schema: public; Owner: -
--

CREATE UNIQUE INDEX "NexusUser_organizationId_email_key" ON public."NexusUser" USING btree ("organizationId", email);

--
-- Name: NexusUser_organizationId_osUsername_osDomain_key; Type: INDEX; Schema: public; Owner: -
--

CREATE UNIQUE INDEX "NexusUser_organizationId_osUsername_osDomain_key" ON public."NexusUser" USING btree ("organizationId", "osUsername", "osDomain");

--
-- Name: NexusUser_organizationId_status_idx; Type: INDEX; Schema: public; Owner: -
--

CREATE INDEX "NexusUser_organizationId_status_idx" ON public."NexusUser" USING btree ("organizationId", status);

--
-- Name: Organization_code_key; Type: INDEX; Schema: public; Owner: -
--

CREATE UNIQUE INDEX "Organization_code_key" ON public."Organization" USING btree (code);

--
-- Name: Organization_parentId_idx; Type: INDEX; Schema: public; Owner: -
--

CREATE INDEX "Organization_parentId_idx" ON public."Organization" USING btree ("parentId");

--
-- Name: Project_code_key; Type: INDEX; Schema: public; Owner: -
--

CREATE UNIQUE INDEX "Project_code_key" ON public."Project" USING btree (code);

--
-- Name: Project_organizationId_idx; Type: INDEX; Schema: public; Owner: -
--

CREATE INDEX "Project_organizationId_idx" ON public."Project" USING btree ("organizationId");

--
-- Name: ProviderHealth_providerId_key; Type: INDEX; Schema: public; Owner: -
--

CREATE UNIQUE INDEX "ProviderHealth_providerId_key" ON public."ProviderHealth" USING btree ("providerId");

--
-- Name: Provider_name_key; Type: INDEX; Schema: public; Owner: -
--

CREATE UNIQUE INDEX "Provider_name_key" ON public."Provider" USING btree (name);

--
-- Name: Provider_pathPrefix_key; Type: INDEX; Schema: public; Owner: -
--

CREATE UNIQUE INDEX "Provider_pathPrefix_key" ON public."Provider" USING btree ("pathPrefix");

--
-- Name: QuotaOverride_targetType_idx; Type: INDEX; Schema: public; Owner: -
--

CREATE INDEX "QuotaOverride_targetType_idx" ON public."QuotaOverride" USING btree ("targetType");

--
-- Name: QuotaOverride_targetType_targetId_key; Type: INDEX; Schema: public; Owner: -
--

CREATE UNIQUE INDEX "QuotaOverride_targetType_targetId_key" ON public."QuotaOverride" USING btree ("targetType", "targetId");

--
-- Name: QuotaPolicy_enabled_idx; Type: INDEX; Schema: public; Owner: -
--

CREATE INDEX "QuotaPolicy_enabled_idx" ON public."QuotaPolicy" USING btree (enabled);

--
-- Name: QuotaPolicy_scope_idx; Type: INDEX; Schema: public; Owner: -
--

CREATE INDEX "QuotaPolicy_scope_idx" ON public."QuotaPolicy" USING btree (scope);

--
-- Name: RefreshToken_expiresAt_idx; Type: INDEX; Schema: public; Owner: -
--

CREATE INDEX "RefreshToken_expiresAt_idx" ON public."RefreshToken" USING btree ("expiresAt");

--
-- Name: RefreshToken_sessionId_idx; Type: INDEX; Schema: public; Owner: -
--

CREATE INDEX "RefreshToken_sessionId_idx" ON public."RefreshToken" USING btree ("sessionId");

--
-- Name: RefreshToken_userId_idx; Type: INDEX; Schema: public; Owner: -
--

CREATE INDEX "RefreshToken_userId_idx" ON public."RefreshToken" USING btree ("userId");

--
-- Name: RevokedToken_expiresAt_idx; Type: INDEX; Schema: public; Owner: -
--

CREATE INDEX "RevokedToken_expiresAt_idx" ON public."RevokedToken" USING btree ("expiresAt");

--
-- Name: RevokedToken_targetDeviceId_idx; Type: INDEX; Schema: public; Owner: -
--

CREATE INDEX "RevokedToken_targetDeviceId_idx" ON public."RevokedToken" USING btree ("targetDeviceId") WHERE ("targetDeviceId" IS NOT NULL);

--
-- Name: RevokedToken_targetSessionId_idx; Type: INDEX; Schema: public; Owner: -
--

CREATE INDEX "RevokedToken_targetSessionId_idx" ON public."RevokedToken" USING btree ("targetSessionId") WHERE ("targetSessionId" IS NOT NULL);

--
-- Name: RevokedToken_targetUserId_idx; Type: INDEX; Schema: public; Owner: -
--

CREATE INDEX "RevokedToken_targetUserId_idx" ON public."RevokedToken" USING btree ("targetUserId") WHERE ("targetUserId" IS NOT NULL);

--
-- Name: RoutingRule_enabled_idx; Type: INDEX; Schema: public; Owner: -
--

CREATE INDEX "RoutingRule_enabled_idx" ON public."RoutingRule" USING btree (enabled);

--
-- Name: RoutingRule_enabled_pipelineStage_priority_idx; Type: INDEX; Schema: public; Owner: -
--

CREATE INDEX "RoutingRule_enabled_pipelineStage_priority_idx" ON public."RoutingRule" USING btree (enabled, "pipelineStage", priority);

--
-- Name: RoutingRule_name_key; Type: INDEX; Schema: public; Owner: -
--

CREATE UNIQUE INDEX "RoutingRule_name_key" ON public."RoutingRule" USING btree (name);

--
-- Name: RoutingRule_priority_idx; Type: INDEX; Schema: public; Owner: -
--

CREATE INDEX "RoutingRule_priority_idx" ON public."RoutingRule" USING btree (priority);

--
-- Name: ScimToken_identityProviderId_idx; Type: INDEX; Schema: public; Owner: -
--

CREATE INDEX "ScimToken_identityProviderId_idx" ON public."ScimToken" USING btree ("identityProviderId");

--
-- Name: ScimToken_tokenHash_key; Type: INDEX; Schema: public; Owner: -
--

CREATE UNIQUE INDEX "ScimToken_tokenHash_key" ON public."ScimToken" USING btree ("tokenHash");

--
-- Name: UserFederatedIdentity_idpId_externalSubject_key; Type: INDEX; Schema: public; Owner: -
--

CREATE UNIQUE INDEX "UserFederatedIdentity_idpId_externalSubject_key" ON public."UserFederatedIdentity" USING btree ("idpId", "externalSubject");

--
-- Name: UserFederatedIdentity_userId_idx; Type: INDEX; Schema: public; Owner: -
--

CREATE INDEX "UserFederatedIdentity_userId_idx" ON public."UserFederatedIdentity" USING btree ("userId");

--
-- Name: VirtualKey_keyHash_idx; Type: INDEX; Schema: public; Owner: -
--

CREATE INDEX "VirtualKey_keyHash_idx" ON public."VirtualKey" USING btree ("keyHash");

--
-- Name: VirtualKey_keyHash_key; Type: INDEX; Schema: public; Owner: -
--

CREATE UNIQUE INDEX "VirtualKey_keyHash_key" ON public."VirtualKey" USING btree ("keyHash");

--
-- Name: VirtualKey_name_idx; Type: INDEX; Schema: public; Owner: -
--

CREATE INDEX "VirtualKey_name_idx" ON public."VirtualKey" USING btree (name);

--
-- Name: VirtualKey_ownerId_idx; Type: INDEX; Schema: public; Owner: -
--

CREATE INDEX "VirtualKey_ownerId_idx" ON public."VirtualKey" USING btree ("ownerId");

--
-- Name: VirtualKey_projectId_idx; Type: INDEX; Schema: public; Owner: -
--

CREATE INDEX "VirtualKey_projectId_idx" ON public."VirtualKey" USING btree ("projectId");

--
-- Name: VirtualKey_vkStatus_idx; Type: INDEX; Schema: public; Owner: -
--

CREATE INDEX "VirtualKey_vkStatus_idx" ON public."VirtualKey" USING btree ("vkStatus");

--
-- Name: VirtualKey_vkType_idx; Type: INDEX; Schema: public; Owner: -
--

CREATE INDEX "VirtualKey_vkType_idx" ON public."VirtualKey" USING btree ("vkType");

--
-- Name: compliance_exemption_grant_effective_from_expires_at_idx; Type: INDEX; Schema: public; Owner: -
--

CREATE INDEX compliance_exemption_grant_effective_from_expires_at_idx ON public.compliance_exemption_grant USING btree (effective_from, expires_at);

--
-- Name: compliance_exemption_grant_exemption_request_id_idx; Type: INDEX; Schema: public; Owner: -
--

CREATE INDEX compliance_exemption_grant_exemption_request_id_idx ON public.compliance_exemption_grant USING btree (exemption_request_id);

--
-- Name: compliance_exemption_grant_expires_at_idx; Type: INDEX; Schema: public; Owner: -
--

CREATE INDEX compliance_exemption_grant_expires_at_idx ON public.compliance_exemption_grant USING btree (expires_at DESC);

--
-- Name: config_change_event_actor_id_timestamp_idx; Type: INDEX; Schema: public; Owner: -
--

CREATE INDEX config_change_event_actor_id_timestamp_idx ON public.config_change_event USING btree (actor_id, "timestamp");

--
-- Name: config_change_event_config_key_timestamp_idx; Type: INDEX; Schema: public; Owner: -
--

CREATE INDEX config_change_event_config_key_timestamp_idx ON public.config_change_event USING btree (config_key, "timestamp");

--
-- Name: config_change_event_thing_type_timestamp_idx; Type: INDEX; Schema: public; Owner: -
--

CREATE INDEX config_change_event_thing_type_timestamp_idx ON public.config_change_event USING btree (thing_type, "timestamp");

--
-- Name: dsar_request_status_createdAt_idx; Type: INDEX; Schema: public; Owner: -
--

CREATE INDEX "dsar_request_status_createdAt_idx" ON public.dsar_request USING btree (status, "createdAt");

--
-- Name: dsar_request_subject_id_idx; Type: INDEX; Schema: public; Owner: -
--

CREATE INDEX dsar_request_subject_id_idx ON public.dsar_request USING btree (subject_id);

--
-- Name: enrollment_token_token_hash_key; Type: INDEX; Schema: public; Owner: -
--

CREATE UNIQUE INDEX enrollment_token_token_hash_key ON public.enrollment_token USING btree (token_hash);

--
-- Name: exemption_request_status_createdAt_idx; Type: INDEX; Schema: public; Owner: -
--

CREATE INDEX "exemption_request_status_createdAt_idx" ON public.exemption_request USING btree (status, "createdAt");

--
-- Name: idx_diag_crash_cohort; Type: INDEX; Schema: public; Owner: -
--

CREATE INDEX idx_diag_crash_cohort ON public.thing_diag_event USING btree (agent_version, ((os_info ->> 'os'::text)), occurred_at DESC) WHERE (event_type = 'crash'::text);

--
-- Name: idx_ops_1d_fleet_time; Type: INDEX; Schema: public; Owner: -
--

CREATE INDEX idx_ops_1d_fleet_time ON public.metric_ops_rollup_1d USING btree (thing_type, bucket_start DESC) WHERE (thing_id IS NULL);

--
-- Name: idx_ops_1d_thing_time; Type: INDEX; Schema: public; Owner: -
--

CREATE INDEX idx_ops_1d_thing_time ON public.metric_ops_rollup_1d USING btree (thing_id, bucket_start DESC) WHERE (thing_id IS NOT NULL);

--
-- Name: idx_ops_1h_fleet_time; Type: INDEX; Schema: public; Owner: -
--

CREATE INDEX idx_ops_1h_fleet_time ON public.metric_ops_rollup_1h USING btree (thing_type, bucket_start DESC) WHERE (thing_id IS NULL);

--
-- Name: idx_ops_1h_thing_time; Type: INDEX; Schema: public; Owner: -
--

CREATE INDEX idx_ops_1h_thing_time ON public.metric_ops_rollup_1h USING btree (thing_id, bucket_start DESC) WHERE (thing_id IS NOT NULL);

--
-- Name: idx_ops_1mo_fleet_time; Type: INDEX; Schema: public; Owner: -
--

CREATE INDEX idx_ops_1mo_fleet_time ON public.metric_ops_rollup_1mo USING btree (thing_type, bucket_start DESC) WHERE (thing_id IS NULL);

--
-- Name: idx_ops_1mo_thing_time; Type: INDEX; Schema: public; Owner: -
--

CREATE INDEX idx_ops_1mo_thing_time ON public.metric_ops_rollup_1mo USING btree (thing_id, bucket_start DESC) WHERE (thing_id IS NOT NULL);

--
-- Name: idx_tco_expires; Type: INDEX; Schema: public; Owner: -
--

CREATE INDEX idx_tco_expires ON public.thing_config_override USING btree (expires_at) WHERE (expires_at IS NOT NULL);

--
-- Name: idx_traffic_event_api_key_fingerprint_timestamp; Type: INDEX; Schema: public; Owner: -
--

CREATE INDEX idx_traffic_event_api_key_fingerprint_timestamp ON public.traffic_event USING btree (api_key_fingerprint, "timestamp") WHERE (api_key_fingerprint IS NOT NULL);

--
-- Name: interception_domain_enabled_priority_idx; Type: INDEX; Schema: public; Owner: -
--

CREATE INDEX interception_domain_enabled_priority_idx ON public.interception_domain USING btree (enabled, priority);

--
-- Name: interception_domain_name_key; Type: INDEX; Schema: public; Owner: -
--

CREATE UNIQUE INDEX interception_domain_name_key ON public.interception_domain USING btree (name);

--
-- Name: interception_path_domain_id_enabled_priority_idx; Type: INDEX; Schema: public; Owner: -
--

CREATE INDEX interception_path_domain_id_enabled_priority_idx ON public.interception_path USING btree (domain_id, enabled, priority);

--
-- Name: job_run_jobId_startedAt_idx; Type: INDEX; Schema: public; Owner: -
--

CREATE INDEX "job_run_jobId_startedAt_idx" ON public.job_run USING btree ("jobId", "startedAt" DESC);

--
-- Name: metric_ops_raw_metric_name_sampled_at_idx; Type: INDEX; Schema: public; Owner: -
--

CREATE INDEX metric_ops_raw_metric_name_sampled_at_idx ON public.metric_ops_raw USING btree (metric_name, sampled_at DESC);

--
-- Name: metric_ops_raw_sampled_at_thing_id_metric_name_dimension_ke_key; Type: INDEX; Schema: public; Owner: -
--

CREATE UNIQUE INDEX metric_ops_raw_sampled_at_thing_id_metric_name_dimension_ke_key ON public.metric_ops_raw USING btree (sampled_at, thing_id, metric_name, dimension_key);

--
-- Name: metric_ops_raw_thing_id_sampled_at_idx; Type: INDEX; Schema: public; Owner: -
--

CREATE INDEX metric_ops_raw_thing_id_sampled_at_idx ON public.metric_ops_raw USING btree (thing_id, sampled_at DESC);

--
-- Name: metric_ops_raw_thing_type_sampled_at_idx; Type: INDEX; Schema: public; Owner: -
--

CREATE INDEX metric_ops_raw_thing_type_sampled_at_idx ON public.metric_ops_raw USING btree (thing_type, sampled_at DESC);

--
-- Name: metric_ops_rollup_1d_metric_name_bucket_start_idx; Type: INDEX; Schema: public; Owner: -
--

CREATE INDEX metric_ops_rollup_1d_metric_name_bucket_start_idx ON public.metric_ops_rollup_1d USING btree (metric_name, bucket_start DESC);

--
-- Name: metric_ops_rollup_1h_metric_name_bucket_start_idx; Type: INDEX; Schema: public; Owner: -
--

CREATE INDEX metric_ops_rollup_1h_metric_name_bucket_start_idx ON public.metric_ops_rollup_1h USING btree (metric_name, bucket_start DESC);

--
-- Name: metric_ops_rollup_1mo_metric_name_bucket_start_idx; Type: INDEX; Schema: public; Owner: -
--

CREATE INDEX metric_ops_rollup_1mo_metric_name_bucket_start_idx ON public.metric_ops_rollup_1mo USING btree (metric_name, bucket_start DESC);

--
-- Name: metric_rollup_1d_bucketStart_metricName_dimensionKey_subDim_key; Type: INDEX; Schema: public; Owner: -
--

CREATE UNIQUE INDEX "metric_rollup_1d_bucketStart_metricName_dimensionKey_subDim_key" ON public.metric_rollup_1d USING btree ("bucketStart", "metricName", "dimensionKey", "subDimension");

--
-- Name: metric_rollup_1d_dimensionKey_bucketStart_idx; Type: INDEX; Schema: public; Owner: -
--

CREATE INDEX "metric_rollup_1d_dimensionKey_bucketStart_idx" ON public.metric_rollup_1d USING btree ("dimensionKey", "bucketStart");

--
-- Name: metric_rollup_1d_metricName_bucketStart_idx; Type: INDEX; Schema: public; Owner: -
--

CREATE INDEX "metric_rollup_1d_metricName_bucketStart_idx" ON public.metric_rollup_1d USING btree ("metricName", "bucketStart");

--
-- Name: metric_rollup_1h_bucketStart_metricName_dimensionKey_subDim_key; Type: INDEX; Schema: public; Owner: -
--

CREATE UNIQUE INDEX "metric_rollup_1h_bucketStart_metricName_dimensionKey_subDim_key" ON public.metric_rollup_1h USING btree ("bucketStart", "metricName", "dimensionKey", "subDimension");

--
-- Name: metric_rollup_1h_dimensionKey_bucketStart_idx; Type: INDEX; Schema: public; Owner: -
--

CREATE INDEX "metric_rollup_1h_dimensionKey_bucketStart_idx" ON public.metric_rollup_1h USING btree ("dimensionKey", "bucketStart");

--
-- Name: metric_rollup_1h_metricName_bucketStart_idx; Type: INDEX; Schema: public; Owner: -
--

CREATE INDEX "metric_rollup_1h_metricName_bucketStart_idx" ON public.metric_rollup_1h USING btree ("metricName", "bucketStart");

--
-- Name: metric_rollup_1mo_bucketStart_metricName_dimensionKey_subDi_key; Type: INDEX; Schema: public; Owner: -
--

CREATE UNIQUE INDEX "metric_rollup_1mo_bucketStart_metricName_dimensionKey_subDi_key" ON public.metric_rollup_1mo USING btree ("bucketStart", "metricName", "dimensionKey", "subDimension");

--
-- Name: metric_rollup_1mo_dimensionKey_bucketStart_idx; Type: INDEX; Schema: public; Owner: -
--

CREATE INDEX "metric_rollup_1mo_dimensionKey_bucketStart_idx" ON public.metric_rollup_1mo USING btree ("dimensionKey", "bucketStart");

--
-- Name: metric_rollup_1mo_metricName_bucketStart_idx; Type: INDEX; Schema: public; Owner: -
--

CREATE INDEX "metric_rollup_1mo_metricName_bucketStart_idx" ON public.metric_rollup_1mo USING btree ("metricName", "bucketStart");

--
-- Name: metric_rollup_5m_bucketStart_metricName_dimensionKey_subDim_key; Type: INDEX; Schema: public; Owner: -
--

CREATE UNIQUE INDEX "metric_rollup_5m_bucketStart_metricName_dimensionKey_subDim_key" ON public.metric_rollup_5m USING btree ("bucketStart", "metricName", "dimensionKey", "subDimension");

--
-- Name: metric_rollup_5m_dimensionKey_bucketStart_idx; Type: INDEX; Schema: public; Owner: -
--

CREATE INDEX "metric_rollup_5m_dimensionKey_bucketStart_idx" ON public.metric_rollup_5m USING btree ("dimensionKey", "bucketStart");

--
-- Name: metric_rollup_5m_metricName_bucketStart_idx; Type: INDEX; Schema: public; Owner: -
--

CREATE INDEX "metric_rollup_5m_metricName_bucketStart_idx" ON public.metric_rollup_5m USING btree ("metricName", "bucketStart");

--
-- Name: provider_pricing_adapter_type_priority_idx; Type: INDEX; Schema: public; Owner: -
--

CREATE INDEX provider_pricing_adapter_type_priority_idx ON public.provider_pricing USING btree (adapter_type, priority DESC);

--
-- Name: provider_pricing_provider_id_priority_idx; Type: INDEX; Schema: public; Owner: -
--

CREATE INDEX provider_pricing_provider_id_priority_idx ON public.provider_pricing USING btree (provider_id, priority DESC);

--
-- Name: rule_override_installId_ruleLocalId_key; Type: INDEX; Schema: public; Owner: -
--

CREATE UNIQUE INDEX "rule_override_installId_ruleLocalId_key" ON public.rule_override USING btree ("installId", "ruleLocalId");

--
-- Name: rule_packId_ruleId_key; Type: INDEX; Schema: public; Owner: -
--

CREATE UNIQUE INDEX "rule_packId_ruleId_key" ON public.rule USING btree ("packId", "ruleId");

--
-- Name: rule_pack_name_version_key; Type: INDEX; Schema: public; Owner: -
--

CREATE UNIQUE INDEX rule_pack_name_version_key ON public.rule_pack USING btree (name, version);

--
-- Name: thing_agent_cert_expires_at_idx; Type: INDEX; Schema: public; Owner: -
--

CREATE INDEX thing_agent_cert_expires_at_idx ON public.thing_agent USING btree (cert_expires_at);

--
-- Name: thing_agent_cert_serial_key; Type: INDEX; Schema: public; Owner: -
--

CREATE UNIQUE INDEX thing_agent_cert_serial_key ON public.thing_agent USING btree (cert_serial);

--
-- Name: thing_config_override_set_at_idx; Type: INDEX; Schema: public; Owner: -
--

CREATE INDEX thing_config_override_set_at_idx ON public.thing_config_override USING btree (set_at DESC);

--
-- Name: thing_config_override_thing_id_idx; Type: INDEX; Schema: public; Owner: -
--

CREATE INDEX thing_config_override_thing_id_idx ON public.thing_config_override USING btree (thing_id);

--
-- Name: thing_config_template_type_idx; Type: INDEX; Schema: public; Owner: -
--

CREATE INDEX thing_config_template_type_idx ON public.thing_config_template USING btree (type);

--
-- Name: thing_diag_event_event_type_occurred_at_idx; Type: INDEX; Schema: public; Owner: -
--

CREATE INDEX thing_diag_event_event_type_occurred_at_idx ON public.thing_diag_event USING btree (event_type, occurred_at DESC);

--
-- Name: thing_diag_event_level_occurred_at_idx; Type: INDEX; Schema: public; Owner: -
--

CREATE INDEX thing_diag_event_level_occurred_at_idx ON public.thing_diag_event USING btree (level, occurred_at DESC);

--
-- Name: thing_diag_event_message_hash_occurred_at_idx; Type: INDEX; Schema: public; Owner: -
--

CREATE INDEX thing_diag_event_message_hash_occurred_at_idx ON public.thing_diag_event USING btree (message_hash, occurred_at DESC);

--
-- Name: thing_diag_event_thing_id_occurred_at_idx; Type: INDEX; Schema: public; Owner: -
--

CREATE INDEX thing_diag_event_thing_id_occurred_at_idx ON public.thing_diag_event USING btree (thing_id, occurred_at DESC);

--
-- Name: thing_diag_mode_window_ended_at_idx; Type: INDEX; Schema: public; Owner: -
--

CREATE INDEX thing_diag_mode_window_ended_at_idx ON public.thing_diag_mode_window USING btree (ended_at);

--
-- Name: thing_diag_mode_window_thing_id_started_at_idx; Type: INDEX; Schema: public; Owner: -
--

CREATE INDEX thing_diag_mode_window_thing_id_started_at_idx ON public.thing_diag_mode_window USING btree (thing_id, started_at DESC);

--
-- Name: thing_status_last_seen_at_idx; Type: INDEX; Schema: public; Owner: -
--

CREATE INDEX thing_status_last_seen_at_idx ON public.thing USING btree (status, last_seen_at);

--
-- Name: thing_type_status_idx; Type: INDEX; Schema: public; Owner: -
--

CREATE INDEX thing_type_status_idx ON public.thing USING btree (type, status);

--
-- Name: traffic_event_credential_health_rollup_idx; Type: INDEX; Schema: public; Owner: -
--

CREATE INDEX traffic_event_credential_health_rollup_idx ON public.traffic_event USING btree ("timestamp" DESC, credential_id) WHERE ((source = 'ai-gateway'::text) AND (credential_id IS NOT NULL));

--
-- Name: traffic_event_entity_id_timestamp_idx; Type: INDEX; Schema: public; Owner: -
--

CREATE INDEX traffic_event_entity_id_timestamp_idx ON public.traffic_event USING btree (entity_id, "timestamp");

--
-- Name: traffic_event_normalized_request_status_idx; Type: INDEX; Schema: public; Owner: -
--

CREATE INDEX traffic_event_normalized_request_status_idx ON public.traffic_event_normalized USING btree (request_status);

--
-- Name: traffic_event_normalized_response_status_idx; Type: INDEX; Schema: public; Owner: -
--

CREATE INDEX traffic_event_normalized_response_status_idx ON public.traffic_event_normalized USING btree (response_status);

--
-- Name: traffic_event_org_id_timestamp_idx; Type: INDEX; Schema: public; Owner: -
--

CREATE INDEX traffic_event_org_id_timestamp_idx ON public.traffic_event USING btree (org_id, "timestamp");

--
-- Name: traffic_event_provider_id_timestamp_idx; Type: INDEX; Schema: public; Owner: -
--

CREATE INDEX traffic_event_provider_id_timestamp_idx ON public.traffic_event USING btree (provider_id, "timestamp");

--
-- Name: traffic_event_provider_name_timestamp_idx; Type: INDEX; Schema: public; Owner: -
--

CREATE INDEX traffic_event_provider_name_timestamp_idx ON public.traffic_event USING btree (provider_name, "timestamp");

--
-- Name: traffic_event_request_hook_decision_timestamp_idx; Type: INDEX; Schema: public; Owner: -
--

CREATE INDEX traffic_event_request_hook_decision_timestamp_idx ON public.traffic_event USING btree (request_hook_decision, "timestamp");

--
-- Name: traffic_event_response_hook_decision_timestamp_idx; Type: INDEX; Schema: public; Owner: -
--

CREATE INDEX traffic_event_response_hook_decision_timestamp_idx ON public.traffic_event USING btree (response_hook_decision, "timestamp");

--
-- Name: traffic_event_source_timestamp_idx; Type: INDEX; Schema: public; Owner: -
--

CREATE INDEX traffic_event_source_timestamp_idx ON public.traffic_event USING btree (source, "timestamp");

--
-- Name: traffic_event_target_host_timestamp_idx; Type: INDEX; Schema: public; Owner: -
--

CREATE INDEX traffic_event_target_host_timestamp_idx ON public.traffic_event USING btree (target_host, "timestamp");

--
-- Name: traffic_event_thing_id_timestamp_idx; Type: INDEX; Schema: public; Owner: -
--

CREATE INDEX traffic_event_thing_id_timestamp_idx ON public.traffic_event USING btree (thing_id, "timestamp");

--
-- Name: traffic_event_timestamp_idx; Type: INDEX; Schema: public; Owner: -
--

CREATE INDEX traffic_event_timestamp_idx ON public.traffic_event USING btree ("timestamp");

--
-- Name: uq_ops_rollup_1d; Type: INDEX; Schema: public; Owner: -
--

CREATE UNIQUE INDEX uq_ops_rollup_1d ON public.metric_ops_rollup_1d USING btree (bucket_start, COALESCE(thing_id, ''::text), metric_name, dimension_key);

--
-- Name: uq_ops_rollup_1h; Type: INDEX; Schema: public; Owner: -
--

CREATE UNIQUE INDEX uq_ops_rollup_1h ON public.metric_ops_rollup_1h USING btree (bucket_start, COALESCE(thing_id, ''::text), metric_name, dimension_key);

--
-- Name: uq_ops_rollup_1mo; Type: INDEX; Schema: public; Owner: -
--

CREATE UNIQUE INDEX uq_ops_rollup_1mo ON public.metric_ops_rollup_1mo USING btree (bucket_start, COALESCE(thing_id, ''::text), metric_name, dimension_key);

--
-- Name: AdminApiKey AdminApiKey_ownerUserId_fkey; Type: FK CONSTRAINT; Schema: public; Owner: -
--

ALTER TABLE ONLY public."AdminApiKey"
    ADD CONSTRAINT "AdminApiKey_ownerUserId_fkey" FOREIGN KEY ("ownerUserId") REFERENCES public."NexusUser"(id) ON UPDATE CASCADE ON DELETE SET NULL;

--
-- Name: AgentExemption AgentExemption_deviceId_fkey; Type: FK CONSTRAINT; Schema: public; Owner: -
--

ALTER TABLE ONLY public."AgentExemption"
    ADD CONSTRAINT "AgentExemption_deviceId_fkey" FOREIGN KEY ("deviceId") REFERENCES public.thing(id) ON UPDATE CASCADE ON DELETE CASCADE;

--
-- Name: AgentExemption AgentExemption_groupId_fkey; Type: FK CONSTRAINT; Schema: public; Owner: -
--

ALTER TABLE ONLY public."AgentExemption"
    ADD CONSTRAINT "AgentExemption_groupId_fkey" FOREIGN KEY ("groupId") REFERENCES public."DeviceGroup"(id) ON UPDATE CASCADE ON DELETE CASCADE;

--
-- Name: AlertDispatch AlertDispatch_alertId_fkey; Type: FK CONSTRAINT; Schema: public; Owner: -
--

ALTER TABLE ONLY public."AlertDispatch"
    ADD CONSTRAINT "AlertDispatch_alertId_fkey" FOREIGN KEY ("alertId") REFERENCES public."Alert"(id) ON UPDATE CASCADE ON DELETE CASCADE;

--
-- Name: Alert Alert_ruleId_fkey; Type: FK CONSTRAINT; Schema: public; Owner: -
--

ALTER TABLE ONLY public."Alert"
    ADD CONSTRAINT "Alert_ruleId_fkey" FOREIGN KEY ("ruleId") REFERENCES public."AlertRule"(id) ON UPDATE CASCADE ON DELETE RESTRICT;

--
-- Name: Credential Credential_providerId_fkey; Type: FK CONSTRAINT; Schema: public; Owner: -
--

ALTER TABLE ONLY public."Credential"
    ADD CONSTRAINT "Credential_providerId_fkey" FOREIGN KEY ("providerId") REFERENCES public."Provider"(id) ON UPDATE CASCADE ON DELETE CASCADE;

--
-- Name: DeviceAssignment DeviceAssignment_deviceId_fkey; Type: FK CONSTRAINT; Schema: public; Owner: -
--

ALTER TABLE ONLY public."DeviceAssignment"
    ADD CONSTRAINT "DeviceAssignment_deviceId_fkey" FOREIGN KEY ("deviceId") REFERENCES public.thing(id) ON UPDATE CASCADE ON DELETE RESTRICT;

--
-- Name: DeviceAssignment DeviceAssignment_userId_fkey; Type: FK CONSTRAINT; Schema: public; Owner: -
--

ALTER TABLE ONLY public."DeviceAssignment"
    ADD CONSTRAINT "DeviceAssignment_userId_fkey" FOREIGN KEY ("userId") REFERENCES public."NexusUser"(id) ON UPDATE CASCADE ON DELETE SET NULL;

--
-- Name: DeviceGroupMembership DeviceGroupMembership_deviceId_fkey; Type: FK CONSTRAINT; Schema: public; Owner: -
--

ALTER TABLE ONLY public."DeviceGroupMembership"
    ADD CONSTRAINT "DeviceGroupMembership_deviceId_fkey" FOREIGN KEY ("deviceId") REFERENCES public.thing(id) ON UPDATE CASCADE ON DELETE CASCADE;

--
-- Name: DeviceGroupMembership DeviceGroupMembership_groupId_fkey; Type: FK CONSTRAINT; Schema: public; Owner: -
--

ALTER TABLE ONLY public."DeviceGroupMembership"
    ADD CONSTRAINT "DeviceGroupMembership_groupId_fkey" FOREIGN KEY ("groupId") REFERENCES public."DeviceGroup"(id) ON UPDATE CASCADE ON DELETE CASCADE;

--
-- Name: DeviceGroupPolicyRule DeviceGroupPolicyRule_groupId_fkey; Type: FK CONSTRAINT; Schema: public; Owner: -
--

ALTER TABLE ONLY public."DeviceGroupPolicyRule"
    ADD CONSTRAINT "DeviceGroupPolicyRule_groupId_fkey" FOREIGN KEY ("groupId") REFERENCES public."DeviceGroup"(id) ON UPDATE CASCADE ON DELETE CASCADE;

--
-- Name: IamGroupMembership IamGroupMembership_groupId_fkey; Type: FK CONSTRAINT; Schema: public; Owner: -
--

ALTER TABLE ONLY public."IamGroupMembership"
    ADD CONSTRAINT "IamGroupMembership_groupId_fkey" FOREIGN KEY ("groupId") REFERENCES public."IamGroup"(id) ON UPDATE CASCADE ON DELETE CASCADE;

--
-- Name: IamGroupPolicyAttachment IamGroupPolicyAttachment_groupId_fkey; Type: FK CONSTRAINT; Schema: public; Owner: -
--

ALTER TABLE ONLY public."IamGroupPolicyAttachment"
    ADD CONSTRAINT "IamGroupPolicyAttachment_groupId_fkey" FOREIGN KEY ("groupId") REFERENCES public."IamGroup"(id) ON UPDATE CASCADE ON DELETE CASCADE;

--
-- Name: IamGroupPolicyAttachment IamGroupPolicyAttachment_policyId_fkey; Type: FK CONSTRAINT; Schema: public; Owner: -
--

ALTER TABLE ONLY public."IamGroupPolicyAttachment"
    ADD CONSTRAINT "IamGroupPolicyAttachment_policyId_fkey" FOREIGN KEY ("policyId") REFERENCES public."IamPolicy"(id) ON UPDATE CASCADE ON DELETE CASCADE;

--
-- Name: IamGroup IamGroup_identity_provider_id_fkey; Type: FK CONSTRAINT; Schema: public; Owner: -
--

ALTER TABLE ONLY public."IamGroup"
    ADD CONSTRAINT "IamGroup_identity_provider_id_fkey" FOREIGN KEY (identity_provider_id) REFERENCES public."IdentityProvider"(id) ON UPDATE CASCADE ON DELETE SET NULL;

--
-- Name: IamPolicyAttachment IamPolicyAttachment_policyId_fkey; Type: FK CONSTRAINT; Schema: public; Owner: -
--

ALTER TABLE ONLY public."IamPolicyAttachment"
    ADD CONSTRAINT "IamPolicyAttachment_policyId_fkey" FOREIGN KEY ("policyId") REFERENCES public."IamPolicy"(id) ON UPDATE CASCADE ON DELETE CASCADE;

--
-- Name: IdpGroupMapping IdpGroupMapping_iamGroupId_fkey; Type: FK CONSTRAINT; Schema: public; Owner: -
--

ALTER TABLE ONLY public."IdpGroupMapping"
    ADD CONSTRAINT "IdpGroupMapping_iamGroupId_fkey" FOREIGN KEY ("iamGroupId") REFERENCES public."IamGroup"(id) ON UPDATE CASCADE ON DELETE CASCADE;

--
-- Name: IdpGroupMapping IdpGroupMapping_identityProviderId_fkey; Type: FK CONSTRAINT; Schema: public; Owner: -
--

ALTER TABLE ONLY public."IdpGroupMapping"
    ADD CONSTRAINT "IdpGroupMapping_identityProviderId_fkey" FOREIGN KEY ("identityProviderId") REFERENCES public."IdentityProvider"(id) ON UPDATE CASCADE ON DELETE CASCADE;

--
-- Name: Model Model_providerId_fkey; Type: FK CONSTRAINT; Schema: public; Owner: -
--

ALTER TABLE ONLY public."Model"
    ADD CONSTRAINT "Model_providerId_fkey" FOREIGN KEY ("providerId") REFERENCES public."Provider"(id) ON UPDATE CASCADE ON DELETE CASCADE;

--
-- Name: NexusUser NexusUser_organizationId_fkey; Type: FK CONSTRAINT; Schema: public; Owner: -
--

ALTER TABLE ONLY public."NexusUser"
    ADD CONSTRAINT "NexusUser_organizationId_fkey" FOREIGN KEY ("organizationId") REFERENCES public."Organization"(id) ON UPDATE CASCADE ON DELETE RESTRICT;

--
-- Name: Organization Organization_parentId_fkey; Type: FK CONSTRAINT; Schema: public; Owner: -
--

ALTER TABLE ONLY public."Organization"
    ADD CONSTRAINT "Organization_parentId_fkey" FOREIGN KEY ("parentId") REFERENCES public."Organization"(id) ON UPDATE CASCADE ON DELETE SET NULL;

--
-- Name: Project Project_organizationId_fkey; Type: FK CONSTRAINT; Schema: public; Owner: -
--

ALTER TABLE ONLY public."Project"
    ADD CONSTRAINT "Project_organizationId_fkey" FOREIGN KEY ("organizationId") REFERENCES public."Organization"(id) ON UPDATE CASCADE ON DELETE RESTRICT;

--
-- Name: RefreshToken RefreshToken_clientId_fkey; Type: FK CONSTRAINT; Schema: public; Owner: -
--

ALTER TABLE ONLY public."RefreshToken"
    ADD CONSTRAINT "RefreshToken_clientId_fkey" FOREIGN KEY ("clientId") REFERENCES public."OAuthClient"(id) ON UPDATE CASCADE ON DELETE RESTRICT;

--
-- Name: RefreshToken RefreshToken_userId_fkey; Type: FK CONSTRAINT; Schema: public; Owner: -
--

ALTER TABLE ONLY public."RefreshToken"
    ADD CONSTRAINT "RefreshToken_userId_fkey" FOREIGN KEY ("userId") REFERENCES public."NexusUser"(id) ON UPDATE CASCADE ON DELETE CASCADE;

--
-- Name: ScimToken ScimToken_createdBy_fkey; Type: FK CONSTRAINT; Schema: public; Owner: -
--

ALTER TABLE ONLY public."ScimToken"
    ADD CONSTRAINT "ScimToken_createdBy_fkey" FOREIGN KEY ("createdBy") REFERENCES public."NexusUser"(id) ON UPDATE CASCADE ON DELETE RESTRICT;

--
-- Name: ScimToken ScimToken_identityProviderId_fkey; Type: FK CONSTRAINT; Schema: public; Owner: -
--

ALTER TABLE ONLY public."ScimToken"
    ADD CONSTRAINT "ScimToken_identityProviderId_fkey" FOREIGN KEY ("identityProviderId") REFERENCES public."IdentityProvider"(id) ON UPDATE CASCADE ON DELETE SET NULL;

--
-- Name: UserFederatedIdentity UserFederatedIdentity_idpId_fkey; Type: FK CONSTRAINT; Schema: public; Owner: -
--

ALTER TABLE ONLY public."UserFederatedIdentity"
    ADD CONSTRAINT "UserFederatedIdentity_idpId_fkey" FOREIGN KEY ("idpId") REFERENCES public."IdentityProvider"(id) ON UPDATE CASCADE ON DELETE RESTRICT;

--
-- Name: UserFederatedIdentity UserFederatedIdentity_userId_fkey; Type: FK CONSTRAINT; Schema: public; Owner: -
--

ALTER TABLE ONLY public."UserFederatedIdentity"
    ADD CONSTRAINT "UserFederatedIdentity_userId_fkey" FOREIGN KEY ("userId") REFERENCES public."NexusUser"(id) ON UPDATE CASCADE ON DELETE CASCADE;

--
-- Name: VirtualKey VirtualKey_ownerId_fkey; Type: FK CONSTRAINT; Schema: public; Owner: -
--

ALTER TABLE ONLY public."VirtualKey"
    ADD CONSTRAINT "VirtualKey_ownerId_fkey" FOREIGN KEY ("ownerId") REFERENCES public."NexusUser"(id) ON UPDATE CASCADE ON DELETE SET NULL;

--
-- Name: VirtualKey VirtualKey_projectId_fkey; Type: FK CONSTRAINT; Schema: public; Owner: -
--

ALTER TABLE ONLY public."VirtualKey"
    ADD CONSTRAINT "VirtualKey_projectId_fkey" FOREIGN KEY ("projectId") REFERENCES public."Project"(id) ON UPDATE CASCADE ON DELETE SET NULL;

--
-- Name: cache_provider_config cache_provider_config_provider_id_fkey; Type: FK CONSTRAINT; Schema: public; Owner: -
--

ALTER TABLE ONLY public.cache_provider_config
    ADD CONSTRAINT cache_provider_config_provider_id_fkey FOREIGN KEY (provider_id) REFERENCES public."Provider"(id) ON DELETE CASCADE;

--
-- Name: compliance_exemption_grant compliance_exemption_grant_exemption_request_id_fkey; Type: FK CONSTRAINT; Schema: public; Owner: -
--

ALTER TABLE ONLY public.compliance_exemption_grant
    ADD CONSTRAINT compliance_exemption_grant_exemption_request_id_fkey FOREIGN KEY (exemption_request_id) REFERENCES public.exemption_request(id) ON DELETE SET NULL;

--
-- Name: enrollment_token enrollment_token_thing_id_fkey; Type: FK CONSTRAINT; Schema: public; Owner: -
--

ALTER TABLE ONLY public.enrollment_token
    ADD CONSTRAINT enrollment_token_thing_id_fkey FOREIGN KEY (thing_id) REFERENCES public.thing(id) ON UPDATE CASCADE ON DELETE SET NULL;

--
-- Name: interception_path interception_path_domain_id_fkey; Type: FK CONSTRAINT; Schema: public; Owner: -
--

ALTER TABLE ONLY public.interception_path
    ADD CONSTRAINT interception_path_domain_id_fkey FOREIGN KEY (domain_id) REFERENCES public.interception_domain(id) ON UPDATE CASCADE ON DELETE CASCADE;

--
-- Name: job_run job_run_jobId_fkey; Type: FK CONSTRAINT; Schema: public; Owner: -
--

ALTER TABLE ONLY public.job_run
    ADD CONSTRAINT "job_run_jobId_fkey" FOREIGN KEY ("jobId") REFERENCES public.job(id) ON UPDATE CASCADE ON DELETE CASCADE;

--
-- Name: metric_ops_raw metric_ops_raw_thing_id_fkey; Type: FK CONSTRAINT; Schema: public; Owner: -
--

ALTER TABLE ONLY public.metric_ops_raw
    ADD CONSTRAINT metric_ops_raw_thing_id_fkey FOREIGN KEY (thing_id) REFERENCES public.thing(id) ON UPDATE CASCADE ON DELETE CASCADE;

--
-- Name: metric_ops_retention_config metric_ops_retention_config_updated_by_fkey; Type: FK CONSTRAINT; Schema: public; Owner: -
--

ALTER TABLE ONLY public.metric_ops_retention_config
    ADD CONSTRAINT metric_ops_retention_config_updated_by_fkey FOREIGN KEY (updated_by) REFERENCES public."NexusUser"(id) ON UPDATE CASCADE ON DELETE SET NULL;

--
-- Name: provider_pricing provider_pricing_provider_id_fkey; Type: FK CONSTRAINT; Schema: public; Owner: -
--

ALTER TABLE ONLY public.provider_pricing
    ADD CONSTRAINT provider_pricing_provider_id_fkey FOREIGN KEY (provider_id) REFERENCES public."Provider"(id) ON UPDATE CASCADE ON DELETE CASCADE;

--
-- Name: rule_override rule_override_installId_fkey; Type: FK CONSTRAINT; Schema: public; Owner: -
--

ALTER TABLE ONLY public.rule_override
    ADD CONSTRAINT "rule_override_installId_fkey" FOREIGN KEY ("installId") REFERENCES public.rule_pack_install(id) ON UPDATE CASCADE ON DELETE CASCADE;

--
-- Name: rule rule_packId_fkey; Type: FK CONSTRAINT; Schema: public; Owner: -
--

ALTER TABLE ONLY public.rule
    ADD CONSTRAINT "rule_packId_fkey" FOREIGN KEY ("packId") REFERENCES public.rule_pack(id) ON UPDATE CASCADE ON DELETE CASCADE;

--
-- Name: rule_pack_install rule_pack_install_packId_fkey; Type: FK CONSTRAINT; Schema: public; Owner: -
--

ALTER TABLE ONLY public.rule_pack_install
    ADD CONSTRAINT "rule_pack_install_packId_fkey" FOREIGN KEY ("packId") REFERENCES public.rule_pack(id) ON UPDATE CASCADE ON DELETE RESTRICT;

--
-- Name: thing_agent thing_agent_thing_id_fkey; Type: FK CONSTRAINT; Schema: public; Owner: -
--

ALTER TABLE ONLY public.thing_agent
    ADD CONSTRAINT thing_agent_thing_id_fkey FOREIGN KEY (thing_id) REFERENCES public.thing(id) ON UPDATE CASCADE ON DELETE CASCADE;

--
-- Name: thing_config_override thing_config_override_thing_id_fkey; Type: FK CONSTRAINT; Schema: public; Owner: -
--

ALTER TABLE ONLY public.thing_config_override
    ADD CONSTRAINT thing_config_override_thing_id_fkey FOREIGN KEY (thing_id) REFERENCES public.thing(id) ON UPDATE CASCADE ON DELETE CASCADE;

--
-- Name: thing_diag_event thing_diag_event_thing_id_fkey; Type: FK CONSTRAINT; Schema: public; Owner: -
--

ALTER TABLE ONLY public.thing_diag_event
    ADD CONSTRAINT thing_diag_event_thing_id_fkey FOREIGN KEY (thing_id) REFERENCES public.thing(id) ON UPDATE CASCADE ON DELETE CASCADE;

--
-- Name: thing_diag_mode_window thing_diag_mode_window_set_by_fkey; Type: FK CONSTRAINT; Schema: public; Owner: -
--

ALTER TABLE ONLY public.thing_diag_mode_window
    ADD CONSTRAINT thing_diag_mode_window_set_by_fkey FOREIGN KEY (set_by) REFERENCES public."NexusUser"(id) ON UPDATE CASCADE ON DELETE SET NULL;

--
-- Name: thing_diag_mode_window thing_diag_mode_window_thing_id_fkey; Type: FK CONSTRAINT; Schema: public; Owner: -
--

ALTER TABLE ONLY public.thing_diag_mode_window
    ADD CONSTRAINT thing_diag_mode_window_thing_id_fkey FOREIGN KEY (thing_id) REFERENCES public.thing(id) ON UPDATE CASCADE ON DELETE CASCADE;

--
-- Name: thing_service thing_service_thing_id_fkey; Type: FK CONSTRAINT; Schema: public; Owner: -
--

ALTER TABLE ONLY public.thing_service
    ADD CONSTRAINT thing_service_thing_id_fkey FOREIGN KEY (thing_id) REFERENCES public.thing(id) ON UPDATE CASCADE ON DELETE CASCADE;

--
-- Name: traffic_event_normalized traffic_event_normalized_traffic_event_id_fkey; Type: FK CONSTRAINT; Schema: public; Owner: -
--

ALTER TABLE ONLY public.traffic_event_normalized
    ADD CONSTRAINT traffic_event_normalized_traffic_event_id_fkey FOREIGN KEY (traffic_event_id) REFERENCES public.traffic_event(id) ON UPDATE CASCADE ON DELETE CASCADE;

--
-- Name: traffic_event_payload traffic_event_payload_traffic_event_id_fkey; Type: FK CONSTRAINT; Schema: public; Owner: -
--

ALTER TABLE ONLY public.traffic_event_payload
    ADD CONSTRAINT traffic_event_payload_traffic_event_id_fkey FOREIGN KEY (traffic_event_id) REFERENCES public.traffic_event(id) ON UPDATE CASCADE ON DELETE CASCADE;

--
-- PostgreSQL database dump complete
--

