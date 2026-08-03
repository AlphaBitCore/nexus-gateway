/**
 * TrafficErrorsTab — the traffic-error data source of the Recent Errors
 * page. Aggregated error classes over traffic_event (errorCode ×
 * statusRange × provider × model) with a computed first-suspect
 * attribution: "ours" (gateway routing / translation / provider config),
 * "client" (caller-side), "upstream" (provider-side). Attribution is a
 * triage priority — a filter/sort dimension with one-click narrowing,
 * never an exclusion gate; client errors stay visible. Grouped
 * presentation keeps a noisy tenant from flooding the view. The sibling
 * diag_event tab keeps the system-fault role.
 */
import { useMemo, useState } from 'react';
import { useTranslation } from 'react-i18next';
import { Link } from 'react-router-dom';
import { useApi } from '@/hooks/useApi';
import { systemApi } from '@/api/services';
import type { TrafficErrorGroup, TrafficErrorGroupsResponse } from '../../../api/types';
import { Badge, Button, DataTable, ErrorBanner, Input, LoadingSpinner, Select, Stack } from '@/components/ui';
import { Sparkline } from '@/components/ui/Sparkline';
import { formatDateTime } from '@/lib/format';
import { rangeBounds } from './recentErrorsHelpers';
import styles from './InfraRecentErrorsPage.module.css';

const ATTRIBUTIONS = ['ours', 'client', 'upstream'] as const;

function attributionBadgeVariant(a: TrafficErrorGroup['attribution']): 'danger' | 'warning' | 'info' | 'default' {
  // "ours" is the loudest color: it is the bucket the operator can fix.
  if (a === 'ours') return 'danger';
  if (a === 'upstream') return 'warning';
  // "mixed" (the overflow pseudo-class) is unattributable — keep it quiet.
  if (a === 'mixed') return 'default';
  return 'info';
}

/**
 * One-shot deep-link into the Traffic page pre-filtered to this error
 * class. Carries the FULL group identity — status class, error code,
 * provider, model — plus the aggregation window (from/to), so the
 * drill-down's row count matches the group's count instead of silently
 * widening to all history. Every dimension is carried EXACTLY, absent
 * values included: empty errorCode rides the __unclassified__ sentinel,
 * empty provider/model ride __none__, and the model uses the exact-match
 * modelExact param (substring matching would swallow prefix families —
 * gpt-4o must not include gpt-4o-mini). Lands on the All source tab:
 * groups aggregate every data-plane source, so pinning a source tab
 * would empty the drill-down for proxy/agent classes.
 */
export function trafficErrorGroupLink(g: TrafficErrorGroup, from: string, to: string): string {
  const qs = new URLSearchParams({ status: g.statusRange, from, to });
  qs.set('errorCode', g.errorCode || '__unclassified__');
  qs.set('provider', g.provider || '__none__');
  qs.set('modelExact', g.model || '__none__');
  return `/traffic?${qs.toString()}`;
}

