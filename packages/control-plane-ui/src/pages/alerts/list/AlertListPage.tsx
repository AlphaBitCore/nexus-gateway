/**
 * Alert inbox — unified alert list served by Hub via the CP BFF.
 *
 * Mirrors the QuotaOverrideListPage layout: Card-wrapped filter row on top,
 * DataTable + ListPagination below, row click opens AlertDetailDrawer.
 *
 * Filters: state / severity / sourceType (multi-select) + ruleId (debounced)
 * + date range (since/until). `targetQuery` is intentionally omitted from
 * this task's UI because Hub does not honour the filter server-side yet —
 * it will be added when Hub wires it.
 *
 * State, the paged fetch, the ack/resolve mutations and the auto-refresh loop
 * live in the `useAlertList` hook; this component renders pure layout.
 */
import type {
  Alert,
  AlertSeverity,
  AlertState,
} from '@/api/services';
import {
  PageHeader,
  DataTable,
  Badge,
  Stack,
  Card,
  ErrorBanner,
  Skeleton,
  MultiSelectDropdown,
  Input,
  Button,
  ListPagination,
  RowActions,
  RowActionIconButton,
  OpenActionIcon,
} from '@/components/ui';
import type { BadgeProps, DataTableColumn } from '@/components/ui';
import { AlertDetailDrawer } from '../detail/AlertDetailDrawer';
import { AckActionIcon, ResolveActionIcon } from './AlertListPage.icons';
import { useAlertList } from './useAlertList';
import styles from './AlertListPage.module.css';

const STATE_OPTIONS: AlertState[] = ['firing', 'acknowledged', 'resolved'];
const SEVERITY_OPTIONS: AlertSeverity[] = ['critical', 'high', 'medium', 'low', 'info'];
const SOURCE_TYPE_OPTIONS = ['quota', 'proxy', 'thing', 'provider', 'auth', 'system'];

function severityVariant(s: AlertSeverity): BadgeProps['variant'] {
  switch (s) {
    case 'critical':
    case 'high':
      return 'danger';
    case 'medium':
      return 'warning';
    case 'low':
      return 'info';
    default:
      return 'default';
  }
}

function stateVariant(s: AlertState): BadgeProps['variant'] {
  switch (s) {
    case 'firing':
      return 'danger';
    case 'acknowledged':
      return 'warning';
    case 'resolved':
      return 'success';
    default:
      return 'default';
  }
}

