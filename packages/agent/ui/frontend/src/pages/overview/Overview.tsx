import { useTranslation } from 'react-i18next';
import type { StatusSnapshot } from '@/api/agent';
import styles from './Overview.module.css';

// Overview — the Dashboard's default landing.
//
// Layout:
//   1) Hero status banner (state, SSO email, version, paused/update banners)
//   2) System tile strip (Hub conn / heartbeat age / audit queue / cert expiry / update)
//   3) Today's protection counters (Inspected / Passthrough / Denied)
//   4) Live AI indicator (last provider call age) + recent events (top 5)
//
// Reads only the StatusSnapshot the App already polls every 2 s — no extra
// queries, no extra IPC traffic. Each tile communicates the operational
// invariant the user actually cares about, in priority order: "is the
// agent doing its job RIGHT NOW", then "is the plumbing healthy", then
// "what did it do today", then "what did it just see".

export function Overview({ status }: { status: StatusSnapshot }) {
  const { t, i18n } = useTranslation();
  const stats = status.todayStats;
  const events = (status.recentEvents ?? []).slice(0, 5);

  return (
    <div className={styles.root}>
      <header>
        <h1 className={styles.title}>{t('overview.title')}</h1>
        <p className={styles.subtitle}>{t('overview.subtitle')}</p>
      </header>

      <HeroStatus status={status} />

      <SystemTiles status={status} locale={i18n.language} />

      <section aria-label={t('overview.today')}>
        <h2 className={styles.h2}>{t('overview.todayHeader')}</h2>
        <div className={styles.stats}>
          <Stat label={t('overview.inspected')} value={stats.inspected} accent="info" />
          <Stat label={t('overview.passthrough')} value={stats.passthrough} accent="neutral" />
          <Stat label={t('overview.denied')} value={stats.denied} accent="danger" />
        </div>
      </section>

      <section>
        <div className={styles.eventsHeader}>
          <h2 className={styles.h2}>{t('overview.recentActivity')}</h2>
          <span className={styles.eventsMeta}>{t('overview.recentTopN', { n: 5 })}</span>
        </div>
        {events.length === 0 ? (
          <p className={styles.empty}>{t('activity.empty')}</p>
        ) : (
          <table className={styles.table}>
            <thead>
              <tr>
                <th>{t('overview.col.time')}</th>
                <th>{t('overview.col.process')}</th>
                <th>{t('overview.col.dest')}</th>
                <th>{t('overview.col.action')}</th>
              </tr>
            </thead>
            <tbody>
              {events.map((e, i) => (
                <tr key={i}>
                  <td>{e.time}</td>
                  <td>{e.processName}</td>
                  <td>{e.destHost}</td>
                  <td><ActionBadge action={e.action} /></td>
                </tr>
              ))}
            </tbody>
          </table>
        )}
      </section>
    </div>
  );
}

function HeroStatus({ status }: { status: StatusSnapshot }) {
  const { t } = useTranslation();

  // Pick the dominant condition the user must know about, in priority
  // order: paused > error > update > degraded > active. The hero shows
  // only one — supplementary state goes in the tile strip below.
  let tone: 'ok' | 'warn' | 'danger' = 'ok';
  let titleKey = 'overview.hero.activeTitle';
  let metaKey = 'overview.hero.activeMeta';
  let metaArgs: Record<string, string | number> = { device: status.agent.deviceID.slice(0, 8) };

  if (status.paused) {
    tone = 'warn';
    titleKey = 'overview.hero.pausedTitle';
    metaKey = status.pausedUntil ? 'overview.hero.pausedUntil' : 'overview.hero.pausedIndefinite';
    metaArgs = { until: status.pausedUntil ?? '' };
  } else if (status.state === 'error') {
    tone = 'danger';
    titleKey = 'overview.hero.errorTitle';
    metaKey = 'overview.hero.errorMeta';
    metaArgs = { reason: status.stateReason || '—' };
  } else if (status.state === 'degraded') {
    tone = 'warn';
    titleKey = 'overview.hero.degradedTitle';
    metaKey = 'overview.hero.degradedMeta';
    metaArgs = { reason: status.stateReason || '—' };
  } else if (status.agent.updateAvailable) {
    tone = 'warn';
    titleKey = 'overview.hero.updateTitle';
    metaKey = 'overview.hero.updateMeta';
  }

  const ssoLabel = status.agent.ssoEmail || t('overview.hero.noSso');
  const version = status.agent.version || '—';

  return (
    <section className={styles.hero} data-tone={tone}>
      <span className={styles.heroDot} />
      <div className={styles.heroMain}>
        <p className={styles.heroTitle}>{t(titleKey)}</p>
        <p className={styles.heroMeta}>{t(metaKey, metaArgs)}</p>
      </div>
      <div className={styles.heroSide}>
        <span className={styles.heroSideValue}>{ssoLabel}</span>
        <span className={styles.heroSideLabel}>{t('overview.hero.identity')}</span>
        <span className={styles.heroSideValue}>{version}</span>
        <span className={styles.heroSideLabel}>{t('overview.hero.version')}</span>
      </div>
    </section>
  );
}

