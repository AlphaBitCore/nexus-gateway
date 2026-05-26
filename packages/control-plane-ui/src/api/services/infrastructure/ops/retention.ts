/**
 * Observability retention admin API service —
 * `GET / PUT /api/admin/observability/retention`.
 *
 * Eleven layers (per spec §5.5) live in `metric_ops_retention_config` and are
 * re-read by Hub's ops-retention job on every tick. The server returns the
 * current value plus the allowed [min, max] day window per layer; PUT atomically
 * updates one or more layers and 400s on any out-of-range or unknown key.
 *
 * Layer set is fixed; the server enumerates rows in a stable alphabetical
 * order so the UI list does not jitter across reads.
 */

import { api } from '../../../client';

/** Catalog of known retention layers (spec §5.5). Forward-compat: the server may add new keys. */
export type RetentionLayerName =
  | 'runtime_raw'
  | 'business_raw'
  | 'runtime_1h'
  | 'business_1h'
  | 'runtime_1d'
  | 'business_1d'
  | 'runtime_1mo'
  | 'business_1mo'
  | 'diag_warn'
  | 'diag_error'
  | 'diag_fatal';

/** One row in the GET response: current value + allowed bounds + last-write timestamp. */
export interface RetentionLayer {
  value: number;
  min: number;
  max: number;
  /** Empty / missing on layers never updated since seed. */
  updatedAt?: string;
}

export interface RetentionGetResponse {
  /** Layer key → entry. The server returns keys in deterministic order. */
  retention: Record<string, RetentionLayer>;
}

/** PUT body: a `{ layer: days }` map. Empty body 400s. */
export type RetentionUpdate = Partial<Record<RetentionLayerName | string, number>>;

export interface RetentionPutResponse {
  ok: boolean;
  updated: number;
}

export const retentionApi = {
  get: () => api.get<RetentionGetResponse>('/api/admin/observability/retention'),

  put: (body: RetentionUpdate) =>
    api.put<RetentionPutResponse>('/api/admin/observability/retention', body),
};