export function AlertListPage() {
  const {
    t,
    states,
    severities,
    sourceTypes,
    ruleIdInput,
    since,
    until,
    advancedOpen,
    setAdvancedOpen,
    offset,
    setOffset,
    pageLimit,
    setPageLimit,
    data,
    loading,
    error,
    refetch,
    rows,
    total,
    stateLabel,
    severityLabel,
    ackAlert,
    ackLoading,
    resolveAlert,
    resolveLoading,
    selectedId,
    drawerOpen,
    openDrawer,
    closeDrawer,
    onStatesChange,
    onSeveritiesChange,
    onSourceTypesChange,
    onRuleIdChange,
    onSinceChange,
    onUntilChange,
    resetAdvancedFilters,
    confirmAdvancedFilters,
    clearRuleId,
  } = useAlertList();

  if (loading && !data) return <Skeleton.ListPageSkeleton />;
  if (error) return <ErrorBanner message={error.message} onRetry={refetch} />;

  const columns: DataTableColumn<Alert>[] = [
    {
      key: 'state',
      label: t('pages:alerts.inbox.columns.state'),
      render: (r) => (
        <Badge variant={stateVariant(r.state)}>{stateLabel[r.state] ?? r.state}</Badge>
      ),
    },
    {
      key: 'severity',
      label: t('pages:alerts.inbox.columns.severity'),
      render: (r) => (
        <Badge variant={severityVariant(r.severity)}>
          {severityLabel[r.severity] ?? r.severity}
        </Badge>
      ),
    },
    {
      key: 'sourceType',
      label: t('pages:alerts.inbox.columns.sourceType'),
      render: (r) => <Badge variant="outline">{r.sourceType}</Badge>,
    },
    {
      key: 'ruleId',
      label: t('pages:alerts.inbox.columns.rule'),
      render: (r) => <code className={styles.inlineCode}>{r.ruleId}</code>,
    },
    {
      key: 'targetLabel',
      label: t('pages:alerts.inbox.columns.target'),
      render: (r) => r.targetLabel || r.targetKey,
    },
    {
      key: 'firedAt',
      label: t('pages:alerts.inbox.columns.firedAt'),
      render: (r) => new Date(r.firedAt).toLocaleString(),
    },
    {
      key: 'actions',
      label: t('pages:alerts.inbox.columns.actions'),
      sortable: false,
      render: (r) => (
        <RowActions>
          <RowActionIconButton label={t('common:view', 'View')} onAction={() => openDrawer(r)}>
            <OpenActionIcon />
          </RowActionIconButton>
          {r.state === 'firing' && (
            <RowActionIconButton
              label={t('pages:alerts.inbox.actions.ack')}
              disabled={ackLoading}
              onAction={() => ackAlert(r.id)}
            >
              <AckActionIcon />
            </RowActionIconButton>
          )}
          {r.state !== 'resolved' && (
            <RowActionIconButton
              label={t('pages:alerts.inbox.actions.resolve')}
              disabled={resolveLoading}
              onAction={() => resolveAlert(r.id)}
            >
              <ResolveActionIcon />
            </RowActionIconButton>
          )}
        </RowActions>
      ),
    },
  ];

  return (
    <Stack gap="lg">
      <PageHeader
        title={t('pages:alerts.inbox.title')}
        subtitle={t('pages:alerts.inbox.subtitle')}
      />

      <div className={styles.filterToolbar} role="search">
        <div className={styles.searchBox}>
          <span className={styles.searchIcon} aria-hidden="true" />
          <Input
            id="alerts-rule-id"
            type="text"
            enterKeyHint="search"
            autoComplete="off"
            aria-label={t('pages:alerts.inbox.filters.ruleId')}
            placeholder={t('pages:alerts.inbox.filters.ruleIdPlaceholder')}
            value={ruleIdInput}
            onChange={onRuleIdChange}
            className={styles.searchInput}
          />
          {ruleIdInput.trim().length > 0 && (
            <button
              type="button"
              onClick={clearRuleId}
              className={styles.clearSearchButton}
              aria-label={t('common:clear')}
              title={t('common:clear')}
            >
              <span aria-hidden="true" />
            </button>
          )}
          <button
            type="button"
            className={styles.advancedButton}
            onClick={() => setAdvancedOpen((open) => !open)}
          >
            {t('common:advancedFilter')}
          </button>
          {advancedOpen && (
            <div className={styles.advancedPanel}>
              <div className={styles.advancedGrid}>
                <MultiSelectDropdown
                  label={t('pages:alerts.inbox.filters.state')}
                  emptyLabel={t('pages:alerts.inbox.filters.allStates')}
                  options={STATE_OPTIONS.map((v) => ({ value: v, label: stateLabel[v] }))}
                  value={states}
                  onChange={onStatesChange}
                />
                <MultiSelectDropdown
                  label={t('pages:alerts.inbox.filters.severity')}
                  emptyLabel={t('pages:alerts.inbox.filters.allSeverities')}
                  options={SEVERITY_OPTIONS.map((v) => ({
                    value: v,
                    label: severityLabel[v],
                  }))}
                  value={severities}
                  onChange={onSeveritiesChange}
                />
                <MultiSelectDropdown
                  label={t('pages:alerts.inbox.filters.sourceType')}
                  emptyLabel={t('pages:alerts.inbox.filters.allSourceTypes')}
                  options={SOURCE_TYPE_OPTIONS.map((v) => ({ value: v, label: v }))}
                  value={sourceTypes}
                  onChange={onSourceTypesChange}
                />
                <div className={styles.dateRangeField}>
                  <span className={styles.filterLabel}>{t('pages:alerts.inbox.filters.timeRange', 'Time range')}</span>
                  <div className={styles.dateRangeBox}>
                    <label className={styles.dateRangeItem}>
                      <span className={styles.dateRangeLabel}>{t('pages:alerts.inbox.filters.since')}</span>
                      <Input
                        id="alerts-since"
                        type="datetime-local"
                        data-empty={since === '' || undefined}
                        value={since}
                        onChange={onSinceChange}
                        className={styles.dateInput}
                      />
                    </label>
                    <span className={styles.dateRangeDivider} aria-hidden="true" />
                    <label className={styles.dateRangeItem}>
                      <span className={styles.dateRangeLabel}>{t('pages:alerts.inbox.filters.until')}</span>
                      <Input
                        id="alerts-until"
                        type="datetime-local"
                        data-empty={until === '' || undefined}
                        value={until}
                        onChange={onUntilChange}
                        className={styles.dateInput}
                      />
                    </label>
                  </div>
                </div>
              </div>
              <div className={styles.advancedFooter}>
                <Button variant="secondary" className={styles.advancedFooterButton} onClick={resetAdvancedFilters}>
                  {t('common:reset', 'Reset')}
                </Button>
                <Button className={styles.advancedFooterButton} onClick={confirmAdvancedFilters}>
                  {t('common:confirmSearch', 'Confirm Search')}
                </Button>
              </div>
            </div>
          )}
        </div>
      </div>

      <Card padding="none">
        <DataTable
          hideSearch
          frameless
          pageSize={pageLimit}
          columns={columns}
          data={rows}
          onRowClick={openDrawer}
          emptyMessage={t('pages:alerts.inbox.empty')}
        />
      </Card>

      <ListPagination
        offset={offset}
        limit={pageLimit}
        total={total}
        onOffsetChange={setOffset}
        onLimitChange={setPageLimit}
      />

      <AlertDetailDrawer
        alertId={selectedId}
        visible={drawerOpen}
        onClose={closeDrawer}
        onMutated={() => refetch()}
      />
    </Stack>
  );
}