function SystemTiles({ status, locale }: { status: StatusSnapshot; locale: string }) {
  const { t } = useTranslation();
  const stats = status.todayStats;
  const hbAgeSec = ageSeconds(status.agent.lastHeartbeat);
  const hbFresh = hbAgeSec !== null && hbAgeSec < (status.agent.heartbeatIntervalSec || 60) * 3;
  const queue = status.auditQueue?.unsyncedCount ?? 0;
  const certAge = daysUntil(status.agent.certExpiresAt);

  return (
    <div className={styles.tiles}>
      <Tile
        label={t('overview.tile.hub')}
        value={status.gatewayConnected ? t('overview.tile.connected') : t('overview.tile.disconnected')}
        tone={status.gatewayConnected ? 'ok' : 'danger'}
      />
      <Tile
        label={t('overview.tile.heartbeat')}
        value={hbAgeSec === null ? '—' : formatAge(hbAgeSec, t)}
        tone={hbFresh ? 'ok' : 'warn'}
      />
      <Tile
        label={t('overview.tile.queue')}
        value={queue.toLocaleString(locale)}
        tone={queue > 1000 ? 'warn' : 'ok'}
      />
      <Tile
        label={t('overview.tile.cert')}
        value={certAge === null ? '—' : t('overview.tile.certDays', { days: certAge })}
        tone={certAge !== null && certAge < 14 ? 'warn' : 'ok'}
      />
      <Tile
        label={t('overview.tile.update')}
        value={status.agent.updateAvailable ? t('overview.tile.available') : t('overview.tile.current')}
        tone={status.agent.updateAvailable ? 'warn' : 'ok'}
      />
      {/* Today's Latency tile — shows our overhead vs upstream when
          the agent has had at least one bumped flow today; falls back
          to "—" on a fresh install before the first upstream call. */}
      <Tile
        label={t('overview.tile.latency', "Today's Latency")}
        value={
          stats.avgUsOverheadMs != null && stats.avgUpstreamTotalMs != null
            ? `Us ${stats.avgUsOverheadMs}ms · Up ${stats.avgUpstreamTotalMs}ms`
            : '—'
        }
        tone="ok"
      />
    </div>
  );
}

function Tile({ label, value, tone }: { label: string; value: string; tone: 'ok' | 'warn' | 'danger' }) {
  return (
    <div className={styles.tile} data-tone={tone}>
      <span className={styles.tileDot} />
      <div className={styles.tileBody}>
        <span className={styles.tileValue}>{value}</span>
        <span className={styles.tileLabel}>{label}</span>
      </div>
    </div>
  );
}

function Stat({ label, value, accent }: { label: string; value: number; accent: 'info' | 'neutral' | 'danger' }) {
  return (
    <div className={styles.stat} data-accent={accent}>
      <div className={styles.statValue}>{value}</div>
      <div className={styles.statLabel}>{label}</div>
    </div>
  );
}

function ActionBadge({ action }: { action: string }) {
  return <span className={styles.badge} data-action={action}>{action}</span>;
}

function ageSeconds(iso: string | undefined): number | null {
  if (!iso) return null;
  const t = Date.parse(iso);
  if (!Number.isFinite(t)) return null;
  return Math.max(0, Math.round((Date.now() - t) / 1000));
}

function daysUntil(iso: string | undefined): number | null {
  if (!iso) return null;
  const t = Date.parse(iso);
  if (!Number.isFinite(t)) return null;
  return Math.round((t - Date.now()) / (24 * 3600 * 1000));
}

function formatAge(sec: number, t: (k: string, opts?: Record<string, number>) => string): string {
  if (sec < 60) return t('overview.age.justNow');
  if (sec < 3600) return t('overview.age.minutes', { n: Math.round(sec / 60) });
  if (sec < 86400) return t('overview.age.hours', { n: Math.round(sec / 3600) });
  return t('overview.age.days', { n: Math.round(sec / 86400) });
}
