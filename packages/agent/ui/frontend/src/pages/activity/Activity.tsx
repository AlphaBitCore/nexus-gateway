import { useState } from 'react';
import { useQuery } from '@tanstack/react-query';
import { useTranslation } from 'react-i18next';
import { Button } from '@nexus-gateway/ui-shared';
import { agentApi } from '@/api/agent';
import type { LifecycleEvent } from '@/api/agent';
import styles from '../overview/Overview.module.css';
import page from '../_shared/pageStyles.module.css';

const PAGE_SIZE = 50;

/**
 * Activity page — agent lifecycle event timeline.
 *
 * Reads agent.{startup,shutdown,paused,resumed,sso_login,sso_logout}
 * rows from the local lifecycle_event table via QUERY_LIFECYCLE_EVENTS IPC.
 * Distinct from Traffic Events, which shows per-connection network audit rows.
 */
export function Activity() {
  const { t, i18n } = useTranslation();
  const [offset, setOffset] = useState(0);

  const { data, isLoading } = useQuery({
    queryKey: ['agent', 'lifecycle', offset],
    queryFn: () => agentApi.queryLifecycle({ offset, limit: PAGE_SIZE }),
    staleTime: 2_000,
  });

  const events = data?.events ?? [];
  const total = data?.total ?? 0;
  const hasMore = offset + PAGE_SIZE < total;

  return (
    <div className={styles.root}>
      <header>
        <h1 className={styles.title}>{t('activity.title')}</h1>
        <p className={styles.subtitle}>{t('activity.subtitle')}</p>
      </header>

      {isLoading ? (
        <p className={styles.empty}>Loading…</p>
      ) : events.length === 0 ? (
        <p className={styles.empty}>{t('activity.empty')}</p>
      ) : (
        <>
          <table className={styles.table}>
            <thead>
              <tr>
                <th>{t('activity.col.time')}</th>
                <th>{t('activity.col.action')}</th>
                <th>{t('activity.col.details')}</th>
              </tr>
            </thead>
            <tbody>
              {events.map((e, i) => (
                <tr key={e.id ?? `${offset}-${i}`}>
                  <td>{fmtTime(e.occurredAt, i18n.language)}</td>
                  <td>
                    <span className={styles.badge} data-action={e.action}>
                      {t(`activity.action.${toI18nActionKey(e.action)}`, { defaultValue: e.action })}
                    </span>
                  </td>
                  <td>{fmtDetails(e)}</td>
                </tr>
              ))}
            </tbody>
          </table>
          <div className={page.rowJustified}>
            <span className={page.mutedSmall}>
              {offset + 1}–{Math.min(offset + events.length, total)} of {total}
            </span>
            <div className={page.row}>
              <Button variant="ghost" disabled={offset === 0} onClick={() => setOffset(Math.max(0, offset - PAGE_SIZE))}>Previous</Button>
              <Button variant="ghost" disabled={!hasMore} onClick={() => setOffset(offset + PAGE_SIZE)}>Next</Button>
            </div>
          </div>
        </>
      )}
    </div>
  );
}

// Convert the wire action ("agent.startup") to the i18n action key
// ("agentStartup") so we can look it up in dashboard.json under
// activity.action.* without leaking dots through the key path.
function toI18nActionKey(action: string): string {
  // agent.startup -> agentStartup
  return action
    .split('.')
    .map((part, idx) => (idx === 0 ? part : part.charAt(0).toUpperCase() + part.slice(1)))
    .join('')
    .replace(/_(.)/g, (_, c) => c.toUpperCase());
}

function fmtTime(iso: string, locale: string): string {
  if (!iso) return '—';
  try {
    return new Date(iso).toLocaleString(locale);
  } catch {
    return iso;
  }
}

// Build a human-readable details string from the event's attrs.
// Most attrs surface as "key=value", but a few have nicer presentation:
//   shutdown.reason          → "(signal:15)" / "(user_quit_flag)"
//   paused.durationSec       → "(15 min)" / "(indefinite)"
//   sso_login.email          → "(tiebin@example.com)"
// Everything else falls back to a JSON dump so nothing is silently lost.
function fmtDetails(e: LifecycleEvent): string {
  const attrs = e.attrs ?? {};
  switch (e.action) {
    case 'agent.shutdown': {
      const reason = attrs.reason as string | undefined;
      return reason ? `reason: ${reason}` : '';
    }
    case 'agent.paused': {
      const secs = Number(attrs.durationSec ?? 0);
      if (secs <= 0) return 'until I resume';
      if (secs < 3600) return `${Math.round(secs / 60)} min`;
      return `${(secs / 3600).toFixed(secs % 3600 === 0 ? 0 : 1)} hr`;
    }
    case 'agent.sso_login': {
      const email = attrs.email as string | undefined;
      return email ?? '';
    }
    default: {
      // Strip the "action" key — it's already rendered as the badge.
      const rest: Record<string, unknown> = { ...attrs };
      delete rest.action;
      const keys = Object.keys(rest);
      if (keys.length === 0) return '';
      return keys.map((k) => `${k}: ${String(rest[k])}`).join(' · ');
    }
  }
}
