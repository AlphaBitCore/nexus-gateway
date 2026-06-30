/**
 * TypeScript interfaces for API responses.
 */

import type {
  HookCategory,
  HookClassificationSource,
  HookExecutionFlowKind,
  HookStage,
} from '../constants/hooks';
import { HOOK_EXECUTION_FLOW_KIND } from '../constants/hooks';
import type { Device, AuditEvent } from '@nexus-gateway/ui-shared';

export type { HookCategory, HookClassificationSource, HookExecutionFlowKind, HookStage };
// Re-export the shared base types so consumers that historically
// imported the agent-device-shaped types from this file can keep
// doing so, while the canonical definition lives in
// @nexus-gateway/ui-shared.
export type { Device, AuditEvent };

export interface WhoAmI {
  keyId: string;
  keyName: string;
  roles: string[];
  authPrincipalType?: 'admin_user' | 'api_key';
  /** Present for `admin_user` principals (password login or delegated API key). */
  email?: string | null;
  /** Present when the request was authenticated with an API key delegated to an AdminUser. */
  delegatedFromApiKeyId?: string;
  /** IANA TZ name. Empty / absent → use browser default. */
  preferredTimezone?: string;
}

export interface PaginatedResponse<T> {
  data: T[];
  total: number;
  offset?: number;
  limit?: number;
}


export interface AnalyticsSummary {
  totalRequests: number;
  errorCount: number;
  errorRate: number;
  avgLatencyMs: number;
  p95LatencyMs: number;
  cacheHitRate: number;
  totalTokens: number;
  totalPromptTokens: number;
  totalCompletionTokens: number;
  totalEstimatedCostUsd: number;
}

export interface SparklineBucket {
  bucketStart: string;
  values: {
    request_count?: number;
    status_4xx_count?: number;
    status_5xx_count?: number;
    latency_sum?: number;
    latency_count?: number;
    estimated_cost_usd?: number;
    total_tokens?: number;
    cache_hit_count?: number;
    cache_saved_cost_usd?: number;
    cache_net_savings_usd?: number;
    // Phase metric sums/counts written by the Hub rollup_5m writer.
    latency_us_sum?: number;
    latency_us_count?: number;
    latency_upstream_ttfb_sum?: number;
    latency_upstream_ttfb_count?: number;
    latency_upstream_total_sum?: number;
    latency_upstream_total_count?: number;
    latency_hooks_sum?: number;
    latency_hooks_count?: number;
  };
}

export interface SparklineResponse {
  series: SparklineBucket[];
}

export interface ProviderBreakdown {
  provider: string;
  providerLabel?: string;
  requestCount: number;
  avgLatencyMs: number;
  totalTokens: number;
  totalEstimatedCostUsd: number;
}

export interface UsageData {
  group: string;
  /** Present when `group` is an id (project, virtual key, org, model); use for chart labels. */
  groupLabel?: string;
  /** Supplementary info (e.g. user name for VK/device). */
  groupExtra?: string;
  totalPromptTokens: number;
  totalCompletionTokens: number;
  totalTokens: number;
  requestCount: number;
}

export interface CostData {
  group: string;
  groupLabel?: string;
  groupExtra?: string;
  totalCostUsd: number;
  requestCount: number;
  totalTokens: number;
  cacheHitCount?: number;
  cacheNetSavingsUsd?: number;
  gatewayCacheSavingsUsd?: number;
}

// Organizations & Projects

export interface Organization {
  id: string;
  name: string;
  code: string;
  parentId?: string;
  path?: string;
  description?: string;
  contactName?: string;
  contactEmail?: string;
  contactPhone?: string;
  enabled: boolean;
  timezone: string;
  /** Provisioning source: 'local' | 'idp'. */
  source?: 'local' | 'idp';
  /** External IdP group identifier for IdP-provisioned orgs. */
  externalGroupId?: string;
  createdAt: string;
  updatedAt: string;
  childCount?: number;
  projectCount?: number;
  userCount?: number;
  quotaLimitUsd?: number;
  quotaUsedUsd?: number;
  _count?: { children: number; projects: number };
  children?: Organization[];
  projects?: Project[];
  parent?: { id: string; name: string; code: string } | null;
}

export interface Project {
  id: string;
  name: string;
  code: string;
  organizationId: string;
  organization?: { id: string; name: string; code: string };
  description?: string;
  contactName?: string;
  contactEmail?: string;
  status: string;
  createdAt: string;
  updatedAt: string;
  _count?: { virtualKeys: number };
}


export interface Provider {
  id: string;
  name: string;
  displayName?: string;
  description?: string;
  /**
   * Canonical wire adapter for this provider (see `providers.Format`
   * on the AI Gateway). One of: `openai`, `anthropic`, `gemini`,
   * `glm`, `deepseek`, `azure-openai`, `minimax`, `bedrock`, `vertex`.
   * Always present from the admin API; replaces the legacy `type`
   * column that held `builtin` / `openai-compatible` labels.
   */
  adapterType: string;
  baseUrl: string;
  pathPrefix: string;
  apiVersion?: string | null;
  region?: string | null;
  headers?: Record<string, string> | null;
  enabled: boolean;
  /**
   * Whether this provider's endpoint serves the OpenAI Responses API
   * (`/v1/responses`). `null`/absent means "use the adapter type's default"
   * (the gateway decides from the wire adapter's capabilities). `false`
   * forces the gateway to downgrade to `/v1/chat/completions` for
   * OpenAI-compatible endpoints that only implement chat completions.
   * Downgrade-only: a `true` value cannot make an adapter serve Responses
   * if its wire format does not support it.
   */
  servesResponsesApi?: boolean | null;
  createdAt: string;
  updatedAt?: string;
}

/**
 * Per-endpoint capability block carried in Model.capabilityJson.
 * Mirrors capability.EmbeddingsCapability on the AI Gateway (Go).
 */
export interface ModelEmbeddingsCapability {
  /** Max tokens the model accepts per input string. */
  max_input_tokens?: number;
  /** Supported vector dimensions (e.g. [256, 512, 1536]). Empty = all. */
  supported_dimensions?: number[];
  /** Default dimension when the client omits `dimensions`. */
  default_dimension?: number;
  /** Max number of inputs per batch request. */
  max_batch_size?: number;
  /** Supported encoding formats: float | base64 | int8 | uint8 | binary | ubinary */
  supported_encoding_formats?: string[];
  /** Cohere: search_document | search_query | classification | clustering | query | document */
  supported_input_types?: string[];
  /** Gemini: semantic_similarity | classification | clustering | retrieval_document | retrieval_query */
  supported_task_types?: string[];
  /**
   * Nexus extension keys callers MUST set for this model (e.g.,
   * `nexus.ext.cohere.input_type`, `nexus.ext.gemini.taskType`). Admin-declared;
   * surfaced in the routing-rule available-capabilities panel + 400 envelope
   * when a request is rejected for missing one of these extensions.
   */
  required_extensions?: string[];
}

/**
 * capabilityJson document shape — only the embeddings block is consumed by
 * the routing pre-filter today; future blocks (image, audio) will extend this.
 */
