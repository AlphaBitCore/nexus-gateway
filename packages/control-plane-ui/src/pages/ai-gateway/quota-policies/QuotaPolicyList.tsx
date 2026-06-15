import { useState, useCallback, useMemo } from 'react';
import { useNavigate } from 'react-router-dom';
import { useTranslation } from 'react-i18next';
import { useApi } from '@/hooks/useApi';
import { quotaPolicyApi, organizationApi } from '@/api/services';
import type { QuotaPolicy } from '@/api/services';
import { useDebouncedValue } from '@/hooks/useDebouncedValue';
import { useMutation } from '@/hooks/useMutation';
import { usePermission } from '@/hooks/usePermission';
import {
  PageHeader, DataTable, ListFilterToolbar, AlertDialog,
  Skeleton, ErrorBanner, Button, Stack, Card,
  ListPagination, DEFAULT_ADMIN_LIST_PAGE_SIZE, type AdminListPageSize,
  RowActions, RowActionIconButton, RowDeleteAction, OpenActionIcon,
} from '@/components/ui';
import type { DataTableColumn } from '@/components/ui';
import styles from './QuotaPolicyList.module.css';

export function QuotaPolicyListPage() {
  const { t } = useTranslation();
  const navigate = useNavigate();
  const [search, setSearch] = useState('');
  const debouncedSearch = useDebouncedValue(search, 300);
  const [scopeFilter, setScopeFilter] = useState('');
  const [enabledFilter, setEnabledFilter] = useState('');
  const [offset, setOffset] = useState(0);
  const [pageLimit, setPageLimit] = useState<AdminListPageSize>(DEFAULT_ADMIN_LIST_PAGE_SIZE);

  const { data: orgData } = useApi(
    () => organizationApi.list({ limit: '500' }),
    ['admin', 'organizations', 'list', 'quota-policy-picker'],
  );
  const orgMap = useMemo(() => {
    const m = new Map<string, string>();
    for (const o of orgData?.data ?? []) m.set(o.id, o.name);
    return m;
  }, [orgData]);

  const { data, loading, error, refetch } = useApi<{ data: QuotaPolicy[]; total: number }>(
    () => {
      const params: Record<string, string> = {
        limit: String(pageLimit),
        offset: String(offset),
      };
      const q = debouncedSearch.trim();
      if (q) params.q = q;
      if (scopeFilter) params.scope = scopeFilter;
      if (enabledFilter) params.enabled = enabledFilter;
      return quotaPolicyApi.list(params);
    },
    ['admin', 'quota-policies', 'list', debouncedSearch, scopeFilter, enabledFilter, offset, pageLimit],
  );

  const [deleting, setDeleting] = useState<QuotaPolicy | null>(null);

  const canCreate = usePermission('quotaPolicy:create');
  const canUpdate = usePermission('quotaPolicy:update');
  const canDelete = usePermission('quotaPolicy:delete');

  const { mutate: deletePolicy } = useMutation(
    (id: string) => quotaPolicyApi.delete(id),
    {
      onSuccess: () => { setDeleting(null); refetch(); },
      successMessage: t('pages:quotaPolicies.policyDeleted'),
    },
  );

  const rows = data?.data ?? [];
  const total = data?.total ?? 0;

  const onSearchChange = useCallback((v: string) => {
    setSearch(v);
    setOffset(0);
  }, []);

  const onScopeFilterChange = useCallback((e: React.ChangeEvent<HTMLSelectElement>) => {
    setScopeFilter(e.target.value);
    setOffset(0);
  }, []);

  const onEnabledFilterChange = useCallback((e: React.ChangeEvent<HTMLSelectElement>) => {
    setEnabledFilter(e.target.value);
    setOffset(0);
  }, []);

  if (loading) return <Skeleton.ListPageSkeleton />;
  if (error) return <ErrorBanner message={error.message} onRetry={refetch} />;

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

  const columns: DataTableColumn<QuotaPolicy>[] = [
    { key: 'name', label: t('pages:quotaPolicies.name') },
    {
      key: 'scope',
      label: t('pages:quotaPolicies.scope'),
      render: (r) => {
        const main = scopeLabel[r.scope] ?? r.scope;
        const period = periodLabel[r.periodType] ?? r.periodType;
        // Compose the sub-label from the scope's natural qualifier
        // (org name for organization scope, vk type for vk scope, etc.)
        // followed by the period. We always show period because it's
        // a critical reading-the-policy signal that doesn't fit in main.
        let qualifier = '';
        if (r.scope === 'organization') {
          qualifier = r.organizationId
            ? (orgMap.get(r.organizationId) ?? r.organizationId)
            : t('pages:quotaPolicies.allOrganizations');
        } else if (r.scope === 'vk') {
          qualifier = r.vkType
            ? (vkTypeLabel[r.vkType] ?? r.vkType)
            : t('pages:quotaPolicies.allTypes');
        }
        const sub = qualifier ? `${qualifier} · ${period}` : period;
        return (
          <div className={styles.scopeCell}>
            <div className={styles.scopeMain}>{main}</div>
            <div className={styles.scopeSub}>{sub}</div>
          </div>
        );
      },
    },
    {
      key: 'costLimitUsd',
      label: t('pages:quotaPolicies.costLimit'),
      render: (r) => r.costLimitUsd != null ? `$${Number(r.costLimitUsd).toFixed(2)}` : '-',
    },
    {
      key: 'enforcementMode',
      label: t('pages:quotaPolicies.enforcementMode'),
      render: (r) => enforcementLabel[r.enforcementMode] ?? r.enforcementMode,
    },
    {
      key: 'enabled',
      label: t('pages:quotaPolicies.enabled'),
      render: (r) => r.enabled ? t('common:yes') : t('common:no'),
    },
    {
      key: 'actions',
      label: t('pages:quotaPolicies.actions'),
      render: (r) => (
        <RowActions>
          {canUpdate && (
            <RowActionIconButton label={t('common:edit')} onAction={() => navigate(`/ai-gateway/quota-policies/${r.id}/edit`)}>
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
        title={t('pages:quotaPolicies.title')}
        subtitle={t('pages:quotaPolicies.subtitle')}
        action={
          canCreate ? (
            <Button onClick={() => navigate('/ai-gateway/quota-policies/new')}>{t('pages:quotaPolicies.createPolicy')}</Button>
          ) : undefined
        }
      />

      <ListFilterToolbar
        variant="boxed"
        searchPlaceholder={t('pages:quotaPolicies.searchPlaceholder')}
        searchValue={search}
        onSearchChange={onSearchChange}
        meta={
          total === 0
            ? t('pages:quotaPolicies.noPoliciesMatch')
            : t('pages:quotaPolicies.showingPolicies', { count: rows.length, total: total.toLocaleString() })
        }
      >
        <select
          aria-label={t('pages:quotaPolicies.filterByScope')}
          value={scopeFilter}
          onChange={onScopeFilterChange}
          className={styles.filterSelect}
        >
          <option value="">{t('pages:quotaPolicies.allScopes')}</option>
          <option value="user">{t('pages:quotaPolicies.scopeUser')}</option>
          <option value="vk">{t('pages:quotaPolicies.scopeVk')}</option>
          <option value="project">{t('pages:quotaPolicies.scopeProject')}</option>
          <option value="organization">{t('pages:quotaPolicies.scopeOrganization')}</option>
        </select>
        <select
          aria-label={t('pages:quotaPolicies.filterByEnabled')}
          value={enabledFilter}
          onChange={onEnabledFilterChange}
          className={styles.filterSelect}
        >
          <option value="">{t('pages:quotaPolicies.allStatuses')}</option>
          <option value="true">{t('common:enabled')}</option>
          <option value="false">{t('common:disabled')}</option>
        </select>
      </ListFilterToolbar>

      <Card padding="none">
        <DataTable
          hideSearch
          frameless
          pageSize={pageLimit}
          columns={columns}
          data={rows}
          onRowClick={(r) => navigate(`/ai-gateway/quota-policies/${r.id}`)}
          emptyMessage={t('pages:quotaPolicies.noPoliciesConfigured')}
        />
      </Card>

      <ListPagination variant="plain" offset={offset} limit={pageLimit} total={total} onOffsetChange={setOffset} onLimitChange={setPageLimit} />

      <AlertDialog
        open={!!deleting}
        onOpenChange={(open) => { if (!open) setDeleting(null); }}
        title={t('pages:quotaPolicies.deletePolicy')}
        description={t('pages:quotaPolicies.deleteConfirm')}
        confirmLabel={t('common:delete')}
        onConfirm={() => { if (deleting) deletePolicy(deleting.id); }}
        variant="danger"
      />
    </Stack>
  );
}
