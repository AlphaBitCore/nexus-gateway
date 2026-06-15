import { useState, useCallback } from 'react';
import { useNavigate } from 'react-router-dom';
import { useTranslation } from 'react-i18next';
import { useApi } from '@/hooks/useApi';
import { quotaOverrideApi, organizationApi, projectApi, virtualKeyApi, iamApi } from '@/api/services';
import type { QuotaOverride } from '@/api/services';
import { useMutation } from '@/hooks/useMutation';
import { usePermission } from '@/hooks/usePermission';
import {
  PageHeader, DataTable, AlertDialog, Badge,
  Skeleton, ErrorBanner, Button, Stack, Card, SearchableCombobox,
  ListPagination, DEFAULT_ADMIN_LIST_PAGE_SIZE, type AdminListPageSize,
  RowActions, RowActionIconButton, RowDeleteAction, OpenActionIcon,
} from '@/components/ui';
import type { DataTableColumn, ComboboxOption } from '@/components/ui';
import styles from './QuotaOverrideList.module.css';

/* -- Target search fetcher ------------------------------------------------- */

async function fetchTargetOptions(targetType: string, query: string): Promise<ComboboxOption[]> {
  const params: Record<string, string> = { limit: '100' };
  if (query.trim()) params.q = query.trim();
  switch (targetType) {
    case 'user': {
      const res = await iamApi.listUsers(params);
      const rows = (res as { data: Array<{ id: string; displayName?: string; email?: string }> }).data ?? [];
      return rows.map((u) => ({ id: u.id, label: u.displayName ?? u.email ?? u.id }));
    }
    case 'vk': {
      const res = await virtualKeyApi.list(params);
      return (res.data ?? []).map((k) => ({ id: k.id, label: k.name }));
    }
    case 'project': {
      const res = await projectApi.list(params);
      return (res.data ?? []).map((p) => ({ id: p.id, label: p.name }));
    }
    case 'organization': {
      const res = await organizationApi.list(query.trim() ? { q: query.trim() } : undefined);
      return (res.data ?? []).slice(0, 100).map((o) => ({ id: o.id, label: o.name }));
    }
    default:
      return [];
  }
}

/* -- Component ------------------------------------------------------------- */

