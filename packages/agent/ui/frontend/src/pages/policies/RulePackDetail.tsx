import { useTranslation } from 'react-i18next';
import { Link, useParams } from 'react-router-dom';
import { useAppliedConfig } from './useAppliedConfig';
import styles from './policies.module.css';

export function RulePackDetail() {
  const { t, i18n } = useTranslation();
  const { id } = useParams<{ id: string }>();
  const { data, isLoading } = useAppliedConfig();

  const p = data?.rulePacks.find((x) => x.id === id);

  return (
    <div className={styles.root}>
      <nav className={styles.breadcrumb}>
        <Link to="/policies">{t('policies.breadcrumb.root')}</Link>
        <span className={styles.breadcrumbSep}>/</span>
        <Link to="/policies/rule-packs">{t('policies.rulePacks.title')}</Link>
        <span className={styles.breadcrumbSep}>/</span>
        <span>{p?.name || id}</span>
      </nav>

      {isLoading || !data ? (
        <p className={styles.empty}>{t('policies.loading')}</p>
      ) : !p ? (
        <p className={styles.empty}>{t('policies.notFound')}</p>
      ) : (
        <>
          <header className={styles.pageHeader}>
            <div>
              <h1 className={styles.title}>{p.name}</h1>
              <p className={styles.subtitle}>{p.description || '—'}</p>
            </div>
            <span className={styles.badge} data-tone={p.enabled ? 'ok' : 'muted'}>
              {p.enabled ? t('policies.rulePacks.cols.enabled') : t('policies.rulePacks.cols.disabled')}
            </span>
          </header>

          <section className={styles.section}>
            <h2 className={styles.sectionTitle}>{t('policies.detail.attributes')}</h2>
            <dl className={styles.dl}>
              <dt>{t('policies.rulePacks.cols.version')}</dt>
              <dd><code className={styles.mono}>{p.version || '—'}</code></dd>
              <dt>{t('policies.rulePacks.cols.maintainer')}</dt>
              <dd>{p.maintainer || '—'}</dd>
              <dt>{t('policies.rulePacks.cols.boundHook')}</dt>
              <dd>
                {p.boundHookId ? (
                  <Link to={`/policies/hooks/${encodeURIComponent(p.boundHookId)}`}>
                    <code className={styles.mono}>{p.boundHookId}</code>
                  </Link>
                ) : '—'}
              </dd>
              <dt>{t('policies.rulePacks.cols.rules')}</dt>
              <dd>{p.ruleCount}</dd>
              <dt>{t('policies.rulePacks.detail.packId')}</dt>
              <dd><code className={styles.mono}>{p.packId || '—'}</code></dd>
              <dt>{t('policies.rulePacks.detail.installedAt')}</dt>
              <dd>{p.installedAt ? new Date(p.installedAt).toLocaleString(i18n.language) : '—'}</dd>
              <dt>{t('policies.rulePacks.detail.installId')}</dt>
              <dd><code className={styles.mono}>{p.id}</code></dd>
            </dl>
          </section>

          <section className={styles.section}>
            <h2 className={styles.sectionTitle}>
              {t('policies.rulePacks.detail.rules')} ({p.rules?.length ?? 0})
            </h2>
            {!p.rules || p.rules.length === 0 ? (
              <div className={styles.empty}>{t('policies.rulePacks.detail.noRules')}</div>
            ) : (
              <table className={styles.table}>
                <thead>
                  <tr>
                    <th>{t('policies.rulePacks.detail.ruleCols.ruleId')}</th>
                    <th>{t('policies.rulePacks.detail.ruleCols.category')}</th>
                    <th>{t('policies.rulePacks.detail.ruleCols.severity')}</th>
                    <th>{t('policies.rulePacks.detail.ruleCols.pattern')}</th>
                    <th>{t('policies.rulePacks.detail.ruleCols.description')}</th>
                  </tr>
                </thead>
                <tbody>
                  {p.rules.map((r) => (
                    <tr key={r.id}>
                      <td><code className={styles.mono}>{r.ruleId || r.id.slice(0, 8)}</code></td>
                      <td>{r.category || '—'}</td>
                      <td>
                        <span className={styles.badge} data-tone={severityTone(r.severity)}>
                          {r.severity || '—'}
                        </span>
                      </td>
                      <td><code className={styles.mono}>{r.pattern || '—'}</code></td>
                      <td>{r.description || '—'}</td>
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

function severityTone(severity?: string): 'ok' | 'warn' | 'danger' | 'muted' {
  switch ((severity ?? '').toLowerCase()) {
    case 'hard':
    case 'critical':
    case 'high':
      return 'danger';
    case 'medium':
    case 'soft':
      return 'warn';
    case 'low':
    case 'info':
      return 'ok';
    default:
      return 'muted';
  }
}
