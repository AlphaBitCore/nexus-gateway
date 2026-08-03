/**
 * Analytics & metrics API service.
 */
import { api } from '../../client';
import type { AnalyticsSummary, ProviderBreakdown, CostData, UsageData, MetricAggregatesResponse, SparklineResponse } from '../../types';

export interface CacheROIByAdapter {
  adapter: string;
  estimatedCostUsd: number;
  gatewayCacheSavingsUsd: number;
  gatewayCacheHitCount: number;
  cacheWriteCostUsd: number;
  cacheReadSavingsUsd: number;
  cacheNetSavingsUsd: number;
  promptTokens: number;
  completionTokens: number;
  cacheCreationTokens: number;
  cacheReadTokens: number;
  requestsWithCacheHit: number;
}

export interface CacheROIDay {
  date: string;
  gatewayCacheSavingsUsd: number;
  cacheWriteCostUsd: number;
  cacheReadSavingsUsd: number;
  cacheNetSavingsUsd: number;
  cacheCreationTokens: number;
  cacheReadTokens: number;
}

// Returned by GET /api/admin/analytics/cost-summary.
export interface CostSummaryResponse {
  totalCostUsd: number;
  totalGatewayCacheSavingsUsd: number;
  totalProviderPromptCacheNetSavingsUsd: number;
  totalCombinedSavingsUsd: number;
  totalReasoningCostUsd: number;
  totalEmbeddingCostUsd: number;
  totalAiGuardCostUsd: number;
  /**
   * When true, ai-guard + L2 embedding costs stay on dedicated metric
   * series and are NOT folded into the customer's billed total.
   * When false (default), they count toward the customer's quota.
   */
  excludeInternalOpsFromBilledCost: boolean;
  periodDays: number;
  since: string;
  byOrg: Array<{ orgId: string; orgName?: string; costUsd: number }>;
  byProvider: Array<{ providerId: string; costUsd: number }>;
}

export interface CacheROISummary {
  since: string;
  until: string;
  periodDays: number;
  // actual cost paid to providers (non-gateway-cached requests)
  totalEstimatedCostUsd: number;
  // Gateway response cache savings (full upstream cost avoided)
  totalGatewayCacheSavingsUsd: number;
  gatewayCacheHitCount: number;
  // Upstream provider prompt-cache discount (input tokens billed at reduced rate)
  totalCacheWriteCostUsd: number;
  totalCacheReadSavingsUsd: number;
  totalCacheNetSavingsUsd: number;
  totalPromptTokens: number;
  totalCompletionTokens: number;
  totalCacheCreationTokens: number;
  totalCacheReadTokens: number;
  totalNormalisedStripCount: number;
  totalNormalisedStripBytes: number;
  totalMarkersInjected: number;
  requestsWithCacheHit: number;
  byAdapter: CacheROIByAdapter[];
  daily: CacheROIDay[];
  /** "rollup" = served from rollup tables; "direct" = computed from raw traffic_event */
  dataSource: 'rollup' | 'direct';
}

