import { useMemo, useState } from 'react';
import { useTranslation } from 'react-i18next';
import { Link } from 'react-router-dom';
import type { PolicyExemption } from '@/api/agent';
import { useAppliedConfig, useRefreshPolicies } from './useAppliedConfig';
import { Paginator } from './DomainsList';
import styles from './policies.module.css';

const PAGE_SIZE = 20;

export function ExemptionsList() {
  const { t } = useTranslation();
  const { data, isLoading } = useAppliedConfig();
  const { refreshing, error, trigger, clearError } = useRefreshPolicies();
  const [search, setSearch] = useState('');
  const [page, setPage] = useState(0);

  const rows = data?.exemptions ?? [];
  const filtered = useMemo(() => filterExemptions(rows, search), [rows, search]);
  const totalPages = Math.max(1, Math.ceil(filtered.length / PAGE_SIZE));
  const pageRows = filtered.slice(page * PAGE_SIZE, (page + 1) * PAGE_SIZE);

  return (
    <div className={styles.root}>
      <nav className={styles.breadcrumb}>
        <Link to="/policies">{t('policies.breadcrumb.root')}</Link>
        <span className={styles.breadcrumbSep}>/</span>
        <span>{t('policies.exemptions.title')}</span>
      </nav>
      <header className={styles.pageHeader}>
        <div>
          <h1 className={styles.title}>{t('policies.exemptions.title')}</h1>
          <p className={styles.subtitle}>{t('policies.exemptions.subtitle')}</p>
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

      <div className={styles.toolbar}>
        <input
          className={styles.search}
          type="search"
          placeholder={t('policies.exemptions.searchPlaceholder')}
          value={search}
          onChange={(e) => { setSearch(e.target.value); setPage(0); }}
        />
        <span className={styles.toolbarCount}>
          {t('policies.toolbar.count', {
            shown: pageRows.length,
            filtered: filtered.length,
            total: rows.length,
          })}
        </span>
      </div>

      {isLoading ? (
        <p className={styles.empty}>{t('policies.loading')}</p>
      ) : pageRows.length === 0 ? (
        <p className={styles.empty}>
          {search ? t('policies.noMatch') : t('policies.exemptions.empty')}
        </p>
      ) : (
        <>
          <table className={styles.table}>
            <thead>
              <tr>
                <th>{t('policies.exemptions.cols.hostUser')}</th>
                <th>{t('policies.exemptions.cols.reason')}</th>
              </tr>
            </thead>
            <tbody>
              {pageRows.map((e) => (
                <tr key={e.id}>
                  <td><code className={styles.mono}>{e.host ?? e.user ?? e.id}</code></td>
                  <td>{e.reason || '—'}</td>
                </tr>
              ))}
            </tbody>
          </table>
          <Paginator page={page} totalPages={totalPages} onChange={setPage} />
        </>
      )}
    </div>
  );
}

function filterExemptions(rows: PolicyExemption[], q: string): PolicyExemption[] {
  if (!q.trim()) return rows;
  const needle = q.toLowerCase();
  return rows.filter((e) =>
    (e.host ?? '').toLowerCase().includes(needle) ||
    (e.user ?? '').toLowerCase().includes(needle) ||
    (e.reason ?? '').toLowerCase().includes(needle),
  );
}
