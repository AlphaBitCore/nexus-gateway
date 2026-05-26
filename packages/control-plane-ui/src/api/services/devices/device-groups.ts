import { api } from '../../client';

export interface DeviceGroup {
  id: string;
  name: string;
  description: string | null;
  createdBy: string | null;
  createdAt: string;
  updatedAt: string;
}

export interface DeviceGroupListItem extends DeviceGroup {
  memberCount: number;
}

export interface DeviceGroupMembership {
  id: string;
  groupId: string;
  deviceId: string;
  createdAt: string;
  /** Optional auto-expiry. RFC3339 or null. */
  expiresAt?: string | null;
  device: { id: string; hostname: string; os: string; status: string };
}

export interface DeviceGroupDetail extends DeviceGroup {
  memberships: DeviceGroupMembership[];
  /** Non-null when the group is in smart mode. */
  membershipQuery?: unknown | null;
  /** Group cascade priority (higher = wins on tie). */
  priority?: number;
}

export interface CreateDeviceGroupInput {
  name: string;
  description?: string;
}

export interface UpdateDeviceGroupInput {
  name?: string;
  description?: string;
}

// Smart group preview response.
export interface PreviewMembershipResponse {
  matched: number;
  sample: string[];
}

// Bulk-by-group result row.
export interface BulkActionResult {
  deviceId: string;
  ok: boolean;
  error?: string;
}

export interface BulkActionResponse {
  groupId: string;
  action: string;
  total: number;
  succeeded: number;
  failed: number;
  results: BulkActionResult[];
}

const BASE = '/api/admin/device-groups';

export const deviceGroupsApi = {
  list: (params?: Record<string, string>) =>
    api.get<{ data: DeviceGroupListItem[]; total: number }>(BASE, params),

  get: (id: string) => api.get<DeviceGroupDetail>(`${BASE}/${id}`),

  create: (input: CreateDeviceGroupInput) => api.post<DeviceGroup>(BASE, input),

  update: (id: string, input: UpdateDeviceGroupInput) => api.put<DeviceGroup>(`${BASE}/${id}`, input),

  delete: (id: string) => api.delete(`${BASE}/${id}`),

  addMember: (groupId: string, deviceId: string, expiresAt?: string) =>
    api.post<DeviceGroupMembership>(`${BASE}/${groupId}/members`, expiresAt
      ? { deviceId, expiresAt }
      : { deviceId }),

  removeMember: (groupId: string, deviceId: string) =>
    api.delete(`${BASE}/${groupId}/members/${deviceId}`),

  // Smart-group preview / save.
  previewMembership: (membershipQuery: unknown) =>
    api.post<PreviewMembershipResponse>(`${BASE}/preview-membership`, { membershipQuery }),

  setMembershipQuery: (groupId: string, membershipQuery: unknown | null) =>
    api.put<DeviceGroup>(`${BASE}/${groupId}/membership-query`, { membershipQuery }),

  // Bulk-by-group operations.
  bulkForceRefresh: (groupId: string) =>
    api.post<BulkActionResponse>(`${BASE}/${groupId}/force-refresh`),

  bulkRotateCert: (groupId: string) =>
    api.post<BulkActionResponse>(`${BASE}/${groupId}/rotate-cert`),
};
