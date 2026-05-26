/**
 * Unit tests — TimeSeriesChart.
 *
 * The component is a thin Recharts wrapper. Recharts' `ResponsiveContainer`
 * sizes itself off the parent's measured layout, which jsdom does not
 * compute, so we don't assert on the chart's path geometry — we cover the
 * three explicit states (loading / error / empty) and the value formatter,
 * which are the contract callers depend on.
 */
import { describe, it, expect } from 'vitest';
import { render, screen } from '@testing-library/react';
import { I18nextProvider } from 'react-i18next';
import '@testing-library/jest-dom/vitest';

import i18n from '@/i18n';
import {
  TimeSeriesChart,
  formatValue,
  type TimeSeriesPoint,
} from './TimeSeriesChart';

function renderChart(ui: React.ReactElement) {
  return render(<I18nextProvider i18n={i18n}>{ui}</I18nextProvider>);
}

const SAMPLE: TimeSeriesPoint[] = [
  { bucket: '2026-04-27T10:00:00Z', value: 1024 },
  { bucket: '2026-04-27T10:05:00Z', value: 2048 },
  { bucket: '2026-04-27T10:10:00Z', value: 4096 },
];

describe('TimeSeriesChart', () => {
  it('TestTimeSeriesChart_RendersData renders the recharts surface for a non-empty series', () => {
    const { container } = renderChart(<TimeSeriesChart data={SAMPLE} label="heap" unit="bytes" />);
    // ResponsiveContainer renders its inner wrapper; the empty / loading /
    // error placeholders use a different markup path. Asserting the absence
    // of the empty-state text confirms the chart branch was taken.
    expect(container.querySelector('.recharts-responsive-container, [class*="recharts-responsive-container"]')).not.toBeNull();
    expect(screen.queryByText(/no data in this window/i)).toBeNull();
  });

  it('TestTimeSeriesChart_EmptyState shows the empty placeholder when data is empty', () => {
    renderChart(<TimeSeriesChart data={[]} label="heap" />);
    expect(screen.getByText(/no data in this window/i)).toBeInTheDocument();
  });

  it('TestTimeSeriesChart_ErrorState shows the alert placeholder when an error is present', () => {
    renderChart(<TimeSeriesChart data={[]} label="heap" error={new Error('boom')} />);
    const alert = screen.getByRole('alert');
    expect(alert).toHaveTextContent(/boom/);
  });

  it('TestTimeSeriesChart_LoadingState shows a spinner when loading', () => {
    renderChart(<TimeSeriesChart data={[]} label="heap" loading />);
    expect(screen.getByRole('status')).toBeInTheDocument();
  });

  it('formatValue humanises bytes with KiB / MiB / GiB suffixes', () => {
    expect(formatValue(0, 'bytes')).toBe('0 B');
    expect(formatValue(2048, 'bytes')).toMatch(/KiB/);
    expect(formatValue(5 * 1024 * 1024, 'bytes')).toMatch(/MiB/);
    expect(formatValue(3 * 1024 * 1024 * 1024, 'bytes')).toMatch(/GiB/);
  });

  it('formatValue humanises ms (>= 1s) and counts (compact)', () => {
    expect(formatValue(750, 'ms')).toMatch(/ms$/);
    expect(formatValue(1500, 'ms')).toMatch(/s$/);
    expect(formatValue(2_500_000, 'count')).toMatch(/M$/);
    expect(formatValue(2_500, 'count')).toMatch(/k$/);
  });
});
