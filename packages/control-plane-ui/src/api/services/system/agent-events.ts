import { api } from '../../client';
import type { AgentAuditEvent } from '../../types';

/** Row shape returned by `GET /api/admin/agent-events` (joined device fields may be flat or nested). */
export type AgentEventListRow = AgentAuditEvent & {
  deviceHostname?: string | null;
  deviceOs?: string | null;
  device?: { hostname: string; os: string };
};

export interface AgentEventExportResponse {
  exportedAt: string;
  truncated: boolean;
  count: number;
  entries: AgentEventListRow[];
}

export const agentEventsApi = {
  search: (params?: Record<string, string>) =>
    api.get<{ data: AgentEventListRow[]; total: number }>('/api/admin/agent-events', params),

  export: (params?: Record<string, string>) =>
    api.get<AgentEventExportResponse>('/api/admin/agent-events/export', params),
};
