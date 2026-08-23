import { useState } from 'react';
import { useTranslation } from 'react-i18next';
import {
  analyticsApi,
  type VendorBillReconciliationRow,
  type VendorBillCoverage,
} from '@/api/services/overview/analytics';
import { useApi } from '@/hooks/useApi';
import { Button } from '@nexus-gateway/ui-shared';
import {
  ListPagination,
  DEFAULT_ADMIN_LIST_PAGE_SIZE,
  type AdminListPageSize,
} from '@/components/ui/ListPagination';
import styles from './VendorBillReconciliationPage.module.css';

// Providers with no dollar-denominated cost API in v1 — surfaced honestly so
// an operator knows the report's coverage boundary rather than assuming silence
// means "no drift".
const NOT_COVERED = ['gemini', 'deepseek', 'moonshot'] as const;

/**
 * Reporting window, as day-count tokens matching the Cache ROI dashboard's
 * range selector so the two Overview reports read the same way. Reconciliation
 * adds 180d: vendor billing questions are often raised a quarter or two after
 * the fact, and its rows are one per provider per day, so a long window stays
 * small. 30d is the default — it lines up with how vendors bill.
 */
type Period = '7d' | '30d' | '90d' | '180d';
const PERIODS: readonly Period[] = ['7d', '30d', '90d', '180d'] as const;
const PERIOD_DAYS: Record<Period, number> = { '7d': 7, '30d': 30, '90d': 90, '180d': 180 };
/** Widest window offered — an empty result here means "no rows at all". */
const WIDEST_PERIOD: Period = '180d';

const isoDay = (d: Date) => d.toISOString().slice(0, 10);

/**
 * Window for a period, as whole UTC days. The endpoint takes YYYY-MM-DD and the
 * job writes one row per UTC day, so the window is counted in whole days back
 * from today.
 */
function buildRange(period: Period, now: Date = new Date()): { from: string; to: string } {
  const from = new Date(now);
  from.setUTCDate(from.getUTCDate() - PERIOD_DAYS[period]);
  return { from: isoDay(from), to: isoDay(now) };
}

function fmtUsd(v: number | null): string {
  return v === null ? '—' : `$${v.toFixed(2)}`;
}
function fmtPct(v: number | null): string {
  return v === null ? '—' : `${(v * 100).toFixed(1)}%`;
}

const rowKey = (r: VendorBillReconciliationRow) => `${r.providerId}:${r.day}`;

/**
 * Whether the row's reconciliation basis was actually recorded. When it was not,
 * `ourVendorSpendUsd` and `ourInternalOpsUsd` are placeholder zeros standing for
 * "unknown", and rendering them as `$0.00` would let an operator sum the column
 * and understate real vendor spend — so those cells show an em-dash instead.
 *
 * The second clause is what keeps a genuinely idle day out of that treatment. A
 * day the vendor reported $0 for and we recorded no spend on is written by the
 * reconcile job as a real `scoped` row of zeros, precisely so a blank cannot be
 * read as either "no traffic" or "never reconciled". Those zeros ARE the
 * measurement; showing them as em-dashes would put the ambiguity straight back
 * and, through `isPreCutover` below, brand a perfectly comparable row "not
 * comparable". A `fetch_failed` row is excluded by this clause as it should be:
 * its `vendorReportedUsd` is null, not 0, because no vendor figure was obtained.
 *
 * CROSS-PACKAGE COUPLING: reading `=== 0` as "not recorded" is only sound
 * because the Hub's rollup never persists a zero-valued row
 * (`rollup_5m_vendor_spend.go` drops zero-amount components and `rollup_5m.go`
 * drops zero-valued rows), so a recorded `vendor_spend_usd` sum is always
 * strictly positive. Nothing enforces that across the two packages; the Go-side
 * note lives on `TestReconcileProvider_ZeroVendorSpendIsWrittenWhenTheSeriesExists`,
 * and `drift_alert.go`'s `comparable` check depends on the same invariant.
 */
const hasRecordedBasis = (r: VendorBillReconciliationRow) =>
  r.ourVendorSpendUsd > 0 || r.vendorReportedUsd === 0;

