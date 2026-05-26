/**
 * Traffic page — outbound network connections this agent intercepted.
 *
 * Built for the device's primary user (not a fleet admin), so the list
 * stays at "what app, where, allowed?". Engineering signals (port,
 * bytes, IP, hooks pipeline) live in the click-to-open drawer
 * (TrafficEventDetail.tsx). Visual hierarchy:
 *   - Time: relative ("3 min ago", absolute on hover)
 *   - App: process name (Chrome, Cursor, curl …)
 *   - Site: hostname only (we read NEAppProxyFlow.remoteHostname → DB)
 *   - Latency: human-readable (520ms / 1.2s)
 *   - Status: human label (Passed through / Processed / Blocked) with
 *             tone color
 *   - Tags: AI chip (frontend host registry — see lib/aiHosts.ts),
 *           hook-decision chip when set, policy-rule chip when set
 *
 * Action filter dropdown replaces the older "AI only" checkbox: now
 * one control covers All / AI / Blocked / Processed scopes.
 */
import { useState, useEffect, useMemo } from 'react';
import { useQuery } from '@tanstack/react-query';
import { useTranslation } from 'react-i18next';
import { Button } from '@nexus-gateway/ui-shared';
import { agentApi } from '@/api/agent';
import type { AgentEvent } from '@/api/agent';
import { TrafficEventDetail } from './TrafficEventDetail';
import { classify, isAITraffic, statusDescriptor } from '@/lib/classify';
import styles from './Traffic.module.css';

const PAGE_SIZE_OPTIONS = [10, 25, 50, 100] as const;
const DEFAULT_PAGE_SIZE = 10;

// #88 — time-window selector. Pushes sinceUnixMillis down into the
// daemon's SQL WHERE so the UI never relies on over-fetch + JS
// filtering. Defaults to 24h so the page loads with a sensible
// "recent activity" frame instead of every event ever captured.
const SINCE_OPTIONS = [
  { value: '1h',  labelKey: 'traffic.since.1h',  ms: 60 * 60 * 1000 },
  { value: '24h', labelKey: 'traffic.since.24h', ms: 24 * 60 * 60 * 1000 },
  { value: '7d',  labelKey: 'traffic.since.7d',  ms: 7 * 24 * 60 * 60 * 1000 },
  { value: '30d', labelKey: 'traffic.since.30d', ms: 30 * 24 * 60 * 60 * 1000 },
  { value: 'all', labelKey: 'traffic.since.all', ms: 0 },
] as const;
const DEFAULT_SINCE = '24h';
const SINCE_STORAGE_KEY = 'nexus.agent.traffic.since';

// Auto-refresh cadence options (ms). 0 = off; otherwise a fixed
// React-Query refetchInterval. The Traffic page persists the user's
// choice in localStorage so it survives reloads / app restarts.
const REFRESH_OPTIONS = [
  { value: 0, labelKey: 'traffic.refresh.off' },
  { value: 5_000, labelKey: 'traffic.refresh.5s' },
  { value: 15_000, labelKey: 'traffic.refresh.15s' },
  { value: 30_000, labelKey: 'traffic.refresh.30s' },
  { value: 60_000, labelKey: 'traffic.refresh.1m' },
  { value: 300_000, labelKey: 'traffic.refresh.5m' },
] as const;
const DEFAULT_REFRESH_MS = 5_000;
const REFRESH_STORAGE_KEY = 'nexus.agent.traffic.refreshMs';

type ActionFilter = 'all' | 'ai' | 'blocked' | 'processed';

function fmtLatency(ms?: number | null): string {
  if (!ms || ms <= 0) return '—';
  if (ms >= 1000) return `${(ms / 1000).toFixed(2)}s`;
  return `${ms}ms`;
}

/**
 * Relative time formatter. Shows "just now" under 30s, "X min ago"
 * under an hour, "X hr ago" under a day, then locale date. Agent UI
 * is meant for live observation so coarse buckets beat exact seconds.
 */
