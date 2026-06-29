/**
 * MetadataPanel — Node Detail Overview tab metadata renderer.
 *
 * Renders `thing.metadata` (a flexible JSONB blob written by services
 * via selfreg, enrollment, etc.) in three zones:
 *   1. Common — friendly-labelled rows for the well-known keys most
 *      operators look for first (hostname, OS, who enrolled this, etc.).
 *   2. Custom — every remaining key the producer wrote that we don't
 *      have an explicit label for. Shown as `key → JSON value` so new
 *      keys appear automatically without a UI change.
 *   3. Raw JSON — full payload pretty-printed, default collapsed so it
 *      doesn't push the Configuration/Runtime tabs off-screen.
 *
 * The component is presentational; the parent (InfraNodeDetailPage)
 * supplies the metadata object directly off the Node API response.
 */
import { useTranslation } from 'react-i18next';
import styles from './MetadataPanel.module.css';

export interface MetadataPanelProps {
  metadata: Record<string, unknown> | null | undefined;
}

/**
 * Curated key list rendered in the "Common" zone, in display order.
 * Keep i18n labels in sync with this list in
 * src/i18n/locales/{en,zh,es}/pages.json
 * `infrastructure.overview.metadata.commonLabel.<key>`.
 *
 * New keys producers commonly add: add the key here AND in the locale
 * files. Keys that should NEVER auto-render (e.g. opaque tokens) can be
 * surfaced via the raw JSON details element below; the Custom zone
 * filters on COMMON_KEYS so explicit suppression isn't needed for typical
 * fields.
 */
const COMMON_KEYS: readonly string[] = [
  'hostname',
  'os',
  'osVersion',
  'pid',
  'role',
  'metricsUrl',
  'schedulerEnabled',
  'enrolledBy',
  'source_ip',
  'auth_type',
  'conn_protocol',
] as const;

function formatValue(v: unknown): string {
  if (v === null || v === undefined) return '—';
  if (typeof v === 'string') return v;
  if (typeof v === 'number' || typeof v === 'boolean') return String(v);
  try {
    return JSON.stringify(v);
  } catch {
    return String(v);
  }
}

export function MetadataPanel({ metadata }: MetadataPanelProps) {
  const { t } = useTranslation();

  const entries = metadata && typeof metadata === 'object' ? Object.entries(metadata) : [];
  const isEmpty = entries.length === 0;

  // Split into Common (curated order) and Custom (rest, sorted alphabetically).
  const presentCommon = COMMON_KEYS.filter((k) =>
    Object.prototype.hasOwnProperty.call(metadata ?? {}, k),
  );
  const customKeys = entries
    .map(([k]) => k)
    .filter((k) => !COMMON_KEYS.includes(k))
    .sort();

  return (
    <div>
      {isEmpty && (
        <p className={styles.empty}>
          {t('pages:infrastructure.overview.metadata.empty')}
        </p>
      )}

      {!isEmpty && presentCommon.length > 0 && (
        <>
          <div className={styles.subhead}>
            {t('pages:infrastructure.overview.metadata.commonSectionLabel')}
          </div>
          <dl className={styles.grid}>
            {presentCommon.map((key) => (
              <div key={key} className={styles.row}>
                <dt className={styles.label}>
                  {t(`pages:infrastructure.overview.metadata.commonLabel.${key}`, key)}
                </dt>
                <dd className={styles.value}>
                  {formatValue((metadata as Record<string, unknown>)[key])}
                </dd>
              </div>
            ))}
          </dl>
        </>
      )}

      {!isEmpty && customKeys.length > 0 && (
        <>
          <div className={styles.subhead}>
            {t('pages:infrastructure.overview.metadata.customSectionLabel')}
          </div>
          <dl className={styles.grid}>
            {customKeys.map((key) => (
              <div key={key} className={styles.row}>
                <dt className={styles.label}>{key}</dt>
                <dd className={styles.value}>
                  {formatValue((metadata as Record<string, unknown>)[key])}
                </dd>
              </div>
            ))}
          </dl>
        </>
      )}

      {!isEmpty && (
        <details className={styles.rawDetails}>
          <summary className={styles.rawSummary}>
            {t('pages:infrastructure.overview.metadata.rawSummary')}
          </summary>
          <pre className={styles.rawJson}>
            {JSON.stringify(metadata, null, 2)}
          </pre>
        </details>
      )}
    </div>
  );
}
