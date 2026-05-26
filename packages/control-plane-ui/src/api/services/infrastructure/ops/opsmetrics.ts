/**
 * Ops-metrics admin API service — bound to `/api/admin/ops-metrics/*`.
 *
 * The Control Plane reads `metric_ops_*` (raw + 1h/1d/1mo rollups) directly
 * via pgx; this service exposes the three CP endpoints that surface those
 * tables to the admin UI:
 *
 *   GET /api/admin/ops-metrics/current     — latest sample per (node, metric, dim)
 *                                             within the last 90s.
 *   GET /api/admin/ops-metrics/timeseries  — bucketed series for one tuple.
 *   GET /api/admin/ops-metrics/fleet       — fleet-aggregate (nodeId IS NULL)
 *                                             buckets for `nodeType=agent`.
 *
 * Wire shapes mirror the Go structs in
 * `packages/control-plane/internal/store/opsmetrics_store.go` (OpsMetricSample
 * + OpsMetricBucket); the CP handler always wraps payloads in `{ data: [...] }`,
 * with `granularity` echoed back on the timeseries / fleet endpoints so the
 * UI can show how the server resolved an `auto` request.
 */

import { api } from '../../../client';

export type NodeTypeFilter =
  | 'service'
  | 'agent'
  | 'control-plane'
  | 'ai-gateway'
  | 'compliance-proxy'
  | 'nexus-hub';

export type MetricKind = 'gauge' | 'counter' | 'histogram';

export type Granularity = 'auto' | 'raw' | '1h' | '1d' | '1mo';

/** Resolved granularity that comes back from the server (no `auto`). */
export type ResolvedGranularity = Exclude<Granularity, 'auto'>;

/** One row from `metric_ops_raw` (or its synthesized "latest per key" view). */
export interface OpsMetricSample {
  sampledAt: string;
  nodeId: string;
  nodeType: string;
  metricName: string;
  metricKind: MetricKind | string;
  dimensionKey: string;
  /** Counter increments and gauges; histograms have no scalar value. */
  value?: number | null;
  metadata?: Record<string, unknown>;
}

/** One bucket from a rollup table (or a single-row view of a raw sample). */
export interface OpsMetricBucket {
  bucketStart: string;
  /** Null for fleet aggregates (per-node rows always set nodeId). */
  nodeId?: string | null;
  nodeType: string;
  metricName: string;
  metricKind: MetricKind | string;
  dimensionKey: string;
  valueAvg?: number | null;
  valueSum?: number | null;
  valueMin?: number | null;
  valueMax?: number | null;
  sampleCount: number;
  metadata?: Record<string, unknown>;
}

export interface OpsMetricsCurrentParams {
  nodeType?: NodeTypeFilter;
  nodeId?: string;
}

export interface OpsMetricsTimeseriesParams {
  nodeId: string;
  metric: string;
  from: string;
  to: string;
  dim?: string;
  granularity?: Granularity;
}

export interface OpsMetricsFleetParams {
  nodeType: NodeTypeFilter;
  metric: string;
  from: string;
  to: string;
  dim?: string;
  granularity?: Granularity;
}

export interface TimeseriesResponse {
  data: OpsMetricBucket[];
  granularity: ResolvedGranularity;
}

/**
 * Build the `?…` suffix or empty string for a flat string-keyed param object.
 * Empty/undefined values are dropped so the URL stays canonical.
 */
function qs(p: Record<string, string | undefined>): string {
  const u = new URLSearchParams();
  for (const [k, v] of Object.entries(p)) {
    if (v != null && v !== '') u.set(k, v);
  }
  const s = u.toString();
  return s ? `?${s}` : '';
}

export const opsMetricsApi = {
  current: (p: OpsMetricsCurrentParams = {}) =>
    api
      .get<{ data: OpsMetricSample[] }>(`/api/admin/ops-metrics/current${qs({
        nodeType: p.nodeType,
        nodeId: p.nodeId,
      })}`)
      .then((r) => r.data),

  timeseries: (p: OpsMetricsTimeseriesParams) =>
    api.get<TimeseriesResponse>(
      `/api/admin/ops-metrics/timeseries${qs({
        nodeId: p.nodeId,
        metric: p.metric,
        from: p.from,
        to: p.to,
        dim: p.dim,
        granularity: p.granularity,
      })}`,
    ),

  fleet: (p: OpsMetricsFleetParams) =>
    api.get<TimeseriesResponse>(
      `/api/admin/ops-metrics/fleet${qs({
        nodeType: p.nodeType,
        metric: p.metric,
        from: p.from,
        to: p.to,
        dim: p.dim,
        granularity: p.granularity,
      })}`,
    ),
};
