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
 * Auto-refresh: every 15s the page calls `refetch()` so the inbox stays
 * fresh without the user hitting reload. The interval is cleared on unmount.
 */
import { useCallback, useEffect, useMemo, useState } from 'react';
import { useTranslation } from 'react-i18next';
import { useApi } from '@/hooks/useApi';
import { useDebouncedValue } from '@/hooks/useDebouncedValue';
import { alertsApi } from '@/api/services';
import type {
  Alert,
  AlertListResponse,
  AlertSeverity,
  AlertState,
} from '@/api/services';
import {
  PageHeader,
  DataTable,
  Badge,
  Button,
  Stack,
  Card,
  ErrorBanner,
  Skeleton,
  MultiSelectDropdown,
  Input,
  ListPagination,
  DEFAULT_ADMIN_LIST_PAGE_SIZE,
  type AdminListPageSize,
} from '@/components/ui';
import type { BadgeProps, DataTableColumn } from '@/components/ui';
import { useMutation } from '@/hooks/useMutation';
import { AlertDetailDrawer } from '../detail/AlertDetailDrawer';
import styles from './AlertListPage.module.css';

const AUTO_REFRESH_MS = 15_000;

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
  const { t } = useTranslation();

  /* ── Filters ───────────────────────────────────────────────────────────── */
  const [states, setStates] = useState<AlertState[]>([]);
  const [severities, setSeverities] = useState<AlertSeverity[]>([]);
  const [sourceTypes, setSourceTypes] = useState<string[]>([]);
  const [ruleIdInput, setRuleIdInput] = useState('');
  const [since, setSince] = useState('');
  const [until, setUntil] = useState('');
  const [offset, setOffset] = useState(0);
  const [pageLimit, setPageLimit] = useState<AdminListPageSize>(
    DEFAULT_ADMIN_LIST_PAGE_SIZE,
  );

  const debouncedRuleId = useDebouncedValue(ruleIdInput, 300);

  // Convert local datetime strings into ISO-Z so Hub receives UTC regardless
  // of the user's timezone. Empty string → undefined (omit the filter).
  const sinceIso = since ? new Date(since).toISOString() : undefined;
  const untilIso = until ? new Date(until).toISOString() : undefined;

  /* ── List fetch ────────────────────────────────────────────────────────── */
  const { data, loading, error, refetch } = useApi<AlertListResponse>(
    () =>
      alertsApi.list({
        state: states.length ? states : undefined,
        severity: severities.length ? severities : undefined,
        sourceType: sourceTypes.length ? sourceTypes : undefined,
        ruleId: debouncedRuleId || undefined,
        since: sinceIso,
        until: untilIso,
        offset,
        limit: pageLimit,
      }),
    [
      'admin',
      'alerts',
      'inbox',
      states.join(','),
      severities.join(','),
      sourceTypes.join(','),
      debouncedRuleId,
      sinceIso ?? '',
      untilIso ?? '',
      offset,
      pageLimit,
    ],
  );

  // Auto-refresh every 15s. The interval does not debounce user filter
  // changes — React Query dedupes rapidly fired refetches on the same key.
  useEffect(() => {
    const id = setInterval(() => {
      refetch();
    }, AUTO_REFRESH_MS);
    return () => clearInterval(id);
  }, [refetch]);

  /* ── Row-action mutations (refetch on success) ─────────────────────────── */
  const [selectedId, setSelectedId] = useState<string | null>(null);
  const [drawerOpen, setDrawerOpen] = useState(false);

  const { mutate: ackAlert, loading: ackLoading } = useMutation<string, Alert>(
    (id) => alertsApi.ack(id),
    {
      onSuccess: () => refetch(),
      successMessage: t('pages:alerts.inbox.ackSuccess'),
    },
  );

  const { mutate: resolveAlert, loading: resolveLoading } = useMutation<
    string,
    Alert
  >((id) => alertsApi.resolve(id), {
    onSuccess: () => refetch(),
    successMessage: t('pages:alerts.inbox.resolveSuccess'),
  });

  const rows = data?.alerts ?? [];
  const total = data?.total ?? 0;

  const stateLabel = useMemo<Record<AlertState, string>>(
    () => ({
      firing: t('pages:alerts.inbox.states.firing'),
      acknowledged: t('pages:alerts.inbox.states.acknowledged'),
      resolved: t('pages:alerts.inbox.states.resolved'),
    }),
    [t],
  );
  const severityLabel = useMemo<Record<AlertSeverity, string>>(
    () => ({
      critical: t('pages:alerts.inbox.severities.critical'),
      high: t('pages:alerts.inbox.severities.high'),
      medium: t('pages:alerts.inbox.severities.medium'),
      low: t('pages:alerts.inbox.severities.low'),
      info: t('pages:alerts.inbox.severities.info'),
    }),
    [t],
  );

  /* ── Filter handlers reset paging ──────────────────────────────────────── */
  const resetPaging = () => setOffset(0);

  const onStatesChange = useCallback((next: string[]) => {
    setStates(next as AlertState[]);
    resetPaging();
  }, []);
  const onSeveritiesChange = useCallback((next: string[]) => {
    setSeverities(next as AlertSeverity[]);
    resetPaging();
  }, []);
  const onSourceTypesChange = useCallback((next: string[]) => {
    setSourceTypes(next);
    resetPaging();
  }, []);
  const onRuleIdChange = useCallback((e: React.ChangeEvent<HTMLInputElement>) => {
    setRuleIdInput(e.target.value);
    resetPaging();
  }, []);
  const onSinceChange = useCallback((e: React.ChangeEvent<HTMLInputElement>) => {
    setSince(e.target.value);
    resetPaging();
  }, []);
  const onUntilChange = useCallback((e: React.ChangeEvent<HTMLInputElement>) => {
    setUntil(e.target.value);
    resetPaging();
  }, []);

  const openDrawer = useCallback((row: Alert) => {
    setSelectedId(row.id);
    setDrawerOpen(true);
  }, []);

  const closeDrawer = useCallback(() => {
    setDrawerOpen(false);
  }, []);

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
        <Stack direction="horizontal" gap="xs" onClick={(e) => e.stopPropagation()}>
          {r.state === 'firing' && (
            <Button
              variant="secondary"
              size="sm"
              loading={ackLoading}
              onClick={(e) => {
                e.stopPropagation();
                ackAlert(r.id);
              }}
            >
              {t('pages:alerts.inbox.actions.ack')}
            </Button>
          )}
          {r.state !== 'resolved' && (
            <Button
              variant="secondary"
              size="sm"
              loading={resolveLoading}
              onClick={(e) => {
                e.stopPropagation();
                resolveAlert(r.id);
              }}
            >
              {t('pages:alerts.inbox.actions.resolve')}
            </Button>
          )}
        </Stack>
      ),
    },
  ];

  return (
    <Stack gap="lg">
      <PageHeader
        title={t('pages:alerts.inbox.title')}
        subtitle={t('pages:alerts.inbox.subtitle')}
      />

      <Card>
        <div className={styles.filterGrid}>
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
          <div className={styles.filterField}>
            <label className={styles.filterLabel} htmlFor="alerts-rule-id">
              {t('pages:alerts.inbox.filters.ruleId')}
            </label>
            <Input
              id="alerts-rule-id"
              type="search"
              placeholder={t('pages:alerts.inbox.filters.ruleIdPlaceholder')}
              value={ruleIdInput}
              onChange={onRuleIdChange}
            />
          </div>
          <div className={styles.filterField}>
            <label className={styles.filterLabel} htmlFor="alerts-since">
              {t('pages:alerts.inbox.filters.since')}
            </label>
            <Input
              id="alerts-since"
              type="datetime-local"
              value={since}
              onChange={onSinceChange}
            />
          </div>
          <div className={styles.filterField}>
            <label className={styles.filterLabel} htmlFor="alerts-until">
              {t('pages:alerts.inbox.filters.until')}
            </label>
            <Input
              id="alerts-until"
              type="datetime-local"
              value={until}
              onChange={onUntilChange}
            />
          </div>
        </div>
      </Card>

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
