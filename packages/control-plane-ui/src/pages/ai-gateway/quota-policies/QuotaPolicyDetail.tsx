import { useMemo } from 'react';
import { useParams, useNavigate, Link } from 'react-router-dom';
import { useTranslation } from 'react-i18next';
import { useApi } from '@/hooks/useApi';
import { quotaPolicyApi, organizationApi } from '@/api/services';
import type { QuotaPolicy } from '@/api/services';
import { usePermission } from '@/hooks/usePermission';
import {
  Breadcrumb, Button, Card, ErrorBanner, PageHeader, Skeleton, Stack,
} from '@/components/ui';
import { formatDateTime } from '@/lib/format';
import iamStyles from '@/pages/iam/_shared/Iam.module.css';

export function QuotaPolicyDetail() {
  const { t } = useTranslation();
  const { id } = useParams<{ id: string }>();
  const navigate = useNavigate();
  const canUpdate = usePermission('quotaPolicy:update');

  const { data: orgData } = useApi(
    () => organizationApi.list({ limit: '500' }),
    ['admin', 'organizations', 'list', 'quota-policy-detail'],
  );
  const orgMap = useMemo(() => {
    const m = new Map<string, string>();
    for (const o of orgData?.data ?? []) m.set(o.id, o.name);
    return m;
  }, [orgData]);

  const { data: policy, loading, error, refetch } = useApi<QuotaPolicy>(
    () => quotaPolicyApi.get(id!),
    ['admin', 'quota-policies', 'detail', id],
  );

  if (loading) return <Skeleton.DetailPageSkeleton />;
  if (error) return <ErrorBanner message={error.message} onRetry={refetch} />;
  if (!policy) return null;

  const scopeLabel: Record<string, string> = {
    user: t('pages:quotaPolicies.scopeUser'),
    vk: t('pages:quotaPolicies.scopeVk'),
    project: t('pages:quotaPolicies.scopeProject'),
    organization: t('pages:quotaPolicies.scopeOrganization'),
  };
  const periodLabel: Record<string, string> = {
    daily: t('pages:quotaPolicies.daily'),
    weekly: t('pages:quotaPolicies.weekly'),
    monthly: t('pages:quotaPolicies.monthly'),
  };
  const enforcementLabel: Record<string, string> = {
    reject: t('pages:quotaPolicies.reject'),
    downgrade: t('pages:quotaPolicies.downgrade'),
    'notify-and-proceed': t('pages:quotaPolicies.notifyAndProceed'),
    'track-only': t('pages:quotaPolicies.trackOnly'),
  };
  const vkTypeLabel: Record<string, string> = {
    personal: t('pages:quotaPolicies.vkTypePersonal'),
    application: t('pages:quotaPolicies.vkTypeApplication'),
  };

  const orgDisplay = policy.organizationId
    ? (orgMap.get(policy.organizationId) ?? policy.organizationId)
    : t('pages:quotaPolicies.allOrganizations');
  const thresholds = (policy.alertThresholds ?? [80, 90]).map((v) => `${v}%`).join(' · ');

  return (
    <Stack gap="lg">
      <Breadcrumb
        items={[
          { label: t('pages:quotaPolicies.title'), to: '/ai-gateway/quota-policies' },
          { label: policy.name },
        ]}
      />
      <PageHeader
        title={policy.name}
        subtitle={policy.description || t('pages:quotaPolicies.detailSubtitle')}
        action={
          canUpdate ? (
            <Button onClick={() => navigate(`/ai-gateway/quota-policies/${policy.id}/edit`)}>
              {t('common:edit')}
            </Button>
          ) : undefined
        }
      />
      <Card>
        <div className={iamStyles.kvGrid}>
          <div>
            <div className={iamStyles.kvLabel}>{t('pages:quotaPolicies.name')}</div>
            <div className={iamStyles.kvValue}>{policy.name}</div>
          </div>
          <div>
            <div className={iamStyles.kvLabel}>{t('pages:quotaPolicies.description')}</div>
            <div className={iamStyles.kvValue}>{policy.description || '\u2014'}</div>
          </div>
          <div>
            <div className={iamStyles.kvLabel}>{t('pages:quotaPolicies.scope')}</div>
            <div className={iamStyles.kvValue}>{scopeLabel[policy.scope] ?? policy.scope}</div>
          </div>
          <div>
            <div className={iamStyles.kvLabel}>{t('pages:quotaPolicies.organization')}</div>
            <div className={iamStyles.kvValue}>{orgDisplay}</div>
          </div>
          <div>
            <div className={iamStyles.kvLabel}>{t('pages:quotaPolicies.vkType')}</div>
            <div className={iamStyles.kvValue}>
              {policy.vkType ? (vkTypeLabel[policy.vkType] ?? policy.vkType) : t('pages:quotaPolicies.allTypes')}
            </div>
          </div>
          <div>
            <div className={iamStyles.kvLabel}>{t('pages:quotaPolicies.periodType')}</div>
            <div className={iamStyles.kvValue}>{periodLabel[policy.periodType] ?? policy.periodType}</div>
          </div>
          <div>
            <div className={iamStyles.kvLabel}>{t('pages:quotaPolicies.costLimit')}</div>
            <div className={iamStyles.kvValue}>
              {policy.costLimitUsd != null ? `$${Number(policy.costLimitUsd).toFixed(2)}` : '\u2014'}
            </div>
          </div>
          <div>
            <div className={iamStyles.kvLabel}>{t('pages:quotaPolicies.tokenLimit')}</div>
            <div className={iamStyles.kvValue}>
              {policy.tokenLimit != null ? String(policy.tokenLimit) : '\u2014'}
            </div>
          </div>
          <div>
            <div className={iamStyles.kvLabel}>{t('pages:quotaPolicies.enforcementMode')}</div>
            <div className={iamStyles.kvValue}>
              {enforcementLabel[policy.enforcementMode] ?? policy.enforcementMode}
            </div>
          </div>
          <div>
            <div className={iamStyles.kvLabel}>{t('pages:quotaPolicies.priority')}</div>
            <div className={iamStyles.kvValue}>{policy.priority != null ? String(policy.priority) : '0'}</div>
          </div>
          <div>
            <div className={iamStyles.kvLabel}>{t('pages:quotaPolicies.enabled')}</div>
            <div className={iamStyles.kvValue}>{policy.enabled ? t('common:yes') : t('common:no')}</div>
          </div>
          <div>
            <div className={iamStyles.kvLabel}>{t('pages:quotaPolicies.alertThresholds')}</div>
            <div className={iamStyles.kvValue}>{thresholds}</div>
          </div>
          {policy.createdBy ? (
            <div>
              <div className={iamStyles.kvLabel}>{t('pages:iam.createdBy')}</div>
              <div className={iamStyles.kvValue}>{policy.createdBy}</div>
            </div>
          ) : null}
          <div>
            <div className={iamStyles.kvLabel}>{t('pages:iam.created')}</div>
            <div className={iamStyles.kvValue}>{formatDateTime(policy.createdAt)}</div>
          </div>
          <div>
            <div className={iamStyles.kvLabel}>{t('pages:iam.updated')}</div>
            <div className={iamStyles.kvValue}>{formatDateTime(policy.updatedAt)}</div>
          </div>
        </div>
        <p style={{ marginTop: 'var(--g-space-4)' }}>
          <Link to="/ai-gateway/quota-policies">{t('pages:quotaPolicies.backToList')}</Link>
        </p>
      </Card>
    </Stack>
  );
}
