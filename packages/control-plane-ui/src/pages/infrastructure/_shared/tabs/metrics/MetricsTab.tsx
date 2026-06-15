/**
 * MetricsTab — Nodes detail "Metrics" tab.
 *
 * Layout (per spec §11.6):
 *
 *   ┌── Time range ───────────────────────────────────────────┐
 *   │ [1h] [6h] [1d] [7d] [30d] [1y]   ← retention-aware      │
 *   └─────────────────────────────────────────────────────────┘
 *   ┌── Runtime ─────────────────────────────────────────────┐
 *   │ heap_alloc · goroutines · gc_pause_p50 · cpu · uptime  │
 *   └─────────────────────────────────────────────────────────┘
 *   ┌── Business ─────────────────────────────────────────────┐
 *   │ per-thing-type catalog (§6.3)                           │
 *   └─────────────────────────────────────────────────────────┘
 *
 * The selected time range lives in URL query params `from` / `to` (ISO 8601)
 * so it survives tab switches and is shared with the Logs tab (spec §11.8 —
 * "operator spots a heap spike at 14:32 and immediately sees the relevant
 * ERROR events at the same time"). Range buttons rewrite both params at once.
 *
 * Retention awareness: on mount we fetch the 11-layer retention config
 * (T39's `retentionApi.get`) and gray out range buttons whose window exceeds
 * the relevant tier — for example, when `runtime_1h` = 90 days the "1y"
 * button is disabled because no buckets older than 90 days exist.
 *
 * Histogram metrics from §6.3 (`hook.pipeline_ms`, `tls.handshake_ms`,
 * etc.) are reduced client-side to a p50 line via the bucket layout from
 * §6.4 (boundaries `[50, 100, 200, 500, 1000]` ms), so every chart on this
 * tab renders as a single-line `TimeSeriesChart`.
 */
import { useMemo } from 'react';
import { useTranslation } from 'react-i18next';
import { useSearchParams } from 'react-router-dom';

import { useApi } from '@/hooks/useApi';
import { opsMetricsApi } from '@/api/services/infrastructure/ops/opsmetrics';
import type {
  OpsMetricBucket, OpsMetricsTimeseriesParams, TimeseriesResponse,
} from '@/api/services/infrastructure/ops/opsmetrics';
import { retentionApi } from '@/api/services/infrastructure/ops/retention';
import type { RetentionGetResponse } from '@/api/services/infrastructure/ops/retention';
import { Card, Stack, Button, ErrorBanner } from '@/components/ui';
import { TimeSeriesChart } from '@/components/charts/TimeSeriesChart';
import type {
  TimeSeriesPoint, TimeSeriesUnit,
} from '@/components/charts/TimeSeriesChart';
import styles from './MetricsTab.module.css';

/** Range presets, including the maximum lookback in milliseconds. */
const RANGE_PRESETS: ReadonlyArray<{ key: string; ms: number; labelKey: string }> = [
  { key: '1h', ms: 60 * 60 * 1000, labelKey: 'range1h' },
  { key: '6h', ms: 6 * 60 * 60 * 1000, labelKey: 'range6h' },
  { key: '1d', ms: 24 * 60 * 60 * 1000, labelKey: 'range1d' },
  { key: '7d', ms: 7 * 24 * 60 * 60 * 1000, labelKey: 'range7d' },
  { key: '30d', ms: 30 * 24 * 60 * 60 * 1000, labelKey: 'range30d' },
  { key: '1y', ms: 365 * 24 * 60 * 60 * 1000, labelKey: 'range1y' },
];

/** Default range when the URL has no `from` / `to` — last 1h, per spec §11.6. */
const DEFAULT_RANGE = RANGE_PRESETS[0];

/** Boundary set for histogram bucket interpolation (spec §6.4). */
const HIST_BOUNDARIES_MS = [50, 100, 200, 500, 1000];

/** Linear interpolation of p50 from the §6.4 6-bucket histogram layout. */
export function bucketToP50(buckets: number[], boundariesMs: number[] = HIST_BOUNDARIES_MS): number {
  const total = buckets.reduce((a, b) => a + b, 0);
  if (total === 0) return 0;
  const target = total / 2;
  let cum = 0;
  for (let i = 0; i < buckets.length; i += 1) {
    cum += buckets[i];
    if (cum >= target) {
      const lower = i === 0 ? 0 : boundariesMs[i - 1];
      const upper = boundariesMs[i] ?? boundariesMs[boundariesMs.length - 1] * 2;
      return lower + (upper - lower) * ((target - (cum - buckets[i])) / Math.max(buckets[i], 1));
    }
  }
  return boundariesMs[boundariesMs.length - 1] * 2;
}

