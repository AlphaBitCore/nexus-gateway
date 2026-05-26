import { useState, useCallback } from 'react';
import { useTranslation } from 'react-i18next';
import { useNavigate } from 'react-router-dom';
import clsx from 'clsx';
import { useApi } from '@/hooks/useApi';
import { hookApi } from '@/api/services';
import { useDebouncedValue } from '@/hooks/useDebouncedValue';
import { useMutation } from '@/hooks/useMutation';
import { usePermission } from '@/hooks/usePermission';
import {
  PageHeader, DataTable, ListFilterToolbar,
  AlertDialog, Skeleton, ErrorBanner, Button, Stack, Card,
  ListPagination, DEFAULT_ADMIN_LIST_PAGE_SIZE, type AdminListPageSize,
  ListEnabledSwitchCell,
  RowActions, RowActionTextButton, RowDeleteAction,
} from '@/components/ui';
import type { DataTableColumn } from '@/components/ui';
import { HookPipelinePanel } from '../panels/HookPipelinePanel';
import type { AdminHookListResponse, HookCategory, HookConfig } from '@/api/types';
import {
  HOOK_CATEGORY,
} from '@/constants/hooks';
import styles from './HookList.module.css';

type PipelineTab = 'all' | 'request' | 'response';

function categoryBadgeClass(cat: HookCategory | undefined, s: Record<string, string>): string {
  switch (cat) {
    case HOOK_CATEGORY.COMPLIANCE:       return s.badgeCategoryCompliance;
    case HOOK_CATEGORY.TRAFFIC_CONTROL:  return s.badgeCategoryTrafficControl;
    case HOOK_CATEGORY.QUALITY:          return s.badgeCategoryQuality;
    case HOOK_CATEGORY.OBSERVABILITY:    return s.badgeCategoryObservability;
    default:                             return s.badgeCategoryDefault;
  }
}

function stageLabel(hook: HookConfig, t: ReturnType<typeof useTranslation>['t']): string {
  const stage = hook.stage?.toLowerCase();
  if (stage === 'response') return t('pages:hooks.stageResponse', 'Response');
  return t('pages:hooks.stageRequest', 'Request');
}

function stageBadgeClass(label: string, s: Record<string, string>): string {
  if (label === 'Response') return s.badgeStageResponse;
  return s.badgeStageRequest;
}

