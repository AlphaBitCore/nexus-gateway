import { api } from '../../client';
import type {
  AgentUserSafe, AgentUserDetail, FleetUserDevice,
  FleetAuditEvent, DeviceAssignmentDetail,
} from '../../types';

export const fleetApi = {
  listAgentUsers: (params?: Record<string, string>) =>
    api.get<{ data: AgentUserSafe[]; total: number }>('/api/admin/agent-users', params),

  getAgentUser: (id: string) =>
    api.get<AgentUserDetail>(`/api/admin/agent-users/${id}`),

  getUserDevices: (id: string, params?: Record<string, string>) =>
    api.get<{ data: FleetUserDevice[]; total: number }>(`/api/admin/agent-users/${id}/devices`, params),

  getUserAudit: (id: string, params?: Record<string, string>) =>
    api.get<{ data: FleetAuditEvent[]; total: number }>(`/api/admin/agent-users/${id}/audit`, params),

  suspendUser: (id: string) =>
    api.post<AgentUserSafe>(`/api/admin/agent-users/${id}/suspend`),

  activateUser: (id: string) =>
    api.post<AgentUserSafe>(`/api/admin/agent-users/${id}/activate`),

  getDeviceAudit: (id: string, params?: Record<string, string>) =>
    api.get<{ data: FleetAuditEvent[]; total: number }>(`/api/admin/agent-devices/${id}/audit`, params),

  getDeviceConfig: (id: string) =>
    api.get<Record<string, unknown>>(`/api/admin/agent-devices/${id}/config`),

  getDeviceTimeline: (id: string) =>
    api.get<{ data: DeviceAssignmentDetail[] }>(`/api/admin/agent-devices/${id}/timeline`),

  getDeviceAuthSettings: () =>
    api.get<{
      mode: string;
      ssoConfigured: boolean;
      ssoProviders: Array<{ id: string; type: string; name: string }>;
      localLoginAvailable: boolean;
    }>('/api/admin/settings/device-auth'),

  updateDeviceAuthSettings: (data: { mode: string }) =>
    api.put<{
      mode: string;
      ssoConfigured: boolean;
      ssoProviders: Array<{ id: string; type: string; name: string }>;
      localLoginAvailable: boolean;
    }>('/api/admin/settings/device-auth', data),
};
