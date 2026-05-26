/**
 * IAM API service — users, groups, policies, simulator.
 */
import { api } from '../../client';
import type {
  AdminApiKey,
  AdminApiKeyStatus,
  AdminUser,
  IamGroup,
  IamGroupMembership,
  IamPolicy,
  IamPolicyAttachment,
  IamPolicyDocument,
  IamSimulationResponse,
  IdentityProvider,
  IdentityProviderWriteRequest,
  IdentityProviderProbeResult,
  ScimToken,
  IdpGroupMapping,
  WhoAmI,
} from '../../types';

export interface CreateAdminUserInput {
  username: string;
  email?: string | null;
  password: string;
  organizationId?: string | null;
  canAccessControlPlane?: boolean;
}

export interface UpdateAdminUserInput {
  displayName?: string;
  email?: string | null;
  enabled?: boolean;
  password?: string;
  organizationId?: string;
  canAccessControlPlane?: boolean;
}

export interface CreateAdminApiKeyInput {
  name: string;
  expiresAt?: string | null;
  ownerUserId?: string;
}

export interface PatchAdminApiKeyInput {
  name?: string;
  expiresAt?: string | null | '';
  enabled?: boolean;
}

export interface PatchMeInput {
  email?: string | null;
  username?: string;
  currentPassword?: string;
  newPassword?: string;
}

export interface CreateIamPolicyInput {
  name: string;
  description?: string | null;
  document: IamPolicyDocument;
}

export interface UpdateIamPolicyInput {
  name?: string;
  description?: string | null;
  document?: IamPolicyDocument;
  enabled?: boolean;
}

export interface IamGroupWriteInput {
  name: string;
  description?: string | null;
}

export interface IamGroupUpdateInput {
  name?: string;
  description?: string | null;
}

export interface IamAddGroupMemberInput {
  principalType: string;
  principalId: string;
}

export interface IamAttachPolicyInput {
  policyId: string;
  /**
   * Break-glass attach window. RFC3339 timestamp; nil = permanent.
   * Engine.loadPolicies SQL drops the attachment past this deadline;
   * cache may serve a stale grant for up to L2 TTL (60s default) —
   * acceptable for break-glass use cases measured in hours.
   */
  expiresAt?: string;
}

export interface IamSimulateRequestBody {
  principal: { type: string; id: string };
  action: string;
  resource: string;
  context?: Record<string, unknown>;
}

// Mirrors packages/shared/iam.Catalog → /api/admin/iam/action-catalog
// response shape. The IAM Simulator + IAM Policy Editor + SIEM filter UI
// all consume this rather than hard-coding any action / resource list.
export interface ActionCatalogAction {
  /** Canonical kebab-case verb (e.g. "create", "force-resync"). */
  verb: string;
  /** Full IAM action string usable in policy documents — `admin:<resource>.<verb>`. */
  name: string;
  /** SIEM eventType — equals `name` with the `admin:` prefix stripped. */
  siem: string;
}

export interface ActionCatalogEntry {
  /** Canonical kebab-case resource name (e.g. "virtual-key", "audit-log"). */
  type: string;
  /** Owning platform component (`gateway` | `admin` | `compliance`). */
  service: string;
  /** Canonical NRN wildcard (`scope=*`, `id=*`). */
  nrn: string;
  /** Every verb declared for this resource. Empty for internal resources. */
  actions: ActionCatalogAction[];
}

export interface ActionCatalogResponse {
  resources: ActionCatalogEntry[];
}

