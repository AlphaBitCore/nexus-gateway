import { useState } from 'react';
import { useTranslation } from 'react-i18next';
import type { SessionMeta } from './streamChat';

interface SessionHistoryProps {
  showHistory: boolean;
  sessions: SessionMeta[];
  loadSession: (id: string) => void;
  removeSession: (id: string) => void;
}

// relTime renders an age compactly ("just now", "5m ago", "3h ago", "2d ago") —
// the same buckets the TUI picker shows, so both surfaces read alike.
function relTime(iso: string, t: (k: string, o?: Record<string, unknown>) => string): string {
  const ms = Date.now() - Date.parse(iso);
  if (Number.isNaN(ms)) return '';
  const min = Math.floor(ms / 60_000);
  if (min < 1) return t('common:assistant.timeJustNow');
  if (min < 60) return t('common:assistant.timeMinAgo', { n: min });
  const h = Math.floor(min / 60);
  if (h < 24) return t('common:assistant.timeHourAgo', { n: h });
  return t('common:assistant.timeDayAgo', { n: Math.floor(h / 24) });
}

// SessionHistory renders the history panel: the caller's own conversations
// (newest first, as the server lists them) with a live title filter, relative
// time, and load + delete actions. Stateless beyond the filter — list data and
// actions are threaded from the parent, which owns the showHistory toggle and
// the server calls. Renders nothing until showHistory is set.
export function SessionHistory({ showHistory, sessions, loadSession, removeSession }: SessionHistoryProps) {
  const { t } = useTranslation();
  const [filter, setFilter] = useState('');
  if (!showHistory) return null;
  if (sessions.length === 0) {
    return <p className="text-sm text-muted-foreground">{t('common:assistant.noSessions')}</p>;
  }
  const q = filter.trim().toLowerCase();
  const visible = q === '' ? sessions : sessions.filter((s) => (s.title || s.id).toLowerCase().includes(q));
  return (
    <>
      <input
        type="text"
        value={filter}
        onChange={(e) => setFilter(e.target.value)}
        placeholder={t('common:assistant.filterSessions')}
        aria-label={t('common:assistant.filterSessions')}
        className="mb-1 w-full rounded-md border border-border bg-background px-2 py-1 text-sm outline-none focus:border-primary"
      />
      {visible.length === 0 ? (
        <p className="text-sm text-muted-foreground">{t('common:assistant.noSessionsMatch', { filter })}</p>
      ) : (
        visible.map((s) => (
          <div key={s.id} className="flex items-center justify-between gap-2 rounded-md px-2 py-1 hover:bg-muted">
            <button
              type="button"
              onClick={() => void loadSession(s.id)}
              className="flex min-w-0 flex-1 items-center gap-1.5 text-left text-sm"
            >
              <span className="truncate">{s.title || s.id}</span>
              {s.updatedAt && (
                <span className="shrink-0 text-[10px] text-muted-foreground">{relTime(s.updatedAt, t)}</span>
              )}
            </button>
            <button
              type="button"
              aria-label={t('common:assistant.deleteSession')}
              title={t('common:assistant.deleteSession')}
              onClick={() => void removeSession(s.id)}
              className="text-muted-foreground transition-colors hover:text-destructive"
            >
              <span aria-hidden="true">🗑</span>
            </button>
          </div>
        ))
      )}
    </>
  );
}
