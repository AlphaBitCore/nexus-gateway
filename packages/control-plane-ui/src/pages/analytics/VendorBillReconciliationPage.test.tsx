import { describe, it, expect, vi, beforeEach, afterEach } from 'vitest';
import { render, screen, fireEvent, waitFor } from '@testing-library/react';

// t echoes the key (plus interpolated who) so assertions don't depend on i18n init.
vi.mock('react-i18next', () => ({
  useTranslation: () => ({
    t: (k: string, vars?: Record<string, string>) => (vars?.who ? `${k}:${vars.who}` : k),
  }),
}));

let mockData: { rows: unknown[] } = { rows: [] };
const { mockRefetch, mockReviewSpy } = vi.hoisted(() => ({
  mockRefetch: vi.fn(),
  mockReviewSpy: vi.fn(async (_providerId?: string, _day?: string, _note?: string) => ({
    status: 'reviewed',
    reviewedBy: 'me',
    providerId: 'p',
    day: 'd',
  })),
}));
// Capture the fetcher + queryKey so the window actually sent to the API (and the
// key that drives refetching) can be asserted, not just the rendered markup.
let lastFetcher: (() => unknown) | null = null;
let lastKey: unknown[] = [];
vi.mock('@/hooks/useApi', () => ({
  useApi: (fetcher: () => unknown, key: unknown[]) => {
    lastFetcher = fetcher;
    lastKey = key;
    return { data: mockData, loading: false, error: null, refetch: mockRefetch };
  },
}));
const { mockListSpy } = vi.hoisted(() => ({ mockListSpy: vi.fn() }));
vi.mock('@/api/services/overview/analytics', () => ({
  analyticsApi: {
    vendorBillReconciliation: mockListSpy,
    reviewVendorBillReconciliation: mockReviewSpy,
  },
}));

import { VendorBillReconciliationPage } from './VendorBillReconciliationPage';

/** Invoke whatever fetcher the page handed useApi and return its params. */
function fetchedRange(): { from: string; to: string } {
  mockListSpy.mockClear();
  lastFetcher?.();
  return mockListSpy.mock.calls[0][0] as { from: string; to: string };
}

const scopedRow = {
  providerId: 'prov1',
  providerName: 'OpenAI',
  day: '2026-07-17',
  ourBilledUsd: 9,
  vendorReportedUsd: 10,
  diffUsd: 1,
  diffPct: 0.1,
  scopeKind: 'project',
  coverage: 'scoped',
  status: 'open',
  reviewedBy: null,
  note: null,
  updatedAt: 'x',
};

describe('VendorBillReconciliationPage', () => {
  beforeEach(() => {
    mockRefetch.mockClear();
    mockReviewSpy.mockClear();
  });

  it('renders a scoped row with its coverage badge and formatted diff', () => {
    mockData = { rows: [scopedRow] };
    render(<VendorBillReconciliationPage />);
    expect(screen.getByText('OpenAI')).toBeTruthy();
    expect(screen.getByText('pages:vendorBillReconciliation.coverage.scoped')).toBeTruthy();
    expect(screen.getByText('$1.00')).toBeTruthy();
    expect(screen.getByText('10.0%')).toBeTruthy();
  });

  it('keeps null vendor/diff as em-dash (fetch_failed row)', () => {
    mockData = {
      rows: [{ ...scopedRow, coverage: 'fetch_failed', vendorReportedUsd: null, diffUsd: null, diffPct: null }],
    };
    render(<VendorBillReconciliationPage />);
    expect(screen.getByText('pages:vendorBillReconciliation.coverage.fetch_failed')).toBeTruthy();
    // two em-dashes: vendor + diff (diffPct also shows em-dash → at least 2)
    expect(screen.getAllByText('—').length).toBeGreaterThanOrEqual(2);
  });

  it('review action posts the ack with the note and refetches', async () => {
    mockData = { rows: [scopedRow] };
    render(<VendorBillReconciliationPage />);
    fireEvent.change(screen.getByPlaceholderText('pages:vendorBillReconciliation.notePlaceholder'), {
      target: { value: 'looks fine' },
    });
    fireEvent.click(screen.getByText('pages:vendorBillReconciliation.markReviewed'));
    await waitFor(() => expect(mockReviewSpy).toHaveBeenCalledWith('prov1', '2026-07-17', 'looks fine'));
    await waitFor(() => expect(mockRefetch).toHaveBeenCalled());
  });

  it('shows the not-covered panel with all three v1-excluded providers', () => {
    mockData = { rows: [] };
    render(<VendorBillReconciliationPage />);
    expect(screen.getByText('pages:vendorBillReconciliation.notCovered.gemini')).toBeTruthy();
    expect(screen.getByText('pages:vendorBillReconciliation.notCovered.deepseek')).toBeTruthy();
    expect(screen.getByText('pages:vendorBillReconciliation.notCovered.moonshot')).toBeTruthy();
  });
});

describe('VendorBillReconciliationPage — reporting period', () => {
  beforeEach(() => {
    mockData = { rows: [] };
    mockListSpy.mockClear();
    // Freeze time so the computed window is deterministic. Mid-month and
    // mid-year on purpose, so month/year arithmetic cannot accidentally pass
    // via a boundary coincidence.
    vi.useFakeTimers();
    vi.setSystemTime(new Date('2026-07-21T09:30:00Z'));
  });
  afterEach(() => {
    vi.useRealTimers();
  });

  it('offers the same day-count tokens as the Cache ROI selector, plus 180d', () => {
    render(<VendorBillReconciliationPage />);
    for (const token of ['7d', '30d', '90d', '180d']) {
      expect(screen.getByText(token)).toBeTruthy();
    }
  });

  it('defaults to 30d', () => {
    render(<VendorBillReconciliationPage />);
    expect(fetchedRange()).toEqual({ from: '2026-06-21', to: '2026-07-21' });
    expect(lastKey).toContain('30d');
  });

  // Each token must resolve to exactly that many days back — an off-by-one or a
  // month/year approximation would silently query the wrong window.
  it.each([
    ['7d', '2026-07-14'],
    ['30d', '2026-06-21'],
    ['90d', '2026-04-22'],
    ['180d', '2026-01-22'],
  ])('%s spans exactly that many days back', (token, expectedFrom) => {
    render(<VendorBillReconciliationPage />);
    fireEvent.click(screen.getByText(token));
    expect(fetchedRange()).toEqual({ from: expectedFrom, to: '2026-07-21' });
    // The key must change, otherwise the cached previous window would be reused
    // and the table would silently show the wrong period's rows.
    expect(lastKey).toContain(token);
  });

  it('empty 30d reads as "nothing in this window", empty 180d as "nothing at all"', () => {
    render(<VendorBillReconciliationPage />);
    // Default (30d): absence is about the window, not the whole report.
    expect(screen.getByText('pages:vendorBillReconciliation.emptyForPeriod')).toBeTruthy();

    fireEvent.click(screen.getByText('180d'));
    // Nothing across the widest window offered means the job has genuinely
    // never produced a row.
    expect(screen.getByText('pages:vendorBillReconciliation.empty')).toBeTruthy();
  });
});