export function ConfigHooksPage() {
  const { t } = useTranslation();
  const navigate = useNavigate();
  const [search, setSearch] = useState('');
  const debouncedSearch = useDebouncedValue(search, 300);
  const [enabledFilter, setEnabledFilter] = useState('');
  const [activeTab, setActiveTab] = useState<PipelineTab>('all');
  const [offset, setOffset] = useState(0);
  const [pageLimit, setPageLimit] = useState<AdminListPageSize>(DEFAULT_ADMIN_LIST_PAGE_SIZE);
  const { data, loading, error, refetch } = useApi<AdminHookListResponse>(() => {
    const params: Record<string, string> = {
      limit: String(pageLimit),
      offset: String(offset),
    };
    const q = debouncedSearch.trim();
    if (q) params.q = q;
    if (enabledFilter === 'enabled') params.enabled = 'true';
    if (enabledFilter === 'disabled') params.enabled = 'false';
    if (activeTab === 'request') params.pipeline = 'request';
    if (activeTab === 'response') params.pipeline = 'response';
    return hookApi.list(params);
  }, ['admin', 'hooks', 'list', debouncedSearch, enabledFilter, offset, pageLimit, activeTab]);
  const [deleting, setDeleting] = useState<HookConfig | null>(null);
  const [showPipeline, setShowPipeline] = useState(false);
  const canCreate = usePermission('hook:create');
  const canDelete = usePermission('hook:delete');

  const { mutate: toggleHook, loading: togglingHookEnabled } = useMutation(
    (payload: { id: string; enabled: boolean }) => hookApi.update(payload.id, { enabled: payload.enabled }),
    { invalidateQueries: [['api', 'admin', 'hooks']], successMessage: 'Hook updated' },
  );

  const { mutate: deleteHook } = useMutation(
    (id: string) => hookApi.delete(id),
    {
      invalidateQueries: [['api', 'admin', 'hooks']],
      onSuccess: () => { setDeleting(null); },
      successMessage: 'Hook deleted',
    },
  );

  const rows = data?.data ?? [];
  const total = data?.total ?? 0;

  const onSearchChange = useCallback((v: string) => {
    setSearch(v);
    setOffset(0);
  }, []);

  const onEnabledFilterChange = useCallback((e: React.ChangeEvent<HTMLSelectElement>) => {
    setEnabledFilter(e.target.value);
    setOffset(0);
  }, []);

  const selectTab = useCallback((key: PipelineTab) => {
    setActiveTab(key);
    setOffset(0);
  }, []);

  if (loading) return <Skeleton.ListPageSkeleton />;
  if (error) return <ErrorBanner message={error.message} onRetry={refetch} />;

  const columns: DataTableColumn<HookConfig>[] = [
    { key: 'name', label: t('pages:hooks.nameCol', 'Name') },
    {
      key: 'category',
      label: t('pages:hooks.categoryCol', 'Category'),
      render: (r) => {
        const c = r.classification?.category;
        return (
          <span className={clsx(styles.categoryBadge, categoryBadgeClass(c, styles))}>
            {r.classification?.categoryLabel ?? '-'}
          </span>
        );
      },
    },
    {
      key: 'stage',
      label: t('pages:hooks.stageCol', 'Stage'),
      render: (r) => {
        const label = stageLabel(r, t);
        return (
          <span className={clsx(styles.stageBadge, stageBadgeClass(label, styles))}>
            {label}
          </span>
        );
      },
    },
    {
      key: 'enabled',
      label: t('pages:hooks.statusCol', 'Status'),
      render: (r) => (
        <ListEnabledSwitchCell
          enabled={r.enabled}
          canToggle
          toggleDisabled={togglingHookEnabled}
          ariaLabel={t('common:listToggleEnabledAria', { name: r.name })}
          onToggle={(enabled) => { void toggleHook({ id: r.id, enabled }); }}
        />
      ),
    },
    { key: 'priority', label: t('pages:hooks.priorityCol', 'Priority') },
    {
      key: 'actions',
      label: t('pages:hooks.actionsCol', 'Actions'),
      render: (r) => (
        <RowActions>
          <RowActionTextButton label={t('pages:hooks.view', 'View')} onAction={() => navigate(`/compliance/hooks/${r.id}`)} />
          {canDelete && (
            <RowDeleteAction label={t('common:delete')} onAction={() => setDeleting(r)} />
          )}
        </RowActions>
      ),
    },
  ];

  const tabs = [
    { key: 'all' as const, label: t('pages:hooks.allHooks', 'All hooks') },
    { key: 'request' as const, label: t('pages:hooks.requestPipeline', 'Request pipeline') },
    { key: 'response' as const, label: t('pages:hooks.responsePipeline', 'Response pipeline') },
  ];

  return (
    <Stack gap="lg">
      <PageHeader
        title={t('pages:hooks.title')}
        subtitle={t('pages:hooks.subtitle')}
        action={
          canCreate ? (
            <Button onClick={() => navigate('/compliance/hooks/new')}>{t('pages:hooks.createHook')}</Button>
          ) : undefined
        }
      />

      <div className={styles.toolbarCard}>
        <div className={styles.segmentedTabRow}>
          {tabs.map(tab => (
            <button
              key={tab.key}
              type="button"
              data-design-system-escape="segmented-tab"
              onClick={() => selectTab(tab.key)}
              className={activeTab === tab.key ? styles.segmentedTabActive : styles.segmentedTab}
            >
              {tab.label}
            </button>
          ))}
        </div>
        <Button variant="ghost" onClick={() => setShowPipeline(!showPipeline)}>
          {showPipeline ? t('pages:hooks.hidePipeline', 'Hide execution pipeline') : t('pages:hooks.showPipeline', 'Show execution pipeline')}
        </Button>
        {showPipeline && (
          <div className={styles.pipelineWrap}>
            <HookPipelinePanel />
          </div>
        )}
      </div>

      <ListFilterToolbar
        searchPlaceholder={t('pages:hooks.searchPlaceholder')}
        searchValue={search}
        onSearchChange={onSearchChange}
        meta={
          total === 0
            ? t('pages:hooks.noMatch', 'No hooks match the current filters')
            : t('pages:hooks.showingMeta', 'Showing {{count}} hook(s) on this page · {{total}} total matching', { count: rows.length, total: total.toLocaleString() })
        }
      >
        <select aria-label={t('pages:hooks.filterByEnabledStatus')} value={enabledFilter} onChange={onEnabledFilterChange} className={styles.filterSelect}>
          <option value="">{t('pages:hooks.allHooks', 'All hooks')}</option>
          <option value="enabled">{t('pages:hooks.enabledOnly', 'Enabled only')}</option>
          <option value="disabled">{t('pages:hooks.disabledOnly', 'Disabled only')}</option>
        </select>
      </ListFilterToolbar>

      <Card padding="none">
        <DataTable
          hideSearch
          frameless
          pageSize={pageLimit}
          columns={columns}
          data={rows}
          emptyMessage={t('pages:hooks.noHooks', 'No hooks configured')}
          onRowClick={(r) => navigate(`/compliance/hooks/${r.id}`)}
        />
      </Card>

      <ListPagination offset={offset} limit={pageLimit} total={total} onOffsetChange={setOffset} onLimitChange={setPageLimit} />

      <AlertDialog
        open={!!deleting}
        onOpenChange={(open) => { if (!open) setDeleting(null); }}
        title={t('pages:hooks.deleteHook')}
        description={t('pages:hooks.deleteConfirm', 'Are you sure you want to delete hook "{{name}}"? This action cannot be undone.', { name: deleting?.name })}
        confirmLabel={t('common:delete')}
        onConfirm={() => { if (deleting) deleteHook(deleting.id); }}
        variant="danger"
      />
    </Stack>
  );
}