export interface ModelCapabilityJson {
  embeddings?: ModelEmbeddingsCapability;
}

export interface Model {
  id: string;
  code: string;
  name: string;
  description?: string;
  providerId: string;
  providerModelId: string;
  type: string;
  features: string[];
  inputPricePerMillion?: number;
  outputPricePerMillion?: number;
  /** Per-million price for cache READ hits. NULL = no discount; cost calc
   *  falls back to inputPricePerMillion. */
  cachedInputReadPricePerMillion?: number;
  /** Per-million price for cache WRITE creation (Anthropic ~1.25× input).
   *  NULL = no surcharge; cost calc falls back to inputPricePerMillion. */
  cachedInputWritePricePerMillion?: number;
  maxContextTokens?: number;
  maxOutputTokens?: number;
  /** active | deprecated | disabled | preview — may be omitted by older API responses */
  status?: string | null;
  /** ISO 8601 timestamp — set when status is 'deprecated' to indicate the retirement date. */
  deprecationDate?: string | null;
  /** Code of the model that replaces this one (populated when status is 'deprecated'). */
  replacedBy?: string | null;
  aliases?: string[];
  /**
   * Per-endpoint capability matrix. Present when the admin has
   * configured capability data for this model; absent for models without
   * capability overrides. Structured per ModelCapabilityJson.
   */
  capabilityJson?: ModelCapabilityJson | null;
  enabled: boolean;
  createdAt?: string;
  updatedAt?: string;
}

/** POST /api/admin/providers/:id/models */
export interface CreateModelInput {
  name: string;
  providerModelId: string;
  /** Customer-facing identifier in {"model":"..."} requests. Defaults to providerModelId if omitted. */
  code?: string;
  type: string;
  description?: string;
  inputPricePerMillion?: number;
  outputPricePerMillion?: number;
  cachedInputReadPricePerMillion?: number;
  cachedInputWritePricePerMillion?: number;
  maxContextTokens?: number;
  maxOutputTokens?: number;
  features?: string[];
  aliases?: string[];
  /** Per-endpoint capability overrides (e.g. embeddings dimensions / required
   *  extensions), structured per ModelCapabilityJson. */
  capabilityJson?: ModelCapabilityJson | null;
}

/** PUT /api/admin/models/:id */
export type UpdateModelInput = Partial<{
  code: string;
  providerModelId: string;
  name: string;
  description: string;
  type: string;
  features: string[];
  inputPricePerMillion: number;
  outputPricePerMillion: number;
  cachedInputReadPricePerMillion: number;
  cachedInputWritePricePerMillion: number;
  maxContextTokens: number;
  maxOutputTokens: number;
  status: string;
  deprecationDate: string;
  replacedBy: string;
  aliases: string[];
  enabled: boolean;
  /**
   * Capability matrix JSON. Send null to clear the stored document;
   * omit the key entirely to leave it unchanged. Validated server-side
   * against the ModelCapabilityJson shape.
   */
  capabilityJson: ModelCapabilityJson | null;
}>;

/** GET /api/admin/models — grouped by provider */
export interface AdminModelsByProvider {
  provider: {
    id: string;
    name: string;
    displayName?: string | null;
    description?: string | null;
    adapterType: string;
    enabled: boolean;
    modelCount: number;
  };
  models: Model[];
}

export interface HookClassification {
  category: HookCategory;
  categoryLabel: string;
  categorySource: HookClassificationSource;
  implementationId: string | null;
  implementationLabel: string | null;
  logicalHookType?: string | null;
  phase: HookStage;
  phaseLabel: string;
  supportedStages: HookStage[];
  dualPhaseCapable: boolean;
}

/** GET /api/admin/hooks */
export interface AdminHookListResponse {
  data: HookConfig[];
  total: number;
}

/** GET /api/admin/hooks/implementations — registry entry */
export interface HookImplementationSummary {
  implementationId: string;
  hookType: string;
  supportedStages: string[];
  configSchema?: Record<string, unknown>;
}

export interface HookCategoryDefinitionDto {
  code: string;
  name: string;
}

/** GET /api/admin/hooks/implementations */
export interface AdminHookImplementationsResponse {
  data: HookImplementationSummary[];
  hookCategories: HookCategoryDefinitionDto[];
}

export interface HookConfig {
  id: string;
  name: string;
  type: string;
  /** Registered implementation key (gateway dispatch) */
  implementationId?: string;
  stage: string;
  /** When null, category label is derived from built-in implementation registry. */
  category?: string | null;
  endpoint?: string;
  script?: string;
  config?: unknown;
  priority: number;
  timeoutMs: number;
  failBehavior: string;
  enabled: boolean;
  /**
   * Ingress codes this hook applies to. `["ALL"]` matches every ingress
   * type. Other codes: AI_GATEWAY, COMPLIANCE_PROXY, AGENT.
   * Server-side default is `["ALL"]`.
   */
  applicableIngress?: string[];
  createdAt: string;
  updatedAt?: string;
  /** Set by gateway on list/get/create/update */
  classification?: HookClassification;
}

export interface HookChainStep {
  order: number;
  hookConfigId: string;
  name: string;
  priority: number;
  enabled: boolean;
  type: string;
  wired: boolean;
  executionMode: string;
  classification: HookClassification;
}

/** POST /api/admin/hooks/reorder */
export interface HookReorderResponse {
  ok: true;
  stage: string;
  count: number;
}

export interface HookExecutionChain {
  totalHooks: number;
  enabledHooks: number;
  requestHooks: HookChainStep[];
  responseHooks: HookChainStep[];
  flow: Array<
    | { kind: typeof HOOK_EXECUTION_FLOW_KIND.MILESTONE; id: string; label: string }
    | {
        kind: typeof HOOK_EXECUTION_FLOW_KIND.HOOK_SEGMENT;
        id: string;
        label: string;
        phase: HookStage;
        steps: HookChainStep[];
      }
  >;
}

/**
 * Error class string accepted by the data-plane retry pipeline.
 * Backend validates: every entry of `RetryPolicy.retryOn` must be one of these.
 */
export type ErrorClass = 'network' | 'timeout' | '429' | '5xx';

/**
 * Per-rule retry override. Only the two fields the admin UI surfaces are
 * modeled here; backoff knobs are YAML-only by design (see
 * docs/users/api/openapi/admin/e34-s3-routing-retry-policy.yaml §6.3).
 *
 * Wire-shape semantics on PUT /api/admin/routing-rules/{id}:
 *   field absent       → backend leaves the column unchanged
 *   `retryPolicy: null` → backend clears the override (rule inherits YAML default)
 *   `retryPolicy: {…}`  → backend persists the override verbatim
 */
export interface RetryPolicy {
  /** 1-5 inclusive when set. */
  maxAttemptsPerTarget?: number;
  /** Error classes that trigger an L2 retry on the same target. */
  retryOn?: ErrorClass[];
}

