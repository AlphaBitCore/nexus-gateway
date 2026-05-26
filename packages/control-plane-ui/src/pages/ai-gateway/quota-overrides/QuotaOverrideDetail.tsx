import { useParams, useNavigate, Link } from 'react-router-dom';
import { useTranslation } from 'react-i18next';
import { useApi } from '@/hooks/useApi';
import { quotaOverrideApi } from '@/api/services';
import type { QuotaOverride } from '@/api/services';
import { usePermission } from '@/hooks/usePermission';
import {
  Badge, Breadcrumb, Button, Card, ErrorBanner, PageHeader, Skeleton, Stack,
} from '@/components/ui';
import { formatDateTime } from '@/lib/format';
import iamStyles from '@/pages/iam/_shared/Iam.module.css';

export function QuotaOverrideDetail() {
  const { t } = useTranslation();
  const { id } = useParams<{ id: string }>();
  const navigate = useNavigate();
  const canUpdate = usePermission('quotaPolicy:update');

  const { data: row, loading, error, refetch } = useApi<QuotaOverride>(
    () => quotaOverrideApi.get(id!),
    ['admin', 'quota-overrides', 'detail', id],
  );

  if (loading) return <Skeleton.DetailPageSkeleton />;
  if (error) return <ErrorBanner message={error.message} onRetry={refetch} />;
  if (!row) return null;

  const scopeLabel: Record<string, string> = {
    user: t('pages:quotaOverrides.scopeUser'),
    vk: t('pages:quotaOverrides.scopeVk'),
    project: t('pages:quotaOverrides.scopeProject'),
    organization: t('pages:quotaOverrides.scopeOrganization'),
  };
  const periodLabel: Record<string, string> = {
    daily: t('pages:quotaOverrides.daily'),
    weekly: t('pages:quotaOverrides.weekly'),
    monthly: t('pages:quotaOverrides.monthly'),
  };
  const enforcementLabel: Record<string, string> = {
    reject: t('pages:quotaOverrides.reject'),
    downgrade: t('pages:quotaOverrides.downgrade'),
    'notify-and-proceed': t('pages:quotaOverrides.notifyAndProceed'),
    'track-only': t('pages:quotaOverrides.trackOnly'),
  };

  const title = row.targetName ?? row.targetId;

  return (
    <Stack gap="lg">
      <Breadcrumb
        items={[
          { label: t('pages:quotaOverrides.title'), to: '/ai-gateway/quota-overrides' },
          { label: title },
        ]}
      />
      <PageHeader
        title={title}
        subtitle={t('pages:quotaOverrides.detailSubtitle')}
        action={
          canUpdate ? (
            <Button onClick={() => navigate(`/ai-gateway/quota-overrides/${row.id}/edit`)}>
              {t('common:edit')}
            </Button>
          ) : undefined
        }
      />
      <Card>
        <div className={iamStyles.kvGrid}>
          <div>
            <div className={iamStyles.kvLabel}>{t('pages:quotaOverrides.targetType')}</div>
            <div className={iamStyles.kvValue}>
              <Badge variant="default">{scopeLabel[row.targetType] ?? row.targetType}</Badge>
            </div>
          </div>
          <div>
            <div className={iamStyles.kvLabel}>{t('pages:quotaOverrides.target')}</div>
            <div className={iamStyles.kvValue}>{row.targetName ?? row.targetId}</div>
          </div>
          <div>
            <div className={iamStyles.kvLabel}>{t('pages:quotaOverrides.targetId')}</div>
            <div className={iamStyles.kvValue}>
              <code>{row.targetId}</code>
            </div>
          </div>
          {row.targetOrgName || row.targetOrgId ? (
            <div>
              <div className={iamStyles.kvLabel}>{t('pages:quotaPolicies.organization')}</div>
              <div className={iamStyles.kvValue}>{row.targetOrgName ?? row.targetOrgId}</div>
            </div>
          ) : null}
          <div>
            <div className={iamStyles.kvLabel}>{t('pages:quotaOverrides.costLimit')}</div>
            <div className={iamStyles.kvValue}>
              {row.costLimitUsd != null ? `$${Number(row.costLimitUsd).toFixed(2)}` : '\u2014'}
            </div>
          </div>
          <div>
            <div className={iamStyles.kvLabel}>{t('pages:quotaPolicies.tokenLimit')}</div>
            <div className={iamStyles.kvValue}>
              {row.tokenLimit != null ? String(row.tokenLimit) : '\u2014'}
            </div>
          </div>
          <div>
            <div className={iamStyles.kvLabel}>{t('pages:quotaOverrides.enforcementMode')}</div>
            <div className={iamStyles.kvValue}>
              {row.enforcementMode
                ? (enforcementLabel[row.enforcementMode] ?? row.enforcementMode)
                : t('pages:quotaOverrides.inheritFromPolicy')}
            </div>
          </div>
          <div>
            <div className={iamStyles.kvLabel}>{t('pages:quotaOverrides.periodType')}</div>
            <div className={iamStyles.kvValue}>
              {row.periodType ? (periodLabel[row.periodType] ?? row.periodType) : t('pages:quotaOverrides.inheritFromPolicy')}
            </div>
          </div>
          <div>
            <div className={iamStyles.kvLabel}>{t('pages:quotaOverrides.reason')}</div>
            <div className={iamStyles.kvValue}>{row.reason ?? '\u2014'}</div>
          </div>
          {row.createdBy ? (
            <div>
              <div className={iamStyles.kvLabel}>{t('pages:iam.createdBy')}</div>
              <div className={iamStyles.kvValue}>{row.createdBy}</div>
            </div>
          ) : null}
          <div>
            <div className={iamStyles.kvLabel}>{t('pages:iam.created')}</div>
            <div className={iamStyles.kvValue}>{formatDateTime(row.createdAt)}</div>
          </div>
          <div>
            <div className={iamStyles.kvLabel}>{t('pages:iam.updated')}</div>
            <div className={iamStyles.kvValue}>{formatDateTime(row.updatedAt)}</div>
          </div>
        </div>
        <p style={{ marginTop: 'var(--g-space-4)' }}>
          <Link to="/ai-gateway/quota-overrides">{t('pages:quotaOverrides.backToList')}</Link>
        </p>
      </Card>
    </Stack>
  );
}
