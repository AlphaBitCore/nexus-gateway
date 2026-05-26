/**
 * Quota Analytics API service — dashboard data for quota usage, trends, and top consumers.
 */
import { api } from '../../client';

export const quotaAnalyticsApi = {
  overview: (params: Record<string, string>) =>
    api.get<{ data: QuotaUsageRow[] }>('/api/admin/quota-analytics/overview', params),

  trend: (params: Record<string, string>) =>
    api.get<{ data: QuotaTrendPoint[] }>('/api/admin/quota-analytics/trend', params),

  top: (params: Record<string, string>) =>
    api.get<{ data: QuotaTopConsumer[] }>('/api/admin/quota-analytics/top', params),
};

export interface QuotaUsageRow {
  entityId: string;
  entityName: string;
  entityType: string;
  costLimitUsd: number;
  currentCostUsd: number;
  usagePercent: number;
  alertLevel: string;
}

export interface QuotaTrendPoint {
  date: string;
  costUsd: number;
}

export interface QuotaTopConsumer {
  entityId: string;
  entityName: string;
  entityType: string;
  totalCostUsd: number;
}
