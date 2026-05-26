/**
 * QuotaVkExpiringRenderer — presents a `quota.vk_expiring` alert's evidence.
 *
 * Expected `alert.details` shape (from
 * `packages/nexus-hub/internal/jobs/vk_expiry.go`):
 *
 *   {
 *     vkId:      string,
 *     name:      string,
 *     expiresAt: string (RFC3339),
 *     daysLeft:  number,
 *   }
 *
 * The Hub raises one alert per expiring Virtual Key, so each alert carries a
 * single key's metadata — not an array. We render the single row as a compact
 * table so adding a future batched producer remains a drop-in (just loop the
 * same renderer over `details.keys` once Hub populates it).
 *
 * Missing or wrong-typed fields render as an em dash.
 */
import { useTranslation } from 'react-i18next';
import styles from './renderer.module.css';
import type { DetailRendererProps } from './types';

const DASH = '—';

function strOrUndef(v: unknown): string | undefined {
  return typeof v === 'string' && v.length > 0 ? v : undefined;
}

function numOrUndef(v: unknown): number | undefined {
  return typeof v === 'number' && Number.isFinite(v) ? v : undefined;
}

function fmtDate(raw: string | undefined): string {
  if (!raw) return DASH;
  const d = new Date(raw);
  if (Number.isNaN(d.getTime())) return raw;
  return d.toLocaleString();
}

export function QuotaVkExpiringRenderer({ alert }: DetailRendererProps) {
  const { t } = useTranslation();
  const d = alert.details ?? {};

  const name = strOrUndef(d.name);
  const vkId = strOrUndef(d.vkId);
  const expiresAt = strOrUndef(d.expiresAt);
  const daysLeft = numOrUndef(d.daysLeft);

  const nameCell = name ?? vkId ?? DASH;

  return (
    <table className={styles.table}>
      <thead>
        <tr>
          <th>{t('pages:alerts.detailRenderers.quotaVkExpiring.name')}</th>
          <th>{t('pages:alerts.detailRenderers.quotaVkExpiring.expiresAt')}</th>
          <th>{t('pages:alerts.detailRenderers.quotaVkExpiring.daysLeft')}</th>
        </tr>
      </thead>
      <tbody>
        <tr>
          <td>{nameCell}</td>
          <td>{fmtDate(expiresAt)}</td>
          <td>{daysLeft != null ? daysLeft : DASH}</td>
        </tr>
      </tbody>
    </table>
  );
}