export const iamApi = {
  // Users
  listUsers: (params?: Record<string, string>) =>
    api.get<{ data: AdminUser[]; total: number }>('/api/admin/users', params),

  getUser: (id: string) =>
    api.get<AdminUser>(`/api/admin/users/${id}`),

  createUser: (data: CreateAdminUserInput) =>
    api.post<AdminUser>('/api/admin/users', data),

  updateUser: (id: string, data: UpdateAdminUserInput) =>
    api.put<AdminUser>(`/api/admin/users/${id}`, data),

  deleteUser: (id: string) =>
    api.delete(`/api/admin/users/${id}`),

  // API Keys
  listApiKeys: (params?: Record<string, string>) =>
    api.get<{ data: AdminApiKey[] }>('/api/admin/api-keys', params),

  createApiKey: (data: CreateAdminApiKeyInput) =>
    api.post<AdminApiKey & { key: string }>('/api/admin/api-keys', data),

  deleteApiKey: (id: string) =>
    api.delete(`/api/admin/api-keys/${id}`),

  patchApiKey: (id: string, data: PatchAdminApiKeyInput) =>
    api.patch<{ data: AdminApiKey }>(`/api/admin/api-keys/${id}`, data),

  regenerateApiKey: (id: string) =>
    api.post<{ id: string; key: string; keyPrefix: string }>(`/api/admin/api-keys/${id}/regenerate`, {}),

  /**
   * Rotate an admin API key. Mints a new key that inherits the predecessor's
   * name + owner, flips the predecessor to status='rotating', and returns the
   * new plaintext value (visible exactly once, matching the create/regenerate
   * contract). Both keys remain valid until the operator calls retireApiKey
   * on the predecessor.
   */
  rotateApiKey: (id: string, body?: { expiresAt?: string }) =>
    api.post<{
      id: string;
      key: string;
      keyPrefix: string;
      expiresAt?: string;
      predecessor: { id: string; status: AdminApiKeyStatus; rotatedAt?: string };
      message: string;
    }>(`/api/admin/api-keys/${id}/rotate`, body ?? {}),

  /**
   * Retire an admin API key. Pass targetStatus='expired' for a natural sunset
   * (typically closes a rotation window) or 'unavailable' for an active
   * revocation (compromise / withdrawal). Either way, the auth middleware
   * stops accepting the key immediately after this call returns 200.
   */
  retireApiKey: (id: string, targetStatus: 'expired' | 'unavailable' = 'expired') =>
    api.put<{ data: AdminApiKey }>(`/api/admin/api-keys/${id}/retire`, { targetStatus }),

  patchMe: (data: PatchMeInput) =>
    api.patch<WhoAmI>('/api/admin/me', data),

  // Policies
  listPolicies: (params?: Record<string, string>) =>
    api.get<{ data: IamPolicy[]; total: number }>('/api/admin/iam/policies', params),

  getPolicy: (id: string) =>
    api.get<IamPolicy>(`/api/admin/iam/policies/${id}`),

  createPolicy: (data: CreateIamPolicyInput) =>
    api.post<IamPolicy>('/api/admin/iam/policies', data),

  updatePolicy: (id: string, data: UpdateIamPolicyInput) =>
    api.put<IamPolicy>(`/api/admin/iam/policies/${id}`, data),

  deletePolicy: (id: string) =>
    api.delete(`/api/admin/iam/policies/${id}`),

  getPolicyAttachments: (id: string) =>
    api.get<{ data: IamPolicyAttachment[] }>(`/api/admin/iam/policies/${id}/attachments`),

  // Groups
  listGroups: (params?: Record<string, string>) =>
    api.get<{ data: IamGroup[]; total: number }>('/api/admin/iam/groups', params),

  getGroup: (id: string) =>
    api.get<IamGroup>(`/api/admin/iam/groups/${id}`),

  createGroup: (data: IamGroupWriteInput) =>
    api.post<IamGroup>('/api/admin/iam/groups', data),

  updateGroup: (id: string, data: IamGroupUpdateInput) =>
    api.put<IamGroup>(`/api/admin/iam/groups/${id}`, data),

  deleteGroup: (id: string) =>
    api.delete(`/api/admin/iam/groups/${id}`),

  listGroupMembers: (groupId: string, params?: Record<string, string>) =>
    api.get<{ data: IamGroupMembership[]; total: number }>(`/api/admin/iam/groups/${groupId}/members`, params),

  addGroupMember: (groupId: string, data: IamAddGroupMemberInput) =>
    api.post<IamGroupMembership>(`/api/admin/iam/groups/${groupId}/members`, data),

  removeGroupMember: (groupId: string, membershipId: string) =>
    api.delete(`/api/admin/iam/groups/${groupId}/members/${membershipId}`),

  addGroupPolicy: (groupId: string, data: IamAttachPolicyInput) =>
    api.post<Record<string, unknown>>(`/api/admin/iam/groups/${groupId}/policies`, data),

  removeGroupPolicy: (groupId: string, attachmentId: string) =>
    api.delete(`/api/admin/iam/groups/${groupId}/policies/${attachmentId}`),

  // Principals
  getPrincipalPolicies: (type: string, id: string, params?: Record<string, string>) =>
    api.get<{ data: IamPolicyAttachment[] }>(`/api/admin/iam/principals/${type}/${id}/policies`, params),

  attachPrincipalPolicy: (type: string, id: string, data: IamAttachPolicyInput) =>
    api.post<Record<string, unknown>>(`/api/admin/iam/principals/${type}/${id}/policies`, data),

  detachPrincipalPolicy: (type: string, id: string, attachmentId: string) =>
    api.delete(`/api/admin/iam/principals/${type}/${id}/policies/${attachmentId}`),

  // Simulator
  simulate: (data: IamSimulateRequestBody) =>
    api.post<IamSimulationResponse>('/api/admin/iam/simulate', data),

  // Canonical resource × verb catalog. Source of truth for the IAM
  // Simulator dropdowns, IAM Policy Editor quick-add groups, and the
  // SIEM eventType picker.
  getActionCatalog: () =>
    api.get<ActionCatalogResponse>('/api/admin/iam/action-catalog'),

  // External Identity Provider endpoints. The platform is the SP;
  // these manage external IdPs (Okta, Azure AD, Google, …) we federate
  // with. Local fallback rows are included; UI filters by `type !== 'local'`.
  listIdentityProviders: () =>
    api.get<{ data: IdentityProvider[]; total: number }>('/api/admin/identity-providers'),
  getIdentityProvider: (id: string) =>
    api.get<IdentityProvider>(`/api/admin/identity-providers/${id}`),
  createIdentityProvider: (body: IdentityProviderWriteRequest) =>
    api.post<IdentityProvider>('/api/admin/identity-providers', body),
  updateIdentityProvider: (id: string, body: IdentityProviderWriteRequest) =>
    api.put<IdentityProvider>(`/api/admin/identity-providers/${id}`, body),
  deleteIdentityProvider: (id: string, opts?: { force?: boolean }) =>
    api.delete(`/api/admin/identity-providers/${id}${opts?.force ? '?force=true' : ''}`),
  testCandidateIdentityProvider: (body: IdentityProviderWriteRequest) =>
    api.post<IdentityProviderProbeResult>('/api/admin/identity-providers/test', body),
  testSavedIdentityProvider: (id: string, body?: { token?: string }) =>
    api.post<IdentityProviderProbeResult>(`/api/admin/identity-providers/${id}/test`, body ?? {}),

  // SCIM tokens
  listScimTokens: (idpId: string) =>
    api.get<{ data: ScimToken[]; total: number }>(`/api/admin/identity-provider/${idpId}/scim-tokens`),
  createScimToken: (idpId: string, name: string) =>
    api.post<ScimToken & { token: string }>(`/api/admin/identity-provider/${idpId}/scim-tokens`, { name }),
  revokeScimToken: (idpId: string, tokenId: string) =>
    api.delete(`/api/admin/identity-provider/${idpId}/scim-tokens/${tokenId}`),

  // IdP group → IamGroup mappings
  listIdpGroupMappings: (idpId: string) =>
    api.get<{ data: IdpGroupMapping[]; total: number }>(`/api/admin/identity-provider/${idpId}/group-mappings`),
  createIdpGroupMapping: (idpId: string, data: { externalGroupId: string; externalGroupName?: string; iamGroupId: string }) =>
    api.post<IdpGroupMapping>(`/api/admin/identity-provider/${idpId}/group-mappings`, data),
  deleteIdpGroupMapping: (idpId: string, mappingId: string) =>
    api.delete(`/api/admin/identity-provider/${idpId}/group-mappings/${mappingId}`),
};
