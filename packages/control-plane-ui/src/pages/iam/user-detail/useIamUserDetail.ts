import { useState } from 'react';
import { useParams, useNavigate } from 'react-router-dom';
import { useTranslation } from 'react-i18next';
import { iamApi } from '@/api/services';
import type { CreateAdminApiKeyInput, UpdateAdminUserInput } from '@/api/services';
import { useApi } from '@/hooks/useApi';
import { useMutation } from '@/hooks/useMutation';
import type { AdminUser, AdminApiKey, IamGroup, IamPolicy } from '@/api/types';

export function useIamUserDetail() {
  const { t } = useTranslation();
  const { id } = useParams<{ id: string }>();
  const navigate = useNavigate();

  // Edit state
  const [isEditing, setIsEditing] = useState(false);

  // Edit form state
  const [editDisplayName, setEditDisplayName] = useState('');
  const [editEmail, setEditEmail] = useState('');
  const [editEnabled, setEditEnabled] = useState(true);
  const [editOrgId, setEditOrgId] = useState<string>('');
  const [editCanAccessCP, setEditCanAccessCP] = useState(true);

  // Password reset state (separate from main edit)
  const [isResettingPassword, setIsResettingPassword] = useState(false);
  const [resetPassword, setResetPassword] = useState('');
  const [resetPasswordConfirm, setResetPasswordConfirm] = useState('');

  // Admin key creation state
  const [showCreateKey, setShowCreateKey] = useState(false);
  const [newKeyName, setNewKeyName] = useState('');
  const [createdKeyValue, setCreatedKeyValue] = useState<string | null>(null);
  const [keyCopied, setKeyCopied] = useState(false);
  const [deletingKey, setDeletingKey] = useState<AdminApiKey | null>(null);
  const [deletingUser, setDeletingUser] = useState(false);

  // Rotation flow state:
  //   `rotatingKey`  — the predecessor row the operator is rotating. Drives
  //                    the confirmation dialog; on success the response's
  //                    plaintext key flows into `createdKeyValue` (reusing
  //                    the same one-time-visible banner as create/regenerate).
  //   `retiringKey`  — the predecessor row the operator is sunsetting (after
  //                    a rotation) OR a key being actively revoked. The
  //                    dialog selects the target status (expired / unavailable).
  const [rotatingKey, setRotatingKey] = useState<AdminApiKey | null>(null);
  const [retiringKey, setRetiringKey] = useState<AdminApiKey | null>(null);
  const [retireTargetStatus, setRetireTargetStatus] = useState<'expired' | 'unavailable'>('expired');

  // Roles state
  const [showAddRole, setShowAddRole] = useState(false);
  const [selectedGroupId, setSelectedGroupId] = useState('');

  // Confirm dialog for removing role
  const [removingRole, setRemovingRole] = useState<{ groupId: string; groupName: string } | null>(null);

  // Direct policies state
  const [showAttachPolicy, setShowAttachPolicy] = useState(false);
  const [selectedPolicyId, setSelectedPolicyId] = useState('');
  const [detachingPolicy, setDetachingPolicy] = useState<{ attachmentId: string; policyName: string } | null>(null);

  // Data fetching — single user by ID
  const { data: user, loading, error, refetch } = useApi<AdminUser>(
    () => iamApi.getUser(id!),
    ['admin', 'iam', 'users', 'detail', id],
  );

  const { data: keysData } = useApi<{ data: AdminApiKey[] }>(
    () => iamApi.listApiKeys({ ownerUserId: id! }),
    ['admin', 'iam', 'api-keys', 'by-owner', id],
  );

  // Only fetched when "Attach" forms are shown (lazy)
  const { data: allGroupsData } = useApi<{ data: IamGroup[] }>(
    () => showAddRole ? iamApi.listGroups() : Promise.resolve({ data: [] }),
    ['admin', 'iam', 'groups', 'list', 'user-detail', showAddRole ? 'active' : 'idle'],
  );
  const { data: allPoliciesData } = useApi<{ data: IamPolicy[] }>(
    () => showAttachPolicy ? iamApi.listPolicies({ limit: '500' }) : Promise.resolve({ data: [], total: 0 }),
    ['admin', 'iam', 'policies', 'list', 'user-detail', showAttachPolicy ? 'active' : 'idle'],
  );

  // Mutations
  const { mutate: saveUser, loading: saveLoading } = useMutation(
    (data: UpdateAdminUserInput) => iamApi.updateUser(id!, data),
    {
      invalidateQueries: [['api', 'admin', 'iam']],
      onSuccess: () => { setIsEditing(false); },
      successMessage: t('pages:iam.userUpdated'),
    },
  );

  const { mutate: resetPasswordMutate, loading: resetPasswordLoading } = useMutation(
    (password: string) => iamApi.updateUser(id!, { password }),
    {
      invalidateQueries: [],
      onSuccess: () => {
        setIsResettingPassword(false);
        setResetPassword('');
        setResetPasswordConfirm('');
      },
      successMessage: t('pages:iam.passwordReset'),
    },
  );

  const { mutate: deleteUser, loading: deleteUserLoading } = useMutation(
    (_unused: void) => iamApi.deleteUser(id!),
    {
      invalidateQueries: [['api', 'admin', 'iam']],
      onSuccess: () => navigate('/iam/users'),
      successMessage: t('pages:iam.userDeleted'),
    },
  );

  const { mutate: createApiKey, loading: createKeyLoading } = useMutation(
    (data: CreateAdminApiKeyInput) => iamApi.createApiKey(data),
    {
      invalidateQueries: [['api', 'admin', 'iam']],
      onSuccess: (result) => {
        setCreatedKeyValue(result.key);
        setNewKeyName('');
        setShowCreateKey(false);
      },
      successMessage: t('pages:iam.apiKeyCreatedMsg'),
    },
  );

  const { mutate: deleteApiKey, loading: deleteKeyLoading } = useMutation(
    (keyId: string) => iamApi.deleteApiKey(keyId),
    {
      invalidateQueries: [['api', 'admin', 'iam']],
      onSuccess: () => { setDeletingKey(null); },
      successMessage: t('pages:iam.apiKeyDeletedMsg'),
    },
  );

  const { mutate: rotateApiKey, loading: rotateKeyLoading } = useMutation(
    (keyId: string) => iamApi.rotateApiKey(keyId),
    {
      invalidateQueries: [['api', 'admin', 'iam']],
      onSuccess: (result) => {
        // Successor's plaintext value flows into the same one-time-visible
        // banner used by create / regenerate.
        setCreatedKeyValue(result.key);
        setRotatingKey(null);
      },
      successMessage: t('pages:iam.apiKeyRotatedMsg'),
    },
  );

  const { mutate: retireApiKey, loading: retireKeyLoading } = useMutation(
    ({ keyId, targetStatus }: { keyId: string; targetStatus: 'expired' | 'unavailable' }) =>
      iamApi.retireApiKey(keyId, targetStatus),
    {
      invalidateQueries: [['api', 'admin', 'iam']],
      onSuccess: () => { setRetiringKey(null); },
      successMessage: t('pages:iam.apiKeyRetiredMsg'),
    },
  );

  const { mutate: addToGroup, loading: addGroupLoading } = useMutation(
    (groupId: string) =>
      iamApi.addGroupMember(groupId, { principalType: 'nexus_user', principalId: id! }),
    {
      invalidateQueries: [['api', 'admin', 'iam']],
      onSuccess: () => {
        setShowAddRole(false);
        setSelectedGroupId('');
      },
      successMessage: t('pages:iam.roleAttached'),
    },
  );

  const { mutate: attachPolicy, loading: attachPolicyLoading } = useMutation(
    (policyId: string) =>
      iamApi.attachPrincipalPolicy('nexus_user', id!, { policyId }),
    {
      invalidateQueries: [['api', 'admin', 'iam']],
      onSuccess: () => {
        setShowAttachPolicy(false);
        setSelectedPolicyId('');
      },
      successMessage: t('pages:iam.policyAttached'),
    },
  );

  const { mutate: detachPolicy, loading: detachPolicyLoading } = useMutation(
    (attachmentId: string) =>
      iamApi.detachPrincipalPolicy('nexus_user', id!, attachmentId),
    {
      invalidateQueries: [['api', 'admin', 'iam']],
      onSuccess: () => { setDetachingPolicy(null); },
      successMessage: t('pages:iam.policyDetached'),
    },
  );

  const { mutate: removeFromGroup, loading: removeGroupLoading } = useMutation(
    async (groupId: string) => {
      const detail = await iamApi.getGroup(groupId) as unknown as { members: Array<{ id: string; principalType: string; principalId: string }> };
      const membership = detail?.members?.find(
        (m: { principalType: string; principalId: string }) =>
          m.principalType === 'nexus_user' && m.principalId === id,
      );
      if (!membership) throw new Error('Membership not found');
      return iamApi.removeGroupMember(groupId, membership.id);
    },
    {
      invalidateQueries: [['api', 'admin', 'iam']],
      onSuccess: () => {
        setRemovingRole(null);
      },
      successMessage: t('pages:iam.roleMembershipRemoved'),
    },
  );

  // Handlers
  const UUID_RE = /^[0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12}$/i;

  const startEditing = () => {
    if (!user) return;
    setEditDisplayName(user.displayName);
    setEditEmail(user.email ?? '');
    setEditEnabled(user.status === 'active');
    const orgId = user.organizationId ?? '';
    setEditOrgId(UUID_RE.test(orgId) ? orgId : '');
    setEditCanAccessCP(user.canAccessControlPlane ?? true);
    setIsEditing(true);
  };

  const handleSave = () => {
    const payload: UpdateAdminUserInput = {
      displayName: editDisplayName || undefined,
      email: editEmail || undefined,
      enabled: editEnabled,
      canAccessControlPlane: editCanAccessCP,
      ...(editOrgId && { organizationId: editOrgId }),
    };
    saveUser(payload);
  };

  const handleResetPassword = () => {
    if (!resetPassword || resetPassword !== resetPasswordConfirm) return;
    resetPasswordMutate(resetPassword);
  };

  const handleCreateKey = () => {
    if (!user || !newKeyName.trim()) return;
    createApiKey({
      name: newKeyName.trim(),
      ownerUserId: user.id,
    });
  };

  // Derived data
  const userKeys = keysData?.data ?? [];
  const policyAttachments = user?.policyAttachments ?? [];
  const groupPolicies = policyAttachments.filter(a => a.source === 'group');
  // Deduplicate groups by groupId
  const seenGroupIds = new Set<string>();
  const currentRoles: Array<{ groupId: string; groupName: string }> = [];
  for (const p of groupPolicies) {
    if (p.groupId && !seenGroupIds.has(p.groupId)) {
      seenGroupIds.add(p.groupId);
      currentRoles.push({ groupId: p.groupId, groupName: p.groupName ?? p.groupId });
    }
  }
  const directPolicies = policyAttachments.filter(a => a.source === 'direct');
  const directPolicyIds = new Set(directPolicies.map(p => p.policyId));
  const allGroups = allGroupsData?.data ?? [];
  const availableGroups = allGroups.filter(g => !seenGroupIds.has(g.id));
  const allPolicies = allPoliciesData?.data ?? [];
  const availablePolicies = allPolicies.filter(p => !directPolicyIds.has(p.id));
  return {
    // Core
    t,
    user,
    loading,
    error,
    refetch,

    // Edit state
    isEditing,
    setIsEditing,
    editDisplayName,
    setEditDisplayName,
    editEmail,
    setEditEmail,
    editEnabled,
    setEditEnabled,
    editOrgId,
    setEditOrgId,
    editCanAccessCP,
    setEditCanAccessCP,
    saveLoading,
    startEditing,
    handleSave,

    // Password reset
    isResettingPassword,
    setIsResettingPassword,
    resetPassword,
    setResetPassword,
    resetPasswordConfirm,
    setResetPasswordConfirm,
    resetPasswordLoading,
    handleResetPassword,

    // Delete user
    deletingUser,
    setDeletingUser,
    deleteUser,
    deleteUserLoading,

    // API keys
    userKeys,
    showCreateKey,
    setShowCreateKey,
    newKeyName,
    setNewKeyName,
    createdKeyValue,
    setCreatedKeyValue,
    keyCopied,
    setKeyCopied,
    deletingKey,
    setDeletingKey,
    createKeyLoading,
    handleCreateKey,
    deleteApiKey,
    deleteKeyLoading,

    // Rotation / retirement
    rotatingKey,
    setRotatingKey,
    rotateApiKey,
    rotateKeyLoading,
    retiringKey,
    setRetiringKey,
    retireTargetStatus,
    setRetireTargetStatus,
    retireApiKey,
    retireKeyLoading,

    // Direct policies
    directPolicies,
    showAttachPolicy,
    setShowAttachPolicy,
    selectedPolicyId,
    setSelectedPolicyId,
    detachingPolicy,
    setDetachingPolicy,
    availablePolicies,
    attachPolicy,
    attachPolicyLoading,
    detachPolicy,
    detachPolicyLoading,

    // Roles
    showAddRole,
    setShowAddRole,
    selectedGroupId,
    setSelectedGroupId,
    removingRole,
    setRemovingRole,
    currentRoles,
    allGroups,
    availableGroups,
    addToGroup,
    addGroupLoading,
    removeFromGroup,
    removeGroupLoading,
  };
}

export type UseIamUserDetailReturn = ReturnType<typeof useIamUserDetail>;