/** One chart definition — drives the per-metric `useApi` calls below. */
interface MetricChart {
  metric: string;
  labelKey: string;
  unit?: TimeSeriesUnit;
  /** Optional dimension filter for §6.3 metrics that have multiple series. */
  dim?: string;
  /** When true, treat the bucket payload as a histogram and reduce to p50 ms. */
  histogram?: boolean;
}

/** L1 runtime catalog (spec §6.1) — five small-multiples charts. */
const RUNTIME_CHARTS: ReadonlyArray<MetricChart> = [
  { metric: 'runtime.heap_alloc_bytes',         labelKey: 'runtime.heapAlloc',     unit: 'bytes' },
  { metric: 'runtime.goroutines',               labelKey: 'runtime.goroutines',    unit: 'count' },
  { metric: 'runtime.gc_pause_p50_ms',          labelKey: 'runtime.gcPauseP50',    unit: 'ms' },
  { metric: 'runtime.cpu_user_seconds_total',   labelKey: 'runtime.cpuUser',       unit: 'seconds' },
  { metric: 'runtime.uptime_seconds',           labelKey: 'runtime.uptime',        unit: 'seconds' },
];

/**
 * Per-thing-type business catalog (spec §6.3). Each entry is the canonical set
 * surfaced on this tab — operators who want the full catalog go to the metrics
 * explorer. Histograms are reduced to a p50 line per §6.4.
 */
const BUSINESS_CHARTS_BY_TYPE: Record<string, ReadonlyArray<MetricChart>> = {
  'nexus-hub': [
    { metric: 'things.connected',          labelKey: 'hub.thingsConnected',    unit: 'count' },
    { metric: 'ws.messages_total',         labelKey: 'hub.wsMessages',         unit: 'count' },
    { metric: 'jobs.duration_ms',          labelKey: 'hub.jobsDuration',       unit: 'ms', histogram: true },
    { metric: 'mq.lag_messages',           labelKey: 'hub.mqLag',              unit: 'count' },
    { metric: 'shadow.drift_things',       labelKey: 'hub.shadowDrift',        unit: 'count' },
    { metric: 'db.query_ms',               labelKey: 'hub.dbQuery',            unit: 'ms', histogram: true },
  ],
  'control-plane': [
    { metric: 'http.requests_total',       labelKey: 'cp.httpRequests',        unit: 'count' },
    { metric: 'http.duration_ms',          labelKey: 'cp.httpDuration',        unit: 'ms', histogram: true },
    { metric: 'auth.attempts_total',       labelKey: 'cp.authAttempts',        unit: 'count' },
    { metric: 'sessions.active',           labelKey: 'cp.sessionsActive',      unit: 'count' },
    { metric: 'hub_api.duration_ms',       labelKey: 'cp.hubApiDuration',      unit: 'ms', histogram: true },
    { metric: 'db.query_ms',               labelKey: 'cp.dbQuery',             unit: 'ms', histogram: true },
  ],
  'ai-gateway': [
    { metric: 'cache.hits_total',          labelKey: 'ai.cacheHits',           unit: 'count' },
    { metric: 'cache.misses_total',        labelKey: 'ai.cacheMisses',         unit: 'count' },
    { metric: 'streams.active',            labelKey: 'ai.streamsActive',       unit: 'count' },
    { metric: 'provider.call_ms',          labelKey: 'ai.providerCall',        unit: 'ms', histogram: true },
    { metric: 'hook.pipeline_ms',          labelKey: 'ai.hookPipeline',        unit: 'ms', histogram: true },
    { metric: 'routing.decisions_total',   labelKey: 'ai.routingDecisions',    unit: 'count' },
  ],
  'compliance-proxy': [
    { metric: 'tunnels.active',            labelKey: 'cp_proxy.tunnelsActive', unit: 'count' },
    { metric: 'tls.handshake_ms',          labelKey: 'cp_proxy.tlsHandshake',  unit: 'ms', histogram: true },
    { metric: 'cert_cache.size',           labelKey: 'cp_proxy.certCacheSize', unit: 'count' },
    { metric: 'hook.pipeline_ms',          labelKey: 'cp_proxy.hookPipeline',  unit: 'ms', histogram: true },
    { metric: 'buffer.bytes_active',       labelKey: 'cp_proxy.bufferActive',  unit: 'bytes' },
    { metric: 'bytes_proxied_total',       labelKey: 'cp_proxy.bytesProxied',  unit: 'bytes' },
  ],
  agent: [
    { metric: 'interception.state',        labelKey: 'agent.interception',     unit: 'count' },
    { metric: 'connections.active',        labelKey: 'agent.connectionsActive', unit: 'count' },
    { metric: 'requests.total',            labelKey: 'agent.requests',         unit: 'count' },
    { metric: 'relay.dial_total',          labelKey: 'agent.relayDial',        unit: 'count' },
    { metric: 'audit.queue_depth',         labelKey: 'agent.auditQueue',       unit: 'count' },
    { metric: 'audit.uploads_total',       labelKey: 'agent.auditUploads',     unit: 'count' },
  ],
};

