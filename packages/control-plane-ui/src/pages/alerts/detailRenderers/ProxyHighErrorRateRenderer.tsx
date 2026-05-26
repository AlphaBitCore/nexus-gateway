/**
 * ProxyHighErrorRateRenderer — presents a `proxy.high_error_rate` alert's
 * evidence (5xx response rate).
 *
 * Hub registers the rule with params `{ thresholdPct, windowSec, minSamples }`
 * (`packages/nexus-hub/internal/alerting/rules/builtin.go`). A dedicated
 * producer for this rule is not in the tree yet — the expected details shape
 * mirrors the hook-rate producers' convention (`ratePct` + a sample total and
 * a failure total). The renderer is field-tolerant: it reads whatever numeric
 * fields arrive (`ratePct`, `errors` / `fivexx`, `total` / `requests`,
 * `thresholdPct`, `windowSec`) and renders an em dash or skips rows for the
 * rest so a future producer can light up cleanly.
 */
import { useTranslation } from 'react-i18next';
import styles from './renderer.module.css';
import type { DetailRendererProps } from './types';

const DASH = '—';

function numOrUndef(v: unknown): number | undefined {
  return typeof v === 'number' && Number.isFinite(v) ? v : undefined;
}

function fmtPct(n: number | undefined): string {
  if (n == null) return DASH;
  return `${n.toFixed(1)}%`;
}

export function ProxyHighErrorRateRenderer({ alert }: DetailRendererProps) {
  const { t } = useTranslation();
  const d = alert.details ?? {};

  const ratePct = numOrUndef(d.ratePct);
  const thresholdPct = numOrUndef(d.thresholdPct);
  const windowSec = numOrUndef(d.windowSec);
  // Accept either naming convention the Hub producer may pick.
  const errors = numOrUndef(d.errors) ?? numOrUndef(d.fivexx);
  const total = numOrUndef(d.total) ?? numOrUndef(d.requests);

  return (
    <div>
      <div className={styles.emphasis}>{fmtPct(ratePct)}</div>
      <div className={styles.muted}>
        {thresholdPct != null
          ? t('pages:alerts.detailRenderers.highErrorRate.rateVsThreshold', {
              threshold: fmtPct(thresholdPct),
            })
          : t('pages:alerts.detailRenderers.highErrorRate.observedRate')}
      </div>

      <div className={styles.statGrid} style={{ marginTop: 'var(--g-space-3)' }}>
        <span className={styles.statLabel}>
          {t('pages:alerts.detailRenderers.highErrorRate.errorCount')}
        </span>
        <span className={styles.statValue}>
          {errors != null ? errors : DASH}
        </span>

        <span className={styles.statLabel}>
          {t('pages:alerts.detailRenderers.highErrorRate.requestCount')}
        </span>
        <span className={styles.statValue}>{total != null ? total : DASH}</span>

        {windowSec != null && (
          <>
            <span className={styles.statLabel}>
              {t('pages:alerts.detailRenderers.highErrorRate.window')}
            </span>
            <span className={styles.statValue}>
              {t('pages:alerts.detailRenderers.highErrorRate.windowValue', {
                seconds: windowSec,
              })}
            </span>
          </>
        )}
      </div>
    </div>
  );
}