function fmtRelativeTime(iso?: string): string {
  if (!iso) return '—';
  const t = Date.parse(iso);
  if (!Number.isFinite(t)) return iso;
  const deltaSec = Math.max(0, Math.round((Date.now() - t) / 1000));
  if (deltaSec < 30) return 'just now';
  if (deltaSec < 60) return `${deltaSec}s ago`;
  const deltaMin = Math.floor(deltaSec / 60);
  if (deltaMin < 60) return `${deltaMin} min ago`;
  const deltaHr = Math.floor(deltaMin / 60);
  if (deltaHr < 24) return `${deltaHr} hr ago`;
  return new Date(t).toLocaleDateString();
}

function fmtAbsoluteTime(iso?: string): string {
  if (!iso) return '—';
  try {
    return new Date(iso).toLocaleString();
  } catch {
    return iso;
  }
}

/**
 * Renders the Status column. Delegates to `statusDescriptor(event)` so
 * the badge text + tone come from the same classification function the
 * AI tag and the upload filter use.
 */
function StatusBadge({ event }: { event: AgentEvent }) {
  const { label, tone } = statusDescriptor(event);
  const palette: Record<string, { bg: string; fg: string }> = {
    good: { bg: 'var(--color-success-bg)', fg: 'var(--color-success)' },
    warn: { bg: 'var(--color-warning-bg)', fg: 'var(--color-warning)' },
    bad: { bg: 'var(--color-danger-bg)', fg: 'var(--color-danger)' },
    muted: { bg: 'var(--color-bg-subtle)', fg: 'var(--color-text-muted)' },
  };
  const { bg, fg } = palette[tone];
  return (
    <span
      style={{
        display: 'inline-block',
        padding: 'var(--g-space-0-5) var(--g-space-2)',
        borderRadius: 'var(--g-radius-sm)',
        background: bg,
        color: fg,
        fontSize: 'var(--g-font-size-xs)',
        fontWeight: 'var(--g-font-weight-semibold)',
        whiteSpace: 'nowrap',
      }}
    >
      {label}
    </span>
  );
}

/** Generic compact chip for the Tags column (AI / hook / policy). */
function TagChip({ label, tone = 'muted' }: { label: string; tone?: 'info' | 'warn' | 'bad' | 'muted' }) {
  const palette: Record<string, { bg: string; fg: string }> = {
    info: { bg: 'var(--color-info-bg)', fg: 'var(--color-info)' },
    warn: { bg: 'var(--color-warning-bg)', fg: 'var(--color-warning)' },
    bad: { bg: 'var(--color-danger-bg)', fg: 'var(--color-danger)' },
    muted: { bg: 'var(--color-bg-subtle)', fg: 'var(--color-text-muted)' },
  };
  const { bg, fg } = palette[tone];
  return (
    <span
      style={{
        display: 'inline-block',
        padding: 'var(--g-space-0-5) var(--g-space-1-5)',
        marginRight: 'var(--g-space-1)',
        borderRadius: 'var(--g-radius-sm)',
        background: bg,
        color: fg,
        fontSize: 'var(--g-font-size-xs)',
        fontWeight: 'var(--g-font-weight-semibold)',
        whiteSpace: 'nowrap',
      }}
    >
      {label}
    </span>
  );
}

