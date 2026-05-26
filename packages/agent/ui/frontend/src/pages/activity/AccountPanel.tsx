import { useTranslation } from 'react-i18next';
import type { StatusSnapshot } from '@/api/agent';
import { useAppliedConfig } from '../policies/useAppliedConfig';
import settings from '../settings/Settings.module.css';

const TRUST_LABELS: Record<number, string> = {
  0: 'Revoked',
  1: 'Enrolled',
  2: 'Linked',
  3: 'Compliant',
};

/**
 * AccountPanel — Settings-page card showing how this device is
 * registered with the gateway.
 */
export function AccountPanel({ status }: { status: StatusSnapshot }) {
  const { t } = useTranslation();
  const { data: applied } = useAppliedConfig();
  const a = status.agent;
  const user = applied?.userContext;
  const orgs = applied?.organizationTree ?? [];

  const deviceRows: Array<[string, string | undefined]> = [
    [t('identity.deviceID'), a.deviceID],
    [t('identity.trustLevel'), `${a.trustLevel} — ${TRUST_LABELS[a.trustLevel] ?? 'unknown'}`],
    [t('identity.ssoEmail'), a.ssoEmail],
    [t('identity.deviceAuthMode'), a.deviceAuthMode],
    [t('identity.certExpiresAt'), a.certExpiresAt],
  ];

  // Build a "Root › ... › Current" breadcrumb for the organization
  // ancestor chain. Hub already returns the rows sorted root → leaf,
  // so we just walk them in order. We render the *current* org's
  // metadata (timezone, contact info) in the dl below since that's
  // the operationally-relevant context.
  const currentOrg = orgs.length > 0 ? orgs[orgs.length - 1] : undefined;
  const orgRows: Array<[string, string | undefined]> = currentOrg
    ? [
        [t('identity.org.name'), currentOrg.name],
        [t('identity.org.code'), currentOrg.code],
        [t('identity.org.timezone'), currentOrg.timezone],
        [t('identity.org.description'), currentOrg.description],
      ]
    : [];

  return (
    <>
      <section className={settings.card}>
        <h2 className={settings.cardTitle}>{t('settings.accountTitle')}</h2>
        <p className={settings.cardDesc}>{t('settings.accountSubtitle')}</p>
        <dl className={settings.kvList}>
          {user && (
            <>
              <div className={settings.kvRow}>
                <dt>{t('identity.user.displayName')}</dt>
                <dd>{user.displayName}</dd>
              </div>
              {user.email && (
                <div className={settings.kvRow}>
                  <dt>{t('identity.user.email')}</dt>
                  <dd>{user.email}</dd>
                </div>
              )}
              {user.status && (
                <div className={settings.kvRow}>
                  <dt>{t('identity.user.status')}</dt>
                  <dd>{user.status}</dd>
                </div>
              )}
            </>
          )}
          {deviceRows.map(([label, value]) =>
            value ? (
              <div key={label} className={settings.kvRow}>
                <dt>{label}</dt>
                <dd>{value}</dd>
              </div>
            ) : null,
          )}
        </dl>
      </section>

      {orgs.length > 0 && (
        <section className={settings.card}>
          <h2 className={settings.cardTitle}>{t('identity.org.title')}</h2>
          <p className={settings.cardDesc}>{t('identity.org.subtitle')}</p>
          <nav aria-label={t('identity.org.hierarchyAria', 'Organization hierarchy')} style={{ fontSize: 'var(--g-font-size-base)', marginBottom: 'var(--g-space-4)' }}>
            {orgs.map((o, i) => (
              <span key={o.id}>
                {i > 0 && <span style={{ margin: 'var(--g-space-0) var(--g-space-2)', color: 'var(--color-text-muted)' }}>›</span>}
                <span style={{ fontWeight: i === orgs.length - 1 ? 600 : 400 }}>{o.name}</span>
              </span>
            ))}
          </nav>
          <dl className={settings.kvList}>
            {orgRows.map(([label, value]) =>
              value ? (
                <div key={label} className={settings.kvRow}>
                  <dt>{label}</dt>
                  <dd>{value}</dd>
                </div>
              ) : null,
            )}
          </dl>
        </section>
      )}
    </>
  );
}

/**
 * AboutFooter — compact footer line at the bottom of Settings showing
 * the daemon version and gateway connection status.
 */
export function AboutFooter({ status }: { status: StatusSnapshot }) {
  const { t } = useTranslation();
  const parts: string[] = ['Nexus Agent'];
  if (status.agent.version) parts.push(status.agent.version);
  parts.push(status.gatewayConnected
    ? t('settings.aboutConnected')
    : t('settings.aboutDisconnected'));
  return (
    <footer className={settings.aboutFooter}>{parts.join(' · ')}</footer>
  );
}
