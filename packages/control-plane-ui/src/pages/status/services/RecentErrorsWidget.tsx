/**
 * RecentErrorsWidget — compact 5-row preview of the latest ERROR/FATAL
 * `thing_diag_event` rows, surfaced at the bottom of the Status Overview tab.
 *
 * The widget reads from `/api/admin/diag-events?level=error&limit=5`. The CP
 * handler validates levels individually (no combined `error,fatal` filter),
 * so this view shows ERROR rows only — the dedicated Recent Errors page
 * renders the full ERROR + FATAL stream with filters and pagination.
 *
 * Visibility rules:
 *   - 0 events → an "empty" hint sits in place of the table.
 *   - Any auth/IAM error from the API surfaces silently as the empty state;
 *     full-page errors are owned by the dedicated page.
 *   - The "View all" link points at `/infrastructure/errors`.
 */

import { Link } from 'react-router-dom';
import { useTranslation } from 'react-i18next';
import { useApi } from '../../../hooks/useApi';
import { Card } from '@/components/ui';
import { diagEventsApi } from '@/api/services';
import type { DiagEventListResponse } from '@/api/services/infrastructure/diag/diagevents';
import styles from '../overview/StatusPage.module.css';

const RECENT_ERRORS_LIMIT = 5;

/** Format an ISO timestamp into a compact local time string. */
function fmtTime(iso: string): string {
  try {
    return new Date(iso).toLocaleTimeString();
  } catch {
    return iso;
  }
}

/** Truncate the message to one line and at most 120 chars for the compact row. */
function truncate(s: string, max = 120): string {
  if (s.length <= max) return s;
  return s.slice(0, max - 1) + '…';
}

export function RecentErrorsWidget() {
  const { t } = useTranslation();
  const { data, loading } = useApi<DiagEventListResponse>(
    () =>
      diagEventsApi
        .list({ level: 'error', limit: RECENT_ERRORS_LIMIT })
        .catch((): DiagEventListResponse => ({ data: [], nextCursor: '' })),
    ['admin', 'diag-events', 'recent-overview', RECENT_ERRORS_LIMIT],
  );

  const events = data?.data ?? [];

  return (
    <Card>
      <div className={styles.cacheHeaderRow}>
        <div className={styles.cacheTitle}>{t('pages:status.recentErrors.title')}</div>
        <Link to="/infrastructure/errors" className={styles.scopeLink}>
          {t('pages:status.recentErrors.viewAll')}
        </Link>
      </div>
      {events.length === 0 ? (
        <div className={styles.emptyText}>
          {loading ? '…' : t('pages:status.recentErrors.empty')}
        </div>
      ) : (
        <div className={styles.tableWrapper}>
          <table className={styles.table}>
            <thead>
              <tr>
                <th>{t('pages:status.recentErrors.colTime')}</th>
                <th>{t('pages:status.recentErrors.colLevel')}</th>
                <th>{t('pages:status.recentErrors.colThing')}</th>
                <th>{t('pages:status.recentErrors.colSource')}</th>
                <th>{t('pages:status.recentErrors.colMessage')}</th>
              </tr>
            </thead>
            <tbody>
              {events.map((e) => (
                <tr key={e.id}>
                  <td>{fmtTime(e.occurredAt)}</td>
                  <td>{String(e.level).toUpperCase()}</td>
                  <td>
                    <code style={{ fontSize: 'var(--g-font-size-xs)' }}>{e.nodeId}</code>
                  </td>
                  <td>{e.source}</td>
                  <td>{truncate(e.message)}</td>
                </tr>
              ))}
            </tbody>
          </table>
        </div>
      )}
    </Card>
  );
}
