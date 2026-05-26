/**
 * Hub API service — Infrastructure management (nodes, config sync, jobs).
 *
 * This module is the UI-side client for the Control Plane's admin API, which
 * in turn proxies the Nexus Hub runtime API. All types and URLs use
 * product-facing terms (node / targetConfig / appliedConfig / outOfSync).
 */
import { api } from '../../../client';

export interface Node {
  id: string;
  type: string;
  name: string;
  status: string;
  listen_address: string | null;
  metrics_url: string | null;
  version: string | null;
  role: string | null;
  auth_type: string;
  conn_protocol: string;
  targetConfig: Record<string, unknown> | null;
  targetVersion: number;
  appliedConfig: Record<string, unknown> | null;
  appliedVersion: number;
  last_seen_at: string | null;
  created_at: string;
  updated_at: string;
  /**
   * Number of active per-Thing overrides on this node (rows in
   * thing_config_override). 0 when none. Set by the Hub ListThings JOIN —
   * GetNode does not populate it.
   */
  overrideCount: number;
  /**
   * Subset of overrideCount where the matching template has bumped past
   * `template_ver_at_set`, i.e. the override is pinning an out-of-date
   * value the admin should review. 0 when none stale.
   */
  overrideStaleCount: number;
  /**
   * True when at least one of the node's active overrides targets
   * `killswitch` AND has `emergency_override=true`. The list page renders
   * these rows with a red bypass marker so an SRE scanning the table can
   * tell at a glance which nodes are deliberately running with the global
   * killswitch off (AC14). Defaults to false on legacy server responses
   * that don't include the field.
   */
  hasKillswitchBypass?: boolean;
  /**
   * Raw thing.metadata blob — flexible JSONB written by services
   * (selfreg for Hub, enrollment for agents, sysinfo for endpoints).
   * May be null when the producer hasn't published metadata yet.
   */
  metadata?: Record<string, unknown> | null;
  /**
   * Per-config-key apply outcome ledger. Keyed by config_key; carries
   * the most recent ApplyError plus the last known successful
   * AppliedAt / AppliedVersion. Empty / missing means the node hasn't
   * reported any apply outcome on its current process.
   */
  appliedOutcomes?: Record<string, NodeAppliedOutcome> | null;
  /**
   * Wall-clock the node's current process came online, captured by
   * Hub on the offline→online edge. Used to derive uptime and to
   * distinguish "fresh process, no apply yet" from "ledger lost".
   * Null until the node connects for the first time.
   */
  processStartedAt?: string | null;
  /**
   * OS hostname promoted out of metadata.staticInfo into a first-class
   * column by migration 20260522_thing_identity_columns. For agents this
   * is the user's machine name; for server services it's the container
   * / EC2 hostname. Empty string when never populated.
   */
  hostname?: string;
  /**
   * Last reported primary IP. Promoted out of metadata.staticInfo.
   * For agents = local NIC IP, for services = listen address.
   */
  primaryIp?: string;
  /** OS family — `darwin` | `linux` | `windows`. */
  os?: string;
  /** OS version label (e.g. `26.3.1`). */
  osVersion?: string;
  /**
   * Stable natural key for this Thing — hardware fingerprint (agents)
   * or yaml id / hostname-type-port derivation (services). See migration
   * 20260521_thing_physical_id_column for the dedupe-via-UNIQUE design.
   */
  physicalId?: string;
  /**
   * Currently-bound user (DeviceAssignment WHERE releasedAt IS NULL).
   * NULL/empty for service Things (they don't carry user assignments)
   * and for agents with no active assignment.
   */
  boundUserId?: string;
  boundUserDisplayName?: string;
  boundUserEmail?: string;
}

/**
 * Single per-config-key apply outcome as reported by a node and
 * persisted by Hub. See packages/shared/thingclient/outcomes.go for
 * the canonical contract.
 */
export interface NodeAppliedOutcome {
  /** Last KNOWN successful apply timestamp (RFC3339). Null before any success. */
  appliedAt?: string | null;
  /** Last KNOWN successful apply version. Null before any success. */
  appliedVersion?: number | null;
  /** Most recent failed apply attempt; cleared the moment a fresh success lands. */
  applyError?: NodeApplyError | null;
}

