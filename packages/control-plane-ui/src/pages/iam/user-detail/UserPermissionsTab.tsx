import { useTranslation } from 'react-i18next';
import { Link } from 'react-router-dom';
import {
  DataTable, Button, Stack, Card, AlertDialog,
} from '@/components/ui';
import type { IamPolicyAttachment } from '@/api/types';
import type { UseIamUserDetailReturn } from './useIamUserDetail';
import styles from '../_shared/Iam.module.css';

type Props = Pick<
  UseIamUserDetailReturn,
  // Roles
  | 'showAddRole'
  | 'setShowAddRole'
  | 'selectedGroupId'
  | 'setSelectedGroupId'
  | 'removingRole'
  | 'setRemovingRole'
  | 'currentRoles'
  | 'availableGroups'
  | 'addToGroup'
  | 'addGroupLoading'
  | 'removeFromGroup'
  | 'removeGroupLoading'
  // Direct policies
  | 'directPolicies'
  | 'showAttachPolicy'
  | 'setShowAttachPolicy'
  | 'selectedPolicyId'
  | 'setSelectedPolicyId'
  | 'detachingPolicy'
  | 'setDetachingPolicy'
  | 'availablePolicies'
  | 'attachPolicy'
  | 'attachPolicyLoading'
  | 'detachPolicy'
  | 'detachPolicyLoading'
> & {
  allPolicies: IamPolicyAttachment[];
};

type RoleRow = { groupId: string; groupName: string };

