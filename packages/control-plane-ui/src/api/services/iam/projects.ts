import { api } from '../../client';
import type { Project } from '../../types';

export interface CreateProjectInput {
  name: string;
  code: string;
  organizationId?: string;
  description?: string;
  contactName?: string;
  contactEmail?: string;
}

export type UpdateProjectInput = Partial<CreateProjectInput> & { status?: string };

export const projectApi = {
  list: (params?: Record<string, string>) =>
    api.get<{ data: Project[]; total: number }>('/api/admin/projects', params),

  get: (id: string) =>
    api.get<Project>(`/api/admin/projects/${id}`),

  create: (data: CreateProjectInput) =>
    api.post<Project>('/api/admin/projects', data),

  update: (id: string, data: UpdateProjectInput) =>
    api.put<Project>(`/api/admin/projects/${id}`, data),

  delete: (id: string) =>
    api.delete(`/api/admin/projects/${id}`),
};
