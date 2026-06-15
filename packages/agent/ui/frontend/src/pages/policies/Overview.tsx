import { useTranslation } from 'react-i18next';
import { Link, useNavigate } from 'react-router-dom';
import { useAppliedConfig, useRefreshPolicies } from './useAppliedConfig';
import type {
  AppliedConfig,
  PolicyDiagMode,
  PolicyInterceptionDomain,
  PolicyHook,
  PolicyExemption,
  PolicyKillSwitch,
  PolicyRulePack,
  PolicySyncStatus,
} from '@/api/agent';
import styles from './policies.module.css';

// Overview = the /policies landing page. Shows a hero status banner, a
// 5-tile KPI strip, and per-section preview cards with "View all (N) →"
// links into the dedicated list pages. Heavy data lives behind those
// click-throughs; this page intentionally renders nothing past the top
// 5 of each list so it stays scannable regardless of fleet size.
export function PoliciesOverview() {
  const { t, i18n } = useTranslation();
  const navigate = useNavigate();
  const { data, isLoading } = useAppliedConfig();
  const { refreshing, error, trigger, clearError } = useRefreshPolicies();

  if (isLoading || !data) {
    return (
      <div className={styles.root}>
        <header className={styles.pageHeader}>
          <div>
            <h1 className={styles.title}>{t('policies.title')}</h1>
            <p className={styles.subtitle}>{t('policies.subtitle')}</p>
          </div>
        </header>
        <p className={styles.empty}>{t('policies.loading')}</p>
      </div>
    );
  }

  return (
    <div className={styles.root}>
      <header className={styles.pageHeader}>
        <div>
          <h1 className={styles.title}>{t('policies.title')}</h1>
          <p className={styles.subtitle}>{t('policies.subtitle')}</p>
        </div>
        <div className={styles.actionRow}>
          <button type="button" className={styles.button} onClick={trigger} disabled={refreshing}>
            {refreshing ? t('policies.refresh.refreshing') : t('policies.refresh.action')}
          </button>
        </div>
      </header>

      {error && (
        <div className={styles.banner} role="alert" onClick={clearError}>
          {t('policies.refresh.failed', { error })}
        </div>
      )}

      <HeroStatus sync={data.sync} killSwitch={data.killSwitch} diag={data.diagMode} locale={i18n.language} />

      <div className={styles.kpis}>
        <KpiTile count={data.interceptionDomains.length} label={t('policies.kpi.domains')} onClick={() => navigate('/policies/domains')} />
        <KpiTile count={data.hooks.length} label={t('policies.kpi.hooks')} onClick={() => navigate('/policies/hooks')} />
        <KpiTile count={data.exemptions.length} label={t('policies.kpi.exemptions')} onClick={() => navigate('/policies/exemptions')} />
        <KpiTile count={data.rulePacks.length} label={t('policies.kpi.rulePacks')} onClick={() => navigate('/policies/rule-packs')} />
        <KpiTile
          label={t('policies.kpi.killSwitch')}
          value={data.killSwitch.engaged ? t('policies.kpi.on') : t('policies.kpi.off')}
          state={data.killSwitch.engaged ? 'danger' : 'off'}
        />
      </div>

      <SyncCard sync={data.sync} locale={i18n.language} />

      <div className={styles.previewGrid}>
        <PreviewCard
          titleKey="policies.interceptionDomains.title"
          count={data.interceptionDomains.length}
          listPath="/policies/domains"
          emptyKey="policies.interceptionDomains.empty"
          items={data.interceptionDomains.slice(0, 5).map((d) => ({
            id: d.id,
            primary: d.hostPattern || d.name,
            secondary: d.enabled ? '' : t('policies.interceptionDomains.cols.disabled'),
            href: d.id ? `/policies/domains/${encodeURIComponent(d.id)}` : undefined,
          }))}
        />
        <PreviewCard
          titleKey="policies.hooks.title"
          count={data.hooks.length}
          listPath="/policies/hooks"
          emptyKey="policies.hooks.empty"
          items={data.hooks
            .slice()
            .sort((a, b) => (a.priority ?? 0) - (b.priority ?? 0))
            .slice(0, 5)
            .map((h) => ({
              id: h.id,
              primary: h.name,
              secondary: h.stage ?? '',
              href: h.id ? `/policies/hooks/${encodeURIComponent(h.id)}` : undefined,
            }))}
        />
        <PreviewCard
          titleKey="policies.exemptions.title"
          count={data.exemptions.length}
          listPath="/policies/exemptions"
          emptyKey="policies.exemptions.empty"
          items={data.exemptions.slice(0, 5).map((e) => ({
            id: e.id,
            primary: e.host ?? e.user ?? e.id,
            secondary: e.reason ?? '',
          }))}
        />
        <PreviewCard
          titleKey="policies.rulePacks.title"
          count={data.rulePacks.length}
          listPath="/policies/rule-packs"
          emptyKey="policies.rulePacks.empty"
          items={data.rulePacks.slice(0, 5).map((p) => ({
            id: p.id,
            primary: p.name,
            secondary: p.version ?? '',
            href: p.id ? `/policies/rule-packs/${encodeURIComponent(p.id)}` : undefined,
          }))}
        />
      </div>

      <QUICFallbackCard bundles={data.deviceDefaults?.forceQUICFallbackBundles ?? []} />
    </div>
  );
}

