import { useState, useCallback } from 'react';
import { useNavigate } from 'react-router-dom';
import { useTranslation } from 'react-i18next';
import { ShadcnButton } from '@nexus-gateway/ui-shared';
import { useApi } from '@/hooks/useApi';
import { useAuth } from '@/auth/context/AuthContext';
import { useDebouncedValue } from '@/hooks/useDebouncedValue';
import { usePermission } from '@/hooks/usePermission';
import { devicesApi } from '@/api/services';
import type { AgentDevice } from '@/api/types';
import type { DataTableColumn, AdminListPageSize } from '@/components/ui';
import {
  PageHeader, DataTable, ListFilterToolbar, Badge,
  Stack, Card, Skeleton, ErrorBanner, ListPagination,
  DEFAULT_ADMIN_LIST_PAGE_SIZE,
} from '@/components/ui';
import { EnrollTokenDialog } from './EnrollTokenDialog';
import { statusVariant } from './utils';
import { cn } from '@/lib/utils';

export function DeviceListPage() {
  const { t } = useTranslation();
  const navigate = useNavigate();
  const [search, setSearch] = useState('');
  const debouncedSearch = useDebouncedValue(search, 300);
  const [offset, setOffset] = useState(0);
  const [pageLimit, setPageLimit] = useState<AdminListPageSize>(DEFAULT_ADMIN_LIST_PAGE_SIZE);
  const [statusFilter, setStatusFilter] = useState('');
  const [osFilter, setOsFilter] = useState('');
  const [enrollOpen, setEnrollOpen] = useState(false);
  const canCreate = usePermission('devices:create');
  // Scope hint: when the caller isn't in the super-admin role, their
  // `agent-device:list` may resolve to a group-scoped NRN set — the
  // returned list is the intersection of every policy's allowed groups.
  // We can't introspect that filter on the client without a new backend
  // endpoint, so surface a passive note pointing at the most common
  // cause of "I expected more devices".
  const { roles } = useAuth();
  const isScopedRole = !roles.includes('super-admins');

  const { data, loading, error, refetch } = useApi(
    () => {
      const params: Record<string, string> = { limit: String(pageLimit), offset: String(offset) };
      const q = debouncedSearch.trim();
      if (q) params.q = q;
      if (statusFilter) params.status = statusFilter;
      if (osFilter) params.os = osFilter;
      return devicesApi.list(params);
    },
    ['admin', 'devices', 'list', debouncedSearch, String(offset), String(pageLimit), statusFilter, osFilter],
  );

  const onSearchChange = useCallback((v: string) => { setSearch(v); setOffset(0); }, []);

  if (loading && !data) return <Skeleton.ListPageSkeleton />;
  if (error) return <ErrorBanner message={error.message} onRetry={refetch} />;

  const items = data?.data ?? [];
  const total = data?.total ?? 0;

  const columns: DataTableColumn<AgentDevice>[] = [
    {
      key: 'hostname',
      label: t('pages:devices.colDevice'),
      render: (r) => {
        // Two-line cell: hostname on top, short physical_id digest below
        // so an admin can scan identity without opening detail.
        const idSummary = r.physicalId ? r.physicalId.slice(0, 12) + '\u2026' : r.id.slice(0, 12) + '\u2026';
        return (
          <span>
            <div style={{ fontWeight: 'var(--g-font-weight-medium)' }}>{r.hostname}</div>
            <div style={{ fontSize: 'var(--g-font-size-xs)', color: 'var(--color-text-muted)', fontFamily: 'monospace' }}>
              {idSummary}
            </div>
          </span>
        );
      },
    },
    {
      key: 'boundUser',
      label: t('pages:devices.colBoundUser'),
      sortable: false,
      render: (r) =>
        r.boundUserDisplayName
          ? <span title={r.boundUserEmail ?? ''}>{r.boundUserDisplayName}</span>
          : <span style={{ color: 'var(--color-text-muted)' }}>{'\u2014'}</span>,
    },
    { key: 'os', label: t('pages:devices.os'), render: (r) => r.os === 'darwin' ? 'macOS' : r.os === 'windows' ? 'Windows' : r.os },
    {
      key: 'primaryIp',
      label: t('pages:devices.colIp'),
      sortable: false,
      render: (r) => r.primaryIp ? <code>{r.primaryIp}</code> : <span style={{ color: 'var(--color-text-muted)' }}>{'\u2014'}</span>,
    },
    { key: 'agentVersion', label: t('pages:devices.agentVersion') },
    { key: 'status', label: t('pages:devices.status'), render: (r) => <Badge variant={statusVariant(r.status)}>{r.status}</Badge> },
    { key: 'lastHeartbeat', label: t('pages:devices.lastHeartbeat'), render: (r) => r.lastHeartbeat ? new Date(r.lastHeartbeat).toLocaleString() : '\u2014' },
  ];

  return (
    <Stack gap="md">
      <PageHeader
        title={t('pages:devices.title')}
        subtitle={t('pages:devices.subtitle')}
        action={canCreate ? <ShadcnButton type="button" onClick={() => setEnrollOpen(true)}>{t('pages:devices.enrollDevice')}</ShadcnButton> : undefined}
      />
      {isScopedRole && (
        <div
          role="note"
          className={cn(
            'rounded-md border border-border bg-muted px-3 py-2 text-xs text-muted-foreground',
          )}
        >
          {t(
            'pages:devices.scopeHint',
            'Your view of devices may be filtered by your IAM policy. If expected devices are missing, ask an administrator to verify your group-scoped grants.',
          )}
        </div>
      )}
      <Card>
        <ListFilterToolbar searchPlaceholder={t('common:search')} searchValue={search} onSearchChange={onSearchChange}>
          <select
            value={statusFilter}
            onChange={(e) => { setStatusFilter(e.target.value); setOffset(0); }}
            className={cn(
              'h-9 min-w-[148px] cursor-pointer rounded-md border border-input bg-background px-3 text-sm text-foreground shadow-xs',
            )}
          >
            <option value="">{t('pages:devices.allStatuses')}</option>
            <option value="ACTIVE">ACTIVE</option>
            <option value="ENROLLED">ENROLLED</option>
            <option value="OFFLINE">OFFLINE</option>
            <option value="REVOKED">REVOKED</option>
          </select>
          <select
            value={osFilter}
            onChange={(e) => { setOsFilter(e.target.value); setOffset(0); }}
            className={cn(
              'h-9 min-w-[148px] cursor-pointer rounded-md border border-input bg-background px-3 text-sm text-foreground shadow-xs',
            )}
          >
            <option value="">{t('pages:devices.allOS')}</option>
            <option value="darwin">macOS</option>
            <option value="windows">Windows</option>
          </select>
        </ListFilterToolbar>
        <DataTable columns={columns} data={items} onRowClick={(r) => navigate(`/devices/${r.id}`)} hideSearch />
        <ListPagination total={total} offset={offset} limit={pageLimit} onOffsetChange={setOffset} onLimitChange={setPageLimit} />
      </Card>
      <EnrollTokenDialog open={enrollOpen} onOpenChange={setEnrollOpen} />
    </Stack>
  );
}
