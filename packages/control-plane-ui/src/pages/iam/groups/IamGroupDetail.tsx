import { useState } from 'react';
import { useParams, useNavigate } from 'react-router-dom';
import { useTranslation } from 'react-i18next';
import { iamApi } from '@/api/services';
import type { IamAddGroupMemberInput, IamAttachPolicyInput } from '@/api/services';
import { useApi } from '../../../hooks/useApi';
import { useMutation } from '../../../hooks/useMutation';
import {
  PageHeader, DataTable, Skeleton, ErrorBanner, AlertDialog,
  Breadcrumb, Button, Stack, Card, Badge,
  Tabs, TabsList, TabsTrigger, TabsContent, ListPagination, Input,
} from '@/components/ui';
import type { IamGroupDetail as IamGroupDetailType, IamPolicy } from '../../../api/types';
import { formatDate, formatDateTime } from '@/lib/format';
import styles from '../_shared/Iam.module.css';

const MEMBERS_PAGE_SIZE = 20;

export function IamGroupDetail() {
  const { t } = useTranslation();
  const { id } = useParams<{ id: string }>();
  const navigate = useNavigate();

  const { data: group, loading, error, refetch } = useApi<IamGroupDetailType>(
    () => iamApi.getGroup(id!) as Promise<unknown> as Promise<IamGroupDetailType>,
    ['admin', 'iam', 'groups', 'detail', id],
  );
  const { data: policiesData } = useApi<{ data: IamPolicy[] }>(
    () => iamApi.listPolicies(),
    ['admin', 'iam', 'policies', 'list'],
  );

  const [memberOffset, setMemberOffset] = useState(0);
  const [memberLimit, setMemberLimit] = useState(MEMBERS_PAGE_SIZE);

  const { data: membersData, refetch: refetchMembers } = useApi<{ data: Array<{ id: string; principalType: string; principalId: string; createdAt: string }>; total: number }>(
    () => iamApi.listGroupMembers(id!, { limit: String(memberLimit), offset: String(memberOffset) }),
    ['admin', 'iam', 'groups', 'members', id, memberOffset],
  );

  const [removingMember, setRemovingMember] = useState<{ id: string; principalId: string } | null>(null);
  const [removingPolicy, setRemovingPolicy] = useState<{ id: string; name: string } | null>(null);
  const [showAddMember, setShowAddMember] = useState(false);
  const [memberPrincipalType, setMemberPrincipalType] = useState('api_key');
  const [memberPrincipalId, setMemberPrincipalId] = useState('');
  const [showAttachPolicy, setShowAttachPolicy] = useState(false);
  const [selectedPolicyId, setSelectedPolicyId] = useState('');

  const { mutate: addMember, loading: addMemberLoading } = useMutation(
    (data: IamAddGroupMemberInput) => iamApi.addGroupMember(id!, data),
    {
      invalidateQueries: [['api', 'admin', 'iam']],
      onSuccess: () => { setShowAddMember(false); setMemberPrincipalType('api_key'); setMemberPrincipalId(''); refetchMembers(); },
      successMessage: t('pages:iam.memberAdded'),
    },
  );

  const { mutate: removeMember } = useMutation(
    (membershipId: string) => iamApi.removeGroupMember(id!, membershipId),
    {
      invalidateQueries: [['api', 'admin', 'iam']],
      onSuccess: () => { setRemovingMember(null); refetchMembers(); },
      successMessage: t('pages:iam.memberRemoved'),
    },
  );

  const { mutate: attachPolicy, loading: attachPolicyLoading } = useMutation(
    (data: IamAttachPolicyInput) => iamApi.addGroupPolicy(id!, data),
    {
      invalidateQueries: [['api', 'admin', 'iam']],
      onSuccess: () => { setShowAttachPolicy(false); setSelectedPolicyId(''); },
      successMessage: t('pages:iam.policyAttached'),
    },
  );

  const { mutate: detachPolicy } = useMutation(
    (attachmentId: string) => iamApi.removeGroupPolicy(id!, attachmentId),
    {
      invalidateQueries: [['api', 'admin', 'iam']],
      onSuccess: () => { setRemovingPolicy(null); },
      successMessage: t('pages:iam.policyDetached'),
    },
  );

  if (loading) return <Skeleton.DetailPageSkeleton />;
  if (error) return <ErrorBanner message={error.message} onRetry={refetch} />;
  if (!group) return null;

  const policyAttachments = group.policyAttachments ?? [];
  const attachedPolicyIds = new Set(policyAttachments.map((a) => a.policyId));
  const availablePolicies = (policiesData?.data ?? []).filter((p) => !attachedPolicyIds.has(p.id));

  const pagedMembers = membersData?.data ?? [];
  const totalMembers = membersData?.total ?? 0;

  return (
    <Stack gap="lg">
      <Breadcrumb items={[
        { label: t('pages:iam.groups'), to: '/iam/groups' },
        { label: group.name },
      ]} />

      <PageHeader title={group.name} subtitle={group.description ?? undefined} />

      <Tabs defaultValue="info">
        <TabsList>
          <TabsTrigger value="info">{t('pages:iam.information')}</TabsTrigger>
          <TabsTrigger value="members">{t('pages:iam.members')} ({totalMembers})</TabsTrigger>
          <TabsTrigger value="policies">{t('pages:iam.attachedPolicies')} ({policyAttachments.length})</TabsTrigger>
        </TabsList>

        {/* ── Info Tab ─────────────────────────────────────────── */}
        <TabsContent value="info">
          <Card>
            <div className={styles.kvGrid}>
              <div>
                <div className={styles.kvLabel}>{t('pages:iam.name')}</div>
                <div className={styles.kvValue}>{group.name}</div>
              </div>
              <div>
                <div className={styles.kvLabel}>{t('pages:iam.description')}</div>
                <div className={styles.kvValue}>{group.description || '\u2014'}</div>
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
                <div className={styles.kvValue}>{formatDateTime(group.createdAt)}</div>
              </div>
              <div>
                <div className={styles.kvLabel}>{t('pages:iam.updated')}</div>
                <div className={styles.kvValue}>{formatDateTime(group.updatedAt)}</div>
              </div>
            </div>
          </Card>
        </TabsContent>

        {/* ── Members Tab ──────────────────────────────────────── */}
        <TabsContent value="members">
          <Card>
            <Stack gap="md">
              <Button onClick={() => setShowAddMember(!showAddMember)}>{t('pages:iam.addMember')}</Button>

              {showAddMember && (
                <div className={styles.inlineForm}>
                  <div>
                    <label className={styles.formLabel}>{t('pages:iam.principalType')}</label>
                    <select
                      value={memberPrincipalType}
                      onChange={(e) => setMemberPrincipalType(e.target.value)}
                      className={styles.filterSelect}
                    >
                      <option value="api_key">{t('pages:iam.adminApiKey')}</option>
                      <option value="virtual_key">{t('pages:iam.virtualKeyType')}</option>
                    </select>
                  </div>
                  <div className={styles.flexOne}>
                    <label className={styles.formLabel}>{t('pages:iam.principalId')}</label>
                    <Input
                      className={styles.filterSelect}
                      style={{ width: '100%' }}
                      value={memberPrincipalId}
                      onChange={(e) => setMemberPrincipalId(e.target.value)}
                      placeholder={t('pages:iam.placeholderPrincipalId')}
                    />
                  </div>
                  <Button
                    onClick={() => addMember({ principalType: memberPrincipalType, principalId: memberPrincipalId })}
                    disabled={addMemberLoading || !memberPrincipalId}
                  >
                    {addMemberLoading ? t('pages:iam.adding') : t('pages:iam.add')}
                  </Button>
                </div>
              )}

              <DataTable
                hideSearch
                columns={[
                  { key: 'principalType', label: t('pages:iam.principalType') },
                  { key: 'principalId', label: t('pages:iam.principalId') },
                  {
                    key: 'createdAt', label: t('pages:iam.added'),
                    render: (r) => <span className={styles.memberDate}>{formatDate(r.createdAt)}</span>,
                  },
                  {
                    key: 'actions', label: '',
                    render: (r) => (
                      <Button variant="danger" size="sm" onClick={() => setRemovingMember({ id: r.id, principalId: r.principalId })}>
                        {t('pages:iam.remove')}
                      </Button>
                    ),
                  },
                ]}
                data={pagedMembers}
                emptyMessage={t('pages:iam.noMembersInGroup')}
              />

              <ListPagination
                offset={memberOffset}
                limit={memberLimit}
                total={totalMembers}
                onOffsetChange={setMemberOffset}
                onLimitChange={setMemberLimit}
              />
            </Stack>
          </Card>
        </TabsContent>

        {/* ── Policies Tab ─────────────────────────────────────── */}
        <TabsContent value="policies">
          <Card>
            <Stack gap="md">
              <Button onClick={() => setShowAttachPolicy(!showAttachPolicy)}>{t('pages:iam.attachPolicy')}</Button>

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
                    key: 'name', label: t('pages:iam.policyName'),
                    render: (r) => (
                      <span className={styles.roleNameLink} onClick={() => navigate(`/iam/policies/${r.policyId}`)}>
                        {r.policy?.name ?? r.policyId}
                      </span>
                    ),
                  },
                  {
                    key: 'type', label: t('pages:iam.type'),
                    render: (r) => r.policy?.type ? (
                      <Badge variant={r.policy.type === 'managed' ? 'info' : 'default'}>{r.policy.type}</Badge>
                    ) : '\u2014',
                  },
                  {
                    key: 'actions', label: '',
                    render: (r) => (
                      <Button variant="danger" size="sm" onClick={() => setRemovingPolicy({ id: r.id, name: r.policy?.name ?? r.policyId })}>
                        {t('pages:iam.detach')}
                      </Button>
                    ),
                  },
                ]}
                data={policyAttachments}
                emptyMessage={t('pages:iam.noPoliciesAttachedToGroup')}
              />
            </Stack>
          </Card>
        </TabsContent>
      </Tabs>

      {/* Confirm Dialogs */}
      <AlertDialog
        open={!!removingMember}
        onOpenChange={(open) => { if (!open) setRemovingMember(null); }}
        title={t('pages:iam.removeMember')}
        description={t('pages:iam.removeMemberFromGroupConfirm', { name: removingMember?.principalId })}
        confirmLabel={t('pages:iam.remove')}
        onConfirm={() => { if (removingMember) removeMember(removingMember.id); }}
        variant="danger"
      />

      <AlertDialog
        open={!!removingPolicy}
        onOpenChange={(open) => { if (!open) setRemovingPolicy(null); }}
        title={t('pages:iam.detachPolicy')}
        description={t('pages:iam.detachPolicyFromGroupConfirm', { name: removingPolicy?.name })}
        confirmLabel={t('pages:iam.detach')}
        onConfirm={() => { if (removingPolicy) detachPolicy(removingPolicy.id); }}
        variant="danger"
      />
    </Stack>
  );
}
