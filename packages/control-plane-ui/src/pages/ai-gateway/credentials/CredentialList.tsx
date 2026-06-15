import { useState, useMemo, useCallback } from 'react';
import { useNavigate } from 'react-router-dom';
import { useTranslation } from 'react-i18next';
import clsx from 'clsx';
import { useApi } from '@/hooks/useApi';
import { credentialApi, providerApi } from '@/api/services';
import { useDebouncedValue } from '@/hooks/useDebouncedValue';
import { useMutation } from '@/hooks/useMutation';
import {
  PageHeader, DataTable, ListFilterToolbar,
  AlertDialog, Skeleton, ErrorBanner, Button, Stack, Card,
  ListPagination, DEFAULT_ADMIN_LIST_PAGE_SIZE, type AdminListPageSize,
  ListEnabledSwitchCell,
  RowActions, RowDeleteAction,
} from '@/components/ui';
import { Badge } from '@/components/ui';
import type { DataTableColumn } from '@/components/ui';
import type { Provider, Credential } from '@/api/types';
import { ADMIN_LIST_FULL_PAGE_PARAMS } from '@/constants/admin-api';
import { formatDateTime } from '@/lib/format';
import { ReliabilityCell } from './ReliabilityCell';
import styles from './CredentialList.module.css';

function relativeTime(dateStr: string): string {
  const now = Date.now();
  const then = new Date(dateStr).getTime();
  const diff = now - then;
  const seconds = Math.floor(diff / 1000);
  if (seconds < 60) return 'just now';
  const minutes = Math.floor(seconds / 60);
  if (minutes < 60) return `${minutes}m ago`;
  const hours = Math.floor(minutes / 60);
  if (hours < 24) return `${hours}h ago`;
  const days = Math.floor(hours / 24);
  if (days < 30) return `${days}d ago`;
  const months = Math.floor(days / 30);
  return `${months}mo ago`;
}

function poolStatusClass(status: string | undefined, s: Record<string, string>): string {
  switch (status) {
    case 'retiring': return s.poolStatusRetiring;
    case 'retired': return s.poolStatusRetired;
    default: return s.poolStatusActive;
  }
}