export const analyticsApi = {
  summary: (params?: Record<string, string>) =>
    api.get<AnalyticsSummary>('/api/admin/analytics/summary', params),

  byProvider: (params?: Record<string, string>) =>
    api.get<{ data: ProviderBreakdown[] }>('/api/admin/analytics/by-provider', params),

  cost: (params?: Record<string, string>) =>
    api.get<{ data: CostData[] }>('/api/admin/analytics/cost', params),

  usage: (params?: Record<string, string>) =>
    api.get<{ data: UsageData[] }>('/api/admin/analytics/usage', params),

  metricsAggregates: (params?: Record<string, string>) =>
    api.get<MetricAggregatesResponse>('/api/admin/metrics/aggregates', params),

  sparkline: (params?: { startTime?: string; endTime?: string }) => {
    const query = new URLSearchParams();
    if (params?.startTime) query.set('startTime', params.startTime);
    if (params?.endTime) query.set('endTime', params.endTime);
    const qs = query.toString();
    return api.get<SparklineResponse>(`/api/admin/analytics/sparkline${qs ? '?' + qs : ''}`);
  },

  cacheROI: (params?: { start?: string; end?: string }) =>
    api.get<CacheROISummary>('/api/admin/analytics/cache-roi', params as Record<string, string>),

  /**
   * GET /api/admin/analytics/cost-summary — 30-day rolling cost breakdown
   * by org and provider, plus the fleet-wide billed-cost policy
   * (excludeInternalOpsFromBilledCost) so the UI can render the
   * "internal-ops counted/excluded" hint accurately on traffic event
   * drawers without a Hub round-trip.
   */
  costSummary: () =>
    api.get<CostSummaryResponse>('/api/admin/analytics/cost-summary'),

  /**
   * GET /api/admin/analytics/latency-phases
   * Returns P50/P95/P99 per phase (total / our overhead / upstream TTFB /
   * upstream total / hooks) per groupBy dimension. Source filter narrows
   * the contributing rows.
   */
  latencyPhases: (params: {
    groupBy: 'provider' | 'model' | 'virtual_key' | 'node' | 'host' | 'device';
    start: string;
    end: string;
    source?: 'all' | 'ai-gateway' | 'compliance-proxy' | 'agent';
  }) =>
    api.get<{
      window: { start: string; end: string };
      rows: LatencyPhaseRow[];
    }>('/api/admin/analytics/latency-phases', params as Record<string, string>),

  /**
   * GET /api/admin/analytics/vendor-bill-reconciliation — provider×day rows
   * comparing our estimated spend against the vendor's authoritative billing
   * API total. Nullable money fields (vendorReportedUsd/diffUsd/diffPct) stay
   * null when the day is not finalized or the vendor fetch failed.
   */
  vendorBillReconciliation: (params?: { from?: string; to?: string }) =>
    api.get<VendorBillReconciliationResponse>(
      '/api/admin/analytics/vendor-bill-reconciliation',
      params as Record<string, string>,
    ),

  /**
   * POST …/vendor-bill-reconciliation/{providerId}/{day}/review — mark a
   * provider×day row reviewed with an optional note. day is YYYY-MM-DD.
   */
  reviewVendorBillReconciliation: (providerId: string, day: string, note?: string) =>
    api.post<{ status: string; reviewedBy: string; providerId: string; day: string }>(
      `/api/admin/analytics/vendor-bill-reconciliation/${encodeURIComponent(providerId)}/${encodeURIComponent(day)}/review`,
      { note: note ?? '' },
    ),
};

/** Coverage of a reconciliation row — drives the UI honesty badge. */
export type VendorBillCoverage = 'scoped' | 'org_only' | 'fetch_failed' | 'priority_tier_undercount';

export interface VendorBillReconciliationRow {
  providerId: string;
  providerName?: string;
  day: string; // YYYY-MM-DD
  ourBilledUsd: number;
  vendorReportedUsd: number | null;
  diffUsd: number | null;
  diffPct: number | null;
  scopeKind: string;
  coverage: VendorBillCoverage;
  status: 'open' | 'reviewed';
  reviewedBy: string | null;
  note: string | null;
  updatedAt: string;
}

export interface VendorBillReconciliationResponse {
  rows: VendorBillReconciliationRow[];
}

/**
 * One row of /analytics/latency-phases response. Percentile fields are
 * nullable when the corresponding column is NULL on every row in the window
 * (e.g. upstreamTtfb when TTFB was not measured).
 */
export interface LatencyPhaseRow {
  groupKey: string;
  groupLabel: string;
  requestCount: number;
  totalP50Ms: number | null;
  totalP95Ms: number | null;
  totalP99Ms: number | null;
  usOverheadP50Ms: number | null;
  usOverheadP95Ms: number | null;
  usOverheadP99Ms: number | null;
  upstreamTtfbP50Ms: number | null;
  upstreamTtfbP95Ms: number | null;
  upstreamTtfbP99Ms: number | null;
  upstreamTotalP50Ms: number | null;
  upstreamTotalP95Ms: number | null;
  upstreamTotalP99Ms: number | null;
  requestHooksP50Ms: number | null;
  requestHooksP95Ms: number | null;
  responseHooksP50Ms: number | null;
  responseHooksP95Ms: number | null;
}
