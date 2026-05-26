import { useTranslation } from 'react-i18next';
import { Link, useParams } from 'react-router-dom';
import { useAppliedConfig } from './useAppliedConfig';
import styles from './policies.module.css';

export function DomainDetail() {
  const { t } = useTranslation();
  const { id } = useParams<{ id: string }>();
  const { data, isLoading } = useAppliedConfig();

  const d = data?.interceptionDomains.find((x) => x.id === id);

  return (
    <div className={styles.root}>
      <nav className={styles.breadcrumb}>
        <Link to="/policies">{t('policies.breadcrumb.root')}</Link>
        <span className={styles.breadcrumbSep}>/</span>
        <Link to="/policies/domains">{t('policies.interceptionDomains.title')}</Link>
        <span className={styles.breadcrumbSep}>/</span>
        <span>{d?.name || d?.hostPattern || id}</span>
      </nav>

      {isLoading || !data ? (
        <p className={styles.empty}>{t('policies.loading')}</p>
      ) : !d ? (
        <p className={styles.empty}>{t('policies.notFound')}</p>
      ) : (
        <>
          <header className={styles.pageHeader}>
            <div>
              <h1 className={styles.title}>{d.name || d.hostPattern}</h1>
              <p className={styles.subtitle}>
                <code className={styles.mono}>{d.hostPattern}</code>
              </p>
            </div>
            <span className={styles.badge} data-tone={d.enabled ? 'ok' : 'muted'}>
              {d.enabled ? t('policies.interceptionDomains.cols.enabled') : t('policies.interceptionDomains.cols.disabled')}
            </span>
          </header>

          <section className={styles.section}>
            <h2 className={styles.sectionTitle}>{t('policies.detail.attributes')}</h2>
            <dl className={styles.dl}>
              <dt>{t('policies.interceptionDomains.cols.matchType')}</dt>
              <dd>{d.hostMatchType || '—'}</dd>
              <dt>{t('policies.interceptionDomains.cols.priority')}</dt>
              <dd>{d.priority ?? '—'}</dd>
              <dt>{t('policies.interceptionDomains.cols.defaultAction')}</dt>
              <dd>{d.defaultPathAction || '—'}</dd>
              <dt>{t('policies.interceptionDomains.detail.adapter')}</dt>
              <dd>{d.adapterId || '—'}</dd>
              <dt>{t('policies.interceptionDomains.detail.onAdapterError')}</dt>
              <dd>{d.onAdapterError || '—'}</dd>
              <dt>{t('policies.interceptionDomains.detail.networkZone')}</dt>
              <dd>{d.networkZone || '—'}</dd>
              <dt>ID</dt>
              <dd><code className={styles.mono}>{d.id}</code></dd>
            </dl>
          </section>

          <section className={styles.section}>
            <h2 className={styles.sectionTitle}>
              {t('policies.interceptionDomains.detail.paths')} ({d.paths?.length ?? 0})
            </h2>
            {!d.paths || d.paths.length === 0 ? (
              <div className={styles.empty}>{t('policies.interceptionDomains.detail.noPaths')}</div>
            ) : (
              <table className={styles.table}>
                <thead>
                  <tr>
                    <th>{t('policies.interceptionDomains.detail.pathPattern')}</th>
                    <th>{t('policies.interceptionDomains.cols.matchType')}</th>
                    <th>{t('policies.interceptionDomains.cols.action')}</th>
                    <th>{t('policies.interceptionDomains.cols.priority')}</th>
                    <th>{t('policies.interceptionDomains.cols.status')}</th>
                  </tr>
                </thead>
                <tbody>
                  {d.paths.map((p) => (
                    <tr key={p.id}>
                      <td><code className={styles.mono}>{p.pathPattern.join(', ') || '*'}</code></td>
                      <td>{p.matchType || '—'}</td>
                      <td>{p.action || '—'}</td>
                      <td>{p.priority ?? '—'}</td>
                      <td>
                        <span className={styles.badge} data-tone={p.enabled ? 'ok' : 'muted'}>
                          {p.enabled ? t('policies.interceptionDomains.cols.enabled') : t('policies.interceptionDomains.cols.disabled')}
                        </span>
                      </td>
                    </tr>
                  ))}
                </tbody>
              </table>
            )}
          </section>
        </>
      )}
    </div>
  );
}
