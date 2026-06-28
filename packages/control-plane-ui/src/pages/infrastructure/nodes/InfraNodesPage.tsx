import { useState, useCallback } from 'react';
import { useNavigate } from 'react-router-dom';
import { useTranslation } from 'react-i18next';
import { useApi } from '@/hooks/useApi';
import { useDebouncedValue } from '@/hooks/useDebouncedValue';
import { hubApi } from '@/api/services/infrastructure/nodes/hub';
import type { Node } from '@/api/services/infrastructure/nodes/hub';
import { thingStatusVariant } from '@/lib/thingStatus';
import type { AdminListPageSize, DataTableColumn } from '@/components/ui';
import { DEFAULT_ADMIN_LIST_PAGE_SIZE } from '@/constants/admin-api';
import {
  PageHeader, DataTable, Badge, Button, Stack, Card, Skeleton, ErrorBanner,
  Tabs, TabsList, TabsTrigger, TabsContent, ListPagination, ListFilterToolbar, Tooltip,
} from '@/components/ui';
import styles from './InfraNodesPage.module.css';

const NODE_TYPES = ['all', 'ai-gateway', 'compliance-proxy', 'control-plane', 'nexus-hub', 'agent'] as const;

const STATUS_OPTIONS = [
  'online', 'offline', 'enrolled', 'revoked', 'drift',
] as const;

function statusKey(s: string): string {
  return s.split('-').map(w => w.charAt(0).toUpperCase() + w.slice(1)).join('');
}

// Count keys in appliedOutcomes whose latest attempt failed. Hub clears
// applyError on the next successful apply, so any non-null entry here
// is currently-active — exactly what an operator should see on the list.
function applyErrorCount(node: Node): number {
  const outcomes = node.appliedOutcomes;
  if (!outcomes) return 0;
  let n = 0;
  for (const key of Object.keys(outcomes)) {
    if (outcomes[key]?.applyError) n++;
  }
  return n;
}

function SyncIndicator({
  target,
  applied,
  errorCount,
  t,
}: {
  target: number;
  applied: number;
  errorCount: number;
  t: (k: string, opts?: Record<string, unknown>) => string;
}) {
  // The version-equality check still drives the headline "in sync / out of
  // sync" badge — operators learn it from the desired/reported version pair
  // every day, and changing the meaning would break their muscle memory.
  // Apply errors layer on top: a node can be "in sync" by version but have
  // a stale ledger entry from before the operator fixed a bad payload.
  const inSync = target === applied;
  const hasError = errorCount > 0;
  return (
    <span
      className={styles.syncBadge}
      data-sync={inSync ? 'ok' : 'out-of-sync'}
      data-apply-error={hasError ? 'true' : 'false'}
      title={hasError ? t('pages:infrastructure.applyErrorsBadgeTooltip', { count: errorCount }) : undefined}
    >
      {inSync ? (
        <>
          <svg width="14" height="14" viewBox="0 0 24 24" fill="none" stroke="currentColor" strokeWidth="2.5" strokeLinecap="round" strokeLinejoin="round">
            <polyline points="20 6 9 17 4 12" />
          </svg>
          {t('pages:infrastructure.inSync')}
        </>
      ) : (
        <>
          <svg width="14" height="14" viewBox="0 0 24 24" fill="none" stroke="currentColor" strokeWidth="2.5" strokeLinecap="round" strokeLinejoin="round">
            <path d="M10.29 3.86L1.82 18a2 2 0 001.71 3h16.94a2 2 0 001.71-3L13.71 3.86a2 2 0 00-3.42 0z" />
            <line x1="12" y1="9" x2="12" y2="13" />
            <line x1="12" y1="17" x2="12.01" y2="17" />
          </svg>
          {t('pages:infrastructure.outOfSync')}
        </>
      )}
      {hasError && (
        <span className={styles.applyErrorBadge} aria-label={t('pages:infrastructure.applyErrorsBadgeTooltip', { count: errorCount })}>
          <svg width="12" height="12" viewBox="0 0 24 24" fill="none" stroke="currentColor" strokeWidth="2.5" strokeLinecap="round" strokeLinejoin="round">
            <circle cx="12" cy="12" r="10" />
            <line x1="12" y1="8" x2="12" y2="12" />
            <line x1="12" y1="16" x2="12.01" y2="16" />
          </svg>
          {errorCount}
        </span>
      )}
    </span>
  );
}