export interface NodeApplyError {
  message: string;
  at: string;
}

export interface NodeListResponse {
  nodes: Node[];
  total: number;
  page: number;
  pageSize: number;
}

export interface ConfigSyncComparison {
  nodeId: string;
  targetConfig: Record<string, unknown>;
  appliedConfig: Record<string, unknown>;
  targetVersion: number;
  appliedVersion: number;
  outOfSync: boolean;
  outOfSyncKeys: string[];
}

export interface OutOfSyncItem {
  nodeId: string;
  nodeType: string;
  name: string;
  outOfSyncKeys: string[];
  lastSeen: string | null;
}

export interface ConfigHistoryEvent {
  id: string;
  nodeType: string;
  configKey: string;
  action: string;
  actorId: string;
  actorName: string;
  newVersion: number;
  sourceIp: string;
  createdAt: string;
}

export interface ConfigHistoryResponse {
  events: ConfigHistoryEvent[];
  total: number;
  page: number;
  pageSize: number;
}

export interface ConfigCatalogEntry {
  nodeType: string;
  configKeys: string[];
}

export interface ConfigCatalogResponse {
  entries: ConfigCatalogEntry[];
}

export interface ScheduledJob {
  id: string;
  name: string;
  description: string;
  interval: number;
  enabled: boolean;
  lastRun: string | null;
  lastDuration: number;
  lastStatus: string;
  lastError?: string;
  nextRun: string | null;
  runCount: number;
  errorCount: number;
}

export interface JobRun {
  id: string;
  jobId: string;
  startedAt: string;
  finishedAt?: string | null;
  durationMs?: number | null;
  status: string; // running | success | error | skipped
  error?: string;
  replicaId?: string;
}

export interface ConfigUpdateRequest {
  nodeType: string;
  configKey: string;
  state?: unknown;
  action?: string;
}

export interface EnrollmentToken {
  id: string;
  token: string;
  label: string;
  nodeType: string;
  usedBy: string | null;
  usedAt: string | null;
  expiresAt: string;
  createdAt: string;
}

/**
 * Per-Thing config override row, mirrors `ThingOverride` schema in
 * docs/users/api/openapi/admin/e34-s1-thing-override-and-force-sync.yaml.
 * `stale` is server-computed (`currentTemplateVer > templateVerAtSet`).
 * Used by the override editor drawer and the global registry page.
 */
export type ThingOverride = {
  configKey: string;
  state: unknown;
  templateVerAtSet: number;
  currentTemplateVer: number;
  stale: boolean;
  setBy: string;
  setAt: string;
  reason?: string;
  expiresAt?: string;
  emergencyOverride: boolean;
};

/**
 * Row in the global override registry (`/infrastructure/overrides`). Adds
 * `nodeId / nodeName / nodeType` so the table can render owning-node
 * context without a per-row Node lookup.
 */
export type GlobalOverrideRow = ThingOverride & {
  nodeId: string;
  nodeName: string;
  nodeType: string;
};

/**
 * Summary counters returned by `GET /api/admin/nodes/overrides`. Drives
 * the four header tiles on the overrides page.
 */
export type GlobalOverridesSummary = {
  totalNodes: number;
  totalOverrides: number;
  staleCount: number;
  /** Rows whose `expiresAt` falls within the next 1 hour. */
  expiringSoonCount: number;
};

export type GlobalOverridesResponse = {
  overrides: GlobalOverrideRow[];
  total: number;
  summary: GlobalOverridesSummary;
};

export type ListThingOverridesResponse = {
  overrides: ThingOverride[];
};

/**
 * Per-key failure entry returned by the whole-Node resync. Mirrors
 * `RePushFailure` on the Hub side. Only populated when at least one key
 * failed to deliver during a `POST /resync` with no body — the call still
 * resolves 200 (best-effort fan-out), and the UI renders a partial-success
 * toast based on `failed.length`.
 */
export type ResyncFailure = {
  configKey: string;
  error: string;
};

/**
 * Response body for `POST /api/admin/nodes/{id}/resync`. `configKey` is
 * present only for the single-key form; `keyCount` and `failed` only for the
 * empty-body whole-Node form. `failed` is omitted when every key delivered.
 */
export type ResyncResponse = {
  ok: true;
  nodeId: string;
  configKey?: string;
  keyCount?: number;
  failed?: ResyncFailure[];
};

