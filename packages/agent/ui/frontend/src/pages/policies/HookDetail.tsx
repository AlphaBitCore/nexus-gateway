import { useTranslation } from 'react-i18next';
import { Link, useParams } from 'react-router-dom';
import { useAppliedConfig } from './useAppliedConfig';
import styles from './policies.module.css';

export function HookDetail() {
  const { t } = useTranslation();
  const { id } = useParams<{ id: string }>();
  const { data, isLoading } = useAppliedConfig();

  const h = data?.hooks.find((x) => x.id === id);
  const prettyConfig = h?.config !== undefined ? JSON.stringify(h.config, null, 2) : '';

  return (
    <div className={styles.root}>
      <nav className={styles.breadcrumb}>
        <Link to="/policies">{t('policies.breadcrumb.root')}</Link>
        <span className={styles.breadcrumbSep}>/</span>
        <Link to="/policies/hooks">{t('policies.hooks.title')}</Link>
        <span className={styles.breadcrumbSep}>/</span>
        <span>{h?.name || id}</span>
      </nav>

      {isLoading || !data ? (
        <p className={styles.empty}>{t('policies.loading')}</p>
      ) : !h ? (
        <p className={styles.empty}>{t('policies.notFound')}</p>
      ) : (
        <>
          <header className={styles.pageHeader}>
            <div>
              <h1 className={styles.title}>{h.name}</h1>
              <p className={styles.subtitle}>{h.stage || '—'}</p>
            </div>
            <span className={styles.badge} data-tone={h.enabled ? 'ok' : 'muted'}>
              {h.enabled ? t('policies.hooks.cols.enabled') : t('policies.hooks.cols.disabled')}
            </span>
          </header>

          <section className={styles.section}>
            <h2 className={styles.sectionTitle}>{t('policies.detail.attributes')}</h2>
            <dl className={styles.dl}>
              <dt>{t('policies.hooks.cols.implementation')}</dt>
              <dd><code className={styles.mono}>{h.implementationId || '—'}</code></dd>
              <dt>{t('policies.hooks.cols.stage')}</dt>
              <dd>{h.stage || '—'}</dd>
              <dt>{t('policies.hooks.cols.priority')}</dt>
              <dd>{h.priority ?? '—'}</dd>
              <dt>{t('policies.hooks.cols.failBehavior')}</dt>
              <dd>{h.failBehavior || '—'}</dd>
              <dt>{t('policies.hooks.detail.timeout')}</dt>
              <dd>{h.timeoutMs ? `${h.timeoutMs} ms` : '—'}</dd>
              <dt>{t('policies.hooks.detail.applicableIngress')}</dt>
              <dd>{h.applicableIngress?.length ? h.applicableIngress.join(', ') : '—'}</dd>
              <dt>ID</dt>
              <dd><code className={styles.mono}>{h.id}</code></dd>
            </dl>
          </section>

          <section className={styles.section}>
            <h2 className={styles.sectionTitle}>{t('policies.hooks.detail.config')}</h2>
            {prettyConfig && prettyConfig !== 'null' ? (
              <pre className={styles.codeBlock}>{prettyConfig}</pre>
            ) : (
              <div className={styles.empty}>{t('policies.hooks.detail.noConfig')}</div>
            )}
          </section>
        </>
      )}
    </div>
  );
}