export default function InfraNodesPage() {
  const { t } = useTranslation();
  const navigate = useNavigate();
  const [selectedType, setSelectedType] = useState<string>('all');
  const [hasOverrides, setHasOverrides] = useState<boolean>(false);
  const [page, setPage] = useState(1);
  const [pageSize, setPageSize] = useState<AdminListPageSize>(DEFAULT_ADMIN_LIST_PAGE_SIZE);
  // P0 filters
  const [searchInput, setSearchInput] = useState('');
  const debouncedSearch = useDebouncedValue(searchInput, 300);
  const [statusFilter, setStatusFilter] = useState('');

  const resetPage = useCallback(() => setPage(1), []);

  const handleTypeChange = useCallback((type: string) => {
    setSelectedType(type);
    setPage(1);
  }, []);

  const toggleHasOverrides = useCallback(() => {
    setHasOverrides((prev) => !prev);
    setPage(1);
  }, []);

  const { data, loading, error, refetch } = useApi(
    () => hubApi.listNodes({
      type: selectedType !== 'all' ? selectedType : undefined,
      status: statusFilter || undefined,
      search: debouncedSearch || undefined,
      hasOverrides: hasOverrides ? true : undefined,
      page,
      pageSize,
    }),
    ['admin', 'nodes', selectedType, statusFilter, debouncedSearch, page, pageSize, hasOverrides],
  );

  const onRowClick = useCallback(
    (row: Node) => navigate(`/infrastructure/nodes/${row.id}`),
    [navigate],
  );

  const nodes = data?.nodes ?? [];
  const total = data?.total ?? 0;

  const columns: DataTableColumn<Node>[] = [
    {
      key: 'name',
      label: t('pages:infrastructure.colName'),
      render: (r) => {
        // Primary line = hostname (human-readable) when present, else
        // the admin-set display name, else thing.id. Secondary line =
        // short physical_id digest for agents (8 hex chars), or thing.id
        // for services (also stable + human-meaningful). Two-line cell
        // lets ops scan identity without opening detail.
        const primary = r.hostname || r.name || r.id;
        const idSummary = r.physicalId ? r.physicalId.slice(0, 12) + '\u2026' : r.id;
        return (
          <span className={styles.nameCell}>
            <span className={styles.nodeName}>{primary}</span>
            <span className={styles.nodeId}>
              <code>{idSummary}</code>
              {r.physicalId && <span className={styles.idMeta}>{' \u00b7 '}{t('pages:infrastructure.physicalId')}</span>}
            </span>
          </span>
        );
      },
    },
    { key: 'type', label: t('pages:infrastructure.colType'), render: (r) => <Badge variant="outline">{r.type}</Badge> },
    {
      key: 'primaryIp',
      label: t('pages:infrastructure.colIp'),
      sortable: false,
      render: (r) => r.primaryIp ? <code>{r.primaryIp}</code> : (r.listen_address ? <code>{r.listen_address}</code> : <span className={styles.dim}>{'\u2014'}</span>),
    },
    { key: 'status', label: t('pages:infrastructure.colStatus'), render: (r) => <Badge variant={thingStatusVariant(r.status)}>{r.status}</Badge> },
    { key: 'version', label: t('pages:infrastructure.colVersion'), render: (r) => r.version ?? '\u2014' },
    {
      key: 'sync',
      label: t('pages:infrastructure.colSync'),
      sortable: false,
      render: (r) => (
        <SyncIndicator
          target={r.targetVersion}
          applied={r.appliedVersion}
          errorCount={applyErrorCount(r)}
          t={t}
        />
      ),
    },
    {
      key: 'last_seen_at',
      label: t('pages:infrastructure.colLastSeen'),
      render: (r) => r.last_seen_at ? new Date(r.last_seen_at).toLocaleString() : '\u2014',
    },
    {
      key: 'details',
      label: t('common:actions'),
      sortable: false,
      cellClassName: styles.detailActionCell,
      cellStyle: { width: 56 },
      render: (r) => (
        <Tooltip content={t('pages:infrastructure.nodeDetail')} side="left" delayDuration={200}>
          <button
            type="button"
            className={styles.detailActionButton}
            aria-label={t('pages:infrastructure.nodeDetail')}
            onClick={(event) => {
              event.stopPropagation();
              onRowClick(r);
            }}
          >
            <svg viewBox="0 0 24 24" fill="none" stroke="currentColor" strokeWidth="2" strokeLinecap="round" strokeLinejoin="round" aria-hidden="true">
              <path d="M9 18l6-6-6-6" />
            </svg>
          </button>
        </Tooltip>
      ),
    },
  ];

  if (loading && !data) return <Skeleton.ListPageSkeleton />;

  return (
    <Stack gap="lg">
      <PageHeader
        title={t('pages:infrastructure.nodesTitle')}
        subtitle={t('pages:infrastructure.nodesDescription')}
        action={
          <Button variant="primary" size="sm" className={styles.refreshButton} onClick={refetch}>
            {t('pages:infrastructure.refresh')}
          </Button>
        }
      />

      {error && <ErrorBanner error={error} onRetry={refetch} />}

      <ListFilterToolbar
        variant="boxed"
        searchWidth={420}
        hideClearButton
        searchPlaceholder={t('pages:infrastructure.searchNodesPlaceholder')}
        searchValue={searchInput}
        onSearchChange={(v) => { setSearchInput(v); resetPage(); }}
      >
        <select
          aria-label={t('pages:infrastructure.filterStatus')}
          value={statusFilter}
          onChange={(e) => { setStatusFilter(e.target.value); resetPage(); }}
          className={styles.filterSelect}
        >
          <option value="">{t('pages:infrastructure.filterAllStatuses')}</option>
          {STATUS_OPTIONS.map((s) => (
            <option key={s} value={s}>{t(`pages:infrastructure.filterStatus${statusKey(s)}`)}</option>
          ))}
        </select>
        <button
          type="button"
          className={styles.filterToggle}
          data-active={hasOverrides ? 'true' : 'false'}
          aria-pressed={hasOverrides}
          onClick={toggleHasOverrides}
        >
          {t('pages:infrastructure.hasOverridesFilter')}
        </button>
      </ListFilterToolbar>

      <Tabs value={selectedType} onValueChange={handleTypeChange}>
        <div className={styles.tabBar}>
          <TabsList className={styles.tabsList}>
            {NODE_TYPES.map((tp) => (
              <TabsTrigger key={tp} value={tp}>
                {tp === 'all' ? t('pages:infrastructure.filterAll') : tp}
              </TabsTrigger>
            ))}
          </TabsList>
        </div>

        <div className={styles.listCount}>
          {t('pages:infrastructure.totalNodes', { count: total })}
        </div>

        {NODE_TYPES.map((tp) => (
          <TabsContent key={tp} value={tp} className={styles.tabContent}>
            <Card padding="none">
              <DataTable
                className={styles.nodesTable}
                columns={columns}
                data={nodes}
                onRowClick={onRowClick}
                emptyMessage={t('pages:infrastructure.noNodes')}
                loading={loading}
                hideSearch
                frameless
                pageSize={pageSize}
                getRowProps={(r) =>
                  r.hasKillswitchBypass
                    ? { 'data-killswitch-bypass': 'true' }
                    : {}
                }
              />
            </Card>
          </TabsContent>
        ))}
      </Tabs>

      {total > 0 ? (
        <ListPagination
          offset={(page - 1) * pageSize}
          limit={pageSize}
          total={total}
          onOffsetChange={(nextOffset) => setPage(Math.floor(nextOffset / pageSize) + 1)}
          onLimitChange={(next) => {
            setPageSize(next);
            setPage(1);
          }}
        />
      ) : null}
    </Stack>
  );
}