/** Map a thing type to its retention layer pair: raw + 1h cap. */
function retentionCap(retention: RetentionGetResponse | null, tier: 'runtime' | 'business'): {
  rawDays: number; oneHourDays: number; oneDayDays: number; oneMonthDays: number;
} {
  const r = retention?.retention ?? {};
  const get = (k: string, d: number) => (r[k]?.value ?? d);
  if (tier === 'runtime') {
    return {
      rawDays: get('runtime_5m', 7),
      oneHourDays: get('runtime_1h', 90),
      oneDayDays: get('runtime_1d', 365),
      oneMonthDays: get('runtime_1mo', 1825),
    };
  }
  return {
    rawDays: get('business_5m', 7),
    oneHourDays: get('business_1h', 90),
    oneDayDays: get('business_1d', 365),
    oneMonthDays: get('business_1mo', 1825),
  };
}

/** True when a range window in ms exceeds the longest retained layer. */
export function isRangeBeyondRetention(rangeMs: number, capDays: number): boolean {
  return rangeMs > capDays * 24 * 60 * 60 * 1000;
}

/** Reduce a `TimeseriesResponse` payload to `{ bucket, value }` rows. */
function shapeSeries(resp: TimeseriesResponse | null, histogram: boolean): TimeSeriesPoint[] {
  if (!resp) return [];
  return resp.data.map((b: OpsMetricBucket) => {
    if (histogram) {
      const buckets = (b.metadata?.buckets as number[] | undefined) ?? [];
      return { bucket: b.bucketStart, value: bucketToP50(buckets) };
    }
    // gauge → valueAvg; counter → valueSum; raw single-row fallback uses min/max if both null.
    const v = b.valueAvg ?? b.valueSum ?? b.valueMax ?? b.valueMin ?? 0;
    return { bucket: b.bucketStart, value: typeof v === 'number' ? v : Number(v) };
  });
}

/**
 * One chart cell — pulls its own series, shapes it, renders the chart.
 *
 * Co-locating the fetch with the cell keeps the parent's `useApi` count
 * predictable (one per cell) and lets recharts errors stay scoped to the cell
 * that produced them.
 */
function MetricCell({ thingId, chart, from, to }: {
  thingId: string;
  chart: MetricChart;
  from: string;
  to: string;
}) {
  const { t } = useTranslation();
  const params = useMemo<OpsMetricsTimeseriesParams>(() => ({
    nodeId: thingId,
    metric: chart.metric,
    from,
    to,
    dim: chart.dim,
    granularity: 'auto',
  }), [thingId, chart.metric, chart.dim, from, to]);

  const { data, loading, error } = useApi<TimeseriesResponse>(
    () => opsMetricsApi.timeseries(params),
    ['admin', 'ops-metrics', 'timeseries', thingId, chart.metric, chart.dim ?? '', from, to],
    { skip: !thingId },
  );

  const points = useMemo(() => shapeSeries(data, !!chart.histogram), [data, chart.histogram]);
  const label = t(`pages:infrastructure.metricsTab.${chart.labelKey}`);

  return (
    <div className={styles.cell}>
      <div className={styles.cellTitle}>{label}</div>
      <TimeSeriesChart
        data={points}
        label={label}
        unit={chart.histogram ? 'ms' : chart.unit}
        loading={loading}
        error={error}
      />
    </div>
  );
}

export interface MetricsTabProps {
  thingId: string;
  thingType: string;
}

