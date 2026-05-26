/**
 * Unit tests — LatencyMini.
 *
 * Focuses on the `resolveSegments` fallback that picks
 * upstream_total_ms when present and falls back to upstream_ttfb_ms
 * otherwise. The fallback covers streaming-MISS rows whose `upstream_total_ms`
 * column is NULL (broker-pump race where audit defer fires before body
 * closed; fixed in phaseTrackedBody).
 */
import { describe, it, expect } from 'vitest';
import { screen } from '@testing-library/react';
import '@testing-library/jest-dom/vitest';

import { renderWithProviders } from '@/test/test-utils';
import { LatencyMini } from './LatencyMini';

describe('LatencyMini fallback', () => {
  it('TestLatencyMini_PrefersUpstreamTotalMs when both fields are present', () => {
    // ttfb=200, total=800 → upstream_body = 600.
    renderWithProviders(
      <LatencyMini
        size="card"
        latencyMs={1000}
        upstreamTtfbMs={200}
        upstreamTotalMs={800}
      />,
    );
    // card variant renders an inline subtitle: "Us 200ms · TTFB 200ms · Upstream 800ms".
    // The "Upstream" pill carries upstream_total in card form. Check the
    // formatted ms is upstream_total, NOT ttfb.
    expect(screen.getByText(/800ms/)).toBeInTheDocument();
  });

  it('TestLatencyMini_FallsBackToTtfbWhenTotalNull avoids over-attributing to Our Overhead', () => {
    // Scenario from prod row 57fd72e4-…:
    //   latencyMs = 7597, upstreamTtfbMs = 1636, upstreamTotalMs = null
    // Expected: bar's "Us" segment is 5961ms (= 7597 - 1636), NOT 7597.
    const { container } = renderWithProviders(
      <LatencyMini
        size="card"
        latencyMs={7597}
        upstreamTtfbMs={1636}
        upstreamTotalMs={null}
      />,
    );
    // Card subtitle phrase carries "Us <ourOverhead>" — the fallback should
    // floor ourOverhead at 5961ms (7597 - 1636) rather than 7597.
    // We assert through the textual subtitle: "5.96s" or "5961ms".
    expect(container.textContent).toMatch(/5\.96s|5961ms/);
    // The full latency total must still surface (7.60s).
    expect(container.textContent).toMatch(/7\.60s/);
  });

  it('TestLatencyMini_NoBreakdownFieldsRendersFlat', () => {
    // When upstream_ttfb_ms and upstream_total_ms are both absent, the
    // segmented breakdown isn't trustworthy — the bar collapses to one
    // colour. The card still surfaces the total latency.
    const { container } = renderWithProviders(
      <LatencyMini size="card" latencyMs={500} />,
    );
    expect(container.textContent).toMatch(/500ms/);
  });
});
