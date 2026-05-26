/**
 * AlertRulesListPage — browse and bulk-enable/disable unified alert rules.
 *
 * Hub seeds a builtin rule catalogue (see
 * `packages/nexus-hub/internal/alerting/rules/builtin.go`); this page lists
 * them as registered on the server via `GET /api/admin/alerts/rules`. The
 * per-row `Switch` fires `updateRule(id, { enabled })` and refetches so the
 * state reflects Hub (which is the source of truth — no optimistic UI).
 *
 * Full per-rule editing (params, severity, cooldown) lives on
 * `AlertRuleEditPage`; the Edit button navigates to `/alerts/rules/:id`.
 */
import { useCallback, useState } from 'react';
import { useNavigate } from 'react-router-dom';
import { useTranslation } from 'react-i18next';
import { useApi } from '@/hooks/useApi';
import { useDebouncedValue } from '@/hooks/useDebouncedValue';
import { useMutation } from '@/hooks/useMutation';
import { alertsApi } from '@/api/services';
import type { AlertRule, AlertSeverity } from '@/api/services';
import {
  PageHeader,
  DataTable,
  Badge,
  Button,
  Stack,
  Card,
  Switch,
  Skeleton,
  ErrorBanner,
  ListFilterToolbar,
  ListPagination,
  DEFAULT_ADMIN_LIST_PAGE_SIZE,
} from '@/components/ui';
import type { AdminListPageSize, BadgeProps, DataTableColumn } from '@/components/ui';
import styles from './AlertRulesListPage.module.css';

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

interface UpdateEnabledInput {
  id: string;
  enabled: boolean;
}

const ALL_SEVERITIES: AlertSeverity[] = ['critical', 'high', 'medium', 'low', 'info'];