export interface RoutingRule {
  id: string;
  name: string;
  description?: string;
  strategyType: string;
  config: unknown;
  matchConditions?: unknown;
  priority: number;
  /** 0 = policy narrowing; 1 = route (default). */
  pipelineStage?: number;
  /** Inline fallback chain: [{ providerId, modelId }] tried in order when primary targets fail. */
  fallbackChain?: Array<{ providerId: string; modelId: string }>;
  /**
   * Persisted retry override. Backend OMITS this field (rather than serializing
   * `null`) when the column is SQL NULL — absence signals "inherit YAML default
   * at runtime". The UI mirrors that semantic via the platform-default radio.
   */
  retryPolicy?: RetryPolicy | null;
  enabled: boolean;
  createdAt: string;
  updatedAt?: string;
}

/** GET /api/admin/routing-rules */
export interface AdminRoutingRuleListResponse {
  data: RoutingRule[];
  total: number;
}

// Traffic / Audit

/**
 * Structured identity snapshot written by the data-plane at event time.
 * Exact sub-fields populated depend on the source:
 *   ai-gateway personal VK:    vk + user + (org)
 *   ai-gateway application VK: vk + project + (org)
 *   agent:                     device + user (when SSO-enrolled)
 *   compliance-proxy:          starts as {status:"pending"}; the Hub
 *                              IdentityEnricher job resolves user/device
 *                              via DeviceAssignment.ip_address + time
 *                              window. Status transitions: pending →
 *                              matched | unmatched | ambiguous.
 * Sub-fields:
 *   vk            — virtual key that authenticated the call.
 *   user          — owning NexusUser (personal VK or matched-by-IP).
 *   project       — owning Project (application VK).
 *   apiCredential — upstream provider credential (orthogonal to caller).
 *   device        — agent Thing (matched-by-IP path).
 *   status        — resolution state ("matched" | "pending" |
 *                   "unmatched" | "ambiguous"). Method/detail are
 *                   diagnostics from the IdentityEnricher.
 */
export interface TrafficIdentity {
  vk?: { id: string; name?: string | null } | null;
  user?: { id: string; name?: string | null } | null;
  project?: { id: string; name?: string | null } | null;
  apiCredential?: { id: string; name?: string | null } | null;
  device?: { id: string; hostname?: string | null } | null;
  status?: 'matched' | 'pending' | 'unmatched' | 'ambiguous' | null;
  method?: 'trace_id' | 'ip_agent' | null;
  detail?: string | null;
}

/**
 * One hook execution recorded inside request_hooks_pipeline /
 * response_hooks_pipeline. The data-plane emitter populates these fields
 * for each HookConfig that ran in the pipeline at the corresponding stage.
 *
 * Different data planes serialize the underlying Go struct with slightly
 * different JSON casing (compliance-proxy uses Go field names — HookName,
 * HookID, … — because shared/hooks.HookResult has no json tags;
 * ai-gateway uses lowerCamel — name, hookId, …). We accept both shapes
 * here so the UI does not care which producer emitted the row; helpers
 * elsewhere in this file resolve the canonical name/id at render time.
 */
export interface HookExecutionRecord {
  // Canonical lowerCamel keys (ai-gateway producer + canonical UI name).
  hookId?: string;
  hookName?: string;
  implementationId?: string;
  name?: string;
  stage?: string;
  order?: number;
  decision?: string;
  reason?: string;
  reasonCode?: string;
  latencyMs?: number;
  // Microsecond-precision per-hook latency (hooks run at microsecond scale, so
  // latencyMs truncates sub-millisecond hooks to 0). Additive; latencyMs kept.
  latencyUs?: number;
  error?: string;
  // PascalCase keys (compliance-proxy producer — shared/hooks.HookResult
  // serializes with Go field names since the struct lacks json tags).
  HookID?: string;
  HookName?: string;
  ImplementationID?: string;
  Decision?: string;
  Reason?: string;
  ReasonCode?: string;
  LatencyMs?: number;
  LatencyUs?: number;
  Order?: number;
  Error?: string;
}

/**
 * Compact attribution for a rule-pack-driven hard/soft reject. Only
 * present when a rule pack actually triggered the rejection; null
 * otherwise.
 */
export interface BlockingRule {
  pack: string;
  packVersion: string;
  ruleId: string;
}

