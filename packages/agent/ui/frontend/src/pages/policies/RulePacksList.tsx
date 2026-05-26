import { useMemo, useState } from 'react';
import { useTranslation } from 'react-i18next';
import { Link, useNavigate } from 'react-router-dom';
import type { PolicyRulePack } from '@/api/agent';
import { useAppliedConfig, useRefreshPolicies } from './useAppliedConfig';
import { Paginator } from './DomainsList';
import styles from './policies.module.css';

const PAGE_SIZE = 20;

export function RulePacksList() {
  const { t } = useTranslation();
  const navigate = useNavigate();
  const { data, isLoading } = useAppliedConfig();
  const { refreshing, error, trigger, clearError } = useRefreshPolicies();
  const [search, setSearch] = useState('');
  const [page, setPage] = useState(0);

  const rows = data?.rulePacks ?? [];
  const filtered = useMemo(() => filterPacks(rows, search), [rows, search]);
  const totalPages = Math.max(1, Math.ceil(filtered.length / PAGE_SIZE));
  const pageRows = filtered.slice(page * PAGE_SIZE, (page + 1) * PAGE_SIZE);

  return (
    <div className={styles.root}>
      <nav className={styles.breadcrumb}>
        <Link to="/policies">{t('policies.breadcrumb.root')}</Link>
        <span className={styles.breadcrumbSep}>/</span>
        <span>{t('policies.rulePacks.title')}</span>
      </nav>
      <header className={styles.pageHeader}>
        <div>
          <h1 className={styles.title}>{t('policies.rulePacks.title')}</h1>
          <p className={styles.subtitle}>{t('policies.rulePacks.subtitle')}</p>
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
          placeholder={t('policies.rulePacks.searchPlaceholder')}
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
          {search ? t('policies.noMatch') : t('policies.rulePacks.empty')}
        </p>
      ) : (
        <>
          <table className={styles.table}>
            <thead>
              <tr>
                <th>{t('policies.rulePacks.cols.name')}</th>
                <th>{t('policies.rulePacks.cols.version')}</th>
                <th>{t('policies.rulePacks.cols.maintainer')}</th>
                <th>{t('policies.rulePacks.cols.boundHook')}</th>
                <th>{t('policies.rulePacks.cols.rules')}</th>
                <th>{t('policies.rulePacks.cols.status')}</th>
              </tr>
            </thead>
            <tbody>
              {pageRows.map((p) => (
                <tr
                  key={p.id}
                  className={styles.row}
                  onClick={() => p.id && navigate(`/policies/rule-packs/${encodeURIComponent(p.id)}`)}
                >
                  <td>{p.name}</td>
                  <td><code className={styles.mono}>{p.version || '—'}</code></td>
                  <td>{p.maintainer || '—'}</td>
                  <td><code className={styles.mono}>{p.boundHookId || '—'}</code></td>
                  <td>{p.ruleCount ?? '—'}</td>
                  <td>
                    <span className={styles.badge} data-tone={p.enabled ? 'ok' : 'muted'}>
                      {p.enabled ? t('policies.rulePacks.cols.enabled') : t('policies.rulePacks.cols.disabled')}
                    </span>
                  </td>
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

function filterPacks(rows: PolicyRulePack[], q: string): PolicyRulePack[] {
  if (!q.trim()) return rows;
  const needle = q.toLowerCase();
  return rows.filter((p) =>
    (p.name || '').toLowerCase().includes(needle) ||
    (p.maintainer || '').toLowerCase().includes(needle) ||
    (p.boundHookId || '').toLowerCase().includes(needle),
  );
}
