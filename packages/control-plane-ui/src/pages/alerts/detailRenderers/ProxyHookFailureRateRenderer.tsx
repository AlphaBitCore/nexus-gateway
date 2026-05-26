/**
 * ProxyHookFailureRateRenderer — presents a `proxy.hook_failure_rate` or
 * `proxy.hook_timeout_rate` alert's evidence.
 *
 * Both rules ship the same details shape from the compliance-proxy evaluator
 * (`packages/compliance-proxy/cmd/compliance-proxy/init.go`):
 *
 *   proxy.hook_failure_rate:
 *     { failures: number, decisions: number, ratePct: number }
 *   proxy.hook_timeout_rate:
 *     { timeouts: number, decisions: number, ratePct: number }
 *
 * The Hub rule params expose a configured `thresholdPct`, but the evaluator
 * does not currently echo it into `details` — so we surface whatever Hub
 * populates. Fields missing from `details` render as an em dash. If a future
 * producer adds a `breakdown: Record<string, number>` field, it is rendered as
 * a small table; otherwise the table is skipped.
 */
import { useTranslation } from 'react-i18next';
import styles from './renderer.module.css';
import type { DetailRendererProps } from './types';

const DASH = '—';

function numOrUndef(v: unknown): number | undefined {
  return typeof v === 'number' && Number.isFinite(v) ? v : undefined;
}

function breakdownOrUndef(v: unknown): Array<[string, number]> | undefined {
  if (!v || typeof v !== 'object' || Array.isArray(v)) return undefined;
  const entries: Array<[string, number]> = [];
  for (const [k, val] of Object.entries(v as Record<string, unknown>)) {
    if (typeof val === 'number' && Number.isFinite(val)) {
      entries.push([k, val]);
    }
  }
  return entries.length > 0 ? entries : undefined;
}

function fmtPct(n: number | undefined): string {
  if (n == null) return DASH;
  return `${n.toFixed(1)}%`;
}

export function ProxyHookFailureRateRenderer({ alert }: DetailRendererProps) {
  const { t } = useTranslation();
  const d = alert.details ?? {};

  const ratePct = numOrUndef(d.ratePct);
  const decisions = numOrUndef(d.decisions);
  // Either "failures" (hook_failure_rate) or "timeouts" (hook_timeout_rate).
  const failures = numOrUndef(d.failures) ?? numOrUndef(d.timeouts);
  const thresholdPct = numOrUndef(d.thresholdPct);
  const breakdown = breakdownOrUndef(d.breakdown);

  return (
    <div>
      <div className={styles.emphasis}>{fmtPct(ratePct)}</div>
      <div className={styles.muted}>
        {thresholdPct != null
          ? t('pages:alerts.detailRenderers.hookRate.rateVsThreshold', {
              threshold: fmtPct(thresholdPct),
            })
          : t('pages:alerts.detailRenderers.hookRate.observedRate')}
      </div>

      <div className={styles.statGrid} style={{ marginTop: 'var(--g-space-3)' }}>
        <span className={styles.statLabel}>
          {t('pages:alerts.detailRenderers.hookRate.failureCount')}
        </span>
        <span className={styles.statValue}>
          {failures != null ? failures : DASH}
        </span>

        <span className={styles.statLabel}>
          {t('pages:alerts.detailRenderers.hookRate.sampleCount')}
        </span>
        <span className={styles.statValue}>
          {decisions != null ? decisions : DASH}
        </span>
      </div>

      {breakdown && (
        <table className={styles.table} style={{ marginTop: 'var(--g-space-3)' }}>
          <thead>
            <tr>
              <th>{t('pages:alerts.detailRenderers.hookRate.breakdownKey')}</th>
              <th>{t('pages:alerts.detailRenderers.hookRate.breakdownValue')}</th>
            </tr>
          </thead>
          <tbody>
            {breakdown.map(([k, v]) => (
              <tr key={k}>
                <td>{k}</td>
                <td>{v}</td>
              </tr>
            ))}
          </tbody>
        </table>
      )}
    </div>
  );
}