export interface TrafficEvent {
  id: string;
  source: string;
  requestId: string;
  timestamp: string;
  sourceIp?: string | null;
  targetHost?: string | null;
  method?: string | null;
  path?: string | null;
  statusCode?: number | null;
  latencyMs?: number | null;
  // Request tracing
  traceId?: string | null;
  externalRequestId?: string | null;
  // Entity attribution (unified across sources)
  entityType?: 'user' | 'project' | 'device' | 'unknown' | null;
  entityId?: string | null;
  entityName?: string | null;
  orgId?: string | null;
  orgName?: string | null;
  identity?: TrafficIdentity | null;
  // AI/Provider (ID + denormalized name)
  providerId?: string | null;
  providerName?: string | null;
  modelId?: string | null;
  modelName?: string | null;
  promptTokens?: number | null;
  completionTokens?: number | null;
  totalTokens?: number | null;
  // Reasoning tokens (subset of completionTokens) + the cost
  // attributable to them. Surfaced for the Traffic Audit Drawer's
  // "thinking ratio" row + the Cost dashboard reasoning widget.
  // Null when the upstream did not report.
  reasoningTokens?: number | null;
  reasoningCostUsd?: number | null;
  // HTTP method + path the gateway actually sent to upstream
  // provider. May differ from method/path on cross-format routes (e.g.
  // Responses-API upgrade); same as method/path for transparent
  // compliance-proxy + agent traffic.
  targetMethod?: string | null;
  targetPath?: string | null;
  // Unified cache_status (HIT | MISS) per
  // cost-estimation-architecture.md § 6.4. The four detail columns below
  // expose the gateway/provider internal breakdown for the audit drawer.
  cacheStatus?: 'HIT' | 'MISS' | null;
  /** Detail-only: gateway-cache decision (extract hit / singleflight hit_inflight / miss / skipped). */
  gatewayCacheStatus?: 'hit' | 'hit_inflight' | 'miss' | 'skipped' | null;
  /** Populated only when `gatewayCacheStatus = 'skipped'`. */
  gatewayCacheSkipReason?: 'disabled' | 'no_cache' | 'passthrough' | 'not_cacheable' | null;
  /** Populated only when `gatewayCacheStatus ∈ {hit, hit_inflight}`. Today always 'extract'. */
  gatewayCacheKind?: 'extract' | 'semantic' | null;
  /**
   * Redis HASH key of the L2 semantic-cache entry that served this row,
   * format "<redis_index_name>:<sha256(EmbeddingInput)[:16]>". Stamped only
   * when `gatewayCacheKind === 'semantic'`; null elsewhere. Posted as the
   * poison-list `entryKey` by the audit drawer "Mark as bad cache hit"
   * thumbs-down so the gateway's IsPoisoned check actually fires on the
   * next FT.SEARCH hit. Before this field the UI posted `selectedEntry.id`
   * which never matched — silent no-op.
   */
  gatewayCacheL2EntryKey?: string | null;
  /** Provider prompt-cache outcome. `na` = no provider call or model doesn't support prompt cache. */
  providerCacheStatus?: 'hit' | 'miss' | 'na' | null;
  // Prompt-cache metrics
  cacheCreationTokens?: number | null;
  cacheReadTokens?: number | null;
  normalizedStripCount?: number | null;
  normalizedStripBytes?: number | null;
  cacheMarkerInjected?: number | null;
  cacheWriteCostUsd?: number | null;
  cacheReadSavingsUsd?: number | null;
  cacheNetSavingsUsd?: number | null;
  gatewayCacheSavingsUsd?: number | null;
  // Internal-ops cost columns: embeddingCostUsd (produced by L2 reader)
  // + aiGuardCostUsd (added on ai-guard classify rows). Both NULL on rows
  // that didn't trigger the corresponding internal call. embeddingModelId
  // is the FK to the embedding Model row.
  embeddingCostUsd?: number | null;
  embeddingModelId?: string | null;
  aiGuardCostUsd?: number | null;
  // Open-ended catch-all for future hook-type model calls — each item
  // `{type, model, prompt_tokens, completion_tokens, cost_usd, latency_ms}`.
  // Unknown types render as a key/value list in the drawer breakdown.
  internalOpsBreakdown?: Array<{
    type: string;
    model?: string;
    promptTokens?: number;
    completionTokens?: number;
    costUsd?: number;
    latencyMs?: number;
  }> | null;
  // Cost-transparency: per-million pricing the proxy applied (LEFT JOIN
  // against Model via routed_model_id at drawer-fetch time). Used by the
  // "Cost transparency" drawer section to render the math for every traffic
  // event. NULL when the model was deleted post-call or routed_model_id
  // is absent (passthrough).
  modelInputPricePerMillion?: number | null;
  modelOutputPricePerMillion?: number | null;
  modelCachedInputReadPricePerMillion?: number | null;
  modelCachedInputWritePricePerMillion?: number | null;
  routedProviderId?: string | null;
  routedProviderName?: string | null;
  routedModelId?: string | null;
  routedModelName?: string | null;
  routingRuleId?: string | null;
  routingRuleName?: string | null;
  // Compliance — dual pipeline. Each stage records its own
  // decision, reason, reason_code, hooks_pipeline JSONB array, and
  // blocking_rule JSONB. `null` on a stage means it did not run for
  // this row (e.g. cache HIT short-circuits the response stage).
  requestHookDecision?: string | null;
  requestHookReason?: string | null;
  requestHookReasonCode?: string | null;
  requestBlockingRule?: BlockingRule | null;
  responseHookDecision?: string | null;
  responseHookReason?: string | null;
  responseHookReasonCode?: string | null;
  responseBlockingRule?: BlockingRule | null;
  /**
   * Compliance tags (ordered, deduped on the backend). Empty array means no
   * tags were emitted by the hook pipeline. Severity, compliance, detector,
   * and category signals are encoded as namespaced tags such as
   * `severity:confidential` or `compliance:pii`.
   */
  complianceTags?: string[];
  bumpStatus?: string | null;
  /**
   * Agent attestation passthrough. Populated by compliance-proxy
   * when a verified X-Nexus-Attestation header let CP transparently
   * tunnel the CONNECT (skipping its MITM + hook pipeline). Both null
   * on regular MITM rows so the audit-drawer's "Attestation: agent X
   * verified" Block only renders for attested traffic.
   */
  attestationVerified?: boolean | null;
  attestationAgentId?: string | null;
  // LLM signal extraction
  apiKeyClass?: string | null;
  apiKeyFingerprint?: string | null;
  usageExtractionStatus?: string | null;
  // Failure-reason classification. Populated by ai-gateway's
  // writeDetailedErr (RATE_LIMITED / QUOTA_EXCEEDED / ROUTING_NO_MATCH /
  // AUTH_INVALID etc.); compliance-proxy / agent may use their own
  // vocabularies. Both NULL on success AND on raw upstream pass-through.
  errorCode?: string | null;
  errorReason?: string | null;
  sourceProcess?: string | null;
  action?: string | null;
  // Per-stage hook execution lists. Each entry is one
  // HookExecutionRecord; the UI renders them as a timeline.
  requestHooksPipeline?: HookExecutionRecord[] | null;
  responseHooksPipeline?: HookExecutionRecord[] | null;
  routingTrace?: unknown;
  details?: unknown;
  /**
   * Request body captured by the data plane (when payload capture is
   * enabled). Populated only on detail responses; omitted from list
   * payloads to keep them light. Stored as raw bytes (BYTEA) on the
   * server and surfaced through the detail API; the UI renders it
   * generically — could be a JSON object/array, a string, or anything
   * else the API returns.
   */
  requestBody?: unknown;
  /** Response body — same semantics as requestBody. */
  responseBody?: unknown;
  /**
   * Spill reference for the request body. Non-null when the body was
   * stored out-of-band (large payload >= the spill threshold). The CP
   * GetTrafficEvent handler resolves the ref and folds the bytes back
   * onto requestBody, but the raw ref is also returned so the UI can
   * render a "stored externally" badge with size + backend metadata.
   */
  requestSpillRef?: SpillRef | null;
  /** Response body spill reference — same semantics as requestSpillRef. */
  responseSpillRef?: SpillRef | null;
  /**
   * Thing (node/service instance) that emitted this traffic_event. Semantic
   * depends on `source`: agent → originating device; ai-gateway → processing
   * gateway instance; compliance-proxy → processing proxy instance. NULL on
   * rows written before the follow-up wire-up landed.
   */
  thingId?: string | null;
  /** Denormalized thing.name snapshot at emit time (NOT auto-updated on rename). */
  thingName?: string | null;
  createdAt?: string;
  /**
   * Latency phase breakdown. All fields nullable; producers omit when
   * not measured. The UI computes the "Us vs Upstream" split as:
   *   upTotalEffective = upstreamTotalMs ?? upstreamTtfbMs ?? 0
   *   ourOverheadMs    = max(0, (latencyMs ?? 0) - upTotalEffective - hooks)
   * The ttfb fallback covers pre-fix streaming-MISS rows that had
   * `upstream_total_ms = NULL` because the audit defer fired before the
   * broker pump closed the body (phaseTrackedBody now stamps totalMs on
   * every Read, so newly-written rows always have a concrete
   * upstream_total_ms — the fallback only matters for older rows).
   */
  upstreamTtfbMs?: number | null;
  upstreamTotalMs?: number | null;
  requestHooksMs?: number | null;
  responseHooksMs?: number | null;
  // Microsecond-precision hook aggregates (additive; the _ms fields are kept).
  // Hooks run at microsecond scale, so the _ms aggregates floor sub-millisecond
  // hooks to 0; these carry the real value for precise display.
  requestHooksUs?: number | null;
  responseHooksUs?: number | null;
  /**
   * Long-tail phase durations. Values are milliseconds EXCEPT keys ending in
   * `_us`, which are microseconds (the sub-millisecond hook framing segments,
   * recorded in µs so they keep resolution instead of flooring to 0/1).
   * Closed key set per `source`:
   *   ai-gateway       — auth_ms / quota_ms / routing_ms / cache_lookup_ms /
   *                       req_adapter_ms / resp_adapter_ms /
   *                       body_read_ms / norm_upstream_ms /
   *                       upstream_body_ms / audit_emit_ms /
   *                       hook_extract_us / hook_build_us /
   *                       hook_pipeline_us / hook_rewrite_us
   *   compliance-proxy — conn_setup_ms / tls_handshake_ms
   *   agent            — intercept_ms
   * Plus a transient `stream_aborted: 1` marker when the upstream stream
   * was closed by client-side abort. Sub-resolution phases are floored
   * to 1 only when ai-gateway is started with
   * `observability.latencyDetail: true` (yaml-only).
   */
  latencyBreakdown?: Record<string, number> | null;
}