export function CredentialListPage() {
  const { t } = useTranslation();
  const navigate = useNavigate();
  const [search, setSearch] = useState('');
  const debouncedSearch = useDebouncedValue(search, 300);
  const [providerFilter, setProviderFilter] = useState('');
  const [enabledFilter, setEnabledFilter] = useState('');
  const [offset, setOffset] = useState(0);
  const [pageLimit, setPageLimit] = useState<AdminListPageSize>(DEFAULT_ADMIN_LIST_PAGE_SIZE);

  const { data: providersData } = useApi<{ data: Provider[]; total: number }>(
    () => providerApi.list({ ...ADMIN_LIST_FULL_PAGE_PARAMS }),
    ['admin', 'providers', 'list', 'credential-list'],
  );

  const providerOptions = useMemo(() => {
    const list = providersData?.data ?? [];
    return [...list].sort((a, b) => (a.displayName || a.name).localeCompare(b.displayName || b.name));
  }, [providersData?.data]);

  const { data, loading, error, refetch } = useApi<{ data: Credential[]; total: number }>(
    () => {
      const params: Record<string, string> = {
        limit: String(pageLimit),
        offset: String(offset),
      };
      const q = debouncedSearch.trim();
      if (q) params.q = q;
      if (providerFilter) params.providerId = providerFilter;
      if (enabledFilter === 'enabled') params.enabled = 'true';
      if (enabledFilter === 'disabled') params.enabled = 'false';
      return credentialApi.list(params);
    },
    ['admin', 'credentials', 'list', debouncedSearch, providerFilter, enabledFilter, offset, pageLimit],
  );

  const providerNameMap = useMemo(() => {
    const map = new Map<string, string>();
    for (const p of providersData?.data ?? []) {
      map.set(p.id, p.displayName?.trim() || p.name);
    }
    return map;
  }, [providersData?.data]);

  const [deleting, setDeleting] = useState<Credential | null>(null);

  const { mutate: toggleCredential, loading: togglingCredentialEnabled } = useMutation(
    (payload: { id: string; enabled: boolean }) => credentialApi.update(payload.id, { enabled: payload.enabled }),
    { invalidateQueries: [['api', 'admin', 'credentials']], successMessage: 'Credential updated' },
  );

  const { mutate: deleteCredential } = useMutation(
    (id: string) => credentialApi.delete(id),
    {
      invalidateQueries: [['api', 'admin', 'credentials']],
      onSuccess: () => { setDeleting(null); },
      successMessage: 'Credential deleted',
    },
  );

  const rows = data?.data ?? [];
  const total = data?.total ?? 0;

  const onSearchChange = useCallback((v: string) => {
    setSearch(v);
    setOffset(0);
  }, []);

  const onProviderFilterChange = useCallback((e: React.ChangeEvent<HTMLSelectElement>) => {
    setProviderFilter(e.target.value);
    setOffset(0);
  }, []);

  const onEnabledFilterChange = useCallback((e: React.ChangeEvent<HTMLSelectElement>) => {
    setEnabledFilter(e.target.value);
    setOffset(0);
  }, []);

  if (loading) return <Skeleton.ListPageSkeleton />;
  if (error) return <ErrorBanner message={error.message} onRetry={refetch} />;

  const columns: DataTableColumn<Credential>[] = [
    { key: 'name', label: t('pages:credentials.name') },
    {
      key: 'providerId', label: t('pages:credentials.provider'),
      render: (r) => <span>{providerNameMap.get(r.providerId) ?? r.providerId}</span>,
    },
    {
      key: 'enabled',
      label: t('pages:credentials.status'),
      render: (r) => (
        <ListEnabledSwitchCell
          enabled={r.enabled}
          canToggle
          toggleDisabled={togglingCredentialEnabled}
          ariaLabel={t('common:listToggleEnabledAria', { name: r.name })}
          onToggle={(enabled) => { void toggleCredential({ id: r.id, enabled }); }}
        />
      ),
    },
    {
      key: 'status', label: t('pages:credentials.poolStatus'),
      render: (r) => {
        const status = r.status ?? 'active';
        return (
          <span className={clsx(styles.poolStatusBadge, poolStatusClass(status, styles))}>
            {t(`pages:credentials.poolStatus_${status}`, { defaultValue: status })}
          </span>
        );
      },
    },
    {
      // Unified Reliability column. Badge color reflects worst-of state;
      // hover reveals the full 5m / 1h / dominant-error / trend breakdown.
      key: 'reliability',
      label: t('pages:credentials.reliability'),
      tooltip: t('pages:credentials.reliabilityColumnTooltip'),
      render: (r) => <ReliabilityCell cred={r} />,
    },
    {
      key: 'expiresAt', label: t('pages:credentials.expires'),
      render: (r) => {
        if (!r.expiresAt) return <span className={styles.mutedText}>—</span>;
        const expired = new Date(r.expiresAt) < new Date();
        return (
          <span title={formatDateTime(r.expiresAt)} className={styles.cursorDefault}>
            {formatDateTime(r.expiresAt)}
            {' '}
            {expired ? (
              <Badge variant="danger">{t('pages:credentials.expiresOverdue')}</Badge>
            ) : r.rotationState === 'pending_rotation' ? (
              <Badge variant="warning">{t('pages:credentials.expiringSoon')}</Badge>
            ) : null}
          </span>
        );
      },
    },
    {
      key: 'lastUsedAt', label: t('pages:credentials.lastUsed'),
      render: (r) => r.lastUsedAt ? (
        <span title={formatDateTime(r.lastUsedAt)} className={styles.cursorDefault}>
          {relativeTime(r.lastUsedAt)}
        </span>
      ) : (
        <span className={styles.mutedText}>{t('pages:credentials.never')}</span>
      ),
    },
    {
      key: 'actions',
      label: t('pages:credentials.actions'),
      render: (r) => (
        <RowActions>
          <RowDeleteAction label={t('common:delete')} onAction={() => setDeleting(r)} />
        </RowActions>
      ),
    },
  ];

  return (
    <Stack gap="lg">
      <PageHeader
        title={t('pages:credentials.title')}
        subtitle={t('pages:credentials.subtitle')}
        action={
          <Button onClick={() => navigate('/ai-gateway/credentials/new')}>
            {t('pages:credentials.addCredential')}
          </Button>
        }
      />

      <ListFilterToolbar
        variant="boxed"
        searchPlaceholder={t('pages:credentials.searchPlaceholder')}
        searchValue={search}
        onSearchChange={onSearchChange}
        meta={
          total === 0
            ? t('pages:credentials.noCredentialsMatch')
            : t('pages:credentials.showingCredentials', { count: rows.length, total: total.toLocaleString() })
        }
      >
        <select aria-label={t('pages:credentials.filterByProvider')} value={providerFilter} onChange={onProviderFilterChange} className={styles.filterSelect}>
          <option value="">{t('pages:credentials.allProviders')}</option>
          {providerOptions.map(p => <option key={p.id} value={p.id}>{p.displayName?.trim() || p.name}</option>)}
        </select>
        <select aria-label={t('pages:credentials.filterByStatus')} value={enabledFilter} onChange={onEnabledFilterChange} className={styles.filterSelect}>
          <option value="">{t('pages:credentials.allStatuses')}</option>
          <option value="enabled">{t('common:enabled')}</option>
          <option value="disabled">{t('common:disabled')}</option>
        </select>
      </ListFilterToolbar>

      <Card padding="none">
        <DataTable<Credential>
          hideSearch
          frameless
          pageSize={pageLimit}
          columns={columns}
          data={rows}
          emptyMessage={t('pages:credentials.noCredentialsConfigured')}
          onRowClick={(r) => navigate(`/ai-gateway/credentials/${r.id}`)}
        />
      </Card>

      <ListPagination variant="plain" offset={offset} limit={pageLimit} total={total} onOffsetChange={setOffset} onLimitChange={setPageLimit} />

      <AlertDialog
        open={!!deleting}
        onOpenChange={(open) => { if (!open) setDeleting(null); }}
        title={t('pages:credentials.deleteCredential')}
        description={t('pages:credentials.deleteConfirm', { name: deleting?.name })}
        confirmLabel={t('common:delete')}
        onConfirm={() => { if (deleting) deleteCredential(deleting.id); }}
        variant="danger"
      />
    </Stack>
  );
}
