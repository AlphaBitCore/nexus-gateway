import { describe, it, expect, vi, beforeEach, afterEach } from 'vitest';
import { render, screen, fireEvent, waitFor } from '@testing-library/react';

// t echoes the key (plus interpolated who) so assertions don't depend on i18n init.
vi.mock('react-i18next', () => ({
  useTranslation: () => ({
    t: (k: string, vars?: Record<string, string>) => (vars?.who ? `${k}:${vars.who}` : k),
  }),
}));

let mockData: { rows: unknown[]; total?: number } = { rows: [], total: 0 };
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
  ourVendorSpendUsd: 9.5,
  ourInternalOpsUsd: 0.5,
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

  it('splits the difference into estimator error and internal-ops spend', () => {
    // The three money bases are distinct quantities and must each get their own
    // cell: the customer-quota estimate, the internal-ops overhead, and the
    // total vendor spend the difference is actually computed from. Collapsing
    // any two would leave "ours $27.93 vs vendor $41.56, difference $0.00"
    // reading as nonsense.
    mockData = {
      rows: [
        {
          ...scopedRow,
          ourBilledUsd: 27.93,
          ourVendorSpendUsd: 41.56,
          ourInternalOpsUsd: 13.63,
          vendorReportedUsd: 41.56,
          diffUsd: 0,
          diffPct: 0,
        },
      ],
    };
    render(<VendorBillReconciliationPage />);
    expect(screen.getByText('$27.93')).toBeTruthy();
    expect(screen.getByText('$13.63')).toBeTruthy();
    expect(screen.getAllByText('$41.56').length).toBe(2); // our vendor spend + vendor billed
    expect(screen.getByText('$0.00')).toBeTruthy();
    // A row with a recorded basis is a live comparison, not a legacy row.
    expect(screen.queryByText('pages:vendorBillReconciliation.notComparable')).toBeNull();
  });

  it('shows an em-dash, never $0.00, for money it does not know', () => {
    // The page's standing rule: an absent money cell shows an em-dash. The two
    // reconciliation columns are placeholder zeros on a pre-cutover row, and an
    // operator summing a window that spans the cutover would otherwise
    // understate months of real vendor spend by treating them as $0.00.
    mockData = {
      rows: [{ ...scopedRow, ourBilledUsd: 27.93, ourVendorSpendUsd: 0, ourInternalOpsUsd: 0 }],
    };
    render(<VendorBillReconciliationPage />);
    expect(screen.queryByText('$0.00')).toBeNull();
    // The customer estimate IS known on a pre-cutover row, so it still renders.
    expect(screen.getByText('$27.93')).toBeTruthy();
  });

  it('renders an idle day as real $0.00 money, not as unknown', () => {
    // A day the vendor reported $0 for and we recorded no spend on is written by
    // the Hub as a scoped row of zeros, precisely so a blank cannot be read as
    // either "no traffic" or "never reconciled". Those zeros ARE the
    // measurement — rendering them as em-dashes would put the ambiguity the row
    // exists to remove straight back on the page.
    mockData = {
      rows: [
        {
          ...scopedRow,
          day: '2026-08-16',
          ourBilledUsd: 0,
          ourVendorSpendUsd: 0,
          ourInternalOpsUsd: 0,
          vendorReportedUsd: 0,
          diffUsd: 0,
          diffPct: 0,
          scopeKind: 'api_key',
          coverage: 'scoped',
        },
      ],
    };
    render(<VendorBillReconciliationPage />);
    // Customer estimate, internal ops, our vendor spend, vendor billed, diff.
    expect(screen.getAllByText('$0.00').length).toBe(5);
    expect(screen.queryByText('—')).toBeNull();
    expect(screen.getByText('pages:vendorBillReconciliation.coverage.scoped')).toBeTruthy();
  });

  it('does not mark an idle day not comparable', () => {
    // Its basis is zero, but so is the vendor's side: the two agree exactly, so
    // it is the most comparable row on the page. The pre-cutover annotation
    // keys on a real difference beside an absent basis, which this is not.
    mockData = {
      rows: [
        {
          ...scopedRow,
          ourBilledUsd: 0,
          ourVendorSpendUsd: 0,
          ourInternalOpsUsd: 0,
          vendorReportedUsd: 0,
          diffUsd: 0,
          diffPct: 0,
        },
      ],
    };
    render(<VendorBillReconciliationPage />);
    expect(screen.queryByText('pages:vendorBillReconciliation.notComparable')).toBeNull();
  });

  it('renders a no_basis row as vendor-only with every own figure unknown', () => {
    // The placeholder the reconcile job writes for a day the vendor billed but
    // we recorded no spend for. The vendor number is real and must show; all
    // three of our own figures are placeholders and must not.
    mockData = {
      rows: [
        {
          ...scopedRow,
          coverage: 'no_basis',
          ourBilledUsd: 0,
          ourVendorSpendUsd: 0,
          ourInternalOpsUsd: 0,
          vendorReportedUsd: 41.56,
          diffUsd: null,
          diffPct: null,
        },
      ],
    };
    render(<VendorBillReconciliationPage />);
    expect(screen.getByText('pages:vendorBillReconciliation.coverage.no_basis')).toBeTruthy();
    expect(screen.getByText('$41.56')).toBeTruthy();
    expect(screen.queryByText('$0.00')).toBeNull();
    // Customer estimate + internal ops + vendor spend + diff + diff% = 5 dashes.
    expect(screen.getAllByText('—').length).toBe(5);
    // No difference to annotate — the coverage badge already says it all.
    expect(screen.queryByText('pages:vendorBillReconciliation.notComparable')).toBeNull();
  });

  it('marks a pre-cutover row not comparable and drops its drift colour', () => {
    // Router cost was never recorded before this shipped, so such a row's
    // difference measures a different quantity and no backfill can fix it. It
    // must be annotated rather than presented next to live differences as if
    // they were the same number.
    mockData = {
      rows: [{ ...scopedRow, ourVendorSpendUsd: 0, ourInternalOpsUsd: 0, diffUsd: 3, diffPct: 0.3 }],
    };
    render(<VendorBillReconciliationPage />);
    const diffCell = screen.getByText('pages:vendorBillReconciliation.notComparable').closest('td');
    expect(diffCell).toBeTruthy();
    expect(diffCell?.textContent).toContain('$3.00');
    // No over/under colour: a difference that is not comparable must not read
    // as a live drift signal.
    expect((diffCell as HTMLElement).style.color).toBe('');
  });

  it('does not mark a fetch_failed row not comparable', () => {
    // A fetch_failed placeholder also carries a zero basis, but it has no
    // difference at all — annotating it would be noise about a row that already
    // reads as em-dashes.
    mockData = {
      rows: [
        {
          ...scopedRow,
          coverage: 'fetch_failed',
          ourVendorSpendUsd: 0,
          ourInternalOpsUsd: 0,
          vendorReportedUsd: null,
          diffUsd: null,
          diffPct: null,
        },
      ],
    };
    render(<VendorBillReconciliationPage />);
    expect(screen.queryByText('pages:vendorBillReconciliation.notComparable')).toBeNull();
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
    expect(fetchedRange()).toMatchObject({ from: '2026-06-21', to: '2026-07-21' });
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
    expect(fetchedRange()).toMatchObject({ from: expectedFrom, to: '2026-07-21' });
    // The key must change, otherwise the cached previous window would be reused
    // and the table would silently show the wrong period's rows.
    expect(lastKey).toContain(token);
  });

  // Paging is server-side, so the page arguments have to reach the request and
  // the pager has to size itself from the window-wide total rather than the
  // length of the page it was handed.
  it('sends a page window and keys the request on it', () => {
    mockData = { rows: [scopedRow], total: 137 };
    render(<VendorBillReconciliationPage />);
    expect(fetchedRange()).toMatchObject({ limit: 10, offset: 0 });
    expect(lastKey).toContain('10');
    expect(lastKey).toContain('0');
  });

  it('advancing a page moves the offset, not just the rendered slice', () => {
    mockData = { rows: [scopedRow], total: 137 };
    render(<VendorBillReconciliationPage />);
    fireEvent.click(screen.getByText('2'));
    expect(fetchedRange()).toMatchObject({ limit: 10, offset: 10 });
  });

  it('returns to page one when the window changes', () => {
    mockData = { rows: [scopedRow], total: 137 };
    render(<VendorBillReconciliationPage />);
    fireEvent.click(screen.getByText('2'));
    expect(fetchedRange()).toMatchObject({ offset: 10 });
    // A shorter window can hold fewer rows than the current offset skips, which
    // would strand the user on a blank page with nothing explaining why.
    fireEvent.click(screen.getByText('7d'));
    expect(fetchedRange()).toMatchObject({ from: '2026-07-14', offset: 0 });
  });

  it('hides the pager when the window holds nothing to page through', () => {
    mockData = { rows: [], total: 0 };
    render(<VendorBillReconciliationPage />);
    expect(screen.queryByLabelText('common:listPaginationNav')).toBeNull();
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