/**
 * Request body for `PUT /api/admin/nodes/{id}/overrides/{configKey}`.
 * Server validates: state is a JSON object, reason ≤ 500 chars,
 * expiresAt ∈ NOW + [5m, 30d] when set.
 */
export type SetOverrideBody = {
  state: Record<string, unknown>;
  reason?: string;
  expiresAt?: string;
};

/**
 * One entry in the Applied Config map keyed by `configKey`. `targetConfig`
 * is the configuration the Hub wants pushed to the node; `appliedConfig` is
 * what the node last acknowledged (raw JSON — the UI does not assume a
 * shape). `appliedConfig` may be `null`/`undefined` when the node has never
 * reported. `lastChange` is omitted when the audit table has no row for this
 * key yet.
 *
 * `templateState` / `templateVer` / `override` are surfaced so the
 * Configuration tab can render the four-column merged view (template ⊕
 * override ⊕ target ⊕ applied) from a single endpoint.
 */
export interface AppliedConfigEntry {
  targetConfig: unknown;
  /** Monotonic Node config target version (same on every key row). */
  targetVersion: number;
  appliedConfig?: unknown;
  /** Monotonic Node config applied version (same on every key row). */
  appliedVersion: number;
  /** Read-only template default for this key — used by the editor drawer's left pane. */
  templateState?: unknown;
  /** thing_config_template.version for this key. */
  templateVer?: number;
  /** Present only when an override is active for this (node, configKey). */
  override?: ThingOverride;
  inSync: boolean;
  lastChange?: {
    timestamp: string;
    actor: string;
    action: string;
    emergencyOverride: boolean;
  };
}

export interface AppliedConfigResponse {
  nodeId: string;
  nodeType: string;
  /** Monotonic Node target-config version. */
  targetVersion?: number;
  /** Monotonic Node applied-config version. */
  appliedVersion?: number;
  configs: Record<string, AppliedConfigEntry>;
}

function toStringParams(
  params?: Record<string, string | number | boolean | undefined>,
): Record<string, string> | undefined {
  if (!params) return undefined;
  const out: Record<string, string> = {};
  for (const [k, v] of Object.entries(params)) {
    if (v === undefined || v === '') continue;
    out[k] = typeof v === 'boolean' ? (v ? 'true' : 'false') : String(v);
  }
  return Object.keys(out).length > 0 ? out : undefined;
}