/**
 * SpillRef is the wire form of a body stored out-of-band of
 * traffic_event_payload (large captured request/response bodies). The CP
 * GetTrafficEvent handler resolves the ref and inlines the bytes, but the
 * ref is also returned so the UI can show a "stored externally" badge
 * with size / backend / sha256 metadata.
 */
export interface SpillRef {
  backend: string;
  key: string;
  size: number;
  sha256?: string;
  contentType?: string;
}

export interface AdminAuditEntry {
  id: string;
  timestamp: string;
  actorId: string;
  actorLabel: string;
  actorRole?: string;
  action: string;
  entityType: string;
  entityId?: string;
  nexusRequestId?: string | null;
  /** Legacy rows only; new writes use gateway-resolved actor id only. */
  clientRequestId?: string | null;
  clientUserId?: string | null;
  clientSessionId?: string | null;
}

/** Response from GET /api/admin/admin-audit-logs/export */
export interface AdminAuditExportResponse {
  exportedAt: string;
  truncated: boolean;
  entries: AdminAuditEntry[];
}

export interface AuditStreamStorageInfo {
  enabled: boolean;
  sink: 'database' | 'file';
  filePath: string | null;
  queryable: boolean;
}

export interface TrafficStorageResponse {
  traffic: AuditStreamStorageInfo;
}


export interface SystemSettings {
  uptime: number;
  version: string;
  goVersion: string;
  maintenanceMode: boolean;
  logLevel: string;
}


export interface ProviderHealth {
  providerId: string;
  providerName: string;
  status: string;
  errorRate: number;
  avgLatencyMs: number;
  sampleCount: number;
}

export interface AdminUser {
  id: string;
  displayName: string;
  email?: string;
  status: string;
  canAccessControlPlane: boolean;
  /** Provisioning source: 'local' | 'oidc' | 'saml' | 'scim'. */
  source?: 'local' | 'oidc' | 'saml' | 'scim';
  roles: string[];
  /** Full policy attachments (direct + group-inherited). Present on GET /users/:id detail response. */
  policyAttachments?: IamPolicyAttachment[];
  organizationId?: string;
  organizationName?: string;
  lastLoginAt?: string;
  createdAt: string;
  updatedAt?: string;
}

/**
 * Lifecycle state for an admin API key. Orthogonal to `enabled`, which is
 * the operator's quick-toggle.
 *  - `active`       — accepted by the auth middleware (steady state)
 *  - `rotating`     — superseded by a newer key but still accepted during
 *                     the rotation window (so callers can swap in the new
 *                     value without service interruption)
 *  - `expired`      — natural sunset; rejected by the auth middleware
 *  - `unavailable`  — actively revoked (compromise / withdrawal); rejected
 */
export type AdminApiKeyStatus = 'active' | 'rotating' | 'expired' | 'unavailable';

export interface AdminApiKey {
  id: string;
  name: string;
  keyPrefix: string;
  enabled: boolean;
  status: AdminApiKeyStatus;
  lastUsedAt?: string;
  expiresAt?: string;
  rotatedAt?: string;
  rotatedFromId?: string;
  ownerUserId?: string;
  createdBy?: string;
  createdAt: string;
}

export interface IamUserView extends AdminApiKey {
  groups: Array<{ id: string; name: string; membershipId: string }>;
  directPolicyCount: number;
}

// Virtual Keys

/** Nexus model access entry for a virtual key (provider UUID + gateway model id; modelId may use globs). */
export interface VirtualKeyAllowedModelRef {
  providerId: string;
  modelId: string;
}

export interface VirtualKey {
  id: string;
  name: string;
  keyPrefix?: string;
  projectId?: string;
  project?: { id: string; name: string; organization?: { id: string; name: string } };
  organizationId?: string;
  sourceApp?: string;
  enabled: boolean;
  expiresAt?: string;
  rateLimitRpm?: number;
  allowedModels?: VirtualKeyAllowedModelRef[];
  createdBy?: string;
  createdAt: string;
}


export interface Credential {
  id: string;
  name: string;
  providerId: string;
  enabled: boolean;
  rotationState?: string;
  lastRotatedAt?: string;
  lastUsedAt?: string;
  lastSuccessAt?: string;
  lastFailureAt?: string;
  lastFailureReason?: string;
  totalUsageCount: number;
  expiresAt?: string | null;
  createdAt: string;
  updatedAt?: string;
  // Pool management fields
  selectionWeight?: number;
  status?: string; // active | retiring | retired
  retireAt?: string | null;
  // Circuit breaker durable state. Flushed from Redis to DB on
  // transitions only (default 30s). Live auth_fails counter lives on the
  // liveCircuit object below — never persisted.
  circuitState?: string; // closed | open | half_open
  circuitReason?: string | null; // auth_fail | rate_limit
  circuitOpenedAt?: string | null;
  circuitNextProbeAt?: string | null;
  // Health classification. Rolled up every 5min by Hub.
  healthStatus?: string; // healthy | degraded | unavailable | unknown | collecting
  healthSuccessRate5m?: number | null;
  healthSuccessRate1h?: number | null;
  healthSamplesObserved?: number;
  healthDominantError?: string | null; // none | auth_fail | rate_limit | upstream_5xx | timeout | client_error | mixed
  healthTrend?: string | null;         // improving | stable | degrading
  healthStatusChangedAt?: string | null;
  healthCheckedAt?: string | null;
  // Per-credential reliability threshold overrides. Optional; missing
  // fields fall back to the global Settings page values.
  reliabilityOverrides?: ReliabilityThresholds | null;
  // Live circuit view from Redis. Present when the credential's
  // circuit hash exists in Redis. authFailsCurrent is intentionally not
  // persisted — per-attempt counter would dominate DB writes if flushed.
  liveCircuit?: {
    state: string;
    openReason?: string;
    openedAt?: string;
    nextProbeAt?: string;
    authFailsCurrent: number;
  };
}

// credstate.Thresholds wire shape (all fields optional in
// override mode; complete in global-config mode).
export interface ReliabilityThresholds {
  authFailThreshold?: number;
  rateLimitCooldownSeconds?: number;
  healthyThresholdPct?: number;
  degradedThresholdPct?: number;
  healthMinSamples?: number;
  healthWindowSeconds?: number;
  healthSustainedDegradedSeconds?: number;
}

