import { useState, useMemo, useCallback } from 'react';
import { useTranslation } from 'react-i18next';
import { useNavigate } from 'react-router-dom';
import { routingApi } from '@/api/services';
import { useApi } from '@/hooks/useApi';
import { useDebouncedValue } from '@/hooks/useDebouncedValue';
import { useMutation } from '@/hooks/useMutation';
import { usePermission } from '@/hooks/usePermission';
import {
  PageHeader, DataTable,
  AlertDialog, Skeleton, ErrorBanner, Button, Stack, Card,
  ListPagination, DEFAULT_ADMIN_LIST_PAGE_SIZE, type AdminListPageSize,
  ListEnabledSwitchCell,
  Input,
  RowActions, RowActionIconButton, RowDeleteAction, OpenActionIcon,
} from '@/components/ui';
import type { DataTableColumn } from '@/components/ui';
import type { AdminRoutingRuleListResponse, RoutingRule } from '@/api/types';
import { RoutingPrimaryWinnerCallout } from '../_shared/RoutingPrimaryWinnerCallout';
import { useStrategyOptions } from '../_shared/useStrategyOptions';
import styles from './RoutingRuleList.module.css';

export function ConfigRoutingPage() {
  const { t } = useTranslation();
  const navigate = useNavigate();
  const [search, setSearch] = useState('');
  const debouncedSearch = useDebouncedValue(search, 300);
  const [strategyFilter, setStrategyFilter] = useState('');
  const [enabledFilter, setEnabledFilter] = useState('');
  const [offset, setOffset] = useState(0);
  const [pageLimit, setPageLimit] = useState<AdminListPageSize>(DEFAULT_ADMIN_LIST_PAGE_SIZE);
  const { data, loading, error, refetch } = useApi<AdminRoutingRuleListResponse>(
    () => {
      const params: Record<string, string> = {
        limit: String(pageLimit),
        offset: String(offset),
      };
      const q = debouncedSearch.trim();
      if (q) params.q = q;
      if (strategyFilter) params.strategyType = strategyFilter;
      if (enabledFilter === 'enabled') params.enabled = 'true';
      if (enabledFilter === 'disabled') params.enabled = 'false';
      return routingApi.list(params);
    },
    ['admin', 'routing-rules', 'list', debouncedSearch, strategyFilter, enabledFilter, offset, pageLimit],
  );
  const [deleting, setDeleting] = useState<RoutingRule | null>(null);
  const canCreate = usePermission('routing-rule:create');
  const canUpdate = usePermission('routing-rule:update');
  const canDelete = usePermission('routing-rule:delete');

  const { mutate: toggleRule, loading: togglingRuleEnabled } = useMutation(
    (payload: { id: string; enabled: boolean }) => routingApi.update(payload.id, { enabled: payload.enabled }),
    { onSuccess: () => refetch(), successMessage: t('pages:routing.ruleUpdated') },
  );

  const { mutate: deleteRule } = useMutation(
    (id: string) => routingApi.delete(id),
    {
      onSuccess: () => { setDeleting(null); refetch(); },
      successMessage: t('pages:routing.ruleDeleted'),
    },
  );

  // The strategy filter offers the full canonical set of strategies (the same six
  // the Create/Edit picker shows), not just the values that happen to appear in the
  // current page of rules — otherwise the filter silently omits strategies (e.g.
  // Fallback Chain, A/B Split) that no existing rule uses yet, and shows raw enum
  // values instead of friendly labels.
  const strategyOptions = useStrategyOptions();

  const rows = data?.data ?? [];
  const total = data?.total ?? 0;

  const onSearchChange = useCallback((v: string) => {
    setSearch(v);
    setOffset(0);
  }, []);

  const onStrategyFilterChange = useCallback((e: React.ChangeEvent<HTMLSelectElement>) => {
    setStrategyFilter(e.target.value);
    setOffset(0);
  }, []);

  const onEnabledFilterChange = useCallback((e: React.ChangeEvent<HTMLSelectElement>) => {
    setEnabledFilter(e.target.value);
    setOffset(0);
  }, []);

  if (loading) return <Skeleton.ListPageSkeleton />;
  if (error) return <ErrorBanner message={error.message} onRetry={refetch} />;

  const columns: DataTableColumn<RoutingRule>[] = [
    {
      key: 'name',
      label: t('pages:routing.nameCol', 'Name'),
      render: (r) => (
        <Stack gap="xs">
          <span>{r.name}</span>
          {r.retryPolicy && (
            <span data-testid="routing-rule-retry-badge" className={styles.retryPolicyBadge}>
              {t('pages:routing.retryPolicy.badgeCustom', {
                n: typeof r.retryPolicy.maxAttemptsPerTarget === 'number'
                  ? r.retryPolicy.maxAttemptsPerTarget
                  : '?',
                classes: Array.isArray(r.retryPolicy.retryOn) && r.retryPolicy.retryOn.length > 0
                  ? r.retryPolicy.retryOn.join(',')
                  : '∅',
              })}
            </span>
          )}
        </Stack>
      ),
    },
    { key: 'strategyType', label: t('pages:routing.strategyCol', 'Strategy') },
    { key: 'priority', label: t('pages:routing.priorityCol', 'Priority') },
    {
      key: 'enabled',
      label: t('pages:routing.statusCol', 'Status'),
      render: (r) => (
        <ListEnabledSwitchCell
          enabled={r.enabled}
          canToggle={canUpdate}
          toggleDisabled={togglingRuleEnabled}
          ariaLabel={t('common:listToggleEnabledAria', { name: r.name })}
          onToggle={(enabled) => { void toggleRule({ id: r.id, enabled }); }}
        />
      ),
    },
    {
      key: 'actions',
      label: t('pages:routing.actionsCol', 'Actions'),
      render: (r) => (
        <RowActions>
          {canUpdate && (
            <RowActionIconButton label={t('common:edit')} onAction={() => navigate(`/ai-gateway/routing/${r.id}`)}>
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
    <Stack gap="lg" className={styles.pageStack}>
      <PageHeader
        title={t('pages:routing.title')}
        subtitle={t('pages:routing.subtitle')}
        action={
          canCreate ? (
            <Button data-testid="routing-rule-new" onClick={() => navigate('/ai-gateway/routing/new')}>{t('pages:routing.createRule')}</Button>
          ) : undefined
        }
      />

      <RoutingPrimaryWinnerCallout />

      <div className={styles.filterToolbar} role="search">
        <div className={styles.filterRow}>
          <div className={styles.searchBox}>
            <span className={styles.searchIcon} aria-hidden="true" />
            <Input
              type="text"
              enterKeyHint="search"
              autoComplete="off"
              aria-label={t('pages:routing.searchPlaceholder')}
              placeholder={t('pages:routing.searchPlaceholder')}
              value={search}
              onChange={(event) => onSearchChange(event.target.value)}
              className={styles.searchInput}
            />
            {search.trim().length > 0 && (
              <button
                type="button"
                onClick={() => onSearchChange('')}
                className={styles.clearSearchButton}
                aria-label={t('common:clear')}
                title={t('common:clear')}
              >
                <span aria-hidden="true" />
              </button>
            )}
          </div>
          <select aria-label={t('pages:routing.filterByStrategy')} value={strategyFilter} onChange={onStrategyFilterChange} className={styles.filterSelect}>
            <option value="">{t('pages:routing.allStrategies', 'All strategies')}</option>
            {strategyOptions.map(opt => <option key={opt.value} value={opt.value}>{opt.label}</option>)}
          </select>
          <select aria-label={t('pages:routing.filterByStatus')} value={enabledFilter} onChange={onEnabledFilterChange} className={styles.filterSelect}>
            <option value="">{t('pages:routing.allStatuses', 'All statuses')}</option>
            <option value="enabled">{t('common:enabled')}</option>
            <option value="disabled">{t('common:disabled')}</option>
          </select>
        </div>
        <div className={styles.filterMeta}>
          {total === 0
            ? t('pages:routing.noMatch', 'No rules match the current filters')
            : t('pages:routing.showingMeta', 'Showing {{count}} rule(s) on this page · {{total}} total matching', { count: rows.length, total: total.toLocaleString() })}
        </div>
      </div>

      <Card data-testid="routing-rules-table" padding="none">
        <DataTable
          hideSearch
          frameless
          pageSize={pageLimit}
          onRowClick={(row) => navigate(`/ai-gateway/routing/${row.id}`)}
          columns={columns}
          data={rows}
          emptyMessage={t('pages:routing.noRules')}
        />
      </Card>

      <ListPagination variant="plain" offset={offset} limit={pageLimit} total={total} onOffsetChange={setOffset} onLimitChange={setPageLimit} />

      <AlertDialog
        open={!!deleting}
        onOpenChange={(open) => { if (!open) setDeleting(null); }}
        title={t('pages:routing.deleteRule')}
        description={t('pages:routing.deleteConfirm', { name: deleting?.name })}
        confirmLabel={t('common:delete')}
        onConfirm={() => { if (deleting) deleteRule(deleting.id); }}
        variant="danger"
      />
    </Stack>
  );
}
