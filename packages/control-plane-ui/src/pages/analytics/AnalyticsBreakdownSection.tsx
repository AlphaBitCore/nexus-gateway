import { useState, useMemo, useCallback, useRef, useEffect } from 'react';
import { useTranslation } from 'react-i18next';
import { analyticsApi } from '@/api/services';
import { useApi } from '../../hooks/useApi';
import { Card, DataTable, Button, type DataTableColumn } from '@/components/ui';
import type { CostData, UsageData } from '../../api/types';
import styles from './AnalyticsPage.module.css';
import { formatTokens } from '@/lib/format';

/* ── CSV export helper ───────────────────────────────────────────────────── */

function downloadCsv(rows: BreakdownRow[], columns: DataTableColumn<BreakdownRow>[], filename: string) {
  const header = columns.map((c) => c.label).join(',');
  const body = rows.map((r) =>
    columns.map((c) => {
      const val = r[c.key as keyof BreakdownRow];
      const str = String(val ?? '');
      return str.includes(',') || str.includes('"') ? `"${str.replace(/"/g, '""')}"` : str;
    }).join(','),
  ).join('\n');
  const blob = new Blob([header + '\n' + body], { type: 'text/csv' });
  const url = URL.createObjectURL(blob);
  const a = document.createElement('a');
  a.href = url;
  a.download = filename;
  a.click();
  URL.revokeObjectURL(url);
}

/* ── Breakdown table ─────────────────────────────────────────────────────── */

interface BreakdownRow {
  name: string;
  extra: string;
  requestCount: number;
  totalTokens: number;
  totalCostUsd: number;
  cacheHitRate?: number;
  cacheSavingsUsd?: number;
}

function makeBaseColumns(t: (key: string) => string): DataTableColumn<BreakdownRow>[] {
  return [
    { key: 'requestCount', label: t('pages:traffic.colRequests'), sortable: true, render: (r) => (r.requestCount ?? 0).toLocaleString() },
    { key: 'totalTokens', label: t('pages:traffic.colTokens'), sortable: true, render: (r) => formatTokens(r.totalTokens ?? 0) },
    { key: 'totalCostUsd', label: t('pages:traffic.colCostUsd'), sortable: true, render: (r) => `$${(r.totalCostUsd ?? 0).toFixed(4)}` },
    {
      key: 'cacheHitRate', label: t('pages:analytics.colCacheHitRate'), sortable: true,
      render: (r) => r.cacheHitRate != null ? `${(r.cacheHitRate * 100).toFixed(1)}%` : '—',
    },
    {
      key: 'cacheSavingsUsd', label: t('pages:analytics.colCacheSavings'), sortable: true,
      render: (r) => r.cacheSavingsUsd != null && r.cacheSavingsUsd > 0
        ? <span className={styles.savingsValue}>${r.cacheSavingsUsd.toFixed(4)}</span>
        : '—',
    },
  ];
}

function makeColumns(nameLabel: string, t: (key: string) => string, extraLabel?: string): DataTableColumn<BreakdownRow>[] {
  const cols: DataTableColumn<BreakdownRow>[] = [
    { key: 'name', label: nameLabel, sortable: true },
  ];
  if (extraLabel) {
    cols.push({ key: 'extra', label: extraLabel, sortable: true });
  }
  return [...cols, ...makeBaseColumns(t)];
}