// Response from POST /api/admin/credentials/:id/probe.
export interface CredentialProbeResult {
  ok: boolean;
  latencyMs: number;
  detail?: string;
  error?: string;
  providerName?: string;
  adapterType?: string;
  credentialId: string;
  credentialName?: string;
  probedAt: string;
}

// Response from GET /api/admin/settings/credential-reliability.
export interface ReliabilityConfigResponse {
  defaults: ReliabilityThresholds;
  override: ReliabilityThresholds;
  effective: ReliabilityThresholds;
}

// Metric rollups

export interface MetricAggregatePoint {
  bucketStart: string;
  metricName: string;
  dimensionKey: string;
  dimensions: Record<string, unknown>;
  value: string;
}

export interface MetricAggregatesResponse {
  data: MetricAggregatePoint[];
}


export interface IamStatement {
  Sid?: string;
  Effect: 'Allow' | 'Deny';
  Action: string[];
  Resource: string[];
  Condition?: Record<string, Record<string, string>>;
}

export interface IamPolicyDocument {
  Version: string;
  Statement: IamStatement[];
}

export interface IamPolicy {
  id: string;
  name: string;
  description: string | null;
  type: 'managed' | 'custom';
  document: IamPolicyDocument;
  enabled: boolean;
  createdBy: string | null;
  createdAt: string;
  updatedAt: string;
}

export interface IamGroup {
  id: string;
  name: string;
  description: string | null;
  memberCount?: number;
  policyCount?: number;
  createdAt: string;
  updatedAt: string;
}

export interface IamGroupDetail extends IamGroup {
  createdBy?: string;
  members: IamGroupMembership[];
  policyAttachments: Array<{
    id: string;
    policyId: string;
    policy: { id: string; name: string; type: string };
    createdAt: string;
  }>;
}

export interface IamGroupMembership {
  id: string;
  groupId: string;
  principalType: string;
  principalId: string;
  createdAt: string;
}

export interface IamPolicyAttachment {
  id: string;
  principalType?: string;
  principalId?: string;
  policyId: string;
  policyName?: string;
  name?: string;
  source: 'direct' | 'group';
  groupId?: string;
  groupName?: string;
  createdAt: string;
}

export interface IamSimulationRequest {
  principal: { type: string; id: string };
  action: string;
  resource: string;
  context?: Record<string, string | number>;
}

export interface IamSimulationResponse {
  decision: 'Allow' | 'Deny';
  matchedStatements: Array<{
    policyId: string;
    policyName: string;
    sid?: string;
    effect: 'Allow' | 'Deny';
    source: 'direct' | 'group';
    groupName?: string;
  }>;
  reason: string;
}

// Provider Analytics

export interface ProviderAnalytics {
  summary: {
    totalRequests: number;
    errorCount: number;
    errorRate: number;
    avgLatencyMs: number;
    totalTokens: number;
    totalPromptTokens: number;
    totalCompletionTokens: number;
    totalEstimatedCostUsd: number;
    cacheHitCount: number;
    cacheHitRate: number;
  };
  byModel: Array<{
    model: string;
    requestCount: number;
    avgLatencyMs: number;
    totalTokens: number;
    promptTokens: number;
    completionTokens: number;
    estimatedCostUsd: number;
  }>;
  byProject?: Array<{
    projectId: string;
    projectName: string | null;
    projectCode: string | null;
    requestCount: number;
    avgLatencyMs: number;
    totalTokens: number;
    promptTokens: number;
    completionTokens: number;
    estimatedCostUsd: number;
  }>;
  byVirtualKey?: Array<{
    virtualKeyId: string;
    name: string | null;
    keyPrefix: string | null;
    requestCount: number;
    avgLatencyMs: number;
    totalTokens: number;
    promptTokens: number;
    completionTokens: number;
    estimatedCostUsd: number;
  }>;
  daily: Array<{
    date: string;
    requests: number;
    errors: number;
    totalTokens: number;
    estimatedCostUsd: number;
  }>;
  byStatus: Array<{
    statusCode: number;
    count: number;
  }>;
}

// Provider Templates
// The wizard's template catalog is served as static JSON from
// `public/provider-templates/`:
//   /provider-templates/index.json         — list meta (no models)
//   /provider-templates/<name>.json        — full detail (meta + models)
// Edit those files to add/tweak a template; no backend changes required.

export interface ApiTemplateModel {
  /** Customer-facing identifier ("gpt-4o"). Sent in `{"model":"..."}` API
   *  requests; resolved server-side to a UUID. Globally unique. */
  code: string;
  /** Display label ("GPT-4o"). Not unique. */
  name: string;
  description: string;
  /** Vendor's actual model id sent on the upstream wire. Often equals
   *  `code` but can differ (e.g. "gpt-4o-2024-08-06" for a pinned snapshot). */
  providerModelId: string;
  type: string;
  features: string[];
  inputPricePerMillion?: number;
  outputPricePerMillion?: number;
  cachedInputReadPricePerMillion?: number;
  cachedInputWritePricePerMillion?: number;
  maxContextTokens?: number;
  maxOutputTokens?: number;
}

export interface ApiProviderTemplate {
  name: string;
  displayName: string;
  description: string;
  baseUrl: string;
  /**
   * Canonical wire adapter the template installs onto the new Provider
   * row. Same nine-value set as `Provider.adapterType` — mirrors
   * `providers.Format` on the AI Gateway.
   */
  adapterType: string;
  /** Present in index.json for list rendering; omitted from per-template detail. */
  modelCount?: number;
  /** Present in <name>.json; omitted from index.json to keep the list small. */
  models?: ApiTemplateModel[];
}

// Agent Devices

export type AgentDeviceStatus = 'ENROLLED' | 'ACTIVE' | 'OFFLINE' | 'REVOKED';

/**
 * Admin-facing device record. Extends the shared `Device` shape with
 * the CP-only enrolment + fleet-management fields. Adding a field to
 * the shared Device (e.g. for the Wails Dashboard) is automatically
 * visible here; CP-only fields stay in this extension.
 */
export interface AgentDevice extends Device {
  // Narrowing: CP's TypeScript hasn't seen a Linux agent in production
  // yet. Widen when fleet support lands.
  os: 'darwin' | 'windows';
  // Admin status (CP DB enum), distinct from the agent's runtime
  // status (active/paused/…) which lives in the Dashboard's own
  // type extension.
  status: AgentDeviceStatus;
  // CP API always returns these — narrowed from optional to required.
  lastHeartbeat: string | null;
  certExpiresAt: string | null;
  // CP-only enrolment metadata.
  enrolledAt: string;
  enrolledBy: string;
  certSerial: string | null;
  metadata: Record<string, unknown> | null;
  sysinfo: Record<string, unknown> | null;
  _count?: { auditEvents: number };
  /**
   * Last reported primary IP — promoted out of metadata.staticInfo by
   * migration 20260522_thing_identity_columns. Surfaced as a top-level
   * field on the Devices list and detail page Identity card.
   */
  primaryIp?: string;
  /**
   * Stable natural key — hardware fingerprint for agents (sha256 of
   * IOPlatformUUID + serial + MAC + cpu brand). Persisted in
   * thing.physical_id; subject to the partial UNIQUE constraint that
   * prevents duplicate thing rows for the same physical device.
   */
  physicalId?: string;
  /**
   * Free-form tag set. Backed by `thing.tags TEXT[]`. Used
   * by smart-group predicates (`tags_contains` / `tags_contains_all`)
   * and admin filter chips on Devices list.
   */
  tags?: string[];
  /**
   * Currently-bound user (DeviceAssignment with releasedAt IS NULL).
   * Empty / undefined when no active assignment.
   */
  boundUserId?: string;
  boundUserDisplayName?: string;
  boundUserEmail?: string;
}

