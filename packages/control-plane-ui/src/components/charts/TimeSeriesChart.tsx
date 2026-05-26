/**
 * TimeSeriesChart — thin Recharts `LineChart` wrapper used by the Node detail
 * Metrics tab (and future small-multiples surfaces).
 *
 * The component owns chart rendering only — callers shape the data (see
 * `Node detail Metrics tab`) and pick the metric / unit. We keep the API
 * intentionally narrow:
 *
 *   - `data`     : pre-shaped `{ bucket, value }` rows in chronological order.
 *   - `label`    : series legend / tooltip name.
 *   - `unit`     : enables built-in axis humanizers (`bytes`, `ms`, `count`).
 *   - `loading`  : show inline spinner instead of an empty grid.
 *   - `error`    : show inline error message instead of an empty grid.
 *
 * Empty / loading / error states are rendered in-place at the same height as
 * the chart so the surrounding small-multiples grid does not jump as charts
 * resolve at different speeds.
 */
import { useTranslation } from 'react-i18next';
import { useContext, useMemo } from 'react';
import {
  CartesianGrid, Line, LineChart, ResponsiveContainer, Tooltip, XAxis, YAxis,
} from 'recharts';

import { ThemeContext } from '@/theme/ThemeProvider';
import {
  getAxisTickStyle, getGridStroke, getSemanticColor, getTooltipStyle,
} from '@nexus-gateway/ui-shared';
import { LoadingSpinner } from '@/components/ui/LoadingSpinner';
import styles from './TimeSeriesChart.module.css';

/** Known semantic units the axis formatter understands. */
export type TimeSeriesUnit = 'bytes' | 'ms' | 'count' | 'seconds' | 'percent' | string;

export interface TimeSeriesPoint {
  /** ISO timestamp for the X axis tick. */
  bucket: string;
  /** Already-resolved scalar (gauge value, counter delta, or histogram-derived). */
  value: number;
}

export interface TimeSeriesChartProps {
  /** Already-shaped, chronologically sorted rows. */
  data: TimeSeriesPoint[];
  /** Series legend / tooltip name. */
  label: string;
  /** Optional unit for axis humanization. Falls back to a raw number formatter. */
  unit?: TimeSeriesUnit;
  /** Pixel height of the chart area. Defaults to 160 to fit small-multiples grids. */
  height?: number;
  /** When true, swaps the chart for an inline spinner at the same height. */
  loading?: boolean;
  /** When non-null, swaps the chart for an inline error message at the same height. */
  error?: Error | null;
}

const KIB = 1024;
const MIB = KIB * 1024;
const GIB = MIB * 1024;

/** Format an arbitrary scalar against a unit, returning a short human string. */
export function formatValue(value: number, unit?: TimeSeriesUnit): string {
  if (!Number.isFinite(value)) return '—';
  switch (unit) {
    case 'bytes': {
      const abs = Math.abs(value);
      if (abs >= GIB) return `${(value / GIB).toFixed(2)} GiB`;
      if (abs >= MIB) return `${(value / MIB).toFixed(2)} MiB`;
      if (abs >= KIB) return `${(value / KIB).toFixed(1)} KiB`;
      return `${value.toFixed(0)} B`;
    }
    case 'ms':
      if (Math.abs(value) >= 1000) return `${(value / 1000).toFixed(2)} s`;
      return `${value.toFixed(value < 10 ? 2 : 0)} ms`;
    case 'seconds':
      if (Math.abs(value) >= 3600) return `${(value / 3600).toFixed(2)} h`;
      if (Math.abs(value) >= 60) return `${(value / 60).toFixed(1)} min`;
      return `${value.toFixed(value < 10 ? 2 : 0)} s`;
    case 'percent':
      return `${value.toFixed(1)}%`;
    case 'count':
    default:
      if (Math.abs(value) >= 1_000_000) return `${(value / 1_000_000).toFixed(2)}M`;
      if (Math.abs(value) >= 1_000) return `${(value / 1_000).toFixed(1)}k`;
      return value.toFixed(value < 10 && value !== Math.floor(value) ? 2 : 0);
  }
}

/** Compact local-time string for X-axis ticks. */
function formatBucketTick(iso: string): string {
  try {
    const d = new Date(iso);
    return d.toLocaleString(undefined, {
      month: 'short',
      day: 'numeric',
      hour: '2-digit',
      minute: '2-digit',
    });
  } catch {
    return iso;
  }
}

export function TimeSeriesChart({
  data,
  label,
  unit,
  height = 160,
  loading,
  error,
}: TimeSeriesChartProps) {
  const { t } = useTranslation();
  // Resolve theme without throwing when no ThemeProvider is mounted (test
  // wrappers, isolated component renders) — fall back to the light palette.
  const themeCtx = useContext(ThemeContext);
  const resolvedMode = themeCtx?.resolvedMode ?? 'light';
  const stroke = getSemanticColor(resolvedMode, 'requests');
  const gridStroke = getGridStroke(resolvedMode);
  const tickStyle = getAxisTickStyle(resolvedMode);
  const tooltipStyle = getTooltipStyle(resolvedMode);

  const valueTickFormatter = useMemo(
    () => (v: number) => formatValue(v, unit),
    [unit],
  );

  if (loading) {
    return (
      <div
        className={styles.chartState}
        style={{ height }}
        role="status"
        aria-label={t('common:loading', 'Loading')}
      >
        <LoadingSpinner />
      </div>
    );
  }

  if (error) {
    return (
      <div
        className={styles.chartStateError}
        style={{ height }}
        role="alert"
      >
        {error.message}
      </div>
    );
  }

  if (!data || data.length === 0) {
    return (
      <div className={styles.chartStateEmpty} style={{ height }}>
        {t('pages:infrastructure.metricsTab.noDataInWindow')}
      </div>
    );
  }

  return (
    <ResponsiveContainer width="100%" height={height}>
      <LineChart data={data} margin={{ top: 8, right: 16, left: 0, bottom: 0 }}>
        <CartesianGrid strokeDasharray="3 3" stroke={gridStroke} opacity={0.5} />
        <XAxis
          dataKey="bucket"
          tick={tickStyle}
          tickFormatter={formatBucketTick}
          minTickGap={32}
        />
        <YAxis
          tick={tickStyle}
          tickFormatter={valueTickFormatter}
          width={64}
        />
        <Tooltip
          contentStyle={tooltipStyle}
          labelFormatter={(v) => formatBucketTick(String(v))}
          formatter={(value) => {
            const n = typeof value === 'number' ? value : Number(value);
            return [formatValue(n, unit), label] as [string, string];
          }}
        />
        <Line
          type="monotone"
          dataKey="value"
          name={label}
          stroke={stroke}
          dot={false}
          isAnimationActive={false}
          strokeWidth={2}
        />
      </LineChart>
    </ResponsiveContainer>
  );
}

export default TimeSeriesChart;