export function TrafficErrorsTab() {
  const { t } = useTranslation('pages');
  const [timeRange, setTimeRange] = useState('24h');
  const [attribution, setAttribution] = useState<'' | (typeof ATTRIBUTIONS)[number]>('');
  const [search, setSearch] = useState('');

  // Whole-second bounds: the drill-down converts these through a
  // second-precise datetime-local round-trip, so millisecond tails would
  // shift the list window off the aggregation window by a sub-second edge.
  const { from, to } = useMemo(() => {
    const b = rangeBounds(timeRange);
    return {
      from: b.from.replace(/\.\d{3}Z$/, 'Z'),
      to: b.to.replace(/\.\d{3}Z$/, 'Z'),
    };
  }, [timeRange]);

  // Fetch unfiltered; attribution narrowing happens client-side so the
  // chips flip instantly and show honest counts for every bucket.
  const groups = useApi<TrafficErrorGroupsResponse>(
    () => systemApi.listTrafficErrorGroups({ from, to }),
    ['admin', 'traffic-errors', 'groups', from, to],
  );

  const all = useMemo(() => groups.data?.data ?? [], [groups.data]);

  const counts = useMemo(() => {
    const c: Record<string, number> = { ours: 0, client: 0, upstream: 0 };
    for (const g of all) c[g.attribution] = (c[g.attribution] ?? 0) + 1;
    return c;
  }, [all]);

  const filtered = useMemo(() => {
    const term = search.trim().toLowerCase();
    return all.filter((g) => {
      if (attribution && g.attribution !== attribution) return false;
      if (
        term &&
        !g.errorCode.toLowerCase().includes(term) &&
        !g.provider.toLowerCase().includes(term) &&
        !g.model.toLowerCase().includes(term) &&
        !g.sampleReason.toLowerCase().includes(term)
      ) {
        return false;
      }
      return true;
    });
  }, [all, attribution, search]);

  if (groups.loading && !groups.data) return <LoadingSpinner />;
  if (groups.error) return <ErrorBanner message={groups.error.message} onRetry={groups.refetch} />;

  const columns = [
    {
      key: 'attribution',
      label: t('infrastructure.recentErrors.trafficErrors.colAttribution'),
      render: (g: TrafficErrorGroup) => (
        <Badge variant={attributionBadgeVariant(g.attribution)}>
          {t(`infrastructure.recentErrors.trafficErrors.attribution.${g.attribution}`)}
        </Badge>
      ),
    },
    {
      key: 'errorCode',
      label: t('infrastructure.recentErrors.trafficErrors.colErrorCode'),
      render: (g: TrafficErrorGroup) => (
        <span className={styles.codeCell} title={g.sampleReason || undefined}>
          {g.errorCode === '__overflow__'
            ? t('infrastructure.recentErrors.trafficErrors.overflow')
            : g.errorCode || t('infrastructure.recentErrors.trafficErrors.unclassified')}
        </span>
      ),
    },
    {
      key: 'statusRange',
      label: t('infrastructure.recentErrors.trafficErrors.colStatus'),
    },
    {
      key: 'provider',
      label: t('infrastructure.recentErrors.trafficErrors.colProvider'),
      render: (g: TrafficErrorGroup) => g.provider || '—',
    },
    {
      key: 'model',
      label: t('infrastructure.recentErrors.trafficErrors.colModel'),
      render: (g: TrafficErrorGroup) => g.model || '—',
    },
    {
      key: 'count',
      label: t('infrastructure.recentErrors.trafficErrors.colCount'),
      sortable: true,
    },
    {
      key: 'affectedEndUsers',
      label: t('infrastructure.recentErrors.trafficErrors.colEndUsers'),
      tooltip: t('infrastructure.recentErrors.trafficErrors.endUsersTooltip'),
      render: (g: TrafficErrorGroup) => g.affectedEndUsers ?? '—',
    },
    {
      key: 'lastSeen',
      label: t('infrastructure.recentErrors.trafficErrors.colLastSeen'),
      render: (g: TrafficErrorGroup) => formatDateTime(g.lastSeen),
    },
    {
      key: 'trend',
      label: t('infrastructure.recentErrors.trafficErrors.colTrend'),
      render: (g: TrafficErrorGroup) =>
        g.buckets.length >= 2 ? (
          <Sparkline data={g.buckets.map((b) => b.count)} width={80} height={24} />
        ) : null,
    },
    {
      key: 'actions',
      label: t('infrastructure.recentErrors.trafficErrors.colActions'),
      render: (g: TrafficErrorGroup) =>
        // The overflow pseudo-class aggregates classes shed by the rollup
        // cap — no traffic row carries its code, so a drill-down would
        // always come back empty.
        g.errorCode === '__overflow__' ? null : (
          <Link to={trafficErrorGroupLink(g, from, to)}>
            {t('infrastructure.recentErrors.trafficErrors.viewInTraffic')}
          </Link>
        ),
    },
  ];

  return (
    <Stack gap="md">
      {/* Compact control bar, typed input first (list-toolbar convention):
          search, then the time-range select, then the attribution chips. */}
      <div className={styles.filterBar}>
        <div className={styles.teSearch}>
          <Input
            type="text"
            value={search}
            onChange={(e) => setSearch(e.target.value)}
            placeholder={t('infrastructure.recentErrors.trafficErrors.searchPlaceholder')}
            aria-label={t('infrastructure.recentErrors.trafficErrors.searchPlaceholder')}
          />
        </div>
        <div className={styles.filterField}>
          <Select
            value={timeRange}
            onValueChange={setTimeRange}
            options={['1h', '24h', '7d', '30d'].map((r) => ({
              value: r,
              label: t(`infrastructure.recentErrors.range${r}`),
            }))}
          />
        </div>
        <Button
          size="sm"
          variant={attribution === '' ? 'primary' : 'secondary'}
          onClick={() => setAttribution('')}
        >
          {t('infrastructure.recentErrors.trafficErrors.attributionAll', { count: all.length })}
        </Button>
        {ATTRIBUTIONS.map((a) => (
          <Button
            key={a}
            size="sm"
            variant={attribution === a ? 'primary' : 'secondary'}
            onClick={() => setAttribution(attribution === a ? '' : a)}
            data-testid={`traffic-errors-attribution-${a}`}
          >
            {t(`infrastructure.recentErrors.trafficErrors.attribution.${a}`)} ({counts[a] ?? 0})
          </Button>
        ))}
      </div>

      {filtered.length === 0 ? (
        <div className={styles.empty} data-testid="traffic-errors-empty">
          {t('infrastructure.recentErrors.trafficErrors.empty')}
        </div>
      ) : (
        <DataTable
          hideSearch
          columns={columns}
          data={filtered}
          emptyMessage={t('infrastructure.recentErrors.trafficErrors.empty')}
        />
      )}
    </Stack>
  );
}
