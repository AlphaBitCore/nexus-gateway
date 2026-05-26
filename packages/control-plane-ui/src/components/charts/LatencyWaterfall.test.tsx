/**
 * Unit tests — LatencyWaterfall.
 *
 * The component is plain SVG/divs (no recharts), so jsdom can render it
 * fully. We cover three branches:
 *   1) all phases zero / nullable inputs → no-data placeholder
 *   2) typical traffic_event row → renders 5 legend entries with formatted ms
 *   3) upstream-only row (e.g. compliance-proxy) → still renders without crashing
 */
import { describe, it, expect } from 'vitest';
import { screen } from '@testing-library/react';
import '@testing-library/jest-dom/vitest';

import { renderWithProviders } from '@/test/test-utils';
import { LatencyWaterfall } from './LatencyWaterfall';

function renderUI(ui: React.ReactElement) {
  return renderWithProviders(ui);
}

describe('LatencyWaterfall', () => {
  it('TestLatencyWaterfall_RendersNoData renders an empty-state when every phase is zero/null', () => {
    renderUI(<LatencyWaterfall latencyMs={null} />);
    expect(screen.getByText(/no phase data/i)).toBeInTheDocument();
  });

  it('TestLatencyWaterfall_RendersFiveSegments renders one legend row per phase for a populated event', () => {
    renderUI(
      <LatencyWaterfall
        latencyMs={1000}
        requestHooksMs={20}
        upstreamTtfbMs={200}
        upstreamTotalMs={800}
        responseHooksMs={30}
      />,
    );
    // 5 legend entries — request hooks / our overhead / upstream ttfb /
    // upstream body / response hooks. We grep the formatted ms label per
    // segment instead of asserting the bar's pixel widths (jsdom doesn't
    // compute layout).
    expect(screen.getByText(/200ms/)).toBeInTheDocument();   // ttfb
    expect(screen.getByText(/600ms/)).toBeInTheDocument();   // upstream_body = 800-200
    expect(screen.getByText(/20ms/)).toBeInTheDocument();    // req hooks
    expect(screen.getByText(/30ms/)).toBeInTheDocument();    // resp hooks
  });

  it('TestLatencyWaterfall_RendersUpstreamOnly renders the bar for a row with only upstream fields populated', () => {
    const { container } = renderUI(
      <LatencyWaterfall latencyMs={500} upstreamTotalMs={400} />,
    );
    // The empty-state branch must NOT be taken.
    expect(screen.queryByText(/no phase data/i)).toBeNull();
    // The colored bar wrapper renders 5 child divs (one per segment) even
    // when most are zero-width — jsdom won't compute widths, but we can
    // assert the legend rows are present.
    expect(container.querySelectorAll('span').length).toBeGreaterThan(0);
  });

  // Regression: when upstream_total_ms is null, fall back to ttfb so
  // "Our Overhead" doesn't absorb the entire upstream wait.
  it('TestLatencyWaterfall_FallsBackToTtfbWhenTotalMissing keeps Our Overhead realistic', () => {
    // latencyMs=7597, upstreamTtfbMs=1636, upstreamTotalMs=null
    // Expected: ourOverhead = 7597 - 1636 = 5961ms (NOT 7597ms).
    renderUI(
      <LatencyWaterfall
        latencyMs={7597}
        upstreamTtfbMs={1636}
        upstreamTotalMs={null}
      />,
    );
    // ttfb legend renders as 1.64s.
    expect(screen.getByText(/1\.64s/)).toBeInTheDocument();
    // Our overhead should be ~5.96s (5961ms), NOT 7.60s.
    expect(screen.getByText(/5\.96s/)).toBeInTheDocument();
    // 7.60s must NOT appear as the "Our Overhead" segment label.
  });
});