export function AlertRulesListPage() {
  const { t } = useTranslation();
  const navigate = useNavigate();
  const [offset, setOffset] = useState(0);
  const [pageLimit, setPageLimit] = useState<AdminListPageSize>(DEFAULT_ADMIN_LIST_PAGE_SIZE);
  const [searchInput, setSearchInput] = useState('');
  const debouncedSearch = useDebouncedValue(searchInput, 300);
  const [enabledFilter, setEnabledFilter] = useState<'' | 'true' | 'false'>('');
  const [severityFilter, setSeverityFilter] = useState<'' | AlertSeverity>('');
  const [sourceTypeFilter, setSourceTypeFilter] = useState('');

  const resetPage = useCallback(() => setOffset(0), []);

  const { data, loading, error, refetch } = useApi(
    () => alertsApi.listRules({
      limit: pageLimit,
      offset,
      search: debouncedSearch || undefined,
      enabled: enabledFilter === '' ? undefined : enabledFilter === 'true',
      severity: severityFilter || undefined,
      sourceType: sourceTypeFilter || undefined,
    }),
    ['admin', 'alerts', 'rules', 'list', offset, pageLimit, debouncedSearch, enabledFilter, severityFilter, sourceTypeFilter],
  );

  // Distinct sourceTypes harvested from the current page. Not 100% complete
  // for paginated views, but good enough as a quick-pick — and matches the
  // builtin rule catalogue is small enough that one page typically covers
  // every source type that exists.
  const sourceTypeOptions = Array.from(new Set((data?.rules ?? []).map((r) => r.sourceType))).sort();

  const { mutate: toggleEnabled, loading: togglingEnabled } = useMutation<
    UpdateEnabledInput,
    AlertRule
  >(
    ({ id, enabled }) => alertsApi.updateRule(id, { enabled }),
    {
      onSuccess: () => refetch(),
      successMessage: t('pages:alerts.rules.toggleSuccess'),
    },
  );

  const rows = data?.rules ?? [];
  const total = data?.total ?? 0;

  const severityLabel: Record<AlertSeverity, string> = {
    critical: t('pages:alerts.rules.severities.critical'),
    high: t('pages:alerts.rules.severities.high'),
    medium: t('pages:alerts.rules.severities.medium'),
    low: t('pages:alerts.rules.severities.low'),
    info: t('pages:alerts.rules.severities.info'),
  };

  const onEdit = useCallback(
    (row: AlertRule) => {
      navigate(`/alerts/rules/${encodeURIComponent(row.id)}`);
    },
    [navigate],
  );

  if (loading && !data) return <Skeleton.ListPageSkeleton />;
  if (error) return <ErrorBanner message={error.message} onRetry={refetch} />;

  const columns: DataTableColumn<AlertRule>[] = [
    {
      key: 'id',
      label: t('pages:alerts.rules.columns.ruleId'),
      render: (r) => <code className={styles.inlineCode}>{r.id}</code>,
    },
    {
      key: 'displayName',
      label: t('pages:alerts.rules.columns.displayName'),
    },
    {
      key: 'sourceType',
      label: t('pages:alerts.rules.columns.sourceType'),
      render: (r) => <Badge variant="outline">{r.sourceType}</Badge>,
    },
    {
      key: 'defaultSeverity',
      label: t('pages:alerts.rules.columns.severity'),
      render: (r) => (
        <Badge variant={severityVariant(r.defaultSeverity)}>
          {severityLabel[r.defaultSeverity] ?? r.defaultSeverity}
        </Badge>
      ),
    },
    {
      key: 'requiresAck',
      label: t('pages:alerts.rules.columns.requiresAck'),
      render: (r) =>
        r.requiresAck ? (
          <Badge variant="warning">{t('pages:alerts.rules.requiresAckYes')}</Badge>
        ) : (
          <Badge variant="default">{t('pages:alerts.rules.requiresAckNo')}</Badge>
        ),
    },
    {
      key: 'enabled',
      label: t('pages:alerts.rules.columns.enabled'),
      sortable: false,
      render: (r) => (
        <div onClick={(e) => e.stopPropagation()}>
          <Switch
            checked={r.enabled}
            disabled={togglingEnabled}
            onCheckedChange={(next) => {
              toggleEnabled({ id: r.id, enabled: next });
            }}
          />
        </div>
      ),
    },
    {
      key: 'actions',
      label: t('pages:alerts.rules.columns.actions'),
      sortable: false,
      render: (r) => (
        <Stack direction="horizontal" gap="xs" onClick={(e) => e.stopPropagation()}>
          <Button
            variant="secondary"
            size="sm"
            onClick={(e) => {
              e.stopPropagation();
              onEdit(r);
            }}
          >
            {t('common:edit')}
          </Button>
        </Stack>
      ),
    },
  ];

  return (
    <Stack gap="lg">
      <PageHeader
        title={t('pages:alerts.rules.title')}
        subtitle={t('pages:alerts.rules.subtitle')}
      />

      <ListFilterToolbar
        searchPlaceholder={t('pages:alerts.rules.searchPlaceholder')}
        searchValue={searchInput}
        onSearchChange={(v) => { setSearchInput(v); resetPage(); }}
        meta={
          total === 0
            ? undefined
            : t('pages:alerts.rules.showingMeta', { count: rows.length, total })
        }
      >
        <select
          aria-label={t('pages:alerts.rules.filterEnabledAria')}
          value={enabledFilter}
          onChange={(e) => { setEnabledFilter(e.target.value as '' | 'true' | 'false'); resetPage(); }}
          className={styles.filterSelect}
        >
          <option value="">{t('pages:alerts.rules.filterEnabledAll')}</option>
          <option value="true">{t('pages:alerts.rules.filterEnabledOnly')}</option>
          <option value="false">{t('pages:alerts.rules.filterDisabledOnly')}</option>
        </select>
        <select
          aria-label={t('pages:alerts.rules.filterSeverityAria')}
          value={severityFilter}
          onChange={(e) => { setSeverityFilter(e.target.value as '' | AlertSeverity); resetPage(); }}
          className={styles.filterSelect}
        >
          <option value="">{t('pages:alerts.rules.filterSeverityAll')}</option>
          {ALL_SEVERITIES.map((s) => (
            <option key={s} value={s}>{severityLabel[s]}</option>
          ))}
        </select>
        <select
          aria-label={t('pages:alerts.rules.filterSourceTypeAria')}
          value={sourceTypeFilter}
          onChange={(e) => { setSourceTypeFilter(e.target.value); resetPage(); }}
          className={styles.filterSelect}
        >
          <option value="">{t('pages:alerts.rules.filterSourceTypeAll')}</option>
          {sourceTypeOptions.map((s) => (
            <option key={s} value={s}>{s}</option>
          ))}
        </select>
      </ListFilterToolbar>

      <Card padding="none">
        <DataTable
          hideSearch
          frameless
          columns={columns}
          data={rows}
          onRowClick={onEdit}
          emptyMessage={t('pages:alerts.rules.empty')}
        />
        <div style={{ padding: 'var(--g-space-0) var(--g-space-4) var(--g-space-2)' }}>
          <ListPagination
            offset={offset}
            limit={pageLimit}
            total={total}
            onOffsetChange={(v) => setOffset(v)}
            onLimitChange={(v) => { setPageLimit(v); setOffset(0); }}
          />
        </div>
      </Card>
    </Stack>
  );
}