function TagsCell({ event }: { event: AgentEvent }) {
  // AI tag derived from interception_domain match (via domainRuleId).
  // Hook chip only renders when a hook actually ran (PROCESS path).
  const ai = isAITraffic(event);
  const cls = classify(event);
  const hookTone: 'warn' | 'bad' | 'muted' =
    event.hookDecision === 'deny' || event.hookDecision === 'reject_hard' || event.hookDecision === 'block_soft'
      ? 'bad'
      : event.hookDecision === 'approve'
        ? 'warn'
        : 'muted';

  const chips: { key: string; node: ReactJSXNode }[] = [];
  if (ai) chips.push({ key: 'ai', node: <TagChip label="AI" tone="info" /> });
  // Only show hook chip when a hook actually ran. Inspect /
  // BumpFailed / Untracked classifications never have a hookDecision.
  if (cls === 'processed' || cls === 'blocked') {
    if (event.hookDecision) {
      chips.push({
        key: 'hook',
        node: <TagChip label={`hook · ${event.hookDecision}`} tone={hookTone} />,
      });
    }
  }
  if (event.policyRuleId) {
    chips.push({ key: 'policy', node: <TagChip label={`policy`} tone="muted" /> });
  }

  if (chips.length === 0) return <span style={{ color: 'var(--color-text-muted)' }}>—</span>;
  return <>{chips.map((c) => <span key={c.key}>{c.node}</span>)}</>;
}

// Local alias to ReactNode without an import — keeps the file leaner.
type ReactJSXNode = ReturnType<typeof TagChip>;