/**
 * Admin-facing audit event. Extends the shared `AuditEvent` and marks
 * the CP enrichment fields (deviceId, sourceUser, policyRuleId,
 * bumpStatus, byte counts, duration) as required at this level.
 */
export interface AgentAuditEvent extends AuditEvent {
  // Required at this level — CP API always returns these.
  deviceId: string;
  sourceUser: string | null;
  destIp: string;
  destPort: number;
  policyRuleId: string | null;
  bumpStatus: string | null;
  bytesIn: number | null;
  bytesOut: number | null;
  duration: number | null;
  // CP-only.
  createdAt: string;
}

export interface EnrollTokenResponse {
  token: string;
  expiresAt: string;
}

// Fleet Management (D3)

export interface AgentUserSafe {
  id: string;
  displayName: string;
  email: string | null;
  status: string;
  canAccessControlPlane: boolean;
  lastLoginAt: string | null;
  createdAt: string;
  updatedAt: string;
}

export interface AgentUserDetail {
  id: string;
  displayName: string;
  email: string | null;
  status: string;
  osUsername: string | null;
  osDomain: string | null;
  lastLoginAt: string | null;
  createdAt: string;
  updatedAt: string;
}

export interface FleetUserDevice {
  id: string;
  hostname: string;
  os: string;
  osVersion: string;
  agentVersion: string;
  status: AgentDeviceStatus;
  lastHeartbeat: string | null;
  assignedAt: string;
  assignmentSource: string;
}

export interface FleetAuditEvent {
  id: string;
  source: string;
  timestamp: string;
  targetHost: string | null;
  latencyMs: number | null;
  deviceId: string | null;
  subjectId: string | null;
  requestHookDecision: string | null;
  details: Record<string, unknown> | null;
}

export interface DeviceAssignmentDetail {
  id: string;
  deviceId: string;
  userId: string;
  assignedAt: string;
  releasedAt: string | null;
  source: string;
  userDisplayName: string | null;
  userOsUsername: string | null;
  userOsDomain: string | null;
}

// Identity Provider / SCIM

/**
 * External identity provider record. `type === 'local'` rows are
 * the built-in password fallback parent — the admin UI filters them out
 * of management views. OIDC and SAML rows are user-managed external IdPs
 * (Okta, Azure AD, Google Workspace, JumpCloud, OneLogin, ...).
 */
export interface IdentityProvider {
  id: string;
  type: 'local' | 'oidc' | 'saml' | string;
  name: string;
  enabled: boolean;
  /** OIDC fields when type='oidc'; SAML fields when type='saml'. Secret-bearing
   *  fields (`clientSecret`, `certificatePem`) come back masked as "********". */
  config?: Record<string, unknown>;
  defaultRole?: string;
  /** Whether JIT-provisioned users from this IdP get Control Plane access. */
  defaultControlPlaneAccess?: boolean;
  jitEnabled?: boolean;
  createdAt: string;
  updatedAt?: string;
}

/**
 * Request body for POST /api/admin/identity-providers (create) and
 * PUT /api/admin/identity-providers/:id (update). On update, sending
 * a secret field as the literal "********" preserves the saved value.
 */
export interface IdentityProviderWriteRequest {
  type: 'oidc' | 'saml';
  name: string;
  enabled?: boolean;
  config: Record<string, unknown>;
  defaultRole?: string;
  defaultControlPlaneAccess?: boolean;
  jitEnabled?: boolean;
}

/**
 * Response from POST /api/admin/identity-providers[/:id]/test —
 * the connectivity probe. `ok=true` carries discovery details in
 * `detail`; `ok=false` carries a human-readable `error`.
 */
export interface IdentityProviderProbeResult {
  ok: boolean;
  detail?: Record<string, unknown>;
  error?: string;
  elapsedMs: number;
}

/**
 * Response from POST /api/admin/identity-providers/parse-saml-metadata —
 * the SP-relevant fields extracted from an uploaded/pasted IdP metadata XML,
 * used to pre-fill the SAML Add-IdP form. `emailAttribute`/`groupsAttribute`
 * are present only when the metadata declared an attribute that looks like
 * email / groups.
 */
export interface SamlMetadataParseResult {
  entityId: string;
  ssoUrl: string;
  certificatePem: string;
  emailAttribute?: string;
  groupsAttribute?: string;
}

export interface ScimToken {
  id: string;
  name: string;
  tokenPrefix: string;
  identityProviderId?: string | null;
  createdBy: string;
  createdAt: string;
  lastUsedAt?: string | null;
}

export interface IdpGroupMapping {
  id: string;
  identityProviderId: string;
  externalGroupId: string;
  externalGroupName?: string | null;
  iamGroupId: string;
  iamGroupName?: string | null;
  createdAt: string;
}

// NormalizedPayload + sidecar (traffic_event_normalized)

/** kind discriminator on NormalizedPayload — must mirror Go normalize.Kind. */
export type NormalizedKind =
  | 'ai-chat'
  | 'ai-completion'
  | 'ai-embedding'
  | 'ai-image'
  | 'http-json'
  | 'http-text'
  | 'http-form'
  | 'http-multipart'
  | 'http-binary'
  | 'http-sse'
  | 'unsupported';

export interface BinaryRef {
  size: number;
  contentType: string;
  sha256: string;
  spillKey?: string;
}

export interface ToolUse {
  callId?: string;
  name: string;
  input?: Record<string, unknown>;
}

export interface ToolResult {
  callId?: string;
  output?: string;
}

export type ContentBlockType = 'text' | 'image_ref' | 'tool_use' | 'tool_result' | 'reasoning';

export interface NormalizedContentBlock {
  type: ContentBlockType;
  text?: string;
  imageRef?: BinaryRef;
  toolUse?: ToolUse;
  toolResult?: ToolResult;
}

export interface NormalizedMessage {
  role: 'system' | 'user' | 'assistant' | 'tool';
  content: NormalizedContentBlock[];
  finishReason?: string;
}

/** One server-sent event captured by the generic-http SSE projection.
 *  At most one of `data` (frame data parsed as JSON) or `dataText`
 *  (verbatim non-JSON data) is set; a frame whose data line was empty
 *  carries neither. */
export interface SSEFrame {
  event?: string;
  data?: unknown;
  dataText?: string;
}

export interface HTTPBodyView {
  text?: string;
  json?: unknown;
  form?: Record<string, string>;
  binaryRef?: BinaryRef;
  /** kind=http-sse: decoded event frames in stream order. */
  sseFrames?: SSEFrame[];
  /** kind=http-sse: true when the capture limit cut the frame list short. */
  sseTruncated?: boolean;
}

