/**
 * InfraOverridesPage — global registry of every active per-Thing config
 * override. Read-only registry surface (no bulk mutation per spec §11
 * out-of-scope). Per-row actions: View (opens the owning node's
 * Configuration tab), Force resync, Clear, and Extend (Extend routes to
 * the detail Configuration tab where the editor drawer handles TTL edits;
 * the registry surface itself does not embed the editor).
 *
 * Backed by `GET /api/admin/things/overrides` via `hubApi.listGlobalOverrides`,
 * which returns `{overrides, total, summary}`. The summary tiles drive the
 * four header counters (nodes, keys, stale, expiring-within-1h).
 */
import { useState, useCallback, useMemo } from 'react';
import { useNavigate } from 'react-router-dom';
import { useTranslation } from 'react-i18next';
import { useApi } from '@/hooks/useApi';
import { hubApi } from '@/api/services/infrastructure/nodes/hub';
import type { GlobalOverrideRow } from '@/api/services/infrastructure/nodes/hub';
import {
  PageHeader, Stack, Badge, Button, ListPagination, Skeleton, ErrorBanner,
  DataTable, Tabs, TabsList, TabsTrigger, Tooltip,
} from '@/components/ui';
import type { AdminListPageSize, DataTableColumn } from '@/components/ui';
import { DEFAULT_ADMIN_LIST_PAGE_SIZE } from '@/constants/admin-api';
import { useToast } from '@/context/ToastContext';
import styles from './InfraOverridesPage.module.css';

/** Type filter chips. `all` is the default (no filter). */
const TYPE_FILTERS = ['all', 'ai-gateway', 'compliance-proxy', 'control-plane', 'nexus-hub', 'agent'] as const;
type TypeFilter = (typeof TYPE_FILTERS)[number];

/** Format an absolute ISO date as a short locale string for table cells. */
function formatTimestamp(iso?: string | null): string {
  if (!iso) return '—';
  const d = new Date(iso);
  if (Number.isNaN(d.getTime())) return '—';
  return d.toLocaleString();
}

/**
 * Returns true when an override is "expiring soon" — `expiresAt` is set
 * and falls within the next hour. Mirrors the server-side
 * `expiringSoonCount` summary computation, used to flag rows in the UI.
 */
function isExpiringSoon(expiresAt?: string): boolean {
  if (!expiresAt) return false;
  const t = new Date(expiresAt).getTime();
  if (Number.isNaN(t)) return false;
  const now = Date.now();
  return t > now && t - now <= 60 * 60 * 1000;
}

/**
 * Returns true if the row was set within the last 24 hours. Used by the
 * "Set in last 24h" client-side filter chip.
 */
function isRecent(setAt: string): boolean {
  const t = new Date(setAt).getTime();
  if (Number.isNaN(t)) return false;
  return Date.now() - t <= 24 * 60 * 60 * 1000;
}

/** Detect break-glass overrides: `emergencyOverride` flag OR killswitch key. */
function isBreakGlass(row: GlobalOverrideRow): boolean {
  return row.emergencyOverride || row.configKey === 'killswitch';
}

