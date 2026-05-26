import { useMemo, useState } from 'react';
import { useTranslation } from 'react-i18next';
import { Link, useNavigate } from 'react-router-dom';
import type { PolicyHook } from '@/api/agent';
import { useAppliedConfig, useRefreshPolicies } from './useAppliedConfig';
import { Paginator } from './DomainsList';
import { HookPipeline } from './HookPipeline';
import styles from './policies.module.css';

const PAGE_SIZE = 20;

export function HooksList() {
  const { t } = useTranslation();
  const navigate = useNavigate();
  const { data, isLoading } = useAppliedConfig();
  const { refreshing, error, trigger, clearError } = useRefreshPolicies();
  const [search, setSearch] = useState('');
  const [page, setPage] = useState(0);

  const rows = useMemo(
    () => (data?.hooks ?? []).slice().sort((a, b) => (a.priority ?? 0) - (b.priority ?? 0)),
    [data?.hooks],
  );
  const filtered = useMemo(() => filterHooks(rows, search), [rows, search]);
  const totalPages = Math.max(1, Math.ceil(filtered.length / PAGE_SIZE));
  const pageRows = filtered.slice(page * PAGE_SIZE, (page + 1) * PAGE_SIZE);

  return (
    <div className={styles.root}>
      <nav className={styles.breadcrumb}>
        <Link to="/policies">{t('policies.breadcrumb.root')}</Link>
        <span className={styles.breadcrumbSep}>/</span>
        <span>{t('policies.hooks.title')}</span>
      </nav>
      <header className={styles.pageHeader}>
        <div>
          <h1 className={styles.title}>{t('policies.hooks.title')}</h1>
          <p className={styles.subtitle}>{t('policies.hooks.subtitle')}</p>
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

      <HookPipeline hooks={rows} />

      <div className={styles.toolbar}>
        <input
          className={styles.search}
          type="search"
          placeholder={t('policies.hooks.searchPlaceholder')}
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
          {search ? t('policies.noMatch') : t('policies.hooks.empty')}
        </p>
      ) : (
        <>
          <table className={styles.table}>
            <thead>
              <tr>
                <th>{t('policies.hooks.cols.priority')}</th>
                <th>{t('policies.hooks.cols.name')}</th>
                <th>{t('policies.hooks.cols.stage')}</th>
                <th>{t('policies.hooks.cols.implementation')}</th>
                <th>{t('policies.hooks.cols.failBehavior')}</th>
                <th>{t('policies.hooks.cols.status')}</th>
              </tr>
            </thead>
            <tbody>
              {pageRows.map((h) => (
                <tr
                  key={h.id}
                  className={styles.row}
                  onClick={() => h.id && navigate(`/policies/hooks/${encodeURIComponent(h.id)}`)}
                >
                  <td>{h.priority ?? '—'}</td>
                  <td>{h.name}</td>
                  <td>{h.stage || '—'}</td>
                  <td><code className={styles.mono}>{h.implementationId || '—'}</code></td>
                  <td>{h.failBehavior || '—'}</td>
                  <td>
                    <span className={styles.badge} data-tone={h.enabled ? 'ok' : 'muted'}>
                      {h.enabled ? t('policies.hooks.cols.enabled') : t('policies.hooks.cols.disabled')}
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

function filterHooks(rows: PolicyHook[], q: string): PolicyHook[] {
  if (!q.trim()) return rows;
  const needle = q.toLowerCase();
  return rows.filter((h) =>
    (h.name || '').toLowerCase().includes(needle) ||
    (h.implementationId || '').toLowerCase().includes(needle) ||
    (h.stage || '').toLowerCase().includes(needle),
  );
}