export interface HTTPPayload {
  method?: string;
  url?: string;
  headersFiltered?: Record<string, string>;
  bodyView?: HTTPBodyView;
}

export interface NormalizedUsage {
  promptTokens?: number;
  completionTokens?: number;
  totalTokens?: number;
  cacheReadTokens?: number;
  cacheCreationTokens?: number;
  reasoningTokens?: number | null;
}

export interface NormalizedPayload {
  kind: NormalizedKind;
  normalizeVersion: string;
  protocol?: string;
  model?: string;
  stream?: boolean;
  messages?: NormalizedMessage[];
  tools?: unknown[];
  params?: Record<string, unknown>;
  usage?: NormalizedUsage;
  finishReason?: string;
  http?: HTTPPayload;
  /** drop-content placeholder marker — when true the payload is metadata-only. */
  redacted?: boolean;
  /** Why the content was dropped: the operator chose drop-content, or a
   *  redact storage policy could not be applied precisely and degraded.
   *  Absent on rows written before the reason was stamped — the UI then
   *  renders a neutral notice asserting neither story. */
  redactedReason?: 'operator-drop' | 'redact-degraded';
  /** Degradation diagnosis when redactedReason === 'redact-degraded'.
   *  failedAddresses lists content addresses only — never content.
   *  cause is an open vocabulary: the listed tokens render as localized
   *  phrases, unknown future tokens render verbatim. */
  redactedDetail?: {
    cause: 'no-spans' | 'payload-unmarshal' | 'spans-unresolved' | 'marshal-failed' | (string & {});
    failedAddresses?: string[];
  };
  /** rule IDs that triggered the drop-content storage policy. */
  ruleIds?: string[];
  /** Normalizer-reported confidence in [0,1]. Absent/0 on
   *  older rows is interpreted as fully confident (1.0). */
  confidence?: number;
  /** Which spec the normalizer matched. Examples:
   *  "openai-chat" (Tier 1 AI builtin), "chatgpt-web" (Tier 1
   *  per-host adapter), "pattern:chatgpt-web" (Tier 2 multi-spec
   *  pattern probe), "generic-http" (structural fallback projection
   *  of the raw HTTP body, confidence 1.0 = confidence in the
   *  projection only, no AI-semantics claim). Empty on legacy
   *  verbatim rows. */
  detectedSpec?: string;
  /** "host" when the normalizer was selected by interception-domain
   *  host match (a per-host adapter) rather than by decode coverage —
   *  the confidence is then the honest coverage of a known-adapter
   *  body (a single-prompt spec caps near 0.6 by design), NOT a trust
   *  score comparable to a Tier-1 sniffed decode. The badge renders a
   *  "host-matched" label in place of the numeral. Absent for keyed /
   *  sniffed / pattern / fallback rows. */
  selectionEvidence?: 'host';
  /** Text input strings for kind=ai-embedding requests.
   *  Nil/absent when the input was a binary token array (not stored)
   *  or when this is a response payload (embedding vectors are never
   *  stored). */
  inputs?: string[] | null;
}

/** One TransformSpan emitted by hook / aiguard / cache normaliser. */
export interface TransformSpan {
  source: 'hook' | 'aiguard' | 'cache-normaliser' | 'cache-control-inject' | 'cache-key-strip';
  sourceId?: string;
  action: 'redact' | 'strip' | 'inject' | 'replace';
  contentAddress: string;
  start: number;
  end: number;
  replacement?: string;
  reason?: string;
}

export type NormalizeStatus = 'ok' | 'partial' | 'failed';

/** One transform span emitted by a hook / aiguard / cache normaliser. */
export interface TransformSpan {
  source: 'hook' | 'aiguard' | 'cache-normaliser' | 'cache-control-inject' | 'cache-key-strip';
  sourceId?: string;
  action: 'redact' | 'strip' | 'inject' | 'replace';
  contentAddress: string;
  start: number;
  end: number;
  replacement?: string;
  reason?: string;
}

/**
 * Sidecar row for traffic_event_normalized. The Admin API
 * endpoint GET /api/admin/traffic/:id/normalized returns this shape;
 * 404 when no normalize row exists (e.g. capture was disabled).
 */
export interface TrafficEventNormalized {
  trafficEventId: string;
  normalizeVersion: string;
  requestNormalized?: NormalizedPayload | null;
  responseNormalized?: NormalizedPayload | null;
  requestStatus?: NormalizeStatus | null;
  responseStatus?: NormalizeStatus | null;
  requestErrorReason?: string | null;
  responseErrorReason?: string | null;
  requestRedactionSpans?: TransformSpan[] | null;
  responseRedactionSpans?: TransformSpan[] | null;
  createdAt: string;
}

// ── Semantic Cache Config (L1 embedding singleton) ─────────────────────

/**
 * Singleton config record stored server-side as `semantic_cache_config`.
 * Mirrors configstore.SemanticCacheConfigRow (Go).
 * Returned by GET/PUT /api/admin/semantic-cache/config.
 */
export interface SemanticCacheConfig {
  id: string;
  embeddingProviderId?: string | null;
  embeddingModelId?: string | null;
  embeddingDimension?: number | null;
  /** SHA-256 fingerprint of (providerId, modelId, dimension) — server-computed. */
  embeddingFingerprint: string;
  /**
   * Versioned Redis Vector index name (e.g. "nexus:semantic-cache:v1").
   * Bumped on every embedding-model swap to enable blue/green index switching.
   */
  redisIndexName: string;
  /** Fleet-wide kill switch for the semantic cache L2. */
  enabled: boolean;
  /**
   * Fleet-wide cosine similarity gate for L2 hits. Range [0.0, 1.0]; default 0.96.
   * Tune up for precision-sensitive workloads (RAG / factual), tune down for
   * paraphrase-tolerant creative writing.
   */
  threshold: number;
  /**
   * L2 cache isolation scope. "vk" default matches single-tenant deployments;
   * multi-tenant deployments typically flip to "org" or "user".
   */
  varyBy: 'none' | 'user' | 'vk' | 'org';
  /**
   * Embedding input shaping strategy. Default "system_plus_last_user" works
   * for most workloads; "last_user" suits single-turn Q&A, "recent_turns"
   * suits multi-turn agent dialogs.
   */
  embedStrategy:
    | 'last_user'
    | 'system_plus_last_user'
    | 'recent_turns'
    | 'head_plus_tail'
    | 'full_truncated';
  /**
   * When true, an L2 hit can return an entry cached against a different
   * upstream model. Default false (model-specific). Useful with smart routing.
   */
  allowCrossModel: boolean;
  updatedAt: string;
  updatedBy?: string | null;
}

/**
 * Result from POST /api/admin/providers/:id/embedding-probe.
 * Carries the confirmed model metadata and a small latency reading.
 */
export interface ProbeResult {
  ok: boolean;
  providerId?: string;
  modelId?: string;
  modelName?: string;
  dimension?: number;
  latencyMs?: number;
  promptTokens?: number;
  sampleEmbeddingFirst10?: number[];
  error?: string;
}
