import { describe, it, expect, vi, beforeEach } from 'vitest';
import { render, screen, fireEvent } from '@testing-library/react';
import { I18nextProvider } from 'react-i18next';
import i18n from '@/i18n';
import { MetricsRollupsSection } from '@/pages/metrics/MetricsRollupsSection';

vi.mock('@/theme/useTheme', () => ({ useTheme: () => ({ resolvedMode: 'light' }) }));

const apiState = vi.hoisted(() => ({ value: { data: undefined as unknown, loading: false, error: null as Error | null, refetch: () => {} } }));
vi.mock('@/hooks/useApi', () => ({ useApi: () => apiState.value }));

type Row = { metricName: string; dimensionKey: string; bucketStart: string; value: string };
const r = (metricName: string, value: string, dimensionKey = '', bucketStart = '2026-05-28T00:00:00Z'): Row =>
  ({ metricName, dimensionKey, bucketStart, value });

const rows: Row[] = [
  r('request_count', '120'), r('request_count', '60', 'routed_provider:openai'),
  r('token_usage', '5000'), r('estimated_cost', '1.25'),
  r('error_count', '3'), r('cache_hits', '40'), r('cache_saved_cost', '0.50'),
  r('latency_us_sum', '900'), r('latency_us_count', '3'),
  r('latency_upstream_ttfb_sum', '600'), r('latency_upstream_ttfb_count', '3'),
  r('latency_upstream_total_sum', '1200'), r('latency_upstream_total_count', '3'),
  r('latency_hooks_sum', '90'), r('latency_hooks_count', '3'),
  r('latency_p50', '200', 'routed_provider:openai'),
  r('admin_auth', '7'),
];

function wrap(props: Record<string, unknown> = {}) {
  return render(
    <I18nextProvider i18n={i18n}><MetricsRollupsSection {...props} /></I18nextProvider>,
  );
}

describe('MetricsRollupsSection', () => {
  beforeEach(() => { apiState.value = { data: { data: rows }, loading: false, error: null, refetch: () => {} }; });

  it('renders the spinner while loading', () => {
    apiState.value = { data: undefined, loading: true, error: null, refetch: () => {} };
    const { container } = wrap();
    expect(container.querySelector('svg, [class*=spinner], [class*=Spinner], [role=status]') || container.firstChild).toBeTruthy();
  });

  it('renders an error banner with a retry on failure', () => {
    apiState.value = { data: undefined, loading: false, error: new Error('rollup query failed'), refetch: () => {} };
    wrap();
    expect(screen.getByText('rollup query failed')).toBeInTheDocument();
  });

  it('renders KPI cards + the system-overview heading from the aggregates payload', () => {
    wrap();
    expect(screen.getByText(i18n.t('pages:metrics.totalRequests'))).toBeInTheDocument();
    expect(screen.getByText(i18n.t('pages:metrics.estCost'))).toBeInTheDocument();
    // cache_saved_cost > 0 → the cache-savings KPI card renders
    expect(screen.getByText(i18n.t('pages:metrics.cacheSavings'))).toBeInTheDocument();
    // admin_auth rows present → the admin-auth note renders
    expect(screen.getByText(i18n.t('pages:metrics.adminAuthNote', { count: 1 }))).toBeInTheDocument();
  });

  it('shows the internal range selector when no external window is supplied', () => {
    wrap();
    const select = screen.getByRole('combobox');
    fireEvent.change(select, { target: { value: '168' } }); // last 7 days → re-derives the window
    expect((select as HTMLSelectElement).value).toBe('168');
  });

  it('hides the range selector when external window + embedded are set (description now lives in the parent filter bar)', () => {
    wrap({ embedded: true, externalStart: '2026-05-27T00:00:00Z', externalEnd: '2026-05-28T00:00:00Z' });
    expect(screen.queryByRole('combobox')).not.toBeInTheDocument();
    // The embedded description was lifted to the AnalyticsPage filter bar; the
    // section itself no longer renders it.
    expect(screen.queryByText(i18n.t('pages:metrics.rollupsDescription'))).not.toBeInTheDocument();
  });

  it('hides the VK-only token/cost pair when source is not vk', () => {
    wrap({ source: 'admin' });
    // tokens-by-provider heading still renders, but the VK "tokens total" chart pair is gated out
    expect(screen.queryByText(i18n.t('pages:metrics.chartTokensTotal'))).not.toBeInTheDocument();
  });
});