export default function InfraOverridesPage() {
  const { t } = useTranslation();
  const navigate = useNavigate();
  const { addToast } = useToast();

  const [selectedType, setSelectedType] = useState<TypeFilter>('all');
  const [hasTtl, setHasTtl] = useState<boolean>(false);
  const [stale, setStale] = useState<boolean>(false);
  const [recentOnly, setRecentOnly] = useState<boolean>(false);
  const [search, setSearch] = useState<string>('');
  const [page, setPage] = useState(1);
  const [pageSize, setPageSize] = useState<AdminListPageSize>(DEFAULT_ADMIN_LIST_PAGE_SIZE);

  const typeParam = selectedType === 'all' ? undefined : selectedType;

  // `recentOnly` and `search` are applied client-side after the fetch (the
  // server has no equivalents), so they stay OUT of the queryKey to avoid
  // a refetch + skeleton-flash on every keystroke. The remaining params
  // map 1:1 onto the server filter and belong in the cache key.
  const { data, loading, error, refetch } = useApi(
    () => hubApi.listGlobalOverrides({
      type: typeParam,
      hasTtl: hasTtl ? true : undefined,
      stale: stale ? true : undefined,
      limit: pageSize,
      offset: (page - 1) * pageSize,
    }),
    ['admin', 'global-overrides', selectedType, hasTtl, stale, page, pageSize],
  );

  const overrides = data?.overrides ?? [];
  const total = data?.total ?? 0;
  const summary = data?.summary ?? { totalNodes: 0, totalOverrides: 0, staleCount: 0, expiringSoonCount: 0 };

  /** Client-side filter pass: `recentOnly` (last 24h) and free-text search. */
  const filteredRows = useMemo(() => {
    const term = search.trim().toLowerCase();
    return overrides.filter((row) => {
      if (recentOnly && !isRecent(row.setAt)) return false;
      if (term) {
        const inName = row.nodeName.toLowerCase().includes(term);
        const inActor = row.setBy.toLowerCase().includes(term);
        const inKey = row.configKey.toLowerCase().includes(term);
        if (!inName && !inActor && !inKey) return false;
      }
      return true;
    });
  }, [overrides, recentOnly, search]);
  const hasClientFilters = search.trim().length > 0 || recentOnly;
  const visibleSummary = useMemo(() => {
    if (!hasClientFilters) return summary;
    const nodeIds = new Set(filteredRows.map((row) => row.nodeId));
    return {
      totalNodes: nodeIds.size,
      totalOverrides: filteredRows.length,
      staleCount: filteredRows.filter((row) => row.stale).length,
      expiringSoonCount: filteredRows.filter((row) => isExpiringSoon(row.expiresAt)).length,
    };
  }, [filteredRows, hasClientFilters, summary]);
  const visibleTotal = hasClientFilters ? filteredRows.length : total;

  const handleTypeChange = useCallback((next: TypeFilter) => {
    setSelectedType(next);
    setPage(1);
  }, []);

  const toggleHasTtl = useCallback(() => {
    setHasTtl((v) => !v);
    setPage(1);
  }, []);

  const toggleStale = useCallback(() => {
    setStale((v) => !v);
    setPage(1);
  }, []);

  const toggleRecent = useCallback(() => {
    setRecentOnly((v) => !v);
    // recent is client-side only; no need to reset page, but keep the UX
    // consistent with the other chips.
    setPage(1);
  }, []);

  const handleView = useCallback(
    (row: GlobalOverrideRow) => {
      navigate(`/infrastructure/nodes/${row.nodeId}#configuration`);
    },
    [navigate],
  );

  const handleClear = useCallback(
    async (row: GlobalOverrideRow) => {
      try {
        await hubApi.clearOverride(row.nodeId, row.configKey);
        addToast(t('pages:infrastructure.overrides.clearSuccess'), 'success');
        refetch();
      } catch (err) {
        const msg = err instanceof Error ? err.message : t('pages:infrastructure.overrides.mutationFailed');
        addToast(msg, 'error');
      }
    },
    [addToast, refetch, t],
  );

  const handleResync = useCallback(
    async (row: GlobalOverrideRow) => {
      try {
        await hubApi.resyncNodeAll(row.nodeId, { configKey: row.configKey });
        addToast(t('pages:infrastructure.overrides.resyncSuccess'), 'success');
      } catch (err) {
        const msg = err instanceof Error ? err.message : t('pages:infrastructure.overrides.mutationFailed');
        addToast(msg, 'error');
      }
    },
    [addToast, t],
  );

  const columns: DataTableColumn<GlobalOverrideRow>[] = [
    {
      key: 'node',
      label: t('pages:infrastructure.overrides.colNode'),
      render: (r) => (
        <span className={styles.nameCell}>
          <span className={styles.nodeName}>{r.nodeName}</span>
          <span className={styles.nodeId}>{r.nodeId}</span>
        </span>
      ),
    },
    {
      key: 'type',
      label: t('pages:infrastructure.overrides.colType'),
      render: (r) => <Badge variant="outline">{r.nodeType}</Badge>,
    },
    {
      key: 'configKey',
      label: t('pages:infrastructure.overrides.colKeys'),
      render: (r) => (
        <span className={styles.keysCell}>
          <Badge variant="info">{r.configKey}</Badge>
        </span>
      ),
    },
    {
      key: 'setBy',
      label: t('pages:infrastructure.overrides.colSetBy'),
      render: (r) => <span className={styles.actor}>{r.setBy}</span>,
    },
    {
      key: 'setExpires',
      label: t('pages:infrastructure.overrides.colSetExpires'),
      render: (r) => (
        <span className={styles.dim}>
          {formatTimestamp(r.setAt)}
          {' / '}
          {r.expiresAt
            ? formatTimestamp(r.expiresAt)
            : t('pages:infrastructure.overrides.permanentTtl')}
        </span>
      ),
    },
    {
      key: 'status',
      label: t('pages:infrastructure.overrides.colStatus'),
      render: (r) => {
        if (isBreakGlass(r)) {
          return (
            <Badge variant="danger" className={styles.statusBreakGlass}>
              {t('pages:infrastructure.overrides.status.breakGlass')}
            </Badge>
          );
        }
        if (r.stale) {
          return (
            <Badge variant="warning">
              {t('pages:infrastructure.overrides.status.staleBadge')}
            </Badge>
          );
        }
        if (isExpiringSoon(r.expiresAt)) {
          return (
            <Badge variant="warning">
              {t('pages:infrastructure.overrides.status.expiresIn', {
                when: formatTimestamp(r.expiresAt),
              })}
            </Badge>
          );
        }
        return <Badge variant="success">{t('pages:infrastructure.overrides.status.ok')}</Badge>;
      },
    },
    {
      key: 'actions',
      label: t('pages:infrastructure.overrides.colActions'),
      sortable: false,
      render: (r) => (
        <span className={styles.actionsCell} onClick={(e) => e.stopPropagation()}>
          <Button variant="ghost" size="sm" onClick={() => handleView(r)}>
            {t('pages:infrastructure.overrides.actions.view')}
          </Button>
          <Button variant="ghost" size="sm" onClick={() => handleResync(r)}>
            {t('pages:infrastructure.overrides.actions.forceResync')}
          </Button>
          <Button variant="ghost" size="sm" onClick={() => handleClear(r)}>
            {t('pages:infrastructure.overrides.actions.clear')}
          </Button>
          {isExpiringSoon(r.expiresAt) && (
            <Button variant="ghost" size="sm" onClick={() => handleView(r)}>
              {t('pages:infrastructure.overrides.actions.extend')}
            </Button>
          )}
        </span>
      ),
    },
    {
      key: 'details',
      label: t('pages:infrastructure.overrides.colActions'),
      sortable: false,
      cellClassName: styles.detailActionCell,
      cellStyle: { width: 56 },
      render: (r) => (
        <Tooltip content={t('pages:infrastructure.overrides.actions.view')} side="left" delayDuration={200}>
          <button
            type="button"
            className={styles.detailActionButton}
            aria-label={t('pages:infrastructure.overrides.actions.view')}
            onClick={(event) => {
              event.stopPropagation();
              handleView(r);
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
        title={t('pages:infrastructure.overrides.title')}
        subtitle={t('pages:infrastructure.overrides.description')}
        action={
          <Button variant="secondary" size="sm" onClick={refetch}>
            {t('pages:infrastructure.overrides.refresh')}
          </Button>
        }
      />

      <Tabs value={selectedType} onValueChange={(next) => handleTypeChange(next as TypeFilter)} className={styles.typeTabs}>
        <TabsList className={styles.typeTabsList}>
          {TYPE_FILTERS.map((tp) => (
            <TabsTrigger key={tp} value={tp}>
              {tp === 'all' ? t('pages:infrastructure.overrides.filter.all') : tp}
            </TabsTrigger>
          ))}
        </TabsList>
      </Tabs>

      {error && <ErrorBanner message={error.message} onRetry={refetch} />}

      <div className={styles.tableToolbar}>
        <div className={styles.searchBox}>
          <span className={styles.searchIcon} aria-hidden="true" />
          <input
            type="search"
            className={styles.searchInput}
            placeholder={t('pages:infrastructure.overrides.searchPlaceholder')}
            value={search}
            onChange={(e) => {
              setSearch(e.target.value);
              setPage(1);
            }}
            aria-label={t('pages:infrastructure.overrides.searchPlaceholder')}
          />
        </div>
        <div className={styles.filterActions}>
          <button
            type="button"
            className={styles.chip}
            data-active={hasTtl ? 'true' : 'false'}
            aria-pressed={hasTtl}
            onClick={toggleHasTtl}
          >
            {t('pages:infrastructure.overrides.filter.hasTtl')}
          </button>
          <button
            type="button"
            className={styles.chip}
            data-active={stale ? 'true' : 'false'}
            aria-pressed={stale}
            onClick={toggleStale}
          >
            {t('pages:infrastructure.overrides.filter.stale')}
          </button>
          <button
            type="button"
            className={styles.chip}
            data-active={recentOnly ? 'true' : 'false'}
            aria-pressed={recentOnly}
            onClick={toggleRecent}
          >
            {t('pages:infrastructure.overrides.filter.recent')}
          </button>
        </div>
      </div>

      <div className={styles.tableSection}>
        <div className={styles.summary}>
          <span>{t('pages:infrastructure.overrides.summary.nodes', { count: visibleSummary.totalNodes })}</span>
          <span>{t('pages:infrastructure.overrides.summary.keys', { count: visibleSummary.totalOverrides })}</span>
          <span>{t('pages:infrastructure.overrides.summary.stale', { count: visibleSummary.staleCount })}</span>
          <span>{t('pages:infrastructure.overrides.summary.expiring', { count: visibleSummary.expiringSoonCount })}</span>
        </div>
        <DataTable
          className={styles.overridesTable}
          columns={columns}
          data={filteredRows}
          onRowClick={handleView}
          emptyMessage={t('pages:infrastructure.overrides.empty')}
          loading={loading}
          hideSearch
          frameless
          pageSize={pageSize}
        />
      </div>

      {visibleTotal > 0 ? (
        <ListPagination
          offset={hasClientFilters ? 0 : (page - 1) * pageSize}
          limit={pageSize}
          total={visibleTotal}
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
