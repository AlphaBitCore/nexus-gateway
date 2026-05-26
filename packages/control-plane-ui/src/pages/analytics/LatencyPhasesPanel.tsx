/**
 * Analytics → Latency tab.
 *
 * Calls /api/admin/analytics/latency-phases with the page's current time
 * range + source filter and renders:
 *   - 3 KPI cards (Our Overhead P95 / Upstream TTFB P95 / Upstream Total P95)
 *     across the busiest provider in the window.
 *   - Provider Leaderboard table sorted by P95 Upstream Total descending —
 *     answers "which provider is slow right now" at a glance.
 *
 * Stacked-area time-series + groupBy-aware breakdown are deferred to a
 * follow-on; the headline KPIs + leaderboard already deliver the
 * "Us vs Upstream" story this tab exists for.
 */
import { useTranslation } from 'react-i18next';
import { BarChart, Bar, XAxis, YAxis, Tooltip, ResponsiveContainer, Legend, CartesianGrid, AreaChart, Area } from 'recharts';
import { Card, Stack } from '@/components/ui';
import { useApi } from '@/hooks/useApi';
import { analyticsApi, type LatencyPhaseRow } from '@/api/services/overview/analytics';
import type { SparklineResponse } from '@/api/types';
import { useTheme } from '@/theme/useTheme';
import { getPhaseColors } from '@nexus-gateway/ui-shared';

interface Props {
  start: string;
  end: string;
  source?: string;
}

function fmtMs(v: number | null): string {
  if (v == null) return '—';
  if (v < 1000) return `${v}ms`;
  return `${(v / 1000).toFixed(2)}s`;
}

function KpiCard({ label, value, sub }: { label: string; value: string; sub?: string }) {
  return (
    <Card padding="lg">
      <div style={{ color: 'var(--color-text-muted)', fontSize: 'var(--g-font-size-xs)', marginBottom: 'var(--g-space-1)' }}>{label}</div>
      <div style={{ fontSize: 'var(--g-font-size-3xl)', fontWeight: 'var(--g-font-weight-semibold)', lineHeight: 1.1 }}>{value}</div>
      {sub && <div style={{ color: 'var(--color-text-muted)', fontSize: 'var(--g-font-size-xs)', marginTop: 'var(--g-space-1)' }}>{sub}</div>}
    </Card>
  );
}

