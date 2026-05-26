import { useMemo, useState } from 'react';
import { useTranslation } from 'react-i18next';
import { Link, useNavigate } from 'react-router-dom';
import type { PolicyInterceptionDomain } from '@/api/agent';
import { useAppliedConfig, useRefreshPolicies } from './useAppliedConfig';
import styles from './policies.module.css';

const PAGE_SIZE = 20;

export function DomainsList() {
  const { t } = useTranslation();
  const navigate = useNavigate();
  const { data, isLoading } = useAppliedConfig();
  const { refreshing, error, trigger, clearError } = useRefreshPolicies();
  const [search, setSearch] = useState('');
  const [page, setPage] = useState(0);

  const rows = data?.interceptionDomains ?? [];
  const filtered = useMemo(() => filterDomains(rows, search), [rows, search]);
  const totalPages = Math.max(1, Math.ceil(filtered.length / PAGE_SIZE));
  const pageRows = filtered.slice(page * PAGE_SIZE, (page + 1) * PAGE_SIZE);

  return (
    <div className={styles.root}>
      <nav className={styles.breadcrumb}>
        <Link to="/policies">{t('policies.breadcrumb.root')}</Link>
        <span className={styles.breadcrumbSep}>/</span>
        <span>{t('policies.interceptionDomains.title')}</span>
      </nav>
      <header className={styles.pageHeader}>
        <div>
          <h1 className={styles.title}>{t('policies.interceptionDomains.title')}</h1>
          <p className={styles.subtitle}>{t('policies.interceptionDomains.subtitle')}</p>
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
          placeholder={t('policies.interceptionDomains.searchPlaceholder')}
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
          {search ? t('policies.noMatch') : t('policies.interceptionDomains.empty')}
        </p>
      ) : (
        <>
          <table className={styles.table}>
            <thead>
              <tr>
                <th>{t('policies.interceptionDomains.cols.name')}</th>
                <th>{t('policies.interceptionDomains.cols.pattern')}</th>
                <th>{t('policies.interceptionDomains.cols.matchType')}</th>
                <th>{t('policies.interceptionDomains.cols.priority')}</th>
                <th>{t('policies.interceptionDomains.cols.defaultAction')}</th>
                <th>{t('policies.interceptionDomains.cols.status')}</th>
              </tr>
            </thead>
            <tbody>
              {pageRows.map((d) => (
                <tr
                  key={d.id || d.hostPattern}
                  className={styles.row}
                  onClick={() => d.id && navigate(`/policies/domains/${encodeURIComponent(d.id)}`)}
                >
                  <td>{d.name || '—'}</td>
                  <td><code className={styles.mono}>{d.hostPattern}</code></td>
                  <td>{d.hostMatchType || '—'}</td>
                  <td>{d.priority ?? '—'}</td>
                  <td>{d.defaultPathAction || '—'}</td>
                  <td>
                    <span className={styles.badge} data-tone={d.enabled ? 'ok' : 'muted'}>
                      {d.enabled ? t('policies.interceptionDomains.cols.enabled') : t('policies.interceptionDomains.cols.disabled')}
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

function filterDomains(rows: PolicyInterceptionDomain[], q: string): PolicyInterceptionDomain[] {
  if (!q.trim()) return rows;
  const needle = q.toLowerCase();
  return rows.filter((d) =>
    (d.hostPattern || '').toLowerCase().includes(needle) ||
    (d.name || '').toLowerCase().includes(needle) ||
    (d.adapterId || '').toLowerCase().includes(needle),
  );
}

export function Paginator({
  page,
  totalPages,
  onChange,
}: {
  page: number;
  totalPages: number;
  onChange: (next: number) => void;
}) {
  const { t } = useTranslation();
  if (totalPages <= 1) return null;
  return (
    <div className={styles.paginator}>
      <span className={styles.paginatorInfo}>
        {t('policies.paginator.info', { page: page + 1, total: totalPages })}
      </span>
      <div className={styles.paginatorPages}>
        <button
          type="button"
          className={styles.button}
          disabled={page === 0}
          onClick={() => onChange(Math.max(0, page - 1))}
        >
          {t('policies.paginator.prev')}
        </button>
        <button
          type="button"
          className={styles.button}
          disabled={page >= totalPages - 1}
          onClick={() => onChange(Math.min(totalPages - 1, page + 1))}
        >
          {t('policies.paginator.next')}
        </button>
      </div>
    </div>
  );
}
