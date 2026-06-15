import { type ReactNode } from 'react';
import { Card, ExpandableWrapper } from '@/components/ui';
import { formatDateTime, formatUsd } from '@/lib/format';
import styles from './MetricsRollupsSection.module.css';

export function rangeToIso(hours: number): { start: string; end: string } {
  const end = new Date();
  const start = new Date(end.getTime() - hours * 3600_000);
  return { start: start.toISOString(), end: end.toISOString() };
}

type RollupTooltipRow = { bucketAt?: string };

export function tooltipBucketLabel(_label: unknown, payload: readonly unknown[]): string {
  const row = (payload?.[0] as { payload?: RollupTooltipRow } | undefined)?.payload;
  const at = row?.bucketAt;
  if (typeof at === 'string') return formatDateTime(at);
  return '';
}

export function formatLatencyTooltip(value: unknown, name: unknown): [string, string] {
  const n = typeof value === 'number' ? value : Number(value);
  return [`${Number.isFinite(n) ? n : 0} ms`, String(name ?? 'p50')];
}

export function formatUsdBySeriesTooltip(value: unknown, name: unknown): [string, string] {
  const n = typeof value === 'number' ? value : Number(value);
  return [formatUsd(Number.isFinite(n) ? n : 0), String(name ?? '')];
}

export function StatCard({ label, value, subtitle }: { label: string; value: ReactNode; subtitle?: string }) {
  return (
    <Card className={styles.statCard}>
      <div className={styles.statLabel}>{label}</div>
      <div className={styles.statValue}>{value}</div>
      {subtitle && <div className={styles.statSubtitle}>{subtitle}</div>}
    </Card>
  );
}

export function ChartPanel({
  title,
  subtitle,
  empty,
  emptyText,
  children,
}: {
  title: string;
  subtitle?: string;
  empty?: boolean;
  emptyText?: string;
  children: ReactNode;
}) {
  return (
    <ExpandableWrapper>
      <Card className={styles.chartPanel}>
        <div className={styles.chartPanelHeader}>
          <h2 className={styles.chartPanelTitle}>{title}</h2>
          {subtitle ? (
            <p className={styles.chartPanelSubtitle}>{subtitle}</p>
          ) : null}
        </div>
        {empty ? <p className={styles.chartPanelEmpty}>{emptyText}</p> : children}
      </Card>
    </ExpandableWrapper>
  );
}