// QUICFallbackCard surfaces the bundle-ID allowlist the macOS NE
// proxy uses to decide which apps' UDP flows to close. Read-only
// here — admin owns the list in the Control Plane (Settings → Agent).
// Empty/missing list renders an explanatory empty state so the user
// understands "no UDP is being killed" rather than "feature broken".
function QUICFallbackCard({ bundles }: { bundles: string[] }) {
  const { t } = useTranslation();
  return (
    <section className={styles.preview}>
      <header className={styles.previewHeader}>
        <h2 className={styles.previewTitle}>
          {t('policies.quicFallback.title')} ({bundles.length})
        </h2>
      </header>
      <p className={styles.subtitle} style={{ marginBottom: 'var(--g-space-3)' }}>
        {t('policies.quicFallback.desc')}
      </p>
      {bundles.length === 0 ? (
        <p className={styles.empty}>{t('policies.quicFallback.empty')}</p>
      ) : (
        <ul className={styles.previewList}>
          {bundles.map((b) => (
            <li key={b} className={styles.previewItem}>
              <code style={{ fontFamily: 'var(--g-font-family-mono)', fontSize: 'var(--g-font-size-sm)' }}>{b}</code>
            </li>
          ))}
        </ul>
      )}
    </section>
  );
}

function HeroStatus({
  sync,
  killSwitch,
  diag,
  locale,
}: {
  sync: PolicySyncStatus;
  killSwitch: PolicyKillSwitch;
  diag: PolicyDiagMode | undefined;
  locale: string;
}) {
  const { t } = useTranslation();
  const behind = Math.max(0, sync.desiredVersion - sync.reportedVersion);
  let tone: 'ok' | 'warn' | 'danger' = 'ok';
  let titleKey = 'policies.hero.inSync';
  let metaKey = 'policies.hero.inSyncMeta';
  let metaArgs: Record<string, string | number> = { at: fmtTime(sync.lastReportedAt, locale) };

  if (killSwitch.engaged) {
    tone = 'danger';
    titleKey = 'policies.hero.killSwitchEngaged';
    metaKey = 'policies.hero.killSwitchMeta';
    metaArgs = { reason: killSwitch.reason ?? '—' };
  } else if (!sync.inSync || behind > 0) {
    tone = 'warn';
    titleKey = 'policies.hero.drifted';
    metaKey = 'policies.hero.driftedMeta';
    metaArgs = { behind, s: behind === 1 ? '' : 's' };
  } else if (diag?.active) {
    tone = 'warn';
    titleKey = 'policies.hero.diagActive';
    metaKey = 'policies.hero.diagActiveMeta';
    metaArgs = { until: fmtTime(diag.until ?? '', locale) };
  }

  return (
    <section className={styles.hero} data-tone={tone}>
      <span className={styles.heroDot} />
      <div className={styles.heroMain}>
        <p className={styles.heroTitle}>{t(titleKey)}</p>
        <p className={styles.heroMeta}>{t(metaKey, metaArgs)}</p>
      </div>
      <div className={styles.heroVer}>
        <span className={styles.heroVerNum}>v{sync.desiredVersion}</span>
        <span className={styles.heroVerLabel}>{t('policies.hero.desired')}</span>
      </div>
    </section>
  );
}

