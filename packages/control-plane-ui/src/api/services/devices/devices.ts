import { api } from '../../client';
import type { AgentDevice, AgentAuditEvent, EnrollTokenResponse, DeviceAssignmentDetail } from '../../types';

export const devicesApi = {
  list: (params?: Record<string, string>) =>
    api.get<{ data: AgentDevice[]; total: number }>('/api/admin/agent-devices', params),

  get: (id: string) =>
    api.get<AgentDevice>(`/api/admin/agent-devices/${id}`),

  getEvents: (id: string, params?: Record<string, string>) =>
    api.get<{ data: AgentAuditEvent[]; total: number }>(`/api/admin/agent-devices/${id}/events`, params),

  /**
   * DeviceAssignment history for one device — paginated, newest first.
   * Hits CP's GET /api/admin/agent-devices/:id/assignments (IAM:
   * agent-device:read).
   */
  getAssignments: (id: string, params?: Record<string, string>) =>
    api.get<{ data: DeviceAssignmentDetail[]; total: number }>(`/api/admin/agent-devices/${id}/assignments`, params),

  /**
   * Force Hub to re-broadcast every desired config key to this device
   * right now. CP's POST /api/admin/agent-devices/:id/force-refresh
   * (IAM: agent-device:force-resync) proxies to Hub's resync endpoint
   * with an empty body (=> RePushAllKeys).
   */
  forceRefresh: (id: string) =>
    api.post<{ thingsNotified?: number; thingDesiredVer?: number }>(`/api/admin/agent-devices/${id}/force-refresh`),

  /**
   * Force out-of-cycle mTLS cert rotation. CP's
   * POST /api/admin/agent-devices/:id/rotate-cert (IAM:
   * agent-device:rotate) proxies to Hub's /things/:id/rotate-cert which
   * advances thing_agent.cert_expires_at to NOW() + 5min. Agent picks
   * up "near expiry" on next heartbeat (<=15s) and runs the existing
   * /api/internal/things/renew-cert flow.
   */
  rotateCert: (id: string) =>
    api.post<{ ok: boolean; thingId: string }>(`/api/admin/agent-devices/${id}/rotate-cert`),

  /**
   * Replace the device's full tag set. PUT semantics — pass the complete
   * desired array. Empty array clears all tags.
   */
  setTags: (id: string, tags: string[]) =>
    api.put<{ id: string; tags: string[] }>(`/api/admin/agent-devices/${id}/tags`, { tags }),

  generateEnrollToken: (hostname?: string) =>
    api.post<EnrollTokenResponse>('/api/admin/agent-devices/enroll-token', { hostname }),

  unenroll: (id: string) =>
    api.post<AgentDevice>(`/api/admin/agent-devices/${id}/unenroll`),

  /**
   * Fleet-wide agent runtime defaults — pushed to every agent via the
   * agent_settings shadow config key. Covers:
   *   - quitAllowed: gates Restart Agent + Quit menu items on the Swift menu bar
   *   - shutdownWarning: optional per-locale warning text shown on quit attempt
   */
  getAgentSettings: () =>
    api.get<AgentSettingsResponse>('/api/admin/settings/device-defaults'),

  updateAgentSettings: (data: AgentSettingsRequest) =>
    api.put<AgentSettingsResponse>('/api/admin/settings/device-defaults', data),

  /**
   * Self-service: list the currently logged-in admin user's own
   * enrolled agent devices. No IAM gate (data is inherently
   * caller-scoped via DeviceAssignment lookup). Used by the Agent
   * Setup page's live Verify panel to show the user real-time
   * status of THEIR install attempts — including when nothing has
   * enrolled yet (renders an "install in progress" hint).
   *
   * Returns the slimmer FleetUserDevice JSON shape (the backend
   * uses ListDevicesByUserID, NOT the fleet-wide AgentDevice
   * scan). Only the fields needed for the Verify panel are typed.
   */
  listMine: () =>
    api.get<{ data: MyAgentDevice[]; total: number }>('/api/admin/me/agent-devices'),
};

/**
 * Slim per-user device row returned by GET /api/admin/me/agent-devices.
 * Mirrors store.FleetUserDevice (fleet_queries.go). Fewer fields than
 * the fleet-wide AgentDevice — we only have what ListDevicesByUserID
 * pulls (id + hostname + os + status + heartbeat + assignment metadata).
 */
export interface MyAgentDevice {
  id: string;
  hostname: string;
  os: string;
  osVersion: string;
  agentVersion: string;
  /** enrolled | online | offline | drift | revoked */
  status: string;
  lastHeartbeat: string | null;
  /** When the current DeviceAssignment for this user opened. */
  assignedAt: string;
  /** enrollment | login | heartbeat | manual */
  assignmentSource: string;
}