export const hubApi = {
  listNodes: (params?: {
    type?: string;
    status?: string;
    search?: string;
    /** Filter by presence of active per-Thing overrides. true = only nodes with at least one override; false = only nodes without; omit for no filter. */
    hasOverrides?: boolean;
    page?: number;
    pageSize?: number;
  }) =>
    api.get<NodeListResponse>('/api/admin/nodes', toStringParams(params)),

  getNode: (id: string) =>
    api.get<Node>(`/api/admin/nodes/${id}`),

  getNodeConfigSync: (id: string) =>
    api.get<ConfigSyncComparison>(`/api/admin/nodes/${id}/config-sync`),

  listOutOfSync: () =>
    api.get<{ outOfSync: OutOfSyncItem[]; total: number }>('/api/admin/config-sync/out-of-sync'),

  listConfigHistory: (params?: { nodeType?: string; configKey?: string; page?: number; pageSize?: number }) =>
    api.get<ConfigHistoryResponse>('/api/admin/config-sync/history', toStringParams(params)),

  // Catalog of (nodeType, configKey) pairs that actually exist in the Hub's
  // config template table. Feeds the cascading Type / Config Key filter on
  // the Config Sync history page so the dropdowns only offer options that
  // can yield non-empty results.
  listConfigCatalog: () =>
    api.get<ConfigCatalogResponse>('/api/admin/config-sync/catalog'),

  pushConfigUpdate: (data: ConfigUpdateRequest) =>
    api.post<{
      ok: boolean;
      version: number;
      /** Hub thing.desired_ver after update (monotonic shadow revision). */
      targetShadowVersion?: number;
      nodesNotified: number;
      nodesOnline: number;
    }>('/api/admin/config-sync/update', data),

  // Per-node, per-key replay of the Hub's current target state. Unlike
  // pushConfigUpdate, this does not bump the config template version or write
  // a config change history row — it only redelivers the state this node is
  // already supposed to be running. Used by the "Re-sync" buttons on
  // Node Detail and the Out-of-Sync monitor.
  resyncNode: (nodeId: string, configKey: string) =>
    api.post<{ ok: boolean; thingId: string; configKey: string }>(
      `/api/admin/nodes/${encodeURIComponent(nodeId)}/resync`,
      { configKey },
    ),

  listJobs: (params?: { limit?: number; offset?: number; search?: string; enabled?: string }) =>
    api.get<{ jobs: ScheduledJob[]; total: number; limit: number; offset: number }>(
      '/api/admin/jobs',
      toStringParams(params),
    ),

  getJob: (id: string) =>
    api.get<ScheduledJob>(`/api/admin/jobs/${id}`),

  listJobRuns: (id: string, params?: { limit?: number; offset?: number }) =>
    api.get<{ runs: JobRun[]; total: number; limit: number; offset: number }>(
      `/api/admin/jobs/${id}/runs`,
      toStringParams(params),
    ),

  triggerJob: (id: string) =>
    api.post<{ ok: boolean; jobId: string; triggeredAt: string }>(
      `/api/admin/jobs/${id}/trigger`,
    ),

  updateJob: (id: string, data: { enabled: boolean }) =>
    api.put<{ ok: boolean; jobId: string; enabled: boolean }>(
      `/api/admin/jobs/${id}`,
      data,
    ),

  listEnrollmentTokens: () =>
    api.get<{ tokens: EnrollmentToken[]; total: number }>('/api/admin/enrollment/tokens'),

  createEnrollmentToken: (data: { label: string; nodeType?: string }) =>
    api.post<{ token: string; expiresAt: string }>('/api/admin/enrollment/token', data),

  getAppliedConfig: (nodeId: string) =>
    api.get<AppliedConfigResponse>(`/api/admin/nodes/${nodeId}/applied-config`),

  /**
   * Per-Node override registry. Returns every active override for the
   * given node, with `stale` and `currentTemplateVer` populated server-side.
   */
  listOverrides: (nodeId: string) =>
    api.get<ListThingOverridesResponse>(
      `/api/admin/nodes/${encodeURIComponent(nodeId)}/overrides`,
    ),

  /**
   * Set or update a single override (whole-key replacement). Server
   * recomputes the target, bumps the version, audit-logs, and force-pushes
   * the affected key.
   */
  setOverride: (nodeId: string, configKey: string, body: SetOverrideBody) =>
    api.put<ThingOverride>(
      `/api/admin/nodes/${encodeURIComponent(nodeId)}/overrides/${encodeURIComponent(configKey)}`,
      body,
    ),

  /**
   * Remove an override row; key reverts to template, server bumps the
   * target version, audit-logs, and force-pushes. Resolves with no body —
   * `void` matches the `api.delete` client wrapper, which discards the
   * `{ok:true}` envelope the server actually returns.
   */
  clearOverride: (nodeId: string, configKey: string) =>
    api.delete(
      `/api/admin/nodes/${encodeURIComponent(nodeId)}/overrides/${encodeURIComponent(configKey)}`,
    ),

  /**
   * Global override registry. Powers `/infrastructure/overrides`. Read-only
   * — there is no bulk mutation surface here.
   */
  listGlobalOverrides: (params?: {
    type?: string;
    actor?: string;
    /** Only rows with non-NULL `expires_at` (TTL set). */
    hasTtl?: boolean;
    /** Only rows where the template has bumped past `template_ver_at_set`. */
    stale?: boolean;
    limit?: number;
    offset?: number;
  }) =>
    api.get<GlobalOverridesResponse>('/api/admin/nodes/overrides', toStringParams(params)),

  /**
   * Force-sync. Empty body → replay every key in the Node's target config;
   * with `{configKey}` → replay only that key. Audit-logs as
   * `node_force_resync_*` but does NOT bump template versions (this is
   * redelivery, not change).
   */
  resyncNodeAll: (nodeId: string, body?: { configKey?: string }) =>
    api.post<ResyncResponse>(
      `/api/admin/nodes/${encodeURIComponent(nodeId)}/resync`,
      body ?? {},
    ),
};