/**
 * Rows written before the vendor-spend series shipped are not comparable: the
 * router's own cost was never recorded, so their difference measures a
 * different quantity from a row written since, and no backfill can reconstruct
 * it. Such a row is recognisable by a real difference sitting next to an absent
 * basis — the job never computes a difference without a recorded basis, so a
 * comparable row always has one. Detecting it from the data beats a hardcoded
 * cutover date, which would be wrong for every deployment that upgraded on a
 * different day.
 *
 * Deliberately narrower than `!hasRecordedBasis`: `fetch_failed` and `no_basis`
 * rows also lack a basis but carry no difference at all, so they are already
 * self-describing through their coverage badge and need no annotation. Kept in
 * step with `drift_alert.go`'s `comparable` check, which suppresses the same rows.
 */
const isPreCutover = (r: VendorBillReconciliationRow) => r.diffUsd !== null && !hasRecordedBasis(r);

export function VendorBillReconciliationPage() {
  const { t } = useTranslation();
  const [period, setPeriod] = useState<Period>('30d');
  const [limit, setLimit] = useState<AdminListPageSize>(DEFAULT_ADMIN_LIST_PAGE_SIZE);
  const [offset, setOffset] = useState(0);
  // period/limit/offset are all part of the queryKey so changing any of them
  // refetches rather than re-rendering the previous page's rows.
  const { data, loading, error, refetch } = useApi(
    () => analyticsApi.vendorBillReconciliation({ ...buildRange(period), limit, offset }),
    ['admin', 'analytics', 'vendor-bill-reconciliation', period, String(limit), String(offset)],
  );

  const [notes, setNotes] = useState<Record<string, string>>({});
  const [busy, setBusy] = useState<string | null>(null);

  const rows = data?.rows ?? [];
  // Paging is server-side, so the row count of the current page says nothing
  // about the window; `total` is the only figure the pager can size itself from.
  const total = data?.total ?? 0;

  /**
   * Narrowing the window can leave the current offset past the end of the
   * shorter result set, which would land the user on a blank page with no
   * indication why. Every period change therefore returns to page one.
   */
  function changePeriod(p: Period) {
    setPeriod(p);
    setOffset(0);
  }
  const coverageLabel = (c: VendorBillCoverage) => t(`pages:vendorBillReconciliation.coverage.${c}`);

  async function review(r: VendorBillReconciliationRow) {
    const key = rowKey(r);
    setBusy(key);
    try {
      await analyticsApi.reviewVendorBillReconciliation(r.providerId, r.day, notes[key]);
      await refetch();
    } finally {
      setBusy(null);
    }
  }

  return (
    <div className={styles.page}>
      <div className={styles.header}>
        <div>
          <h1 className={styles.title}>{t('pages:vendorBillReconciliation.title')}</h1>
          <p className={styles.subtitle}>{t('pages:vendorBillReconciliation.subtitle')}</p>
        </div>
        <div
          className={styles.periodButtons}
          role="group"
          aria-label={t('pages:vendorBillReconciliation.period.label')}
        >
          {PERIODS.map((p) => (
            <Button
              key={p}
              size="sm"
              variant={period === p ? 'primary' : 'secondary'}
              aria-pressed={period === p}
              onClick={() => changePeriod(p)}
            >
              {/* Rendered raw, like the Cache ROI selector: a day-count token
                  is a unit symbol, not prose to translate. */}
              {p}
            </Button>
          ))}
        </div>
      </div>

      {loading && <p className={styles.muted}>{t('common:loading')}</p>}
      {error && <p className={styles.error}>{t('common:failedToLoad')}</p>}

      {!loading && !error && rows.length === 0 && (
        // Distinguish "the job has never produced anything" from "nothing in the
        // window you picked" — the year view answers which one it is.
        <p className={styles.muted}>
          {period === WIDEST_PERIOD
            ? t('pages:vendorBillReconciliation.empty')
            : t('pages:vendorBillReconciliation.emptyForPeriod')}
        </p>
      )}

      {rows.length > 0 && (
        <table className={styles.table}>
          <thead>
            <tr>
              <th>{t('pages:vendorBillReconciliation.colProvider')}</th>
              <th>{t('pages:vendorBillReconciliation.colDay')}</th>
              <th>{t('pages:vendorBillReconciliation.colOurs')}</th>
              <th>{t('pages:vendorBillReconciliation.colInternalOps')}</th>
              <th>{t('pages:vendorBillReconciliation.colVendorSpend')}</th>
              <th>{t('pages:vendorBillReconciliation.colVendor')}</th>
              <th>{t('pages:vendorBillReconciliation.colDiff')}</th>
              <th>{t('pages:vendorBillReconciliation.colDiffPct')}</th>
              <th>{t('pages:vendorBillReconciliation.colCoverage')}</th>
              <th>{t('pages:vendorBillReconciliation.colStatus')}</th>
              <th>{t('pages:vendorBillReconciliation.colReview')}</th>
            </tr>
          </thead>
          <tbody>
            {rows.map((r) => {
              const key = rowKey(r);
              const preCutover = isPreCutover(r);
              // Placeholder zeros must read as "unknown", not as money: this
              // page's rule is that an absent money cell shows an em-dash, never
              // a fake $0, so that summing a column cannot understate spend.
              const basis = hasRecordedBasis(r) ? r.ourVendorSpendUsd : null;
              const internalOps = hasRecordedBasis(r) ? r.ourInternalOpsUsd : null;
              // A no_basis row was never compared, so its quota figure was never
              // read either — 0 there is a placeholder too. Every other coverage
              // carries a real ourBilledUsd.
              const customerEstimate = r.coverage === 'no_basis' ? null : r.ourBilledUsd;
              // A pre-cutover difference is not measuring the same quantity, so
              // it must not be coloured as if it were a live drift signal.
              const diffColor =
                r.diffUsd === null || preCutover
                  ? undefined
                  : r.diffUsd >= 0
                    ? 'var(--color-danger)'
                    : 'var(--color-success)';
              return (
                <tr key={key}>
                  <td>{r.providerName || r.providerId}</td>
                  <td>{r.day}</td>
                  <td>{fmtUsd(customerEstimate)}</td>
                  <td>{fmtUsd(internalOps)}</td>
                  <td>{fmtUsd(basis)}</td>
                  <td>{fmtUsd(r.vendorReportedUsd)}</td>
                  <td className={preCutover ? styles.muted : undefined} style={{ color: diffColor }}>
                    {fmtUsd(r.diffUsd)}
                    {preCutover && (
                      <span
                        className={styles.badge}
                        data-note="not_comparable"
                        title={t('pages:vendorBillReconciliation.notComparableHint')}
                      >
                        {t('pages:vendorBillReconciliation.notComparable')}
                      </span>
                    )}
                  </td>
                  <td className={preCutover ? styles.muted : undefined}>{fmtPct(r.diffPct)}</td>
                  <td>
                    <span className={styles.badge} data-coverage={r.coverage}>
                      {coverageLabel(r.coverage)}
                    </span>
                  </td>
                  <td>
                    {r.status === 'reviewed'
                      ? t('pages:vendorBillReconciliation.reviewedBy', { who: r.reviewedBy ?? '' })
                      : t('pages:vendorBillReconciliation.statusOpen')}
                  </td>
                  <td>
                    {r.status === 'open' ? (
                      <div className={styles.reviewCell}>
                        <input
                          className={styles.noteInput}
                          value={notes[key] ?? ''}
                          placeholder={t('pages:vendorBillReconciliation.notePlaceholder')}
                          onChange={(e) => setNotes((n) => ({ ...n, [key]: e.target.value }))}
                        />
                        <Button size="sm" disabled={busy === key} onClick={() => void review(r)}>
                          {t('pages:vendorBillReconciliation.markReviewed')}
                        </Button>
                      </div>
                    ) : (
                      <span className={styles.muted}>{r.note}</span>
                    )}
                  </td>
                </tr>
              );
            })}
          </tbody>
        </table>
      )}

      {/* Shares the admin-list pager so this report's paging reads and behaves
          exactly like Audit Logs and every other paged admin table. */}
      <ListPagination
        offset={offset}
        limit={limit}
        total={total}
        onOffsetChange={setOffset}
        onLimitChange={setLimit}
      />

      <section className={styles.notCovered}>
        <h2 className={styles.sectionTitle}>{t('pages:vendorBillReconciliation.notCoveredTitle')}</h2>
        <p className={styles.muted}>{t('pages:vendorBillReconciliation.notCoveredDesc')}</p>
        <ul className={styles.notCoveredList}>
          {NOT_COVERED.map((p) => (
            <li key={p}>{t(`pages:vendorBillReconciliation.notCovered.${p}`)}</li>
          ))}
        </ul>
      </section>
    </div>
  );
}
