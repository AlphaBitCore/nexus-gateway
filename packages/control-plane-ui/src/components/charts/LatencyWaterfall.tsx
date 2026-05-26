/**
 * 5-segment horizontal stacked bar showing latency phase breakdown for a
 * single traffic_event row:
 *
 *   [ request_hooks ][ our_other ][ upstream_ttfb ][ upstream_body ][ response_hooks ]
 *
 * `upstream_body` is `upstream_total_ms - upstream_ttfb_ms`.
 * `our_other` is `latency_ms - upstream_total_ms - hooks`, clamped to zero.
 * Any nullable field renders as a 0-width segment.
 *
 * Plain SVG (no recharts) to keep the bundle lean.
 */
import type { ReactElement } from 'react';
import { useTranslation } from 'react-i18next';
import { useTheme } from '@/theme/useTheme';
import { getPhaseColors } from '@nexus-gateway/ui-shared';

interface LatencyWaterfallProps {
  latencyMs?: number | null;
  upstreamTtfbMs?: number | null;
  upstreamTotalMs?: number | null;
  requestHooksMs?: number | null;
  responseHooksMs?: number | null;
}

type PhaseKey = 'reqHooks' | 'our' | 'ttfb' | 'body' | 'respHooks';

interface Segment {
  key: PhaseKey;
  ms: number;
  labelKey: string;
}

function fmtMs(ms: number): string {
  if (ms < 1000) return `${Math.round(ms)}ms`;
  return `${(ms / 1000).toFixed(2)}s`;
}

export function LatencyWaterfall(props: LatencyWaterfallProps): ReactElement {
  const { t } = useTranslation();
  const { resolvedMode } = useTheme();
  const COLORS = getPhaseColors(resolvedMode);
  const total = props.latencyMs ?? 0;

  const reqHooks = Math.max(0, props.requestHooksMs ?? 0);
  const respHooks = Math.max(0, props.responseHooksMs ?? 0);
  const upTtfb = Math.max(0, props.upstreamTtfbMs ?? 0);
  // Fallback chain: prefer upstream_total_ms; when null but ttfb is
  // present, fall back to ttfb so "Our Overhead" doesn't absorb the
  // entire upstream wait. Matches LatencyMini.resolveSegments.
  const upTotalRaw = props.upstreamTotalMs ?? props.upstreamTtfbMs ?? 0;
  const upTotal = Math.max(0, upTotalRaw);
  const upBody = Math.max(0, upTotal - upTtfb);

  // Our overhead residual = total - upstream_total - hooks.
  const our = Math.max(0, total - upTotal - reqHooks - respHooks);

  const segs: Segment[] = [
    { key: 'reqHooks', ms: reqHooks, labelKey: 'pages:traffic.detail.waterfall.reqHooks' },
    { key: 'our', ms: our, labelKey: 'pages:traffic.detail.waterfall.ourOther' },
    { key: 'ttfb', ms: upTtfb, labelKey: 'pages:traffic.detail.waterfall.upstreamTtfb' },
    { key: 'body', ms: upBody, labelKey: 'pages:traffic.detail.waterfall.upstreamBody' },
    { key: 'respHooks', ms: respHooks, labelKey: 'pages:traffic.detail.waterfall.respHooks' },
  ];
  const sum = segs.reduce((acc, s) => acc + s.ms, 0);
  if (sum <= 0) {
    return (
      <div style={{ color: 'var(--color-text-muted)', fontStyle: 'italic', padding: 'var(--g-space-2) var(--g-space-0)' }}>
        {t('pages:traffic.detail.waterfall.noData', 'No phase data available for this event.')}
      </div>
    );
  }

  let cursor = 0;
  return (
    <div style={{ width: '100%' }}>
      <div style={{ display: 'flex', height: 24, width: '100%', borderRadius: 'var(--g-radius-sm)', overflow: 'hidden', background: 'var(--color-surface-2)' }}>
        {segs.map((s) => {
          const pct = (s.ms / sum) * 100;
          cursor += pct;
          return (
            <div
              key={s.key}
              title={`${t(s.labelKey)}: ${fmtMs(s.ms)}`}
              style={{
                width: `${pct}%`,
                background: COLORS[s.key],
                transition: 'width var(--g-transition-normal)',
              }}
            />
          );
        })}
      </div>
      {/*
        Each phase group is `[●] Label Value` with value tightly coupled
        to its own label. Wider column gap than row gap so the eye parses
        label↔value pairing before group↔group boundaries.
      */}
      <div style={{ marginTop: 'var(--g-space-2)', display: 'flex', flexWrap: 'wrap', rowGap: 'var(--g-space-1-5)', columnGap: 'var(--g-space-6)', fontSize: 'var(--g-font-size-xs)' }}>
        {segs.map((s) => (
          <div key={s.key} style={{ display: 'inline-flex', alignItems: 'center', gap: 'var(--g-space-1-5)', whiteSpace: 'nowrap' }}>
            <span style={{ width: 10, height: 10, background: COLORS[s.key], borderRadius: 'var(--g-radius-sm)', flexShrink: 0 }} />
            <span style={{ color: 'var(--color-text-muted)' }}>{t(s.labelKey)}</span>
            <span style={{ fontVariantNumeric: 'tabular-nums', fontWeight: 'var(--g-font-weight-semibold)' }}>{fmtMs(s.ms)}</span>
          </div>
        ))}
      </div>
    </div>
  );
}