export function LatencyPhasesPanel({ start, end, source }: Props) {
  const { resolvedMode } = useTheme();
  const phase = getPhaseColors(resolvedMode);
  const { t } = useTranslation();
  const allowedSource = source === 'vk' || source === 'proxy' || source === 'agent'
    ? source === 'vk' ? 'ai-gateway' : source === 'proxy' ? 'compliance-proxy' : 'agent'
    : 'all';
  const { data, loading, error } = useApi(
    () => analyticsApi.latencyPhases({
      groupBy: 'provider',
      start,
      end,
      source: allowedSource as 'all' | 'ai-gateway' | 'compliance-proxy' | 'agent',
    }),
    ['admin', 'analytics', 'latency-phases', 'tab', start, end, allowedSource],
  );
  // Pull sparkline buckets to render a stacked-area time series of phase
  // averages alongside the per-provider P95 bar.
  const { data: spark } = useApi<SparklineResponse>(
    () => analyticsApi.sparkline({ startTime: start, endTime: end }),
    ['admin', 'analytics', 'sparkline', 'latency-tab', start, end],
  );

  if (loading) return <Card padding="lg">{t('common:loading', 'Loading…')}</Card>;
  if (error) return <Card padding="lg">{t('common:error', 'Error loading data.')}</Card>;
  const rows: LatencyPhaseRow[] = data?.rows ?? [];
  if (rows.length === 0) {
    return (
      <Card padding="lg">
        {t('pages:analytics.latencyEmpty', 'No latency phase data in the selected window. Run some traffic, or widen the time range.')}
      </Card>
    );
  }

  const byTraffic = [...rows].sort((a, b) => b.requestCount - a.requestCount);
  const top = byTraffic[0];
  const bySlowest = [...rows]
    .filter((r) => r.upstreamTotalP95Ms != null)
    .sort((a, b) => (b.upstreamTotalP95Ms ?? 0) - (a.upstreamTotalP95Ms ?? 0));

  // Banner shown above the panel when phase columns are missing: older rows
  // have NULL on TTFB + hooks columns because the backfill could not
  // reconstruct them — `upstream_ttfb_ms` is not derivable from `latency_ms`
  // alone, and `response_hooks_ms` requires a per-hook timer the legacy emit
  // path didn't capture. Showing this hint once at the top is less noisy
  // than per-chart empty-state copy.
  const showHistoricalNote =
    top.upstreamTtfbP95Ms == null || top.requestHooksP95Ms == null || top.responseHooksP95Ms == null;

  return (
    <Stack gap="lg">
      {showHistoricalNote && (
        <Card padding="md" style={{ borderLeft: '3px solid var(--color-warning)' }}>
          <div style={{ fontSize: 'var(--g-font-size-sm)', lineHeight: 1.5 }}>
            <strong>{t('pages:analytics.latencyHistoricalTitle', 'Some phases show no data in this window')}</strong>
            <div style={{ color: 'var(--color-text-muted)', marginTop: 'var(--g-space-1)' }}>
              {t(
                'pages:analytics.latencyHistoricalBody',
                'Upstream TTFB and response_hooks phases are recorded from 2026-05-14 onward. Rows older than that report total latency only — the backfill cannot reconstruct first-byte timing or per-hook durations. Narrow the time window to last 24 h to see fully populated phase data.',
              )}
            </div>
          </div>
        </Card>
      )}
      <div style={{ display: 'grid', gridTemplateColumns: 'repeat(3, 1fr)', gap: 'var(--g-space-4)' }}>
        <KpiCard
          label={t('pages:analytics.latencyKpiOurOverhead', 'Our Overhead P95')}
          value={fmtMs(top.usOverheadP95Ms)}
          sub={top.groupLabel}
        />
        <KpiCard
          label={t('pages:analytics.latencyKpiUpstreamTtfb', 'Upstream TTFB P95')}
          value={fmtMs(top.upstreamTtfbP95Ms)}
          sub={top.groupLabel}
        />
        <KpiCard
          label={t('pages:analytics.latencyKpiUpstreamTotal', 'Upstream Total P95')}
          value={fmtMs(top.upstreamTotalP95Ms)}
          sub={top.groupLabel}
        />
      </div>

      {/* Stacked-area time-series — phase averages per bucket. Uses the
          sparkline endpoint which the Hub rollup writer feeds with the
          Phase metric names. Empty when the rollup hasn't rebuilt
          yet (post-backfill 24h window — see runbook). */}
      {spark?.series && spark.series.length > 0 && (
        <Card padding="lg">
          <h3 style={{ margin: 'var(--g-space-0)', marginBottom: 'var(--g-space-1)' }}>
            {t('pages:analytics.latencyAreaTitle', 'Phase Averages Over Time')}
          </h3>
          <p style={{ color: 'var(--color-text-muted)', marginTop: 'var(--g-space-0)', fontSize: 'var(--g-font-size-sm)' }}>
            {t('pages:analytics.latencyAreaSubtitle', 'Average ms per phase per bucket, stacked. Empty until the Hub rollup correction job has run after backfill.')}
          </p>
          <ResponsiveContainer width="100%" height={260}>
            <AreaChart
              data={spark.series.map((b: SparklineResponse['series'][number]) => {
                const v = b.values;
                const usCt = v.latency_us_count ?? 0;
                const ttfbCt = v.latency_upstream_ttfb_count ?? 0;
                const upCt = v.latency_upstream_total_count ?? 0;
                const hkCt = v.latency_hooks_count ?? 0;
                return {
                  bucket: b.bucketStart,
                  us: usCt > 0 ? Math.round((v.latency_us_sum ?? 0) / usCt) : 0,
                  ttfb: ttfbCt > 0 ? Math.round((v.latency_upstream_ttfb_sum ?? 0) / ttfbCt) : 0,
                  body: (() => {
                    if (upCt === 0) return 0;
                    const totalAvg = Math.round((v.latency_upstream_total_sum ?? 0) / upCt);
                    const ttfbAvg = ttfbCt > 0 ? Math.round((v.latency_upstream_ttfb_sum ?? 0) / ttfbCt) : 0;
                    return Math.max(0, totalAvg - ttfbAvg);
                  })(),
                  hooks: hkCt > 0 ? Math.round((v.latency_hooks_sum ?? 0) / hkCt) : 0,
                };
              })}
              margin={{ left: 8, right: 8, top: 8, bottom: 8 }}
            >
              <CartesianGrid strokeDasharray="3 3" opacity={0.4} />
              <XAxis dataKey="bucket" tick={{ fontSize: 'var(--g-font-size-xs)' }} hide />
              <YAxis tick={{ fontSize: 'var(--g-font-size-xs)' }} />
              <Tooltip formatter={(v) => `${Number(v)}ms`} />
              <Legend />
              <Area type="monotone" dataKey="hooks" stackId="latency" stroke={phase.reqHooks} fill={phase.reqHooks} name={t('pages:traffic.detail.waterfall.reqHooks', 'Hooks')} />
              <Area type="monotone" dataKey="us" stackId="latency" stroke={phase.our} fill={phase.our} name={t('pages:traffic.detail.waterfall.ourOther', 'Our Overhead')} />
              <Area type="monotone" dataKey="ttfb" stackId="latency" stroke={phase.ttfb} fill={phase.ttfb} name={t('pages:traffic.detail.waterfall.upstreamTtfb', 'Upstream TTFB')} />
              <Area type="monotone" dataKey="body" stackId="latency" stroke={phase.body} fill={phase.body} name={t('pages:traffic.detail.waterfall.upstreamBody', 'Upstream Body')} />
            </AreaChart>
          </ResponsiveContainer>
        </Card>
      )}

      {/* Phase breakdown stacked bar per provider — top 10 by request count.
          Each bar shows our_overhead + upstream_ttfb + upstream_body so
          operators immediately see which provider has the most Nexus
          overhead vs. which is slow on the upstream side. */}
      <Card padding="lg">
        <h3 style={{ margin: 'var(--g-space-0)', marginBottom: 'var(--g-space-1)' }}>
          {t('pages:analytics.latencyStackedTitle', 'P95 Phase Breakdown — Per Provider')}
        </h3>
        <p style={{ color: 'var(--color-text-muted)', marginTop: 'var(--g-space-0)', fontSize: 'var(--g-font-size-sm)' }}>
          {t('pages:analytics.latencyStackedSubtitle', 'Each bar is one provider, segmented by where the time was spent at P95.')}
        </p>
        <ResponsiveContainer width="100%" height={280}>
          <BarChart
            data={byTraffic.slice(0, 10).map((r) => ({
              name: r.groupLabel,
              us: r.usOverheadP95Ms ?? 0,
              ttfb: r.upstreamTtfbP95Ms ?? 0,
              body: Math.max(0, (r.upstreamTotalP95Ms ?? 0) - (r.upstreamTtfbP95Ms ?? 0)),
            }))}
            margin={{ left: 8, right: 8, top: 8, bottom: 24 }}
          >
            <CartesianGrid strokeDasharray="3 3" opacity={0.4} />
            <XAxis dataKey="name" tick={{ fontSize: 'var(--g-font-size-xs)' }} interval={0} />
            <YAxis tick={{ fontSize: 'var(--g-font-size-xs)' }} label={{ value: 'ms', position: 'insideLeft', offset: -2, style: { fontSize: 'var(--g-font-size-xs)' } }} />
            <Tooltip formatter={(v) => `${Number(v)}ms`} />
            <Legend />
            <Bar dataKey="us" stackId="latency" fill={phase.our} name={t('pages:traffic.detail.waterfall.ourOther', 'Our Overhead')} />
            <Bar dataKey="ttfb" stackId="latency" fill={phase.ttfb} name={t('pages:traffic.detail.waterfall.upstreamTtfb', 'Upstream TTFB')} />
            <Bar dataKey="body" stackId="latency" fill={phase.body} name={t('pages:traffic.detail.waterfall.upstreamBody', 'Upstream Body')} />
          </BarChart>
        </ResponsiveContainer>
      </Card>

      <Card padding="lg">
        <h3 style={{ margin: 'var(--g-space-0)', marginBottom: 'var(--g-space-3)' }}>
          {t('pages:analytics.latencyLeaderboardTitle', 'Provider Latency Leaderboard')}
        </h3>
        <p style={{ color: 'var(--color-text-muted)', marginTop: 'var(--g-space-0)', fontSize: 'var(--g-font-size-sm)' }}>
          {t('pages:analytics.latencyLeaderboardSubtitle', 'Sorted by P95 upstream total — the worst provider experiences are at the top.')}
        </p>
        <table style={{ width: '100%', borderCollapse: 'collapse', fontSize: 'var(--g-font-size-sm)' }}>
          <thead>
            <tr style={{ textAlign: 'left', color: 'var(--color-text-muted)', borderBottom: '1px solid var(--color-border)' }}>
              <th style={{ padding: 'var(--g-space-2) var(--g-space-1)' }}>{t('pages:analytics.colProvider', 'Provider')}</th>
              <th style={{ padding: 'var(--g-space-2) var(--g-space-1)', textAlign: 'right' }}>{t('pages:analytics.colRequests', 'Requests')}</th>
              <th style={{ padding: 'var(--g-space-2) var(--g-space-1)', textAlign: 'right' }}>{t('pages:analytics.colTtfbP50', 'P50 TTFB')}</th>
              <th style={{ padding: 'var(--g-space-2) var(--g-space-1)', textAlign: 'right' }}>{t('pages:analytics.colTtfbP95', 'P95 TTFB')}</th>
              <th style={{ padding: 'var(--g-space-2) var(--g-space-1)', textAlign: 'right' }}>{t('pages:analytics.colUpstreamP50', 'P50 Upstream')}</th>
              <th style={{ padding: 'var(--g-space-2) var(--g-space-1)', textAlign: 'right' }}>{t('pages:analytics.colUpstreamP95', 'P95 Upstream')}</th>
              <th style={{ padding: 'var(--g-space-2) var(--g-space-1)', textAlign: 'right' }}>{t('pages:analytics.colUsP95', 'P95 Us')}</th>
            </tr>
          </thead>
          <tbody>
            {bySlowest.map((r) => (
              <tr key={r.groupKey} style={{ borderBottom: '1px solid var(--border-subtle)' }}>
                <td style={{ padding: 'var(--g-space-2) var(--g-space-1)' }}>{r.groupLabel}</td>
                <td style={{ padding: 'var(--g-space-2) var(--g-space-1)', textAlign: 'right', fontVariantNumeric: 'tabular-nums' }}>{r.requestCount.toLocaleString()}</td>
                <td style={{ padding: 'var(--g-space-2) var(--g-space-1)', textAlign: 'right', fontVariantNumeric: 'tabular-nums' }}>{fmtMs(r.upstreamTtfbP50Ms)}</td>
                <td style={{ padding: 'var(--g-space-2) var(--g-space-1)', textAlign: 'right', fontVariantNumeric: 'tabular-nums' }}>{fmtMs(r.upstreamTtfbP95Ms)}</td>
                <td style={{ padding: 'var(--g-space-2) var(--g-space-1)', textAlign: 'right', fontVariantNumeric: 'tabular-nums' }}>{fmtMs(r.upstreamTotalP50Ms)}</td>
                <td style={{ padding: 'var(--g-space-2) var(--g-space-1)', textAlign: 'right', fontVariantNumeric: 'tabular-nums' }}>{fmtMs(r.upstreamTotalP95Ms)}</td>
                <td style={{ padding: 'var(--g-space-2) var(--g-space-1)', textAlign: 'right', fontVariantNumeric: 'tabular-nums' }}>{fmtMs(r.usOverheadP95Ms)}</td>
              </tr>
            ))}
          </tbody>
        </table>
      </Card>
    </Stack>
  );
}