export function UserPermissionsTab({
  // All policies (for effective view)
  allPolicies,
  // Roles
  showAddRole,
  setShowAddRole,
  selectedGroupId,
  setSelectedGroupId,
  removingRole,
  setRemovingRole,
  currentRoles,
  availableGroups,
  addToGroup,
  addGroupLoading,
  removeFromGroup,
  removeGroupLoading,
  // Direct policies
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
}: Props) {
  const { t } = useTranslation();

  return (
    <Stack gap="lg">
      {/* ── Section 1: Effective Policies ── */}
      <Card>
        <Stack direction="horizontal" justify="between" align="center" className={styles.sectionTitleRow}>
          <h3 className={styles.sectionTitleText}>{t('pages:iam.effectivePolicies')}</h3>
          <Button onClick={() => setShowAttachPolicy(!showAttachPolicy)}>
            {t('pages:iam.attachPolicy')}
          </Button>
        </Stack>
        <p className={styles.rolesSectionDesc}>
          {t('pages:iam.effectivePoliciesDesc')}
        </p>

        {showAttachPolicy && (
          <div className={styles.inlineForm}>
            <div className={styles.flexOne}>
              <label className={styles.formLabel}>{t('pages:iam.policy')}</label>
              <select value={selectedPolicyId} onChange={(e) => setSelectedPolicyId(e.target.value)} className={`${styles.filterSelect} ${styles.selectFull}`} aria-label={t('pages:iam.selectPolicyToAttach')}>
                <option value="">{t('pages:iam.selectPolicy')}</option>
                {availablePolicies.map((p) => (
                  <option key={p.id} value={p.id}>{p.name}</option>
                ))}
              </select>
            </div>
            <Button
              onClick={() => { if (selectedPolicyId) attachPolicy(selectedPolicyId); }}
              disabled={attachPolicyLoading || !selectedPolicyId}
            >
              {attachPolicyLoading ? t('pages:iam.attaching') : t('pages:iam.attach')}
            </Button>
            <Button variant="secondary" onClick={() => { setShowAttachPolicy(false); setSelectedPolicyId(''); }}>
              {t('common:cancel')}
            </Button>
          </div>
        )}

        <DataTable hideSearch
          columns={[
            {
              key: 'policyName',
              label: t('pages:iam.policy'),
              render: (r: IamPolicyAttachment) => (
                <Link to={`/iam/policies/${r.policyId}`} className={styles.linkStyle}>
                  {r.policyName ?? r.policyId}
                </Link>
              ),
            },
            {
              key: 'source',
              label: t('pages:iam.source'),
              render: (r: IamPolicyAttachment) =>
                r.source === 'group'
                  ? <span className={styles.roleBadge}>{t('pages:iam.viaRole', { name: r.groupName })}</span>
                  : <span className={styles.sourceDirectBadge}>{t('pages:iam.sourceDirect')}</span>,
            },
            {
              key: 'actions',
              label: '',
              render: (r: IamPolicyAttachment) =>
                r.source === 'direct' ? (
                  <Button
                    variant="danger"
                    size="sm"
                    onClick={(e) => {
                      e.stopPropagation();
                      setDetachingPolicy({ attachmentId: r.id, policyName: r.policyName ?? r.policyId });
                    }}
                  >
                    {t('pages:iam.detach')}
                  </Button>
                ) : null,
            },
          ]}
          data={allPolicies}
          emptyMessage={t('pages:iam.noPoliciesAttached')}
        />
      </Card>

      {/* ── Section 2: Role Memberships ── */}
      <Card>
        <Stack direction="horizontal" justify="between" align="center" className={styles.sectionTitleRow}>
          <h3 className={styles.sectionTitleText}>{t('pages:iam.roleMemberships')}</h3>
          <Button onClick={() => setShowAddRole(!showAddRole)}>
            {t('pages:iam.attachRole')}
          </Button>
        </Stack>
        <p className={styles.rolesSectionDesc}>
          {t('pages:iam.rolesSectionDesc')}
        </p>

        {showAddRole && (
          <div className={styles.inlineForm}>
            <div className={styles.flexOne}>
              <label className={styles.formLabel}>{t('pages:iam.role')}</label>
              <select value={selectedGroupId} onChange={(e) => setSelectedGroupId(e.target.value)} className={`${styles.filterSelect} ${styles.selectFull}`} aria-label={t('pages:iam.selectRoleToAttach')}>
                <option value="">{t('pages:iam.selectRole')}</option>
                {availableGroups.map((g) => (
                  <option key={g.id} value={g.id}>{g.name}</option>
                ))}
              </select>
            </div>
            <Button
              onClick={() => { if (selectedGroupId) addToGroup(selectedGroupId); }}
              disabled={addGroupLoading || !selectedGroupId}
            >
              {addGroupLoading ? t('pages:iam.attaching') : t('pages:iam.attach')}
            </Button>
            <Button variant="secondary" onClick={() => { setShowAddRole(false); setSelectedGroupId(''); }}>
              {t('common:cancel')}
            </Button>
          </div>
        )}

        <DataTable hideSearch
          columns={[
            {
              key: 'groupName',
              label: t('pages:iam.role'),
              render: (r: RoleRow) => (
                <Link to={`/iam/roles/${r.groupId}`} className={styles.linkStyle}>
                  {r.groupName}
                </Link>
              ),
            },
            {
              key: 'actions',
              label: '',
              render: (r: RoleRow) => (
                <Button
                  variant="danger"
                  size="sm"
                  onClick={(e) => {
                    e.stopPropagation();
                    setRemovingRole({ groupId: r.groupId, groupName: r.groupName });
                  }}
                >
                  {t('pages:iam.detach')}
                </Button>
              ),
            },
          ]}
          data={currentRoles}
          emptyMessage={t('pages:iam.noRoleMemberships')}
        />
      </Card>

      {/* Confirm dialogs */}
      <AlertDialog
        open={!!detachingPolicy}
        onOpenChange={(open) => { if (!open) setDetachingPolicy(null); }}
        title={t('pages:iam.detachPolicy')}
        description={t('pages:iam.detachPolicyConfirm', { name: detachingPolicy?.policyName ?? '' })}
        confirmLabel={t('pages:iam.detach')}
        onConfirm={() => { if (detachingPolicy) detachPolicy(detachingPolicy.attachmentId); }}
        variant="danger"
        loading={detachPolicyLoading}
      />

      <AlertDialog
        open={!!removingRole}
        onOpenChange={(open) => { if (!open) setRemovingRole(null); }}
        title={t('pages:iam.removeRoleMembership')}
        description={t('pages:iam.detachRoleConfirm', { name: removingRole?.groupName ?? '' })}
        confirmLabel={t('pages:iam.detach')}
        onConfirm={() => { if (removingRole) removeFromGroup(removingRole.groupId); }}
        variant="danger"
        loading={removeGroupLoading}
      />
    </Stack>
  );
}
