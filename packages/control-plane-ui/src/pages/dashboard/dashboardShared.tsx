import { Tooltip } from '@/components/ui';
import styles from './DashboardPage.module.css';

/* ── Time window ────────────────────────────────────────────────────────── */

export type TimeWindow = '1h' | '1d' | '7d' | '30d';

export const WINDOW_MS: Record<TimeWindow, number> = {
  '1h':  60 * 60_000,
  '1d':  24 * 60 * 60_000,
  '7d':  7  * 24 * 60 * 60_000,
  '30d': 30 * 24 * 60 * 60_000,
};

export const WINDOW_OPTIONS: TimeWindow[] = ['1h', '1d', '7d', '30d'];

/* ── Traffic source ─────────────────────────────────────────────────────── */

// The live traffic sources the analytics endpoints can filter by. NOT a
// benchmark/environment dimension — synthetic benchmark runs live on the
// separate Benchmarks surface, and there is no environment column in the
// analytics rollups, so neither is offered here (would be a dead control).
export type TrafficSource = 'all' | 'vk' | 'proxy' | 'agent';

export const SOURCE_OPTIONS: TrafficSource[] = ['all', 'vk', 'proxy', 'agent'];

// Rollup endpoints (summary, by-provider) accept ?source=vk|proxy|agent
// (empty = all) per sourceSubDimension() in analytics_rollup.go.
export function sourceToRollupParam(s: TrafficSource): Record<string, string> {
  return s === 'all' ? {} : { source: s };
}

// latency-phases accepts ?source=ai-gateway|compliance-proxy|agent|all per
// analytics_latency.go — a different vocabulary from the rollup endpoints.
export function sourceToLatencyParam(s: TrafficSource): 'all' | 'ai-gateway' | 'compliance-proxy' | 'agent' {
  switch (s) {
    case 'vk': return 'ai-gateway';
    case 'proxy': return 'compliance-proxy';
    case 'agent': return 'agent';
    default: return 'all';
  }
}

/* ── Info icon ──────────────────────────────────────────────────────────── */

export function InfoIcon({ description }: { description: string }) {
  return (
    <Tooltip content={description} side="bottom">
      <span className={styles.infoIcon}>i</span>
    </Tooltip>
  );
}
