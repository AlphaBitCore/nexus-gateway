import { useState } from 'react';
import { useParams, useNavigate } from 'react-router-dom';
import { useTranslation } from 'react-i18next';
import { iamApi } from '@/api/services';
import type { IamAddGroupMemberInput, IamAttachPolicyInput, IamGroupUpdateInput } from '@/api/services';
import { useApi } from '../../../hooks/useApi';
import { useMutation } from '../../../hooks/useMutation';
import {
  PageHeader, DataTable, Skeleton, ErrorBanner, AlertDialog,
  Breadcrumb, Button, Stack, Card, Badge,
  Tabs, TabsList, TabsTrigger, TabsContent,
  ListPagination, Input,
} from '@/components/ui';
import type { IamGroupDetail as IamGroupDetailType, IamPolicy } from '../../../api/types';
import { formatDate, formatDateTime } from '@/lib/format';
import styles from '../_shared/Iam.module.css';

const MEMBERS_PAGE_SIZE = 20;

export function IamRoleDetail() {
  const { t } = useTranslation();
  const { id } = useParams<{ id: string }>();
  const navigate = useNavigate();

  const { data: role, loading, error, refetch } = useApi<IamGroupDetailType>(
    () => iamApi.getGroup(id!) as Promise<unknown> as Promise<IamGroupDetailType>,
    ['admin', 'iam', 'roles', 'detail', id],
  );
  const { data: policiesData } = useApi<{ data: IamPolicy[] }>(
    () => iamApi.listPolicies(),
    ['admin', 'iam', 'policies', 'list'],
  );
  const { data: usersData } = useApi<{ data: Array<{ id: string; displayName: string; email?: string; status?: string }> }>(
    () => iamApi.listUsers() as Promise<unknown> as Promise<{ data: Array<{ id: string; displayName: string; email?: string; status?: string }> }>,
    ['admin', 'iam', 'users', 'list'],
  );

  const [memberOffset, setMemberOffset] = useState(0);
  const [memberLimit, setMemberLimit] = useState(MEMBERS_PAGE_SIZE);

  const { data: membersData, refetch: refetchMembers } = useApi<{ data: Array<{ id: string; principalType: string; principalId: string; createdAt: string }>; total: number }>(
    () => iamApi.listGroupMembers(id!, { limit: String(memberLimit), offset: String(memberOffset) }),
    ['admin', 'iam', 'roles', 'members', id, memberOffset, memberLimit],
  );

  const [removingMember, setRemovingMember] = useState<{ id: string; principalId: string } | null>(null);
  const [removingPolicy, setRemovingPolicy] = useState<{ id: string; name: string } | null>(null);
  const [showAddMember, setShowAddMember] = useState(false);
  const [selectedUserId, setSelectedUserId] = useState('');
  const [showAttachPolicy, setShowAttachPolicy] = useState(false);
  const [selectedPolicyId, setSelectedPolicyId] = useState('');

  // Edit state
  const [isEditing, setIsEditing] = useState(false);
  const [editName, setEditName] = useState('');
  const [editDescription, setEditDescription] = useState('');

  const { mutate: addMember, loading: addMemberLoading } = useMutation(
    (data: IamAddGroupMemberInput) => iamApi.addGroupMember(id!, data),
    {
      invalidateQueries: [['api', 'admin', 'iam']],
      onSuccess: () => { setShowAddMember(false); setSelectedUserId(''); refetchMembers(); },
      successMessage: 'Member added',
    },
  );

  const { mutate: removeMember } = useMutation(
    (membershipId: string) => iamApi.removeGroupMember(id!, membershipId),
    {
      invalidateQueries: [['api', 'admin', 'iam']],
      onSuccess: () => { setRemovingMember(null); refetchMembers(); },
      successMessage: 'Member removed',
    },
  );

  const { mutate: attachPolicy, loading: attachPolicyLoading } = useMutation(
    (data: IamAttachPolicyInput) => iamApi.addGroupPolicy(id!, data),
    {
      invalidateQueries: [['api', 'admin', 'iam']],
      onSuccess: () => { setShowAttachPolicy(false); setSelectedPolicyId(''); },
      successMessage: 'Policy attached',
    },
  );

  const { mutate: detachPolicy } = useMutation(
    (attachmentId: string) => iamApi.removeGroupPolicy(id!, attachmentId),
    {
      invalidateQueries: [['api', 'admin', 'iam']],
      onSuccess: () => { setRemovingPolicy(null); },
      successMessage: 'Policy detached',
    },
  );

  const { mutate: updateRole, loading: updateLoading } = useMutation(
    (data: IamGroupUpdateInput) => iamApi.updateGroup(id!, data),
    {
      invalidateQueries: [['api', 'admin', 'iam']],
      onSuccess: () => { setIsEditing(false); },
      successMessage: 'Role updated',
    },
  );

  const startEditing = () => {
    if (!role) return;
    setEditName(role.name);
    setEditDescription(role.description ?? '');
    setIsEditing(true);
  };

  const handleSave = () => {
    updateRole({ name: editName, description: editDescription || null });
  };

  if (loading) return <Skeleton.DetailPageSkeleton />;
  if (error) return <ErrorBanner message={error.message} onRetry={refetch} />;
  if (!role) return null;

  const policyAttachments = role.policyAttachments ?? [];
  const attachedPolicyIds = new Set(policyAttachments.map((a) => a.policyId));
  const availablePolicies = (policiesData?.data ?? []).filter((p) => !attachedPolicyIds.has(p.id));

  // Server-side paginated members
  const pagedMembers = membersData?.data ?? [];
  const totalMembers = membersData?.total ?? 0;

  const allUsers = usersData?.data ?? [];
  const existingMemberIds = new Set(pagedMembers.map((m) => m.principalId));
  const availableUsers = allUsers.filter((u) => u.status === 'active' && !existingMemberIds.has(u.id));

  return (
    <Stack gap="lg">
      <Breadcrumb items={[
        { label: t('pages:iam.roles'), to: '/iam/roles' },
        { label: role.name },
      ]} />

      <PageHeader title={role.name} subtitle={role.description ?? undefined} />

      <Tabs defaultValue="info">
        <TabsList>
          <TabsTrigger value="info">{t('pages:iam.information')}</TabsTrigger>
          <TabsTrigger value="members">{t('pages:iam.members')} ({totalMembers})</TabsTrigger>
          <TabsTrigger value="policies">{t('pages:iam.attachedPolicies')} ({policyAttachments.length})</TabsTrigger>
        </TabsList>

        {/* ── Info Tab ─────────────────────────────────────────── */}
        <TabsContent value="info">
          <Card>
            {isEditing ? (
              <Stack gap="md">
                <div>
                  <label className={styles.formLabel}>{t('pages:iam.name')}</label>
                  <Input
                    className={styles.filterSelect}
                    style={{ width: '100%' }}
                    value={editName}
                    onChange={(e) => setEditName(e.target.value)}
                  />
                </div>
                <div>
                  <label className={styles.formLabel}>{t('pages:iam.description')}</label>
                  <textarea
                    className={styles.filterSelect}
                    style={{ width: '100%', minHeight: 80 }}
                    value={editDescription}
                    onChange={(e) => setEditDescription(e.target.value)}
                  />
                </div>
                <Stack direction="horizontal" gap="sm">
                  <Button onClick={handleSave} disabled={updateLoading || !editName.trim()}>
                    {updateLoading ? t('common:saving') : t('common:save')}
                  </Button>
                  <Button variant="secondary" onClick={() => setIsEditing(false)}>
                    {t('common:cancel')}
                  </Button>
                </Stack>
              </Stack>
            ) : (
              <>
                <div style={{ display: 'flex', justifyContent: 'flex-end', marginBottom: 'var(--g-space-2)' }}>
                  <Button variant="secondary" onClick={startEditing}>{t('common:edit')}</Button>
                </div>
                <div className={styles.kvGrid}>
                  <div>
                    <div className={styles.kvLabel}>{t('pages:iam.name')}</div>
                    <div className={styles.kvValue}>{role.name}</div>
                  </div>
                  <div>
                    <div className={styles.kvLabel}>{t('pages:iam.description')}</div>
                    <div className={styles.kvValue}>{role.description || '\u2014'}</div>
                  </div>
                  <div>
                    <div className={styles.kvLabel}>{t('pages:iam.members')}</div>
                    <div className={styles.kvValue}>{totalMembers}</div>
                  </div>
                  <div>
                    <div className={styles.kvLabel}>{t('pages:iam.attachedPolicies')}</div>
                    <div className={styles.kvValue}>{policyAttachments.length}</div>
                  </div>
                  <div>
                    <div className={styles.kvLabel}>{t('pages:iam.created')}</div>
                    <div className={styles.kvValue}>{formatDateTime(role.createdAt)}</div>
                  </div>
                  <div>
                    <div className={styles.kvLabel}>{t('pages:iam.updated')}</div>
                    <div className={styles.kvValue}>{formatDateTime(role.updatedAt)}</div>
                  </div>
                  {role.createdBy && (
                    <div>
                      <div className={styles.kvLabel}>{t('pages:iam.createdBy')}</div>
                      <div className={styles.kvValue}>{role.createdBy}</div>
                    </div>
                  )}
                </div>
              </>
            )}
          </Card>
        </TabsContent>

        {/* ── Members Tab ──────────────────────────────────────── */}
        <TabsContent value="members">
          <Card>
            <div className={styles.memberBtnRow}>
              <Button onClick={() => setShowAddMember(!showAddMember)}>
                {t('pages:iam.addMember')}
              </Button>
            </div>

            {showAddMember && (
              <div className={styles.inlineForm}>
                <div className={styles.flexOne}>
                  <label className={styles.formLabel}>{t('pages:iam.selectUser')}</label>
                  <select
                    value={selectedUserId}
                    onChange={(e) => setSelectedUserId(e.target.value)}
                    className={`${styles.filterSelect} ${styles.selectFullWidth}`}
                  >
                    <option value="">-- {t('pages:iam.selectUser')} --</option>
                    {availableUsers.map((u) => (
                      <option key={u.id} value={u.id}>
                        {u.displayName}{u.email ? ` (${u.email})` : ''}
                      </option>
                    ))}
                  </select>
                </div>
                <Button
                  onClick={() => addMember({ principalType: 'nexus_user', principalId: selectedUserId })}
                  disabled={addMemberLoading || !selectedUserId}
                >
                  {addMemberLoading ? t('pages:iam.adding') : t('pages:iam.add')}
                </Button>
              </div>
            )}

            <DataTable
              hideSearch
              columns={[
                {
                  key: 'principalId',
                  label: t('pages:iam.user'),
                  render: (r) => {
                    const user = allUsers.find((u) => u.id === r.principalId);
                    return user ? (
                      <span className={styles.memberUsername}>
                        {user.displayName}
                        {user.email ? <span className={styles.memberEmail}> {user.email}</span> : ''}
                      </span>
                    ) : (
                      <span className={`${styles.mono} ${styles.memberIdFallback}`}>{r.principalId}</span>
                    );
                  },
                },
                {
                  key: 'principalType',
                  label: t('pages:iam.type'),
                  render: (r) => r.principalType,
                },
                {
                  key: 'createdAt',
                  label: t('pages:iam.added'),
                  render: (r) => <span className={styles.memberDate}>{formatDate(r.createdAt)}</span>,
                },
                {
                  key: 'actions',
                  label: '',
                  render: (r) => (
                    <Button variant="danger" size="sm" onClick={() => setRemovingMember({ id: r.id, principalId: r.principalId })}>
                      {t('pages:iam.remove')}
                    </Button>
                  ),
                },
              ]}
              data={pagedMembers}
              emptyMessage={t('pages:iam.noMembersInRole')}
            />

            <ListPagination
              offset={memberOffset}
              limit={memberLimit}
              total={totalMembers}
              onOffsetChange={setMemberOffset}
              onLimitChange={setMemberLimit}
            />
          </Card>
        </TabsContent>

        {/* ── Policies Tab ─────────────────────────────────────── */}
        <TabsContent value="policies">
          <Card>
            <div className={styles.memberBtnRow}>
              <Button onClick={() => setShowAttachPolicy(!showAttachPolicy)}>
                {t('pages:iam.attachPolicy')}
              </Button>
            </div>

            {showAttachPolicy && (
              <div className={styles.inlineForm}>
                <div className={styles.flexOne}>
                  <label className={styles.formLabel}>{t('pages:iam.policy')}</label>
                  <select
                    value={selectedPolicyId}
                    onChange={(e) => setSelectedPolicyId(e.target.value)}
                    className={`${styles.filterSelect} ${styles.selectFullWidth}`}
                  >
                    <option value="">{t('pages:iam.selectPolicy')}</option>
                    {availablePolicies.map((p) => (
                      <option key={p.id} value={p.id}>{p.name}</option>
                    ))}
                  </select>
                </div>
                <Button
                  onClick={() => attachPolicy({ policyId: selectedPolicyId })}
                  disabled={attachPolicyLoading || !selectedPolicyId}
                >
                  {attachPolicyLoading ? t('pages:iam.attaching') : t('pages:iam.attach')}
                </Button>
              </div>
            )}

            <DataTable
              hideSearch
              columns={[
                {
                  key: 'name',
                  label: t('pages:iam.policyName'),
                  render: (r) => (
                    <span
                      className={styles.roleNameLink}
                      onClick={() => navigate(`/iam/policies/${r.policyId}`)}
                    >
                      {r.policy?.name ?? r.policyId}
                    </span>
                  ),
                },
                {
                  key: 'type',
                  label: t('pages:iam.type'),
                  render: (r) => r.policy?.type ? (
                    <Badge variant={r.policy.type === 'managed' ? 'info' : 'default'}>
                      {r.policy.type}
                    </Badge>
                  ) : '\u2014',
                },
                {
                  key: 'createdAt',
                  label: t('pages:iam.added'),
                  render: (r) => <span className={styles.memberDate}>{formatDate(r.createdAt)}</span>,
                },
                {
                  key: 'actions',
                  label: '',
                  render: (r) => (
                    <Button variant="danger" size="sm" onClick={() => setRemovingPolicy({ id: r.id, name: r.policy?.name ?? r.policyId })}>
                      {t('pages:iam.detach')}
                    </Button>
                  ),
                },
              ]}
              data={policyAttachments}
              emptyMessage={t('pages:iam.noPoliciesAttachedToRole')}
            />
          </Card>
        </TabsContent>
      </Tabs>

      {/* Confirm Dialogs */}
      <AlertDialog
        open={!!removingMember}
        onOpenChange={(open) => { if (!open) setRemovingMember(null); }}
        title={t('pages:iam.removeMember')}
        description={t('pages:iam.removeMemberConfirm', { name: removingMember?.principalId })}
        confirmLabel={t('pages:iam.remove')}
        onConfirm={() => { if (removingMember) removeMember(removingMember.id); }}
        variant="danger"
      />

      <AlertDialog
        open={!!removingPolicy}
        onOpenChange={(open) => { if (!open) setRemovingPolicy(null); }}
        title={t('pages:iam.detachPolicy')}
        description={t('pages:iam.detachPolicyConfirm', { name: removingPolicy?.name })}
        confirmLabel={t('pages:iam.detach')}
        onConfirm={() => { if (removingPolicy) detachPolicy(removingPolicy.id); }}
        variant="danger"
      />
    </Stack>
  );
}