/**
 * Wire shape of /api/admin/settings/device-defaults responses. Mirrors
 * the AgentSettings JSON the CP backend writes into the `agent.settings`
 * system_metadata row; values fan out to every agent via the
 * `agent_settings` shadow config key. Fields beyond quitAllowed +
 * shutdownWarning were originally fixed at seed-time defaults; admin
 * can now edit them all via #73 (CP UI Runtime Defaults card).
 */
export interface AgentSettingsResponse {
  quitAllowed: boolean;
  shutdownWarning?: Record<string, string>;
  shutdownWarningEnabled?: boolean;
  /** Reporting cadence in seconds. 0 = "agent falls back to YAML default". */
  heartbeatIntervalSec?: number;
  auditDrainIntervalSec?: number;
  configSyncIntervalSec?: number;
  /** Max rows per audit upload batch. */
  auditBatchSize?: number;
  autoUpdateEnabled?: boolean;
  /** "stable" | "beta" — pinned channel for signed updates. */
  autoUpdateChannel?: string;
  /** Daemon log level — "debug" | "info" | "warn" | "error". */
  logLevel?: string;
  /**
   * trafficUploadLevel — closed enum {"all","processed","blocked"} gating
   * which agent FlowResult events reach Hub's traffic_event ledger.
   *   - "all":       every flow, including silent TCP passthroughs
   *   - "processed": flows where the agent inspected HTTP / matched a
   *                  provider adapter / ran hooks (recommended default)
   *   - "blocked":   only deny / block_soft / reject_hard / error rows
   * Empty value means the admin never set it; the agent applies its
   * own default ("processed") via config.applyDefaults.
   */
  trafficUploadLevel?: string;
  /** Theme pack ID applied fleet-wide (see AgentSettingsRequest.themeId). */
  themeId?: string;
  /**
   * forceQUICFallbackBundles — macOS bundle-ID allowlist consumed by the
   * NE proxy. Apps on this list have their UDP flows closed so they fall
   * back from HTTP/3 (QUIC) to HTTP/2 (TCP) which the agent's TLS-bump
   * path actually sees. Empty = no UDP gets killed. NEVER add system
   * processes (mdnsresponder, dhcp, ntp); doing so breaks DNS and takes
   * the host network down (fail-open safety rule — never add system processes).
   */
  forceQUICFallbackBundles?: string[];
  /**
   * bypassBundles — macOS SOURCE-app exemption list consumed by the NE
   * proxy. A flow whose originating bundle matches an entry is passed
   * through WITHOUT inspection (no TLS bump, no audit). Matching is by
   * source bundle, never by host, so the same destination stays inspected
   * from other apps. Empty = exempt nothing (inspect everything); ships
   * empty. Used for trusted tools whose pinned TLS breaks under bump.
   */
  bypassBundles?: string[];
  /**
   * Fleet-wide opt-in for agent attestation. When true, agents sign every
   * outbound CONNECT with their Ed25519 key so the compliance-proxy can
   * transparently tunnel (skip MITM + hooks) on a verified signature.
   * Default false. This is a perf optimization, not a security gate.
   */
  attestationEnabled?: boolean;
}

/**
 * PATCH-shape of the update endpoint. Every field is optional; the
 * server treats `undefined` as "leave existing value alone" so the
 * UI can ship granular updates without re-sending the whole blob.
 */
export interface AgentSettingsRequest {
  quitAllowed?: boolean;
  shutdownWarning?: Record<string, string>;
  shutdownWarningEnabled?: boolean;
  heartbeatIntervalSec?: number;
  auditDrainIntervalSec?: number;
  configSyncIntervalSec?: number;
  auditBatchSize?: number;
  autoUpdateEnabled?: boolean;
  autoUpdateChannel?: string;
  logLevel?: string;
  trafficUploadLevel?: string;
  /**
   * Theme pack ID admin pushes to every agent Dashboard in the fleet.
   * Empty = no fleet override (each user keeps their local pick). Unknown
   * IDs cause the Dashboard to fall back to its bundled `default` theme.
   * See packages/agent/internal/policies/applied.go DeviceDefaultsView.
   */
  themeId?: string;
  /** See AgentSettingsResponse.forceQUICFallbackBundles. */
  forceQUICFallbackBundles?: string[];
  /** See AgentSettingsResponse.bypassBundles. */
  bypassBundles?: string[];
  /** See AgentSettingsResponse.attestationEnabled. */
  attestationEnabled?: boolean;
}
