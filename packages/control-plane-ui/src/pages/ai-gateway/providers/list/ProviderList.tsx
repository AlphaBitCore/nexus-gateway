import { useState, useCallback } from 'react';
import { useTranslation } from 'react-i18next';
import { useNavigate } from 'react-router-dom';
import { providerApi } from '@/api/services';
import { useApi } from '@/hooks/useApi';
import { useDebouncedValue } from '@/hooks/useDebouncedValue';
import { useMutation } from '@/hooks/useMutation';
import { usePermission } from '@/hooks/usePermission';
import {
  PageHeader, DataTable, ListFilterToolbar,
  AlertDialog, Skeleton, ErrorBanner, Button, Stack, Card,
  ListPagination, DEFAULT_ADMIN_LIST_PAGE_SIZE, type AdminListPageSize,
  ListEnabledSwitchCell,
  RowActions, RowActionIconButton, RowDeleteAction, OpenActionIcon,
} from '@/components/ui';
import type { DataTableColumn } from '@/components/ui';
import type { Provider } from '@/api/types';
import styles from './ProviderList.module.css';

export function ConfigProvidersPage() {
  const { t } = useTranslation();
  const navigate = useNavigate();
  const [search, setSearch] = useState('');
  const debouncedSearch = useDebouncedValue(search, 300);
  const [statusFilter, setStatusFilter] = useState('');
  const [offset, setOffset] = useState(0);
  const [pageLimit, setPageLimit] = useState<AdminListPageSize>(DEFAULT_ADMIN_LIST_PAGE_SIZE);
  const { data, loading, error, refetch } = useApi<{ data: Provider[]; total: number }>(
    () => {
      const params: Record<string, string> = {
        limit: String(pageLimit),
        offset: String(offset),
      };
      const q = debouncedSearch.trim();
      if (q) params.q = q;
      if (statusFilter === 'enabled') params.enabled = 'true';
      if (statusFilter === 'disabled') params.enabled = 'false';
      return providerApi.list(params);
    },
    ['admin', 'providers', 'list', debouncedSearch, statusFilter, offset, pageLimit],
  );
  const [deleting, setDeleting] = useState<Provider | null>(null);
  const canCreate = usePermission('provider:create');
  const canUpdate = usePermission('provider:update');
  const canDelete = usePermission('provider:delete');

  const { mutate: toggleProvider, loading: togglingProviderEnabled } = useMutation(
    (data: { id: string; enabled: boolean }) => providerApi.update(data.id, { enabled: data.enabled }),
    { onSuccess: () => refetch(), successMessage: t('pages:providers.providerUpdated') },
  );

  const { mutate: deleteProvider } = useMutation(
    (id: string) => providerApi.delete(id),
    {
      onSuccess: () => { setDeleting(null); refetch(); },
      successMessage: t('pages:providers.providerDeleted'),
    },
  );

  const rows = data?.data ?? [];
  const total = data?.total ?? 0;

  const onSearchChange = useCallback((v: string) => {
    setSearch(v);
    setOffset(0);
  }, []);

  const onStatusFilterChange = useCallback((e: React.ChangeEvent<HTMLSelectElement>) => {
    setStatusFilter(e.target.value);
    setOffset(0);
  }, []);

  if (loading) return <Skeleton.ListPageSkeleton />;
  if (error) return <ErrorBanner message={error.message} onRetry={refetch} />;

  const columns: DataTableColumn<Provider>[] = [
    { key: 'name', label: t('pages:providers.nameCol', 'Name') },
    { key: 'displayName', label: t('pages:providers.displayNameCol', 'Display Name') },
    {
      key: 'adapterType',
      label: t('pages:providers.typeCol', 'Type'),
      render: (r) => <>{t(`pages:providers.adapterOption_${r.adapterType}`, r.adapterType)}</>,
    },
    { key: 'baseUrl', label: t('pages:providers.baseUrlCol', 'Base URL') },
    {
      key: 'enabled',
      label: t('pages:providers.statusCol', 'Status'),
      render: (r) => (
        <ListEnabledSwitchCell
          enabled={r.enabled}
          canToggle={canUpdate}
          toggleDisabled={togglingProviderEnabled}
          ariaLabel={t('common:listToggleEnabledAria', { name: r.displayName || r.name })}
          onToggle={(enabled) => { void toggleProvider({ id: r.id, enabled }); }}
        />
      ),
    },
    {
      key: 'actions',
      label: t('pages:providers.actionsCol', 'Actions'),
      render: (r) => (
        <RowActions>
          {canUpdate && (
            <RowActionIconButton label={t('common:edit')} onAction={() => navigate(`/ai-gateway/providers/${r.id}`)}>
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
        title={t('pages:providers.title')}
        subtitle={t('pages:providers.subtitle')}
        action={
          canCreate ? (
            <Button onClick={() => navigate('/ai-gateway/providers/new')}>{t('pages:providers.createProvider')}</Button>
          ) : undefined
        }
      />

      <ListFilterToolbar
        variant="boxed"
        searchPlaceholder={t('pages:providers.searchPlaceholder')}
        searchValue={search}
        onSearchChange={onSearchChange}
        meta={
          total === 0
            ? t('pages:providers.noMatch', 'No providers match the current filters')
            : t('pages:providers.showingMeta', 'Showing {{count}} provider(s) on this page · {{total}} total matching', { count: rows.length, total: total.toLocaleString() })
        }
      >
        <select
          aria-label={t('pages:providers.filterByStatus')}
          value={statusFilter}
          onChange={onStatusFilterChange}
          className={styles.filterSelect}
        >
          <option value="">{t('pages:providers.allStatuses', 'All statuses')}</option>
          <option value="enabled">{t('common:enabled')}</option>
          <option value="disabled">{t('common:disabled')}</option>
        </select>
      </ListFilterToolbar>

      <Card data-testid="providers-table" padding="none">
        <DataTable
          hideSearch
          frameless
          pageSize={pageLimit}
          onRowClick={(row) => navigate(`/ai-gateway/providers/${row.id}`)}
          columns={columns}
          data={rows}
          emptyMessage={t('pages:providers.noProviders')}
        />
      </Card>

      <ListPagination variant="plain" offset={offset} limit={pageLimit} total={total} onOffsetChange={setOffset} onLimitChange={setPageLimit} />

      <AlertDialog
        open={!!deleting}
        onOpenChange={(open) => { if (!open) setDeleting(null); }}
        title={t('pages:providers.deleteProvider', 'Delete Provider')}
        description={t('pages:providers.deleteConfirm', 'Are you sure you want to delete provider "{{name}}"? This action cannot be undone.', { name: deleting?.displayName || deleting?.name })}
        confirmLabel={t('common:delete')}
        onConfirm={() => { if (deleting) deleteProvider(deleting.id); }}
        variant="danger"
      />
    </Stack>
  );
}
