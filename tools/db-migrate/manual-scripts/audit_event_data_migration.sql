-- Data migration: Populate audit_event from existing audit tables.
-- Run after the Prisma structural migration creates the audit_event table.
-- This is a one-time migration; old tables are kept for 30 days then dropped.

-- 1. VK events (from AuditLog)
INSERT INTO audit_event (
  id, source, timestamp, "sourceIp", "targetHost", method, path,
  "statusCode", "latencyMs", "userId", "organizationId", "subjectId",
  "hookDecision", "hookReason", "hookReasonCode", "hooksPipeline",
  "sourceDetails", "createdAt"
)
SELECT
  id, 'vk', timestamp, "sourceIp", provider, method, path,
  "statusCode", "latencyMs", "userId", "organizationId", "userId",
  "hookDecision", "hookReason", "hookReasonCode", "hooksPipeline"::jsonb,
  jsonb_build_object(
    'requestId', "requestId",
    'virtualKeyId', "virtualKeyId",
    'credentialId', "credentialId",
    'provider', provider,
    'routedProvider', "routedProvider",
    'routedModel', "routedModel",
    'routingRuleId', "routingRuleId",
    'modelUsed', "modelUsed",
    'promptTokens', "promptTokens",
    'completionTokens', "completionTokens",
    'totalTokens', "totalTokens",
    'estimatedCostUsd', "estimatedCostUsd",
    'cacheHit', "cacheHit",
    'department', department,
    'sourceApp', "sourceApp",
    'projectId', "projectId"
  ),
  "createdAt"
FROM "AuditLog"
ON CONFLICT (id) DO NOTHING;

-- 2. Proxy events (from matrix_audit_event)
INSERT INTO audit_event (
  id, source, timestamp, "sourceIp", "targetHost", method, path,
  "statusCode", "latencyMs", "subjectId",
  "hookDecision", "hookReason", "hookReasonCode", "hooksPipeline",
  "dataClassification", "sourceDetails", "createdAt"
)
SELECT
  id, 'proxy', timestamp, source_ip, target_host, method, path,
  status_code, latency_ms, subject_id,
  hook_decision, hook_reason, hook_reason_code, hooks_pipeline::jsonb,
  data_classification::text,
  jsonb_build_object(
    'transactionId', transaction_id,
    'connectionId', connection_id,
    'trafficSource', traffic_source::text,
    'ingressType', ingress_type,
    'bumpStatus', bump_status::text,
    'userAgent', user_agent
  ),
  timestamp
FROM matrix_audit_event
ON CONFLICT (id) DO NOTHING;

-- 3. Agent events (from AgentAuditEvent)
INSERT INTO audit_event (
  id, source, timestamp, "deviceId", "targetHost",
  "hookDecision", "subjectId",
  "sourceDetails", "createdAt"
)
SELECT
  id, 'agent', timestamp, "deviceId", "destHost",
  "hookDecision", COALESCE("subjectId", "sourceUser"),
  jsonb_build_object(
    'action', action,
    'sourceProcess', "sourceProcess",
    'sourceUser', "sourceUser",
    'destIp', "destIp",
    'destPort', "destPort",
    'policyRuleId', "policyRuleId",
    'bumpStatus', "bumpStatus",
    'bytesIn', "bytesIn",
    'bytesOut', "bytesOut",
    'durationMs', duration
  ),
  "createdAt"
FROM "AgentAuditEvent"
ON CONFLICT (id) DO NOTHING;