export function MetricsTab({ thingId, thingType }: MetricsTabProps) {
  const { t } = useTranslation();
  const [searchParams, setSearchParams] = useSearchParams();

  // Resolve the active range from URL params, falling back to the default
  // window. We always keep `from` and `to` together — partial URLs reset.
  const fromParam = searchParams.get('from');
  const toParam = searchParams.get('to');
  const { from, to } = useMemo(() => {
    if (fromParam && toParam) return { from: fromParam, to: toParam };
    const now = Date.now();
    return {
      from: new Date(now - DEFAULT_RANGE.ms).toISOString(),
      to: new Date(now).toISOString(),
    };
  }, [fromParam, toParam]);

  // Retention config drives the gray-out logic on the time-range buttons.
  const { data: retention, error: retentionError } = useApi<RetentionGetResponse>(
    () => retentionApi.get(),
    ['admin', 'ops-metrics', 'retention'],
  );

  // We chose the runtime cap as the gating tier for the chrome — runtime is
  // emitted on every Thing type, while a few business caps are tier-specific.
  const runtimeCap = useMemo(() => retentionCap(retention, 'runtime'), [retention]);
  const longestRuntimeDays = Math.max(
    runtimeCap.rawDays,
    runtimeCap.oneHourDays,
    runtimeCap.oneDayDays,
    runtimeCap.oneMonthDays,
  );

  const rangeButtonDisabled = (rangeMs: number) =>
    isRangeBeyondRetention(rangeMs, longestRuntimeDays);

  const setRange = (rangeMs: number) => {
    const now = Date.now();
    const next = new URLSearchParams(searchParams);
    next.set('from', new Date(now - rangeMs).toISOString());
    next.set('to', new Date(now).toISOString());
    setSearchParams(next, { replace: true });
  };

  // Pick the active button by approximating `to - from`. We snap to the
  // closest preset within ~5% so a re-rendered absolute window still
  // highlights its origin button.
  const activeRangeKey = useMemo(() => {
    const span = new Date(to).getTime() - new Date(from).getTime();
    let best = DEFAULT_RANGE.key;
    let bestDelta = Infinity;
    for (const r of RANGE_PRESETS) {
      const delta = Math.abs(r.ms - span);
      if (delta < bestDelta) {
        best = r.key;
        bestDelta = delta;
      }
    }
    return bestDelta <= span * 0.05 ? best : null;
  }, [from, to]);

  const businessCharts = BUSINESS_CHARTS_BY_TYPE[thingType] ?? [];

  return (
    <Stack gap="lg">
      {/* ── Time range chrome ── */}
      <Card>
        <div className={styles.rangeBar}>
          <span className={styles.rangeLabel}>
            {t('pages:infrastructure.metricsTab.timeRange')}
          </span>
          <div className={styles.rangeButtons}>
            {RANGE_PRESETS.map((r) => {
              const disabled = rangeButtonDisabled(r.ms);
              const isActive = activeRangeKey === r.key;
              return (
                <Button
                  key={r.key}
                  type="button"
                  size="sm"
                  variant={isActive ? 'primary' : 'secondary'}
                  disabled={disabled}
                  onClick={() => setRange(r.ms)}
                  aria-pressed={isActive}
                  title={disabled ? t('pages:infrastructure.metricsTab.rangeBeyondRetention') : undefined}
                >
                  {t(`pages:infrastructure.metricsTab.${r.labelKey}`)}
                </Button>
              );
            })}
          </div>
        </div>
        {retentionError ? (
          <ErrorBanner error={retentionError} />
        ) : null}
      </Card>

      {/* ── Runtime small-multiples ── */}
      <Card>
        <h2 className={styles.sectionTitle}>
          {t('pages:infrastructure.metricsTab.runtimeHeading')}
        </h2>
        <div className={styles.smallMultiples}>
          {RUNTIME_CHARTS.map((chart) => (
            <MetricCell
              key={chart.metric}
              thingId={thingId}
              chart={chart}
              from={from}
              to={to}
            />
          ))}
        </div>
      </Card>

      {/* ── Business per-type ── */}
      <Card>
        <h2 className={styles.sectionTitle}>
          {t('pages:infrastructure.metricsTab.businessHeading')}
        </h2>
        {businessCharts.length === 0 ? (
          <p className={styles.emptyText}>
            {t('pages:infrastructure.metricsTab.noBusinessCatalog')}
          </p>
        ) : (
          <div className={styles.smallMultiples}>
            {businessCharts.map((chart) => (
              <MetricCell
                key={`${chart.metric}:${chart.dim ?? ''}`}
                thingId={thingId}
                chart={chart}
                from={from}
                to={to}
              />
            ))}
          </div>
        )}
      </Card>
    </Stack>
  );
}

export default MetricsTab;
