/**
 * Per-Thing stats admin API — `GET /api/admin/things/:id/stats`.
 *
 * Source of truth for the Stats tab on Fleet device detail (agent) and
 * Node detail (ai-gateway / compliance-proxy). Backed by Hub's
 * `thing_metric_rollup_*` cascade (5m / 1h / 1d / 1mo). Granularity is
 * server-chosen from the requested window and echoed back.
 *
 * Agent things returning `enabled: false` mean Hub's `enableAgentRollup`
 * toggle is OFF — UI renders a "view local UI" banner instead of empty charts.
 */

import { api } from '../../../client';

export type ThingStatsGranule = '5m' | '1h' | '1d' | '1mo';

export interface ThingStatsRow {
  bucketStart: string;
  metricName: string;
  dimensionKey?: string;
  subDimension?: string;
  value: number;
  metadata?: Record<string, unknown> | unknown[] | null;
}

export interface ThingStatsResponse {
  thingId: string;
  thingType: string;
  thingName?: string;
  enabled: boolean;
  rollupDisabledMessage?: string;
  startTime: string;
  endTime: string;
  granule: ThingStatsGranule | string;
  rows: ThingStatsRow[];
  /**
   * Server-side display-name lookup for ID-typed dimension values.
   * Keys are the bare dim value (the part after `=` in `dimensionKey`),
   * values are the matching display name (Provider.name, Model.name,
   * Organization.name, etc.). Missing entries simply fall back to the
   * raw value in the UI. Covered dims: provider / routed_provider /
   * model / routed_model / organization / project / virtual_key /
   * routing_rule / user / device. Pass-through dims (target_host /
   * hook_decision / source) are already human strings and are omitted.
   */
  displayNames?: Record<string, string>;
}

export interface ThingStatsParams {
  start?: string;
  end?: string;
  /** Comma-joined when multiple. */
  metric?: string;
  /** Dimension name, e.g. "model" / "provider" / "target_host" / "source_process" / "action". Empty = global rows only. */
  dimension?: string;
  /** Exact subDimension filter (rarely needed from UI). */
  subDimension?: string;
}

function qs(p: Record<string, string | undefined>): string {
  const u = new URLSearchParams();
  for (const [k, v] of Object.entries(p)) {
    if (v != null && v !== '') u.set(k, v);
  }
  const s = u.toString();
  return s ? `?${s}` : '';
}

export const thingStatsApi = {
  get: (thingId: string, params: ThingStatsParams = {}) =>
    api.get<ThingStatsResponse>(
      `/api/admin/things/${encodeURIComponent(thingId)}/stats${qs({
        start: params.start,
        end: params.end,
        metric: params.metric,
        dimension: params.dimension,
        subDimension: params.subDimension,
      })}`,
    ),
};
