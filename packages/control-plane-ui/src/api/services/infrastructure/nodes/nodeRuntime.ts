/**
 * Node runtime introspection service (e31-s7).
 *
 * Calls Control Plane's GET /api/admin/nodes/:id/runtime, which proxies to
 * Hub's GET /api/hub/things/:id/runtime, which reverse-calls the thing's
 * /debug/runtime endpoint. Returns the live in-memory configuration and
 * cache state of the target node alongside Hub's view of desired/reported
 * shadow state, so the UI can diff "what Hub thinks" vs "what the service
 * actually has applied".
 */
import { api } from '../../../client';

export interface RuntimeSourceResult {
  ok: boolean;
  value?: unknown;
  error?: string;
}

export interface RuntimeServiceMeta {
  service: string;
  thing_id: string;
  thing_version: string;
  process_started_at: string;
}

export interface RuntimeSnapshot {
  meta: RuntimeServiceMeta;
  snapshot_taken_at: string;
  sources: Record<string, RuntimeSourceResult>;
}

export interface RuntimeBridgeMeta {
  thing_id: string;
  thing_type: string;
  thing_status: string;
  desired_ver: number;
  reported_ver: number;
  last_seen_at?: string;
  desired?: Record<string, unknown> | null;
  reported?: Record<string, unknown> | null;
}

export interface NodeRuntimeResponse {
  snapshot: RuntimeSnapshot;
  meta: RuntimeBridgeMeta;
}

export const nodeRuntimeApi = {
  get: (id: string) =>
    api.get<NodeRuntimeResponse>(`/api/admin/nodes/${id}/runtime`),
};
