import { useState } from 'react';
import { useTranslation } from 'react-i18next';
import {
  analyticsApi,
  type VendorBillReconciliationRow,
  type VendorBillCoverage,
} from '@/api/services/overview/analytics';
import { useApi } from '@/hooks/useApi';
import { Button } from '@nexus-gateway/ui-shared';
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

export function VendorBillReconciliationPage() {
  const { t } = useTranslation();
  const [period, setPeriod] = useState<Period>('30d');
  // period is part of the queryKey so switching it refetches rather than
  // re-rendering stale rows from the previous window.
  const { data, loading, error, refetch } = useApi(
    () => analyticsApi.vendorBillReconciliation(buildRange(period)),
    ['admin', 'analytics', 'vendor-bill-reconciliation', period],
  );

  const [notes, setNotes] = useState<Record<string, string>>({});
  const [busy, setBusy] = useState<string | null>(null);

  const rows = data?.rows ?? [];
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
              onClick={() => setPeriod(p)}
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
              const diffColor =
                r.diffUsd === null ? undefined : r.diffUsd >= 0 ? 'var(--color-danger)' : 'var(--color-success)';
              return (
                <tr key={key}>
                  <td>{r.providerName || r.providerId}</td>
                  <td>{r.day}</td>
                  <td>{fmtUsd(r.ourBilledUsd)}</td>
                  <td>{fmtUsd(r.vendorReportedUsd)}</td>
                  <td style={{ color: diffColor }}>{fmtUsd(r.diffUsd)}</td>
                  <td>{fmtPct(r.diffPct)}</td>
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