function KpiTile({
  count,
  label,
  value,
  state,
  onClick,
}: {
  count?: number;
  label: string;
  value?: string;
  state?: 'off' | 'danger';
  onClick?: () => void;
}) {
  const display = value ?? (count ?? 0).toLocaleString();
  const computedState = state ?? (typeof count === 'number' && count === 0 ? 'off' : 'on');
  return (
    <button type="button" className={styles.kpi} data-state={computedState} onClick={onClick} disabled={!onClick}>
      <span className={styles.kpiValue}>{display}</span>
      <span className={styles.kpiLabel}>{label}</span>
    </button>
  );
}

function SyncCard({ sync, locale }: { sync: PolicySyncStatus; locale: string }) {
  const { t } = useTranslation();
  return (
    <div className={styles.syncCard}>
      <div>
        <strong>{t('policies.sync.title')}</strong> · v{sync.desiredVersion}{' '}
        <span className={styles.badge} data-tone={sync.inSync ? 'ok' : 'warn'}>
          {sync.inSync
            ? t('policies.sync.inSync')
            : t('policies.sync.drifted', {
                behind: Math.max(0, sync.desiredVersion - sync.reportedVersion),
                s: 's',
              })}
        </span>
      </div>
      <div className={styles.heroMeta}>
        {t('policies.sync.lastReportedAt')}: {fmtTime(sync.lastReportedAt, locale)}
      </div>
    </div>
  );
}

type PreviewItem = { id: string; primary: string; secondary?: string; href?: string };

function PreviewCard({
  titleKey,
  count,
  listPath,
  emptyKey,
  items,
}: {
  titleKey: string;
  count: number;
  listPath: string;
  emptyKey: string;
  items: PreviewItem[];
}) {
  const { t } = useTranslation();
  return (
    <section className={styles.preview}>
      <header className={styles.previewHeader}>
        <h3 className={styles.previewTitle}>{t(titleKey)} <span style={{ color: 'var(--color-text-muted)', fontWeight: 'var(--g-font-weight-normal)' }}>({count})</span></h3>
        <Link className={styles.previewLink} to={listPath}>{t('policies.viewAll', { n: count })}</Link>
      </header>
      {items.length === 0 ? (
        <div className={styles.empty}>{t(emptyKey)}</div>
      ) : (
        <ul className={styles.previewList}>
          {items.map((item) => (
            <li key={item.id} className={styles.previewItem}>
              {item.href ? (
                <Link className={styles.previewItemKey} to={item.href} style={{ color: 'inherit', textDecoration: 'none' }}>
                  {item.primary}
                </Link>
              ) : (
                <span className={styles.previewItemKey}>{item.primary}</span>
              )}
              {item.secondary && <span className={styles.previewItemMeta}>{item.secondary}</span>}
            </li>
          ))}
        </ul>
      )}
    </section>
  );
}

function fmtTime(iso: string, locale: string): string {
  if (!iso) return '—';
  try {
    return new Date(iso).toLocaleString(locale);
  } catch {
    return iso;
  }
}

// Re-export AppliedConfig types so list pages can import from this barrel
// without each page reaching into '@/api/agent' directly.
export type { AppliedConfig, PolicyHook, PolicyInterceptionDomain, PolicyExemption, PolicyRulePack };
