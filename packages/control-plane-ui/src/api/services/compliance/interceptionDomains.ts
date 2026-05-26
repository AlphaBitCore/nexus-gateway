/**
 * Interception domains API service — CRUD over /api/admin/interception-domains.
 */
import { api } from '../../client';

export type HostMatchType = 'EXACT' | 'PREFIX' | 'GLOB' | 'REGEX';
export type PathMatchType = 'EXACT' | 'PREFIX' | 'GLOB' | 'REGEX';
export type PathAction = 'PROCESS' | 'PASSTHROUGH' | 'BLOCK';
export type DefaultPathAction = 'PROCESS' | 'PASSTHROUGH' | 'BLOCK';
export type FailureAction = 'FAIL_OPEN' | 'FAIL_CLOSED';
export type NetworkZone = 'PUBLIC' | 'INTERNAL';

export interface InterceptionPath {
  id: string;
  domainId?: string;
  pathPattern: string[];
  matchType: PathMatchType;
  action: PathAction;
  priority: number;
  description?: string | null;
  enabled: boolean;
  createdAt?: string;
  updatedAt?: string;
}

export interface InterceptionDomain {
  id: string;
  name: string;
  description?: string | null;
  hostPattern: string;
  hostMatchType: HostMatchType;
  adapterId: string;
  adapterConfig?: Record<string, unknown> | null;
  enabled: boolean;
  priority: number;
  defaultPathAction: DefaultPathAction;
  onAdapterError: FailureAction;
  networkZone: NetworkZone;
  source?: string;
  createdAt?: string;
  updatedAt?: string;
  createdBy?: string | null;
  paths: InterceptionPath[];
}

export interface InterceptionDomainListResponse {
  data: InterceptionDomain[];
  total: number;
}

export interface InterceptionDomainListParams {
  search?: string;
  enabled?: 'true' | 'false';
  limit?: number;
  offset?: number;
}

export interface InterceptionDomainCreatePayload {
  name: string;
  description?: string | null;
  hostPattern: string;
  hostMatchType?: HostMatchType;
  adapterId: string;
  adapterConfig?: Record<string, unknown> | null;
  enabled?: boolean;
  priority?: number;
  defaultPathAction?: DefaultPathAction;
  onAdapterError?: FailureAction;
  networkZone?: NetworkZone;
  source?: string;
  paths?: InterceptionPathCreatePayload[];
}

export type InterceptionDomainUpdatePayload = Partial<
  Omit<InterceptionDomainCreatePayload, 'paths' | 'adapterId'>
> & { adapterId?: string };

export interface InterceptionPathCreatePayload {
  pathPattern: string[];
  matchType?: PathMatchType;
  action: PathAction;
  priority?: number;
  description?: string | null;
  enabled?: boolean;
}

export type InterceptionPathUpdatePayload = Partial<InterceptionPathCreatePayload>;

// paramsToQuery normalises the typed list params into the stringly-typed
// record the api client accepts. Undefined keys are dropped.
function paramsToQuery(p?: InterceptionDomainListParams): Record<string, string> | undefined {
  if (!p) return undefined;
  const out: Record<string, string> = {};
  if (p.search !== undefined && p.search !== '') out.search = p.search;
  if (p.enabled !== undefined) out.enabled = p.enabled;
  if (p.limit !== undefined) out.limit = String(p.limit);
  if (p.offset !== undefined) out.offset = String(p.offset);
  return out;
}

export const interceptionDomainApi = {
  list: (params?: InterceptionDomainListParams) =>
    api.get<InterceptionDomainListResponse>(
      '/api/admin/interception-domains',
      paramsToQuery(params),
    ),

  get: (id: string) =>
    api.get<InterceptionDomain>(`/api/admin/interception-domains/${id}`),

  create: (payload: InterceptionDomainCreatePayload) =>
    api.post<InterceptionDomain>('/api/admin/interception-domains', payload),

  update: (id: string, payload: InterceptionDomainUpdatePayload) =>
    api.put<InterceptionDomain>(`/api/admin/interception-domains/${id}`, payload),

  delete: (id: string) =>
    api.delete(`/api/admin/interception-domains/${id}`),

  createPath: (domainId: string, payload: InterceptionPathCreatePayload) =>
    api.post<InterceptionPath>(
      `/api/admin/interception-domains/${domainId}/paths`,
      payload,
    ),

  updatePath: (
    domainId: string,
    pathId: string,
    payload: InterceptionPathUpdatePayload,
  ) =>
    api.put<InterceptionPath>(
      `/api/admin/interception-domains/${domainId}/paths/${pathId}`,
      payload,
    ),

  deletePath: (domainId: string, pathId: string) =>
    api.delete(`/api/admin/interception-domains/${domainId}/paths/${pathId}`),

  /** Sorted built-in adapter IDs from Control Plane (shared/traffic/adapters). */
  listTrafficAdaptersCatalog: () =>
    api.get<{ data: string[] }>('/api/admin/traffic-adapters'),
};