export function QuotaOverrideListPage() {
  const { t } = useTranslation();
  const navigate = useNavigate();
  const [targetTypeFilter, setTargetTypeFilter] = useState('');
  const [targetIdFilter, setTargetIdFilter] = useState('');
  const [targetLabel, setTargetLabel] = useState('');
  const [offset, setOffset] = useState(0);
  const [pageLimit, setPageLimit] = useState<AdminListPageSize>(DEFAULT_ADMIN_LIST_PAGE_SIZE);

  const { data, loading, error, refetch } = useApi<{ data: QuotaOverride[]; total: number }>(
    () => {
      const params: Record<string, string> = {
        limit: String(pageLimit),
        offset: String(offset),
      };
      if (targetTypeFilter) params.targetType = targetTypeFilter;
      if (targetIdFilter) params.q = targetIdFilter;
      return quotaOverrideApi.list(params);
    },
    ['admin', 'quota-overrides', 'list', targetTypeFilter, targetIdFilter, offset, pageLimit],
  );

  const [deleting, setDeleting] = useState<QuotaOverride | null>(null);

  const canCreate = usePermission('quota:create');
  const canUpdate = usePermission('quota:update');
  const canDelete = usePermission('quota:delete');

  const { mutate: deleteOverride } = useMutation(
    (id: string) => quotaOverrideApi.delete(id),
    {
      onSuccess: () => { setDeleting(null); refetch(); },
      successMessage: t('pages:quotaOverrides.overrideDeleted'),
    },
  );

  const rows = data?.data ?? [];
  const total = data?.total ?? 0;

  const onTargetTypeFilterChange = useCallback((e: React.ChangeEvent<HTMLSelectElement>) => {
    setTargetTypeFilter(e.target.value);
    // Reset target selection when type changes
    setTargetIdFilter('');
    setTargetLabel('');
    setOffset(0);
  }, []);

  const showTargetSearch = ['user', 'vk', 'project', 'organization'].includes(targetTypeFilter);

  if (loading) return <Skeleton.ListPageSkeleton />;
  if (error) return <ErrorBanner message={error.message} onRetry={refetch} />;

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

  const columns: DataTableColumn<QuotaOverride>[] = [
    {
      key: 'targetType',
      label: t('pages:quotaOverrides.targetType'),
      render: (r) => <Badge variant="default">{scopeLabel[r.targetType] ?? r.targetType}</Badge>,
    },
    {
      key: 'targetId',
      label: t('pages:quotaOverrides.target'),
      render: (r) => r.targetName ?? r.targetId,
    },
    {
      key: 'costLimitUsd',
      label: t('pages:quotaOverrides.costLimit'),
      render: (r) => r.costLimitUsd != null ? `$${Number(r.costLimitUsd).toFixed(2)}` : '-',
    },
    {
      key: 'enforcementMode',
      label: t('pages:quotaOverrides.enforcementMode'),
      render: (r) => r.enforcementMode ? (enforcementLabel[r.enforcementMode] ?? r.enforcementMode) : t('pages:quotaOverrides.inheritFromPolicy'),
    },
    {
      key: 'periodType',
      label: t('pages:quotaOverrides.periodType'),
      render: (r) => r.periodType ? (periodLabel[r.periodType] ?? r.periodType) : t('pages:quotaOverrides.inheritFromPolicy'),
    },
    {
      key: 'reason',
      label: t('pages:quotaOverrides.reason'),
      render: (r) => r.reason ?? '-',
    },
    {
      key: 'actions',
      label: t('pages:quotaOverrides.actions'),
      render: (r) => (
        <RowActions>
          {canUpdate && (
            <RowActionIconButton label={t('common:edit')} onAction={() => navigate(`/ai-gateway/quota-overrides/${r.id}/edit`)}>
              <OpenActionIcon />
            </RowActionIconButton>
          )}
          {canDelete && (
            <RowDeleteAction label={t('common:delete')} onAction={() => setDeleting(r)} />
          )}
        </RowActions>
      ),
    },
  ];

  return (
    <Stack gap="lg">
      <PageHeader
        title={t('pages:quotaOverrides.title')}
        subtitle={t('pages:quotaOverrides.subtitle')}
        action={
          canCreate ? (
            <Button onClick={() => navigate('/ai-gateway/quota-overrides/new')}>{t('pages:quotaOverrides.createOverride')}</Button>
          ) : undefined
        }
      />

      {/* Filters: Type select + Target searchable select */}
      <div className={styles.filterToolbar}>
        <Stack direction="horizontal" gap="sm" style={{ alignItems: 'center' }}>
          <div className={styles.filterControl}>
            <select
              aria-label={t('pages:quotaOverrides.filterByTargetType')}
              value={targetTypeFilter}
              onChange={onTargetTypeFilterChange}
              className={styles.filterSelect}
            >
              <option value="">{t('pages:quotaOverrides.allTargetTypes')}</option>
              <option value="user">{t('pages:quotaOverrides.scopeUser')}</option>
              <option value="vk">{t('pages:quotaOverrides.scopeVk')}</option>
              <option value="project">{t('pages:quotaOverrides.scopeProject')}</option>
              <option value="organization">{t('pages:quotaOverrides.scopeOrganization')}</option>
            </select>
          </div>
          {showTargetSearch && (
            <div className={styles.targetSearchControl}>
              <SearchableCombobox
                ariaLabel={t('pages:quotaOverrides.target')}
                placeholder={t('pages:quotaOverrides.searchTarget')}
                className={styles.targetCombobox}
                valueId={targetIdFilter}
                valueLabel={targetLabel}
                allowEmptyQueryFetch
                fetchOptions={(q) => fetchTargetOptions(targetTypeFilter, q)}
                onSelect={(opt) => {
                  setTargetIdFilter(opt?.id ?? '');
                  setTargetLabel(opt?.label ?? '');
                  setOffset(0);
                }}
              />
            </div>
          )}
          {/* total count hidden per user request */}
        </Stack>
      </div>

      <Card padding="none">
        <DataTable
          hideSearch
          frameless
          pageSize={pageLimit}
          columns={columns}
          data={rows}
          onRowClick={(r) => navigate(`/ai-gateway/quota-overrides/${r.id}`)}
          emptyMessage={t('pages:quotaOverrides.noOverridesConfigured')}
        />
      </Card>

      <ListPagination variant="plain" offset={offset} limit={pageLimit} total={total} onOffsetChange={setOffset} onLimitChange={setPageLimit} />

      <AlertDialog
        open={!!deleting}
        onOpenChange={(open) => { if (!open) setDeleting(null); }}
        title={t('pages:quotaOverrides.deleteOverride')}
        description={t('pages:quotaOverrides.deleteConfirm')}
        confirmLabel={t('common:delete')}
        onConfirm={() => { if (deleting) deleteOverride(deleting.id); }}
        variant="danger"
      />
    </Stack>
  );
}