function mergeRows(costData?: CostData[], usageData?: UsageData[]): BreakdownRow[] {
  const map = new Map<string, BreakdownRow>();
  for (const c of costData ?? []) {
    const hits = c.cacheHitCount ?? 0;
    const reqs = c.requestCount ?? 0;
    const gatewaySavings = c.gatewayCacheSavingsUsd ?? 0;
    const l4Savings = c.cacheNetSavingsUsd ?? 0;
    const totalSavings = gatewaySavings + l4Savings;
    map.set(c.group, {
      name: c.groupLabel || c.group,
      extra: c.groupExtra ?? '',
      requestCount: reqs,
      totalTokens: c.totalTokens ?? 0,
      totalCostUsd: c.totalCostUsd ?? 0,
      cacheHitRate: reqs > 0 && hits > 0 ? hits / reqs : undefined,
      cacheSavingsUsd: totalSavings > 0 ? totalSavings : undefined,
    });
  }
  for (const u of usageData ?? []) {
    const existing = map.get(u.group);
    if (existing) {
      existing.totalTokens = u.totalTokens ?? 0;
      existing.requestCount = u.requestCount ?? 0;
      if (!existing.extra && u.groupExtra) existing.extra = u.groupExtra;
    } else {
      map.set(u.group, {
        name: u.groupLabel || u.group,
        extra: u.groupExtra ?? '',
        requestCount: u.requestCount ?? 0,
        totalTokens: u.totalTokens ?? 0,
        totalCostUsd: 0,
      });
    }
  }
  return Array.from(map.values());
}

export function BreakdownTable({ title, nameLabel, extraLabel, groupByKey, timeParams, searchPlaceholder }: {
  title: string;
  nameLabel: string;
  extraLabel?: string;
  groupByKey: string;
  timeParams: Record<string, string>;
  searchPlaceholder?: string;
}) {
  const { t } = useTranslation();
  const [search, setSearch] = useState('');
  const [debouncedSearch, setDebouncedSearch] = useState('');
  const debounceRef = useRef<ReturnType<typeof setTimeout>>(undefined);

  useEffect(() => {
    clearTimeout(debounceRef.current);
    debounceRef.current = setTimeout(() => setDebouncedSearch(search), 300);
    return () => clearTimeout(debounceRef.current);
  }, [search]);

  const queryParams = useMemo(() => ({
    groupBy: groupByKey,
    ...timeParams,
    ...(debouncedSearch && { q: debouncedSearch }),
    limit: '50',
  }), [groupByKey, timeParams, debouncedSearch]);

  const { data: costData } = useApi<{ data: CostData[] }>(
    () => analyticsApi.cost(queryParams),
    ['admin', 'analytics', 'cost', groupByKey, timeParams.startTime, timeParams.endTime, debouncedSearch],
  );
  const { data: usageData } = useApi<{ data: UsageData[] }>(
    () => analyticsApi.usage(queryParams),
    ['admin', 'analytics', 'usage', groupByKey, timeParams.startTime, timeParams.endTime, debouncedSearch],
  );

  const rows = useMemo(() => mergeRows(costData?.data, usageData?.data), [costData, usageData]);
  const columns = useMemo(() => makeColumns(nameLabel, t, extraLabel), [nameLabel, t, extraLabel]);

  const handleExport = useCallback(async () => {
    // Fetch all data (no limit) for export
    const [allCost, allUsage] = await Promise.all([
      analyticsApi.cost({ groupBy: groupByKey, ...timeParams }) as Promise<{ data: CostData[] }>,
      analyticsApi.usage({ groupBy: groupByKey, ...timeParams }) as Promise<{ data: UsageData[] }>,
    ]);
    const allRows = mergeRows(allCost.data, allUsage.data);
    downloadCsv(allRows, columns, `${title.replace(/\s+/g, '-').toLowerCase()}.csv`);
  }, [groupByKey, timeParams, columns, title]);

  return (
    <Card className={styles.tableCard}>
      <div className={styles.tableHeader}>
        <h3 className={styles.tableTitle}>{title}</h3>
        <div className={styles.tableActions}>
          <input
            type="text"
            placeholder={searchPlaceholder ?? t('pages:traffic.searchPlaceholder', { name: nameLabel.toLowerCase() })}
            value={search}
            onChange={(e) => setSearch(e.target.value)}
            className={styles.searchInput}
          />
          <Button variant="secondary" size="sm" onClick={handleExport}>
            {t('pages:traffic.exportCsv')}
          </Button>
        </div>
      </div>
      <DataTable
        columns={columns}
        data={rows}
        pageSize={10}
        hideSearch
        frameless
        emptyMessage={t('pages:traffic.noBreakdownData', { title: title.toLowerCase() })}
      />
    </Card>
  );
}
