import { api } from '../../client';

export interface FleetSummary {
  total: number;
  active: number;
  stale: number;
  critical: number;
  revoked: number;
  stalePct: number;
  criticalPct: number;
}

export interface FleetTrendBucket {
  bucketStart: string;
  dimensions: Record<string, string>;
  value: number;
}

export interface FleetTrendsResponse {
  metric: string;
  startTime: string;
  endTime: string;
  buckets: FleetTrendBucket[];
}

export interface TopDestination {
  destHost: string;
  eventCount: number;
  deviceCount: number;
}

export interface TopDestinationsResponse {
  windowHours: number;
  generatedAt: string;
  data: TopDestination[];
}

const BASE = '/api/admin/fleet-analytics';

export const fleetAnalyticsApi = {
  summary: () => api.get<FleetSummary>(`${BASE}/summary`),
  trends: (params: Record<string, string>) => api.get<FleetTrendsResponse>(`${BASE}/trends`, params),
  topDestinations: (params?: Record<string, string>) => api.get<TopDestinationsResponse>(`${BASE}/top-destinations`, params),
};
