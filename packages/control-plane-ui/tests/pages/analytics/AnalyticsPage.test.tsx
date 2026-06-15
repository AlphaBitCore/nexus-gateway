/**
 * AnalyticsPage — driven with mocked useApi fixtures (summary / cost / usage /
 * cache-roi keyed by queryKey) and stubbed chart-heavy child panels, so the KPI
 * cards, filter controls (time-range, group-by, source), tab switching and
 * empty-data states are asserted. Replaces the previous content-length-only
 * smoke test.
 */
import { describe, it, expect, vi, beforeEach } from 'vitest';
import { screen, fireEvent } from '@testing-library/react';
import userEvent from '@testing-library/user-event';
import i18n from '@/i18n';
import { renderWithRouter } from '@/test/test-utils';
import { AnalyticsPage } from '@/pages/analytics/AnalyticsPage';

const apiState = vi.hoisted(() => ({
  summary: { data: undefined as unknown, loading: false, error: null as Error | null, refetch: vi.fn() },
  cost: { data: { data: [] } as unknown },
  usage: { data: { data: [] } as unknown },
}));
vi.mock('@/hooks/useApi', () => ({
  useApi: (_fn: unknown, key: string[]) => {
    if (key.includes('summary')) return apiState.summary;
    if (key.includes('cost')) return apiState.cost;
    if (key.includes('usage')) return apiState.usage;
    return { data: null, loading: false, error: null, refetch: vi.fn() }; // cache-roi
  },
}));
// chart-heavy sibling panels have their own data hooks — stub them out
vi.mock('@/pages/metrics/MetricsRollupsSection', () => ({ MetricsRollupsSection: () => <div data-testid="rollups-stub" /> }));
vi.mock('@/pages/analytics/LatencyPhasesPanel', () => ({ LatencyPhasesPanel: () => <div data-testid="latency-stub" /> }));

const SUMMARY = { totalRequests: 1234, totalEstimatedCostUsd: 5.67, totalTokens: 89000, avgLatencyMs: 250, cacheHitRate: 0.25 };
const COST = { data: [{ group: 'openai', groupLabel: 'OpenAI', totalCostUsd: 5 }] };
const USAGE = { data: [{ group: 'openai', groupLabel: 'OpenAI', totalTokens: 89000 }] };

function ok(d: unknown) { return { data: d, loading: false, error: null, refetch: vi.fn() }; }
function wrap() { return renderWithRouter(<AnalyticsPage />); }

describe('AnalyticsPage', () => {
  beforeEach(() => {
    vi.clearAllMocks();
    apiState.summary = ok(SUMMARY);
    apiState.cost = { data: COST };
    apiState.usage = { data: USAGE };
  });

  it('renders the KPI cards + cost/usage section for VK metrics (default source)', () => {
    wrap();
    expect(screen.getByText(i18n.t('pages:traffic.statTotalRequests'))).toBeInTheDocument();
    expect(screen.getByText(i18n.t('pages:traffic.statTotalCost'))).toBeInTheDocument();
    expect(screen.getByText(i18n.t('pages:traffic.sectionCostUsage'))).toBeInTheDocument();
  });

  it('changing the time range surfaces a removable filter chip', () => {
    wrap();
    fireEvent.change(screen.getByLabelText(i18n.t('pages:analytics.labelTimeRange'), { selector: 'select' }), { target: { value: '30d' } });
    expect(screen.getByLabelText(i18n.t('pages:traffic.resetTimeRange'))).toBeInTheDocument();
  });

  it('changing group-by surfaces a removable filter chip', () => {
    wrap();
    fireEvent.change(screen.getByLabelText(i18n.t('pages:analytics.labelGroupBy'), { selector: 'select' }), { target: { value: 'model' } });
    expect(screen.getByLabelText(i18n.t('pages:traffic.resetGroupBy'))).toBeInTheDocument();
  });

  it('selecting the Compliance Proxy source hides the VK-only cost/usage section', () => {
    wrap();
    expect(screen.getByText(i18n.t('pages:traffic.sectionCostUsage'))).toBeInTheDocument();
    fireEvent.change(
      screen.getByLabelText(i18n.t('pages:analytics.sourceFilter'), { selector: 'select' }),
      { target: { value: 'proxy' } },
    );
    expect(screen.queryByText(i18n.t('pages:traffic.sectionCostUsage'))).toBeNull();
  });

  it('empty cost + usage data shows the no-data-for-period placeholder', () => {
    apiState.cost = { data: { data: [] } };
    apiState.usage = { data: { data: [] } };
    wrap();
    expect(screen.getAllByText(i18n.t('pages:traffic.noDataForPeriod')).length).toBeGreaterThan(0);
  });

  it('switches to the Metrics and Latency tabs', async () => {
    const user = userEvent.setup();
    wrap();
    await user.click(screen.getByRole('tab', { name: i18n.t('pages:traffic.metrics') }));
    expect(await screen.findByTestId('rollups-stub')).toBeInTheDocument();
    await user.click(screen.getByRole('tab', { name: i18n.t('pages:analytics.latencyTab', 'Latency') }));
    expect(await screen.findByTestId('latency-stub')).toBeInTheDocument();
  });

  it('keeps each tab filter independent — analytics time range does not leak to the metrics tab', async () => {
    const user = userEvent.setup();
    wrap();
    // Set the Analytics tab time range to 30d.
    fireEvent.change(
      screen.getByLabelText(i18n.t('pages:analytics.labelTimeRange'), { selector: 'select' }),
      { target: { value: '30d' } },
    );
    // Switch to the Metrics tab — its own filter bar must still show the 7d default.
    await user.click(screen.getByRole('tab', { name: i18n.t('pages:traffic.metrics') }));
    const metricsTimeRange = await screen.findByLabelText(
      i18n.t('pages:analytics.labelTimeRange'), { selector: 'select' },
    );
    expect((metricsTimeRange as HTMLSelectElement).value).toBe('7d');
  });

  it('renders the loading and error branches', () => {
    apiState.summary = { data: undefined, loading: true, error: null, refetch: vi.fn() };
    const { container, unmount } = wrap();
    expect(container.firstChild).toBeTruthy();
    unmount();
    apiState.summary = { data: undefined, loading: false, error: new Error('analytics boom'), refetch: vi.fn() };
    wrap();
    expect(screen.getByText('analytics boom')).toBeInTheDocument();
  });
});
