/**
 * System API service — audit, health, cache, config, settings.
 */
import { api } from '../../../client';
import { withPrefix } from '@/lib/deploymentPrefix';
import type {
  AdminAuditEntry,
  AdminAuditExportResponse,
  AdminModelsByProvider,
  Model,
  ProviderHealth,
  SystemSettings,
  TrafficEvent,
  TrafficEventNormalized,
  TrafficStorageResponse,
  UpdateModelInput,
  WhoAmI,
} from '../../../types';

export interface UpdateSettingsInput {
  maintenanceMode?: boolean;
  logLevel?: 'error' | 'warn' | 'info' | 'debug';
  defaultHookTimeout?: number;
  defaultFailBehavior?: 'open' | 'closed';
}

// Observability config
export interface ObservabilityConfig {
  otelEnabled: boolean;
  otelEndpoint: string;
  otelServiceName: string;
  samplingRate: number;
  traceViewerUrl: string | null;
}

// Payload capture config (request/response body storage).
//
// maxInlineBodyBytes: inline-vs-spill cutoff; bodies above this are stored
// in the configured spill backend.
// maxRequestBytes / maxResponseBytes: data-plane network read caps.
// The three fields are independent — see architecture docs "Body Capture".
export interface PayloadCaptureConfig {
  storeRequestBody: boolean;
  storeResponseBody: boolean;
  maxInlineBodyBytes: number;
  maxRequestBytes: number;
  maxResponseBytes: number;
}

export type PayloadCaptureUpdateInput = Partial<PayloadCaptureConfig>;

// Streaming compliance global default. Snake_case keys mirror the
// system_metadata['streaming_compliance.config'] row so the admin API and
// the data-plane shared/streaming/policy.LoadGlobalDefault see the same shape.
//
// `warnings` is admin-facing only — surfaced by the backend when the
// selected mode carries gotchas (e.g. buffer_full_block silently drops
// response-hook Modify decisions). Single source of truth in the
// backend's modeWarnings(); the UI just renders the strings so we
// don't have to keep parallel copy in sync. Omitted on success when
// the mode has no advisories.
export interface StreamingComplianceConfig {
  default_mode: 'passthrough' | 'buffer_full_block' | 'chunked_async';
  chunk_bytes: number;
  hook_timeout_ms: number;
  max_buffer_bytes: number;
  fail_behavior: 'fail_open' | 'fail_close';
  capture_request_body: boolean;
  capture_response_body: boolean;
  raw_body_spill_enabled: boolean;
  warnings?: string[];
}

export type StreamingComplianceUpdateInput = Partial<StreamingComplianceConfig>;

// Legacy prompt-cache types removed. The three-tier replacement
// (Global / Adapter / Provider) lives in @/api/services/cache.

export interface CachePreviewRequest {
  traffic_event_id: string;
}

export interface CachePreviewRuleResult {
  rule_id: string;
  adapter_type: string;
  dry_run: boolean;
  enabled: boolean;
  strip_count: number;
  strip_bytes: number;
}

export interface CachePreviewResponse {
  traffic_event_id: string;
  adapter_type: string;
  strip_count: number;
  strip_bytes: number;
  markers_injected: number;
  dry_run: boolean;
  rules_applied: string[];
  rules: CachePreviewRuleResult[];
  body_before?: unknown;
  body_after?: unknown;
  diff_lines?: string[];
}

// SSO config (unified — OIDC + SAML)

export interface OidcProviderConfig {
  enabled: boolean;
  displayName: string;
  issuer: string;
  jwksUri: string;
  clientId: string;
  clientSecret: string;
  redirectUri: string;
  authorizeUrl: string;
  tokenUrl: string;
  audience: string;
  emailClaim: string;
  groupClaim: string;
  groupRoleMap: Record<string, string>;
  defaultRole: string | null;
}

export interface SamlProviderConfig {
  enabled: boolean;
  displayName: string;
  idpMetadataUrl: string;
  idpEntityId: string;
  idpSsoUrl: string;
  idpCert: string;
  spEntityId: string;
  emailAttribute: string;
  groupAttribute: string;
  groupRoleMap: Record<string, string>;
  defaultRole: string | null;
  signAuthnRequest: boolean;
}

export interface SsoConfig {
  oidc: OidcProviderConfig;
  saml: SamlProviderConfig;
}

export interface SsoTestResponse {
  valid: boolean;
  claims?: Record<string, unknown>;
  error?: string;
}

export interface SsoProvider {
  type: 'oidc' | 'saml';
  label: string;
  authorizeUrl?: string;
  loginUrl?: string;
}

// SIEM config
export type SiemFormat = 'json' | 'cef' | 'syslog';

