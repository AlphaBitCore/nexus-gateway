import { api } from '../../client';
import type { Organization } from '../../types';

export interface CreateOrganizationInput {
  name: string;
  code: string;
  parentId?: string;
  description?: string;
  contactName?: string;
  contactEmail?: string;
  contactPhone?: string;
  enabled?: boolean;
  timezone?: string;
}

export type UpdateOrganizationInput = Partial<CreateOrganizationInput>;

export const organizationApi = {
  list: (params?: Record<string, string>) =>
    api.get<{ data: Organization[] }>('/api/admin/organizations', params),

  tree: (params?: Record<string, string>) =>
    api.get<{ data: Organization[] }>('/api/admin/organizations/tree', params),

  get: (id: string) =>
    api.get<Organization>(`/api/admin/organizations/${id}`),

  create: (data: CreateOrganizationInput) =>
    api.post<Organization>('/api/admin/organizations', data),

  update: (id: string, data: UpdateOrganizationInput) =>
    api.put<Organization>(`/api/admin/organizations/${id}`, data),

  delete: (id: string) =>
    api.delete(`/api/admin/organizations/${id}`),

};
