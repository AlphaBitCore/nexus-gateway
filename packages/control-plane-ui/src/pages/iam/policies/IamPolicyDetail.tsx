import { useState, useEffect } from 'react';
import { useParams, useNavigate } from 'react-router-dom';
import { useTranslation } from 'react-i18next';
import { iamApi } from '@/api/services';
import { useApi } from '../../../hooks/useApi';
import { usePermission } from '../../../hooks/usePermission';
import {
  PageHeader, Badge, statusToVariant, Skeleton, ErrorBanner,
  Tooltip, DataTable, Breadcrumb, Button, Stack, Card,
} from '@/components/ui';
import type { IamPolicy } from '../../../api/types';
import { formatDateTime } from '@/lib/format';
import styles from '../_shared/Iam.module.css';

type PolicyDetailTab = 'info' | 'statements' | 'attachments';

/** Policy detail UI lists users and virtual keys; admin API keys inherit via groups / owners and stay hidden here. */
const ATTACHMENT_TAB_PRINCIPAL_TYPES = new Set(['nexus_user', 'virtual_key']);

export function IamPolicyDetail() {
  const { t } = useTranslation();
  const { id } = useParams<{ id: string }>();
  const navigate = useNavigate();
  const [activeTab, setActiveTab] = useState<PolicyDetailTab>('info');
  const [copied, setCopied] = useState(false);
  const canUpdate = usePermission('iam:update');

  const { data: policy, loading, error, refetch } = useApi<IamPolicy>(
    () => iamApi.getPolicy(id!),
    ['admin', 'iam', 'policies', 'detail', id],
  );

  // Fetch roles (groups) and direct attachments for this policy
  const { data: attachData } = useApi<{
    roles: Array<{ id: string; name: string; memberCount: number; members: Array<{ principalType: string; principalId: string }> }>;
    directAttachments: Array<{ principalType: string; principalId: string }>;
  }>(
    () => iamApi.getPolicyAttachments(id!) as Promise<unknown> as Promise<{
      roles: Array<{ id: string; name: string; memberCount: number; members: Array<{ principalType: string; principalId: string }> }>;
      directAttachments: Array<{ principalType: string; principalId: string }>;
    }>,
    ['admin', 'iam', 'policies', 'attachments', id],
  );
  const { data: usersData } = useApi<{ data: Array<{ id: string; displayName: string; email?: string }> }>(
    () => iamApi.listUsers(),
    ['admin', 'iam', 'users', 'list'],
  );

  useEffect(() => {
    setActiveTab('info');
  }, [id]);

  if (loading) return <Skeleton.DetailPageSkeleton />;
  if (error) return <ErrorBanner message={error.message} onRetry={refetch} />;
  if (!policy) return null;

  const statements = policy.document?.Statement ?? [];
  const roles = attachData?.roles ?? [];
  const directAttachments = attachData?.directAttachments ?? [];
  const visibleDirectAttachments = directAttachments.filter((a) =>
    ATTACHMENT_TAB_PRINCIPAL_TYPES.has(a.principalType),
  );
  const allUsers = usersData?.data ?? [];

  // Resolve user IDs from group members (admin users only; API keys are not listed on this page)
  const memberUserIds = new Set(
    roles.flatMap((r) =>
      r.members.filter((m) => m.principalType === 'nexus_user').map((m) => m.principalId),
    ),
  );
  const directUserIds = new Set(
    visibleDirectAttachments.filter((a) => a.principalType === 'nexus_user').map((a) => a.principalId),
  );
  const attachedUsersViaRoles = allUsers.filter(u => memberUserIds.has(u.id));
  const directlyAttachedUsers = allUsers.filter(u => directUserIds.has(u.id));

  const rolesWithUserCounts = roles.map((r) => ({
    ...r,
    adminUserMemberCount: r.members.filter((m) => m.principalType === 'nexus_user').length,
  }));

  const handleCopy = () => {
    navigator.clipboard.writeText(JSON.stringify(policy.document, null, 2));
    setCopied(true);
    setTimeout(() => setCopied(false), 2000);
  };

  return (
    <Stack gap="lg">
      <Breadcrumb items={[
        { label: t('pages:iam.iamPolicies'), to: '/iam/policies' },
        { label: policy.name },
      ]} />

      <PageHeader
        title={policy.name}
        subtitle={policy.description || undefined}
        action={
          <Stack direction="horizontal" gap="sm" className={styles.policyHeaderActions}>
            <span className={policy.type === 'managed' ? styles.typeBadgeManaged : styles.typeBadgeCustom}>{policy.type}</span>
            <Button variant="secondary" onClick={handleCopy}>
              {copied ? t('pages:iam.copied') : t('pages:iam.copyPolicy')}
            </Button>
            {canUpdate && (
              <Button
                onClick={() => navigate(`/iam/policies/${policy.id}/edit`)}
                disabled={policy.type === 'managed'}
              >
                {t('common:edit')}
              </Button>
            )}
          </Stack>
        }
      />

      <div className={styles.tabBar} role="tablist" aria-label={t('pages:iam.ariaPolicySections')}>
        {(
          [
            { id: 'info' as const, label: t('pages:iam.information') },
            { id: 'statements' as const, label: `${t('pages:iam.statements')} (${statements.length})` },
            {
              id: 'attachments' as const,
              label: `${t('pages:iam.attachments')} (${roles.length + visibleDirectAttachments.length})`,
            },
          ] as const
        ).map(({ id: tabId, label }) => (
          <button
            key={tabId}
            type="button"
            role="tab"
            aria-selected={activeTab === tabId}
            onClick={() => setActiveTab(tabId)}
            className={activeTab === tabId ? styles.tabActive : styles.tab}
          >
            {label}
          </button>
        ))}
      </div>

      {activeTab === 'info' && (
        <Card>
          <div className={styles.kvGrid}>
            <div>
              <div className={styles.kvLabelRow}>
                <span className={styles.kvLabel}>{t('pages:iam.policyId')}</span>
                <Tooltip content={t('pages:iam.policyIdTooltip')}>
                  <span className={styles.tooltipIcon}>&#x24D8;</span>
                </Tooltip>
              </div>
              <div className={styles.kvValue}>{policy.id}</div>
            </div>
            <div>
              <div className={styles.kvLabelRow}>
                <span className={styles.kvLabel}>{t('pages:iam.type')}</span>
                <Tooltip content={t('pages:iam.typeTooltip')}>
                  <span className={styles.tooltipIcon}>&#x24D8;</span>
                </Tooltip>
              </div>
              <div className={styles.kvValue}>
                <span className={policy.type === 'managed' ? styles.typeBadgeManaged : styles.typeBadgeCustom}>{policy.type}</span>
              </div>
            </div>
            <div>
              <div className={styles.kvLabelRow}>
                <span className={styles.kvLabel}>{t('pages:iam.status')}</span>
                <Tooltip content={t('pages:iam.statusTooltip')}>
                  <span className={styles.tooltipIcon}>&#x24D8;</span>
                </Tooltip>
              </div>
              <div className={styles.kvValue}>
                <Badge variant={statusToVariant(policy.enabled ? 'enabled' : 'disabled')}>
                  {policy.enabled ? t('common:enabled') : t('common:disabled')}
                </Badge>
              </div>
            </div>
            <div>
              <div className={styles.kvLabelRow}>
                <span className={styles.kvLabel}>{t('pages:iam.documentVersion')}</span>
                <Tooltip content={t('pages:iam.documentVersionTooltip')}>
                  <span className={styles.tooltipIcon}>&#x24D8;</span>
                </Tooltip>
              </div>
              <div className={styles.kvValue}>{policy.document?.Version ?? '\u2014'}</div>
            </div>
            <div>
              <div className={styles.kvLabelRow}>
                <span className={styles.kvLabel}>{t('pages:iam.statements')}</span>
                <Tooltip content={t('pages:iam.statementsTooltip')}>
                  <span className={styles.tooltipIcon}>&#x24D8;</span>
                </Tooltip>
              </div>
              <div className={styles.kvValue}>{statements.length}</div>
            </div>
            <div className={styles.fullSpan}>
              <div className={styles.kvLabelRow}>
                <span className={styles.kvLabel}>{t('pages:iam.description')}</span>
                <Tooltip content={t('pages:iam.descriptionTooltip')}>
                  <span className={styles.tooltipIcon}>&#x24D8;</span>
                </Tooltip>
              </div>
              <div className={`${styles.kvValue} ${styles.descriptionValue}`}>
                {policy.description?.trim() ? policy.description : '\u2014'}
              </div>
            </div>
            <div>
              <div className={styles.kvLabel}>{t('pages:iam.created')}</div>
              <div className={styles.kvValue}>
                {formatDateTime(policy.createdAt)}
              </div>
            </div>
            <div>
              <div className={styles.kvLabel}>{t('pages:iam.updated')}</div>
              <div className={styles.kvValue}>
                {formatDateTime(policy.updatedAt)}
              </div>
            </div>
            <div>
              <div className={styles.kvLabel}>{t('pages:iam.createdBy')}</div>
              <div className={styles.kvValue}>{policy.createdBy ?? '\u2014'}</div>
            </div>
          </div>
        </Card>
      )}

      {activeTab === 'statements' && (
        <Card>
          <h2 className={styles.sectionHeading}>{t('pages:iam.statements')} ({statements.length})</h2>
          <div className={styles.statementsPanel}>
            {statements.length === 0 ? (
              <div className={styles.statementsEmpty}>
                {t('pages:iam.noStatementsInPolicy')}
              </div>
            ) : (
              statements.map((stmt, idx) => (
                <div key={idx} className={styles.statementCard}>
                  <Stack direction="horizontal" gap="sm" className={styles.stmtHeader}>
                    <span className={stmt.Effect === 'Allow' ? styles.effectAllow : styles.effectDeny}>{stmt.Effect}</span>
                    <Tooltip content={t('pages:iam.effectTooltip')}>
                      <span className={styles.tooltipIcon}>&#x24D8;</span>
                    </Tooltip>
                    {stmt.Sid && (
                      <span className={`${styles.mono} ${styles.stmtSidMono}`}>
                        {stmt.Sid}
                      </span>
                    )}
                  </Stack>

                  <div className={styles.stmtSection}>
                    <div className={styles.kvLabelRow}>
                      <span className={styles.kvLabel}>{t('pages:iam.actionsLabel')}</span>
                      <Tooltip content={t('pages:iam.actionsTooltip')}>
                        <span className={styles.tooltipIcon}>&#x24D8;</span>
                      </Tooltip>
                    </div>
                    <div className={styles.chipRow}>
                      {/* Action + Resource are StringList — accept both
                          single string (canonical length-1 form) and array. */}
                      {(Array.isArray(stmt.Action) ? stmt.Action : [stmt.Action]).filter(Boolean).map((a: string, i: number) => (
                        <span key={i} className={styles.codeChip}>{a}</span>
                      ))}
                    </div>
                  </div>

                  <div className={stmt.Condition ? styles.stmtSection : styles.stmtLastSection}>
                    <div className={styles.kvLabelRow}>
                      <span className={styles.kvLabel}>{t('pages:iam.resourcesLabel')}</span>
                      <Tooltip content={t('pages:iam.resourcesTooltip')}>
                        <span className={styles.tooltipIcon}>&#x24D8;</span>
                      </Tooltip>
                    </div>
                    <div className={styles.chipRow}>
                      {(Array.isArray(stmt.Resource) ? stmt.Resource : [stmt.Resource]).filter(Boolean).map((r: string, i: number) => (
                        <span key={i} className={styles.codeChip}>{r}</span>
                      ))}
                    </div>
                  </div>

                  {stmt.Condition && (
                    <div>
                      <div className={styles.kvLabelRow}>
                        <span className={styles.kvLabel}>{t('pages:iam.conditionsLabel')}</span>
                        <Tooltip content={t('pages:iam.conditionsTooltip')}>
                          <span className={styles.tooltipIcon}>&#x24D8;</span>
                        </Tooltip>
                      </div>
                      {Object.entries(stmt.Condition).map(([operator, conditions]) => (
                        <div key={operator} className={styles.conditionRow}>
                          <span className={styles.conditionChip}>{operator}</span>
                          {Object.entries(conditions as Record<string, unknown>).map(([k, v]) => (
                            <span
                              key={k}
                              className={styles.conditionKeyValue}
                            >
                              {k} = {String(v)}
                            </span>
                          ))}
                        </div>
                      ))}
                    </div>
                  )}
                </div>
              ))
            )}
          </div>
        </Card>
      )}

      {activeTab === 'attachments' && (
        <Card>
          <h2 className={styles.sectionHeading}>{t('pages:iam.attachedTo')}</h2>
          <p className={styles.attachmentsIntro}>
            {t('pages:iam.attachmentsIntro')}
          </p>

          <div className={styles.attachmentSection}>
            <h3 className={styles.sectionSubheading}>{t('pages:iam.rolesCount', { count: roles.length })}</h3>
            {roles.length === 0 ? (
              <div className={styles.emptyAttachment}>
                {t('pages:iam.notAttachedToRoles')}
              </div>
            ) : (
              <DataTable
                hideSearch
                columns={[
                  {
                    key: 'name',
                    label: t('pages:iam.roleName'),
                    render: (r: { id: string; name: string }) => (
                      <span
                        className={styles.roleNameLink}
                        onClick={() => navigate(`/iam/groups/${r.id}`)}
                      >
                        {r.name}
                      </span>
                    ),
                  },
                  {
                    key: 'adminUserMemberCount',
                    label: t('pages:iam.adminUsers'),
                    render: (r: { adminUserMemberCount: number }) => String(r.adminUserMemberCount),
                  },
                ]}
                data={rolesWithUserCounts}
                emptyMessage=""
              />
            )}
          </div>

          <div className={styles.attachmentSectionLarge}>
            <h3 className={styles.sectionSubheading}>{t('pages:iam.directAttachments', { count: visibleDirectAttachments.length })}</h3>
            {visibleDirectAttachments.length === 0 ? (
              <div className={styles.emptyAttachment}>
                {t('pages:iam.noPrincipalAttachments')}
              </div>
            ) : (
              <DataTable
                hideSearch
                columns={[
                  { key: 'principalType', label: t('pages:iam.type'), render: (r: { principalType: string }) => r.principalType },
                  {
                    key: 'principalId',
                    label: t('pages:iam.principalId'),
                    render: (r: { principalType: string; principalId: string }) => {
                      if (r.principalType === 'nexus_user') {
                        const user = directlyAttachedUsers.find((u) => u.id === r.principalId);
                        return user ? `${user.displayName}${user.email ? ` (${user.email})` : ''}` : r.principalId;
                      }
                      return r.principalId;
                    },
                  },
                ]}
                data={visibleDirectAttachments}
                emptyMessage=""
              />
            )}
          </div>

          {attachedUsersViaRoles.length > 0 && (
            <div className={styles.attachmentSectionLarge}>
              <h3 className={styles.sectionSubheading}>{t('pages:iam.usersViaRoles', { count: attachedUsersViaRoles.length })}</h3>
              <DataTable
                hideSearch
                columns={[
                  { key: 'displayName', label: t('pages:iam.displayName'), render: (r: { displayName: string }) => r.displayName },
                  { key: 'email', label: t('pages:iam.colEmail'), render: (r: { email?: string }) => r.email ?? '--' },
                ]}
                data={attachedUsersViaRoles}
                emptyMessage=""
              />
            </div>
          )}
        </Card>
      )}
    </Stack>
  );
}
