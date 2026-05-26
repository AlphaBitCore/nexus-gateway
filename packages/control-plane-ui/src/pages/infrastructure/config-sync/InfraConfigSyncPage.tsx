import { useState, useCallback, useEffect, useMemo } from 'react';
import { useTranslation } from 'react-i18next';
import { useApi } from '@/hooks/useApi';
import { useMutation } from '@/hooks/useMutation';
import { hubApi } from '@/api/services/infrastructure/nodes/hub';
import type { ConfigHistoryEvent, ConfigCatalogResponse, OutOfSyncItem } from '@/api/services/infrastructure/nodes/hub';
import {
  PageHeader, Stack, Card, Button, Badge, Select,
  Tabs, TabsList, TabsTrigger, TabsContent,
  DataTable, LoadingSpinner, ErrorBanner,
} from '@/components/ui';
import type { DataTableColumn } from '@/components/ui';
import { ListPagination, DEFAULT_ADMIN_LIST_PAGE_SIZE } from '@/components/ui/ListPagination';
import type { AdminListPageSize } from '@/components/ui/ListPagination';
import styles from './InfraConfigSyncPage.module.css';

export default function InfraConfigSyncPage() {
  const { t } = useTranslation();

  const [tab, setTab] = useState('history');
  const [filterType, setFilterType] = useState('');
  const [filterKey, setFilterKey] = useState('');
  const [resyncingId, setResyncingId] = useState<string | null>(null);
  const [page, setPage] = useState(1);
  const [pageSize, setPageSize] = useState<AdminListPageSize>(DEFAULT_ADMIN_LIST_PAGE_SIZE);

  // Catalog drives the Type / Config Key selects so they reflect actual DB
  // contents (thing_config_template) rather than a hardcoded allow-list.
  // Fetched once per mount; re-renders cascade locally as filterType
  // changes.
  const { data: catalog } = useApi<ConfigCatalogResponse>(
    () => hubApi.listConfigCatalog(),
    ['admin', 'config-sync-catalog'],
  );

  // Radix Select (the underlying primitive) forbids empty-string item
  // values and silently filters them out. We represent the "All" option
  // with a non-empty sentinel and translate to "" (no filter) at the
  // onValueChange boundary so the backend contract stays unchanged.
  const ALL = '__all__';

  const nodeTypeOptions = useMemo(() => {
    const base = [{ value: ALL, label: t('pages:infrastructure.filterAll') }];
    const types = (catalog?.entries ?? []).map((e) => ({ value: e.nodeType, label: e.nodeType }));
    return [...base, ...types];
  }, [catalog, t]);

  // Key options cascade from the selected type:
  //   - filterType === ''  -> union of every key across all types (dedup)
  //   - filterType set     -> only keys present on that type
  // Either way an explicit "All" is always the first option so the user can
  // clear the filter from any state.
  const configKeyOptions = useMemo(() => {
    const base = [{ value: ALL, label: t('pages:infrastructure.filterAll') }];
    const entries = catalog?.entries ?? [];
    let keys: string[];
    if (filterType) {
      keys = entries.find((e) => e.nodeType === filterType)?.configKeys ?? [];
    } else {
      const seen = new Set<string>();
      for (const e of entries) {
        for (const k of e.configKeys) seen.add(k);
      }
      keys = Array.from(seen).sort();
    }
    return [...base, ...keys.map((k) => ({ value: k, label: k }))];
  }, [catalog, filterType, t]);

  // Cascade guard: when Type changes, drop filterKey if the new type no
  // longer exposes that key — otherwise the request would return empty
  // while the UI shows a stale key filter.
  useEffect(() => {
    if (!filterKey) return;
    const stillValid = configKeyOptions.some((o) => o.value === filterKey);
    if (!stillValid) {
      setFilterKey('');
      setPage(1);
    }
  }, [filterKey, configKeyOptions]);

  // Translate between Select's ALL sentinel and the backend "no filter"
  // empty string. Kept local to this file; the Select primitive receives
  // `ALL` as a real option value, the API sees "".
  const handleTypeChange = useCallback((v: string) => {
    setFilterType(v === ALL ? '' : v);
    setPage(1);
  }, []);
  const handleKeyChange = useCallback((v: string) => {
    setFilterKey(v === ALL ? '' : v);
    setPage(1);
  }, []);

  const {
    data: history,
    loading: historyLoading,
    error: historyError,
    refetch: refetchHistory,
  } = useApi(
    () => hubApi.listConfigHistory({
      nodeType: filterType || undefined,
      configKey: filterKey || undefined,
      page,
      pageSize,
    }),
    ['admin', 'config-sync-history', filterType, filterKey, page, pageSize],
  );

  const {
    data: outOfSync,
    loading: outOfSyncLoading,
    error: outOfSyncError,
    refetch: refetchOutOfSync,
  } = useApi(
    () => hubApi.listOutOfSync(),
    ['admin', 'config-sync-oos'],
  );

  // The Out-of-Sync monitor row-level "Re-sync" triggers one resync call per
  // out-of-sync key on that specific node. Sequential (not Promise.all) so a
  // server-side error on key N short-circuits rather than being masked by a
  // later key succeeding.
  const resync = useMutation(
    async (item: OutOfSyncItem) => {
      for (const key of item.outOfSyncKeys) {
        await hubApi.resyncNode(item.nodeId, key);
      }
    },
    {
      successMessage: t('pages:infrastructure.resyncSuccess', 'Config re-sync pushed'),
      invalidateQueries: [['admin', 'config-sync-oos']],
      onSuccess: () => setResyncingId(null),
    },
  );

  const handlePageSizeChange = useCallback((next: AdminListPageSize) => {
    setPageSize(next);
    setPage(1);
  }, []);

  const historyColumns: DataTableColumn<ConfigHistoryEvent>[] = [
    {
      key: 'createdAt',
      label: t('pages:infrastructure.timestamp', 'Timestamp'),
      render: (row) => new Date(row.createdAt).toLocaleString(),
      sortable: true,
    },
    { key: 'nodeType', label: t('pages:infrastructure.nodeType'), sortable: true },
    { key: 'configKey', label: t('pages:infrastructure.configKey', 'Config Key'), sortable: true },
    { key: 'action', label: t('pages:infrastructure.action', 'Action'), sortable: true },
    { key: 'actorName', label: t('pages:infrastructure.actor', 'Actor'), sortable: true },
    { key: 'newVersion', label: t('pages:infrastructure.version'), sortable: true },
  ];

  return (
    <Stack gap="lg">
      <PageHeader
        title={t('pages:infrastructure.configSyncTitle')}
        subtitle={t('pages:infrastructure.configSyncDescription')}
      />

      <Tabs value={tab} onValueChange={setTab}>
        <TabsList>
          <TabsTrigger value="history">
            {t('pages:infrastructure.changeHistory', 'Change History')}
          </TabsTrigger>
          <TabsTrigger value="out-of-sync">
            {t('pages:infrastructure.outOfSyncMonitor', 'Out-of-Sync Monitor')}
          </TabsTrigger>
        </TabsList>

        {/* ── Change History ──────────────────────────────────── */}
        <TabsContent value="history">
          <Stack gap="md">
            <Card>
              <div className={styles.filterBar}>
                <div className={styles.filterField}>
                  <span className={styles.filterLabel}>{t('pages:infrastructure.nodeType')}:</span>
                  <Select
                    value={filterType || ALL}
                    onValueChange={handleTypeChange}
                    options={nodeTypeOptions}
                    placeholder={t('pages:infrastructure.allTypes', 'All types')}
                    className={styles.filterSelect}
                  />
                </div>
                <div className={`${styles.filterField} ${styles['filterField--spaced']}`}>
                  <span className={styles.filterLabel}>{t('pages:infrastructure.configKey', 'Config Key')}:</span>
                  <Select
                    value={filterKey || ALL}
                    onValueChange={handleKeyChange}
                    options={configKeyOptions}
                    placeholder={t('pages:infrastructure.allKeys', 'All keys')}
                    className={styles.filterSelect}
                  />
                </div>
              </div>
            </Card>

            {historyLoading && !history ? (
              <LoadingSpinner />
            ) : historyError ? (
              <ErrorBanner message={historyError.message} onRetry={refetchHistory} />
            ) : (
              <>
                <DataTable<ConfigHistoryEvent>
                  columns={historyColumns}
                  data={history?.events ?? []}
                  hideSearch
                  pageSize={pageSize}
                  loading={historyLoading}
                  emptyMessage={t('pages:infrastructure.noHistory', 'No config change history')}
                />
                {(history?.total ?? 0) > 0 ? (
                  <ListPagination
                    offset={(page - 1) * pageSize}
                    limit={pageSize}
                    total={history?.total ?? 0}
                    onOffsetChange={(nextOffset) => setPage(Math.floor(nextOffset / pageSize) + 1)}
                    onLimitChange={handlePageSizeChange}
                  />
                ) : null}
              </>
            )}
          </Stack>
        </TabsContent>

        {/* ── Out-of-Sync Monitor ─────────────────────────────── */}
        <TabsContent value="out-of-sync">
          <Stack gap="md">
            {outOfSyncLoading && !outOfSync ? (
              <LoadingSpinner />
            ) : outOfSyncError ? (
              <ErrorBanner message={outOfSyncError.message} onRetry={refetchOutOfSync} />
            ) : (outOfSync?.outOfSync ?? []).length === 0 ? (
              <div className={styles.successBanner}>
                <svg width="18" height="18" viewBox="0 0 24 24" fill="none" stroke="currentColor" strokeWidth="2" strokeLinecap="round" strokeLinejoin="round">
                  <path d="M22 11.08V12a10 10 0 1 1-5.93-9.14" />
                  <polyline points="22 4 12 14.01 9 11.01" />
                </svg>
                {t('pages:infrastructure.allInSync')}
              </div>
            ) : (
              (outOfSync?.outOfSync ?? []).map((item) => (
                <div key={item.nodeId} className={styles.outOfSyncCard}>
                  <div className={styles.outOfSyncInfo}>
                    <span className={styles.outOfSyncName}>{item.name}</span>
                    <div className={styles.outOfSyncMeta}>
                      <Badge variant="default">{item.nodeType}</Badge>
                      <span>{t('pages:infrastructure.lastSeen')}: {item.lastSeen ? new Date(item.lastSeen).toLocaleString() : '\u2014'}</span>
                    </div>
                    <div className={styles.outOfSyncKeys}>
                      {item.outOfSyncKeys.map((key) => (
                        <Badge key={key} variant="warning">{key}</Badge>
                      ))}
                    </div>
                  </div>
                  <div className={styles.outOfSyncActions}>
                    <Button
                      variant="primary"
                      size="sm"
                      loading={resyncingId === item.nodeId}
                      onClick={() => { setResyncingId(item.nodeId); resync.mutate(item).catch(() => setResyncingId(null)); }}
                    >
                      {t('pages:infrastructure.resync')}
                    </Button>
                  </div>
                </div>
              ))
            )}
          </Stack>
        </TabsContent>
      </Tabs>
    </Stack>
  );
}
