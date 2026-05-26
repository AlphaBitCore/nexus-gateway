import { useState, useCallback } from 'react';
import { useNavigate } from 'react-router-dom';
import { useTranslation } from 'react-i18next';
import { useApi } from '@/hooks/useApi';
import { useDebouncedValue } from '@/hooks/useDebouncedValue';
import { fleetApi } from '@/api/services';
import type { AgentUserSafe } from '@/api/types';
import type { DataTableColumn, AdminListPageSize } from '@/components/ui';
import {
  PageHeader, DataTable, ListFilterToolbar, Badge,
  Stack, Card, Skeleton, ErrorBanner, ListPagination,
  DEFAULT_ADMIN_LIST_PAGE_SIZE,
} from '@/components/ui';
import styles from './FleetUserListPage.module.css';

export function FleetUserListPage() {
  const { t } = useTranslation();
  const navigate = useNavigate();
  const [search, setSearch] = useState('');
  const debouncedSearch = useDebouncedValue(search, 300);
  const [offset, setOffset] = useState(0);
  const [pageLimit, setPageLimit] = useState<AdminListPageSize>(DEFAULT_ADMIN_LIST_PAGE_SIZE);
  const [statusFilter, setStatusFilter] = useState('');

  const { data, loading, error, refetch } = useApi(
    () => {
      const params: Record<string, string> = { limit: String(pageLimit), offset: String(offset) };
      const q = debouncedSearch.trim();
      if (q) params.q = q;
      if (statusFilter) params.enabled = statusFilter === 'active' ? 'true' : 'false';
      return fleetApi.listAgentUsers(params);
    },
    ['admin', 'agent-users', 'list', debouncedSearch, String(offset), String(pageLimit), statusFilter],
  );

  const onSearchChange = useCallback((v: string) => { setSearch(v); setOffset(0); }, []);

  if (loading && !data) return <Skeleton.ListPageSkeleton />;
  if (error) return <ErrorBanner message={error.message} onRetry={refetch} />;

  const items = data?.data ?? [];
  const total = data?.total ?? 0;

  const columns: DataTableColumn<AgentUserSafe>[] = [
    { key: 'displayName', label: t('pages:fleet.displayName') },
    { key: 'email', label: 'Email', render: (r) => r.email ?? '—' },
    { key: 'status', label: t('pages:fleet.status'), render: (r) => (
      <Badge variant={r.status === 'active' ? 'success' : r.status === 'suspended' ? 'warning' : 'default'}>{r.status}</Badge>
    )},
    { key: 'createdAt', label: t('pages:fleet.createdAt'), render: (r) => new Date(r.createdAt).toLocaleDateString() },
  ];

  return (
    <Stack gap="md">
      <PageHeader title={t('pages:fleet.usersTitle')} subtitle={t('pages:fleet.usersSubtitle')} />
      <Card>
        <ListFilterToolbar searchPlaceholder={t('common:search')} searchValue={search} onSearchChange={onSearchChange}>
          <select value={statusFilter} onChange={e => { setStatusFilter(e.target.value); setOffset(0); }} className={styles.filterSelect}>
            <option value="">{t('pages:fleet.allStatuses')}</option>
            <option value="active">{t('pages:fleet.active')}</option>
            <option value="suspended">{t('pages:fleet.suspended')}</option>
          </select>
        </ListFilterToolbar>
        <DataTable columns={columns} data={items} onRowClick={(r) => navigate(`/fleet/users/${r.id}`)} hideSearch />
        <ListPagination total={total} offset={offset} limit={pageLimit} onOffsetChange={setOffset} onLimitChange={setPageLimit} />
      </Card>
    </Stack>
  );
}
