/**
 * Integration tests — MetricsTab.
 *
 * Covers the four behaviour bullets from the plan:
 *   - default range = last 1 hour when URL params absent
 *   - all six time-range buttons render
 *   - retention config grays out beyond-cap buttons
 *   - both Runtime and Business sections render with the catalog
 *
 * Recharts' `ResponsiveContainer` does not paint a series in jsdom (no
 * measured layout), so assertions target the chrome (buttons, headings,
 * cell titles) and MSW handler call assertions for the timeseries fetcher.
 */
import { describe, it, expect } from 'vitest';
import { screen, waitFor } from '@testing-library/react';
import userEvent from '@testing-library/user-event';
import { Routes, Route } from 'react-router-dom';

import { renderWithRouter, server, http, HttpResponse } from '@/test/test-utils';
import { MetricsTab, bucketToP50, isRangeBeyondRetention } from '../../../../../../src/pages/infrastructure/_shared/tabs/metrics/MetricsTab';

function renderTab(props: { thingId: string; thingType: string } = { thingId: 'thing-abc', thingType: 'ai-gateway' }) {
  return renderWithRouter(
    <Routes>
      <Route path="/m" element={<MetricsTab {...props} />} />
    </Routes>,
    { route: '/m' },
  );
}

describe('MetricsTab', () => {
  it('TestMetricsTab_RendersTimeRangeButtons exposes all six presets', async () => {
    renderTab();
    for (const label of ['1h', '6h', '1d', '7d', '30d', '1y']) {
      await waitFor(() => {
        expect(screen.getByRole('button', { name: new RegExp(`^${label}$`, 'i') })).toBeInTheDocument();
      });
    }
  });

  it('TestMetricsTab_DefaultRangeIsOneHour issues timeseries fetches with a ~1h span', async () => {
    const seenFromTo: Array<{ from: string; to: string }> = [];
    server.use(
      http.get('/api/admin/ops-metrics/timeseries', ({ request }) => {
        const u = new URL(request.url);
        seenFromTo.push({ from: u.searchParams.get('from') ?? '', to: u.searchParams.get('to') ?? '' });
        return HttpResponse.json({ data: [], granularity: 'raw' });
      }),
    );

    renderTab();

    await waitFor(() => {
      expect(seenFromTo.length).toBeGreaterThan(0);
    });

    const span = new Date(seenFromTo[0].to).getTime() - new Date(seenFromTo[0].from).getTime();
    const oneHour = 60 * 60 * 1000;
    // Allow a generous margin to absorb test wall-clock jitter between the
    // moment the page captured Date.now() and the moment we read the params.
    expect(Math.abs(span - oneHour)).toBeLessThan(5_000);
  });

  it('TestMetricsTab_GrayOutBeyondRetention disables ranges past the runtime cap', async () => {
    server.use(
      http.get('/api/admin/observability/retention', () =>
        HttpResponse.json({
          retention: {
            runtime_5m: { value: 1, min: 1, max: 30 },
            runtime_1h: { value: 1, min: 30, max: 365 },
            runtime_1d: { value: 1, min: 90, max: 1095 },
            runtime_1mo: { value: 1, min: 365, max: 3650 },
            business_5m: { value: 7, min: 1, max: 30 },
            business_1h: { value: 90, min: 30, max: 365 },
            business_1d: { value: 365, min: 90, max: 1095 },
            business_1mo: { value: 1825, min: 365, max: 3650 },
            diag_warn: { value: 30, min: 7, max: 90 },
            diag_error: { value: 180, min: 30, max: 730 },
            diag_fatal: { value: 365, min: 90, max: 1825 },
          },
        }),
      ),
    );

    renderTab();

    // Once retention loads, the 7d / 30d / 1y buttons must report disabled.
    await waitFor(() => {
      const oneYear = screen.getByRole('button', { name: /^1y$/i }) as HTMLButtonElement;
      expect(oneYear.disabled).toBe(true);
    });
    expect((screen.getByRole('button', { name: /^30d$/i }) as HTMLButtonElement).disabled).toBe(true);
    expect((screen.getByRole('button', { name: /^7d$/i }) as HTMLButtonElement).disabled).toBe(true);
    // 1h must remain enabled (1 day cap is well above 1 hour).
    expect((screen.getByRole('button', { name: /^1h$/i }) as HTMLButtonElement).disabled).toBe(false);
  });

  it('TestMetricsTab_RendersRuntimeAndBusinessSections renders both sections + per-type catalog', async () => {
    renderTab({ thingId: 'thing-abc', thingType: 'ai-gateway' });

    await waitFor(() => {
      expect(screen.getByText(/runtime/i)).toBeInTheDocument();
    });
    // Business heading
    expect(screen.getByText(/business/i)).toBeInTheDocument();
    // Runtime catalog: Heap alloc + Goroutines + GC pause are the canonical entries.
    expect(screen.getAllByText(/heap alloc/i).length).toBeGreaterThan(0);
    expect(screen.getAllByText(/goroutines/i).length).toBeGreaterThan(0);
    // Business catalog (ai-gateway): cache hits / streams active appear in §6.3.
    expect(screen.getAllByText(/cache hits/i).length).toBeGreaterThan(0);
    expect(screen.getAllByText(/streams active/i).length).toBeGreaterThan(0);
  });

  it('TestMetricsTab_RangeButtonClickUpdatesQueryParams refetches with a new from + to span', async () => {
    const seenFromTo: Array<{ from: string; to: string }> = [];
    server.use(
      http.get('/api/admin/ops-metrics/timeseries', ({ request }) => {
        const u = new URL(request.url);
        seenFromTo.push({ from: u.searchParams.get('from') ?? '', to: u.searchParams.get('to') ?? '' });
        return HttpResponse.json({ data: [], granularity: 'raw' });
      }),
    );

    const user = userEvent.setup();
    renderTab();

    await waitFor(() => {
      expect(screen.getByRole('button', { name: /^7d$/i })).toBeInTheDocument();
    });

    // Wait for initial 1h fetches to settle.
    await waitFor(() => {
      expect(seenFromTo.length).toBeGreaterThan(0);
    });
    const initialCount = seenFromTo.length;

    await user.click(screen.getByRole('button', { name: /^7d$/i }));

    // After the click, additional fetches with a ~7d span must fire.
    await waitFor(() => {
      expect(seenFromTo.length).toBeGreaterThan(initialCount);
    });
    const sevenDays = 7 * 24 * 60 * 60 * 1000;
    const newest = seenFromTo[seenFromTo.length - 1];
    const span = new Date(newest.to).getTime() - new Date(newest.from).getTime();
    expect(Math.abs(span - sevenDays)).toBeLessThan(60_000);
  });
});

describe('bucketToP50', () => {
  it('returns 0 for an empty histogram', () => {
    expect(bucketToP50([0, 0, 0, 0, 0, 0])).toBe(0);
  });

  it('lands inside the lowest bucket when all weight is there', () => {
    const v = bucketToP50([100, 0, 0, 0, 0, 0]);
    expect(v).toBeGreaterThanOrEqual(0);
    expect(v).toBeLessThanOrEqual(50);
  });

  it('lands above the heaviest boundary when the long tail dominates', () => {
    const v = bucketToP50([0, 0, 0, 0, 0, 100]);
    expect(v).toBeGreaterThanOrEqual(1000);
  });
});

describe('isRangeBeyondRetention', () => {
  it('returns true when range exceeds cap days', () => {
    const oneYearMs = 365 * 24 * 60 * 60 * 1000;
    expect(isRangeBeyondRetention(oneYearMs, 7)).toBe(true);
  });

  it('returns false when range fits inside cap days', () => {
    const oneHourMs = 60 * 60 * 1000;
    expect(isRangeBeyondRetention(oneHourMs, 7)).toBe(false);
  });
});