export interface SiemEventTypeInfo {
  /** Canonical SIEM event type — "<resource>.<verb>" form. */
  type: string;
  /** The catalog resource that emits this event. UI uses it as the
   * middle-level grouping key in the three-level service→resource→event tree. */
  resource: string;
  /** The catalog service (gateway / compliance / agent / platform / iam) that owns the resource. */
  service: string;
}

export interface SiemConfig {
  enabled: boolean;
  url: string;
  format: SiemFormat;
  headers: Record<string, string>;
  eventTypes: string[];
}

// Rollup jobs
export interface JobStatus {
  name: string;
  interval: string;
  lastRunAt: string | null;
  lastRunMs: number;
  lastError: string;
  runCount: number;
  errorCount: number;
  running: boolean;
}

export interface WatermarkStatus {
  jobName: string;
  watermark: string;
  updatedAt: string;
}

export interface TableInfo {
  table: string;
  rows: number;
  earliest: string | null;
  latest: string | null;
}

export interface RollupJobsResponse {
  jobs: JobStatus[];
  watermarks: WatermarkStatus[];
  tables: TableInfo[];
}

export interface ServiceInstanceInfo {
  instanceId: string;
  service: string;
  version: string;
  address: string | null;
  status: 'healthy' | 'degraded' | 'unhealthy' | 'offline';
  uptime: number | null;
  checks: Record<string, string | boolean> | null;
  registeredAt: string;
  lastHeartbeatAt: string;
}

export interface ServiceSummary {
  service: string;
  total: number;
  healthy: number;
  degraded: number;
  unhealthy: number;
  offline: number;
}

// Service metrics (aggregated from Prometheus /metrics)
export interface RuntimeMetrics {
  goroutines: number;
  heapAllocMB: number;
  heapSysMB: number;
  gcPauseP50Ms: number;
  gcCount: number;
  threads: number;
}

export interface BreakdownItem {
  value: string;
  count: number;
  p50Ms?: number;
  p99Ms?: number;
}

export interface MetricBreakdown {
  label: string;
  items: BreakdownItem[];
}

export interface ServiceMetricSet {
  instances: number;
  metrics: Record<string, number | boolean>;
  breakdowns?: Record<string, MetricBreakdown>;
  runtime: RuntimeMetrics;
}

export interface ServiceMetricsResponse {
  cachedAt: string;
  fetchErrors?: string[];
  services: Record<string, ServiceMetricSet>;
}