export function Traffic() {
  const { t } = useTranslation();
  const [search, setSearch] = useState('');
  // 250ms debounce so a typing user fires one IPC instead of one per
  // keystroke. Search is server-side (QUERY_EVENTS?q=), not a local filter.
  const [debouncedSearch, setDebouncedSearch] = useState('');
  useEffect(() => {
    const t = setTimeout(() => setDebouncedSearch(search), 250);
    return () => clearTimeout(t);
  }, [search]);

  const [actionFilter, setActionFilter] = useState<ActionFilter>('all');
  const [offset, setOffset] = useState(0);
  const [pageSize, setPageSize] = useState<number>(DEFAULT_PAGE_SIZE);

  // Persisted to localStorage so a user who picks "Off" isn't surprised
  // by silent re-fetches after app restart.
  const [refreshMs, setRefreshMs] = useState<number>(() => {
    try {
      const raw = localStorage.getItem(REFRESH_STORAGE_KEY);
      if (raw == null) return DEFAULT_REFRESH_MS;
      const n = Number(raw);
      return REFRESH_OPTIONS.some((o) => o.value === n) ? n : DEFAULT_REFRESH_MS;
    } catch {
      return DEFAULT_REFRESH_MS;
    }
  });
  useEffect(() => {
    try { localStorage.setItem(REFRESH_STORAGE_KEY, String(refreshMs)); } catch { /* best-effort */ }
  }, [refreshMs]);

  // #88 — Show selector + Since (time window) now BOTH push down into the
  // daemon's SQL WHERE. The pre-#88 client-side AI filter over-fetched
  // pageSize*4 and re-narrowed in JS, which (a) lost earlier AI rows
  // when recent traffic had no AI, (b) showed total = all events not
  // AI events, (c) had no time-window control at all. Now: ai_only +
  // since URL params route to queue.QueryEventsFiltered → SQL.
  const serverAction = actionFilter === 'blocked' ? 'deny' : actionFilter === 'processed' ? 'inspect' : '';
  const aiOnly = actionFilter === 'ai';

  // Time-window dropdown: "Last 1h / 24h / 7d / 30d / All". Persisted
  // to localStorage so the user's choice survives reloads. `All` sends
  // sinceUnixMillis=0 (no filter); the others compute a rolling
  // window relative to wall-clock now.
  const [sinceWindow, setSinceWindow] = useState<string>(() => {
    try {
      return localStorage.getItem(SINCE_STORAGE_KEY) ?? DEFAULT_SINCE;
    } catch {
      return DEFAULT_SINCE;
    }
  });
  useEffect(() => {
    try { localStorage.setItem(SINCE_STORAGE_KEY, sinceWindow); } catch { /* best-effort */ }
  }, [sinceWindow]);
  const sinceUnixMillis = useMemo(() => {
    const opt = SINCE_OPTIONS.find((o) => o.value === sinceWindow);
    if (!opt || opt.ms === 0) return 0;
    return Date.now() - opt.ms;
  }, [sinceWindow]);

  const { data, isLoading } = useQuery({
    queryKey: ['agent', 'traffic', debouncedSearch, offset, actionFilter, pageSize, sinceWindow],
    queryFn: () =>
      agentApi.queryEvents({
        search: debouncedSearch,
        action: serverAction,
        aiOnly,
        sinceUnixMillis,
        offset,
        limit: pageSize,
      }),
    staleTime: 2_000,
    refetchInterval: refreshMs > 0 ? refreshMs : false,
  });

  const events: AgentEvent[] = data?.events ?? [];
  const total = data?.total ?? 0;
  const hasMore = offset + pageSize < total;

  const [selectedEvent, setSelectedEvent] = useState<AgentEvent | null>(null);

  return (
    <div className={styles.root}>
      <header>
        <h1 className={styles.title}>{t('traffic.title')}</h1>
        <p className={styles.subtitle}>{t('traffic.subtitle')}</p>
      </header>

      <div className={styles.filterRow}>
        <input
          type="text"
          className={styles.search}
          placeholder={t('traffic.searchPlaceholder')}
          value={search}
          onChange={(e) => {
            setSearch(e.target.value);
            setOffset(0);
          }}
        />
        <label style={{ display: 'flex', alignItems: 'center', gap: 'var(--g-space-2)', color: 'var(--color-text-muted)', fontSize: 'var(--g-font-size-sm)' }}>
          <span>{t('traffic.filter.label', 'Show')}</span>
          <select
            value={actionFilter}
            onChange={(e) => {
              setActionFilter(e.target.value as ActionFilter);
              setOffset(0);
            }}
            style={{
              background: 'var(--color-surface)',
              color: 'var(--color-text)',
              border: '1px solid var(--color-border)',
              borderRadius: 'var(--g-radius-sm)',
              padding: 'var(--g-space-1) var(--g-space-2)',
              cursor: 'pointer',
            }}
          >
            <option value="all">{t('traffic.filter.all', 'All')}</option>
            <option value="ai">{t('traffic.filter.ai', 'AI only')}</option>
            <option value="blocked">{t('traffic.filter.blocked', 'Blocked only')}</option>
            <option value="processed">{t('traffic.filter.processed', 'Processed only')}</option>
          </select>
        </label>
        <label style={{ display: 'flex', alignItems: 'center', gap: 'var(--g-space-2)', color: 'var(--color-text-muted)', fontSize: 'var(--g-font-size-sm)' }}>
          <span>{t('traffic.since.label', 'Time range')}</span>
          <select
            value={sinceWindow}
            onChange={(e) => {
              setSinceWindow(e.target.value);
              setOffset(0);
            }}
            style={{
              background: 'var(--color-surface)',
              color: 'var(--color-text)',
              border: '1px solid var(--color-border)',
              borderRadius: 'var(--g-radius-sm)',
              padding: 'var(--g-space-1) var(--g-space-2)',
              cursor: 'pointer',
            }}
          >
            {SINCE_OPTIONS.map((opt) => (
              <option key={opt.value} value={opt.value}>{t(opt.labelKey)}</option>
            ))}
          </select>
        </label>
        <label style={{ display: 'flex', alignItems: 'center', gap: 'var(--g-space-2)', color: 'var(--color-text-muted)', fontSize: 'var(--g-font-size-sm)' }}>
          <span>{t('traffic.pageSize', 'Rows per page')}</span>
          <select
            value={pageSize}
            onChange={(e) => {
              setPageSize(Number(e.target.value));
              setOffset(0);
            }}
            style={{
              background: 'var(--color-surface)',
              color: 'var(--color-text)',
              border: '1px solid var(--color-border)',
              borderRadius: 'var(--g-radius-sm)',
              padding: 'var(--g-space-1) var(--g-space-2)',
              cursor: 'pointer',
            }}
          >
            {PAGE_SIZE_OPTIONS.map((n) => (
              <option key={n} value={n}>{n}</option>
            ))}
          </select>
        </label>
        <label
          title={t('traffic.refresh.tooltip', 'Re-fetch the page automatically with the same search and filter. Choose Off to freeze the view.')}
          style={{ display: 'flex', alignItems: 'center', gap: 'var(--g-space-2)', color: 'var(--color-text-muted)', fontSize: 'var(--g-font-size-sm)' }}
        >
          <span>{t('traffic.refresh.label', 'Auto refresh')}</span>
          <select
            value={refreshMs}
            onChange={(e) => setRefreshMs(Number(e.target.value))}
            style={{
              background: 'var(--color-surface)',
              color: 'var(--color-text)',
              border: '1px solid var(--color-border)',
              borderRadius: 'var(--g-radius-sm)',
              padding: 'var(--g-space-1) var(--g-space-2)',
              cursor: 'pointer',
            }}
          >
            {REFRESH_OPTIONS.map((opt) => (
              <option key={opt.value} value={opt.value}>{t(opt.labelKey)}</option>
            ))}
          </select>
        </label>
      </div>

      {isLoading ? (
        <p className={styles.empty}>{t('traffic.loading')}</p>
      ) : events.length === 0 ? (
        <p className={styles.empty}>{t('traffic.empty')}</p>
      ) : (
        <>
          <table className={styles.table}>
            <thead>
              <tr>
                <th>{t('traffic.col.time', 'Time')}</th>
                <th>{t('traffic.col.app', 'App')}</th>
                <th>{t('traffic.col.site', 'Site')}</th>
                <th>{t('traffic.col.method', 'Method')}</th>
                <th>{t('traffic.col.path', 'Path')}</th>
                <th className={styles.numCell}>{t('traffic.col.latency', 'Latency')}</th>
                <th>{t('traffic.col.status', 'Status')}</th>
                <th>{t('traffic.col.tags', 'Tags')}</th>
              </tr>
            </thead>
            <tbody>
              {events.map((e, i) => (
                <tr
                  key={e.id ?? `${offset}-${i}-${e.timestamp}`}
                  onClick={() => setSelectedEvent(e)}
                  style={{ cursor: 'pointer' }}
                >
                  <td title={fmtAbsoluteTime(e.timestamp)}>{fmtRelativeTime(e.timestamp)}</td>
                  <td>{e.sourceProcess ?? '—'}</td>
                  <td className={styles.mono}>{e.destHost ?? e.targetHost ?? '—'}</td>
                  <td className={styles.mono}>{e.method ?? '—'}</td>
                  <td className={styles.mono} title={e.path ?? ''} style={{ maxWidth: '24rem', overflow: 'hidden', textOverflow: 'ellipsis', whiteSpace: 'nowrap' }}>{e.path ?? '—'}</td>
                  <td className={styles.numCell}>{fmtLatency(e.latencyMs)}</td>
                  <td><StatusBadge event={e} /></td>
                  <td><TagsCell event={e} /></td>
                </tr>
              ))}
            </tbody>
          </table>
          <div className={styles.pagination}>
            <span className={styles.muted}>
              {t('traffic.pageInfo', {
                from: offset + 1,
                to: Math.min(offset + events.length, total),
                total,
              })}
            </span>
            <div style={{ display: 'flex', gap: 'var(--g-space-2)' }}>
              <Button
                variant="ghost"
                disabled={offset === 0}
                onClick={() => setOffset(Math.max(0, offset - pageSize))}
              >
                {t('traffic.previous')}
              </Button>
              <Button
                variant="ghost"
                disabled={!hasMore}
                onClick={() => setOffset(offset + pageSize)}
              >
                {t('traffic.next')}
              </Button>
            </div>
          </div>
        </>
      )}
      <TrafficEventDetail event={selectedEvent} onClose={() => setSelectedEvent(null)} />
    </div>
  );
}