export const systemApi = {
  // Traffic (unified audit)
  getTrafficStorage: () =>
    api.get<TrafficStorageResponse>('/api/admin/traffic/storage'),

  /**
   * List traffic events. Accepts either a plain object (single-value params)
   * or a `URLSearchParams` (for repeatable params such as `?tag=a&tag=b`).
   */
  listTrafficEvents: (params?: Record<string, string> | URLSearchParams) => {
    if (params instanceof URLSearchParams) {
      const qs = params.toString();
      return api.get<{ data: TrafficEvent[]; total: number }>(
        `/api/admin/traffic${qs ? `?${qs}` : ''}`,
      );
    }
    return api.get<{ data: TrafficEvent[]; total: number }>('/api/admin/traffic', params);
  },

  getTrafficEvent: (id: string) =>
    api.get<TrafficEvent>(`/api/admin/traffic/${id}`),

  /**
   * Fetch the normalized payload sidecar for a traffic event. Returns 404
   * when no traffic_event_normalized row exists (capture disabled, protocol
   * unsupported). The UI's Normalized tab tolerates 404 and falls back to Raw.
   */
  getTrafficEventNormalized: (id: string) =>
    api.get<TrafficEventNormalized>(`/api/admin/traffic/${id}/normalized`),

  // Admin audit
  listAdminAuditLogs: (params?: Record<string, string>) =>
    api.get<{ data: AdminAuditEntry[]; total: number }>('/api/admin/admin-audit-logs', params),

  exportAdminAuditLogs: (params?: Record<string, string>) =>
    api.get<AdminAuditExportResponse>('/api/admin/admin-audit-logs/export', params),

  listMyAdminAuditLogs: (params?: Record<string, string>) =>
    api.get<{ data: AdminAuditEntry[]; total: number }>('/api/admin/me/admin-audit-logs', params),

  // Health
  listProviderHealth: () =>
    api.get<{ data: ProviderHealth[] }>('/api/admin/provider-health'),

  // Settings
  getSettings: () =>
    api.get<SystemSettings>('/api/admin/settings'),

  updateSettings: (data: UpdateSettingsInput) =>
    api.put<SystemSettings>('/api/admin/settings', data),

  // Models
  listModels: (params?: Record<string, string>) =>
    api.get<{ data: AdminModelsByProvider[] }>('/api/admin/models', params),

  listModelsFlat: (params?: Record<string, string>) =>
    api.get<{ data: Array<Model & { providerDisplay: string }>; total: number }>('/api/admin/models/flat', params),

  updateModel: (id: string, data: UpdateModelInput) =>
    api.put<Model>(`/api/admin/models/${id}`, data),

  deleteModel: (id: string) =>
    api.delete(`/api/admin/models/${id}`),

  // Instances
  listInstances: () =>
    api.get<{
      instances: ServiceInstanceInfo[];
      count: number;
      services: Record<string, ServiceSummary>;
    }>('/api/admin/instances'),

  /**
   * Readiness probe — tolerates 503 (not_ready) and still returns parsed body.
   * Unlike other endpoints, this doesn't use `api.get` because 503 is a valid response.
   */
  checkReady: async (): Promise<{ status: string; checks?: Record<string, string> }> => {
    // /api/admin/ready is the public readiness probe — registered on
    // the root echo instance (no admin auth middleware) so it returns
    // the real DB+Hub status even when the user's session has lapsed.
    // The path stays under /api/admin/* so the dev-time Vite proxy
    // and any reverse proxy rule on /api/* forward it transparently
    // without a separate config entry. Same handler also serves /ready
    // for k8s probes / load-balancer checks.
    const res = await fetch(new URL(withPrefix('/api/admin/ready'), window.location.origin).toString(), {
      credentials: 'include',
    });
    const body = (await res.json().catch(() => ({}))) as { status?: string; checks?: Record<string, string> };
    if (res.status === 503 && (body.checks || body.status)) {
      return { status: body.status ?? 'not_ready', checks: body.checks };
    }
    if (!res.ok) return { status: 'unknown' };
    return { status: body.status ?? 'unknown', checks: body.checks };
  },

  // Current principal (admin_user or api_key) — the OAuth-era replacement for
  // the old /api/admin/whoami endpoint. Response shape is identical to WhoAmI.
  me: () =>
    api.get<WhoAmI>('/api/admin/me'),

  // Observability
  getObservabilityConfig: () =>
    api.get<ObservabilityConfig>('/api/admin/settings/observability'),
  updateObservabilityConfig: (input: { otelEnabled: boolean; samplingRate: number; traceViewerUrl: string }) =>
    api.put<ObservabilityConfig>('/api/admin/settings/observability', input),

  // Payload capture (request/response body storage)
  getPayloadCaptureConfig: () =>
    api.get<PayloadCaptureConfig>('/api/admin/settings/payload-capture'),
  updatePayloadCaptureConfig: (input: PayloadCaptureUpdateInput) =>
    api.put<PayloadCaptureConfig>('/api/admin/settings/payload-capture', input),

  // Legacy prompt-cache endpoints removed. Use @/api/services/cache.cacheApi
  // for the three-tier replacement.
  previewCacheNormaliser: (input: CachePreviewRequest) =>
    api.post<CachePreviewResponse>('/api/admin/cache/preview', input),

  // Global StreamingPolicy default. Per-resource overrides live on
  // interception_domain (compliance-proxy + agent) and Provider (ai-gateway)
  // and are edited from those resources' edit panels.
  getStreamingComplianceConfig: () =>
    api.get<StreamingComplianceConfig>('/api/admin/settings/streaming-compliance'),
  updateStreamingComplianceConfig: (input: StreamingComplianceUpdateInput) =>
    api.put<StreamingComplianceConfig>('/api/admin/settings/streaming-compliance', input),

  // SSO (unified)
  getSsoConfig: () =>
    api.get<SsoConfig>('/api/admin/settings/sso'),
  updateSsoConfig: (input: Partial<SsoConfig>) =>
    api.put<SsoConfig>('/api/admin/settings/sso', input),
  testSsoToken: (token: string) =>
    api.post<SsoTestResponse>('/api/admin/settings/sso/test', { token }),
  fetchSamlMetadata: (metadataUrl: string) =>
    api.post<{ idpEntityId: string; idpSsoUrl: string; idpCert: string }>('/api/admin/settings/sso/saml/fetch-metadata', { url: metadataUrl }),
  getSsoProviders: () =>
    api.get<{ providers: SsoProvider[] }>('/api/admin/auth/sso/providers'),

  // Rollup jobs
  listRollupJobs: () =>
    api.get<RollupJobsResponse>('/api/admin/rollup-jobs'),

  triggerRollupJob: (name: string) =>
    api.post<{ message: string; job: string }>(`/api/admin/rollup-jobs/${name}/trigger`),

  // SIEM
  getSiemConfig: () =>
    api.get<SiemConfig>('/api/admin/settings/siem'),
  updateSiemConfig: (input: Partial<SiemConfig>) =>
    api.put<SiemConfig>('/api/admin/settings/siem', input),
  sendSiemTestEvent: () =>
    api.post<{ ok: boolean; error?: string }>('/api/admin/settings/siem/test'),
  listSiemEventTypes: () =>
    api.get<{ eventTypes: SiemEventTypeInfo[] }>('/api/admin/settings/siem/event-types'),
};
