import { useMemo, useState } from 'react';
import { useTranslation } from 'react-i18next';
import { Link, useNavigate } from 'react-router-dom';

import { rulePacksApi, type RulePackMeta } from '@/api/services';
import { Button, RowActions, RowDeleteAction } from '@/components/ui';
import { useApi } from '@/hooks/useApi';
import { useMutation } from '@/hooks/useMutation';

import { ImportPackModal } from '../import/ImportPackModal';
import styles from './RulePackList.module.css';

function formatCreatedAt(value: string): string {
  const date = new Date(value);
  if (Number.isNaN(date.getTime())) return value;
  return date.toLocaleDateString();
}

export function RulePackList() {
  const { t } = useTranslation();
  const navigate = useNavigate();
  const [maintainerFilter, setMaintainerFilter] = useState('all');
  const [importOpen, setImportOpen] = useState(false);
  const {
    data,
    loading,
    error,
  } = useApi<RulePackMeta[]>(
    () => rulePacksApi.list(),
    ['admin', 'rule-packs', 'list'],
  );

  const { mutate: deletePack, loading: deleting } = useMutation<string, void>(
    (id) => rulePacksApi.delete(id),
    {
      invalidateQueries: [['admin', 'rule-packs', 'list']],
      successMessage: t('pages:hooks.rulePacks.deleteSuccess', 'Rule pack deleted'),
    },
  );

  const maintainers = useMemo(() => {
    const values = new Set((data ?? []).map((pack) => pack.maintainer));
    return ['all', ...Array.from(values).sort()];
  }, [data]);

  const filtered = useMemo(() => {
    if (!data) return [];
    if (maintainerFilter === 'all') return data;
    return data.filter((pack) => pack.maintainer === maintainerFilter);
  }, [data, maintainerFilter]);

  function onDelete(pack: RulePackMeta) {
    const message = t(
      'pages:hooks.rulePacks.deleteConfirm',
      'Delete rule pack "{{name}}" {{version}}? Installs referencing this pack must be removed first.',
      { name: pack.name, version: pack.version },
    );

    if (!window.confirm(message)) return;
    deletePack(pack.id);
  }

  return (
    <div className={styles.page}>
      <div className={styles.header}>
        <h1 className={styles.title}>{t('pages:hooks.rulePacks.listTitle', 'Rule Packs')}</h1>
        <p className={styles.subtitle}>
          {t(
            'pages:hooks.rulePacks.listSubtitle',
            'Author, import, and bind rule packs that power the unified hooks evaluation engine.',
          )}
        </p>
      </div>

      <div className={styles.toolbar}>
        <label className={styles.filterField}>
          <span className={styles.filterLabel}>
            {t('pages:hooks.rulePacks.maintainerFilter', 'Maintainer')}
          </span>
          <select
            aria-label={t('pages:hooks.rulePacks.maintainerFilter', 'Maintainer')}
            className={styles.select}
            value={maintainerFilter}
            onChange={(e) => setMaintainerFilter(e.target.value)}
          >
            {maintainers.map((maintainer) => (
              <option key={maintainer} value={maintainer}>
                {maintainer === 'all'
                  ? t('pages:hooks.rulePacks.allMaintainers', 'All maintainers')
                  : maintainer}
              </option>
            ))}
          </select>
        </label>
        <div className={styles.toolbarActions}>
          <Button variant="secondary" onClick={() => setImportOpen(true)}>
            {t('pages:hooks.rulePacks.importButton', 'Import YAML')}
          </Button>
          <Button onClick={() => navigate('/compliance/rule-packs/create')}>
            {t('pages:hooks.rulePacks.createButton', 'Create pack')}
          </Button>
        </div>
      </div>

      {loading && (
        <div className={styles.state}>
          {t('common:loading', 'Loading…')}
        </div>
      )}

      {error && !loading && (
        <div className={styles.error} role="alert">
          {t('pages:hooks.rulePacks.loadError', 'Failed to load rule packs.')}
        </div>
      )}

      {!loading && !error && filtered.length === 0 && (
        <div className={styles.state}>
          {t('pages:hooks.rulePacks.empty', 'No rule packs found.')}
        </div>
      )}

      {!loading && !error && filtered.length > 0 && (
        <div className={styles.tableWrap}>
          <table className={styles.table}>
            <thead>
              <tr>
                <th>{t('pages:hooks.rulePacks.colName', 'Name')}</th>
                <th>{t('pages:hooks.rulePacks.colVersion', 'Version')}</th>
                <th>{t('pages:hooks.rulePacks.colMaintainer', 'Maintainer')}</th>
                <th>{t('pages:hooks.rulePacks.colCreatedAt', 'Created')}</th>
                <th aria-label={t('pages:hooks.rulePacks.colActions', 'Actions')} />
              </tr>
            </thead>
            <tbody>
              {filtered.map((pack) => (
                <tr key={pack.id}>
                  <td>
                    <Link className={styles.link} to={`/compliance/rule-packs/${pack.id}`}>
                      {pack.name}
                    </Link>
                  </td>
                  <td>{pack.version}</td>
                  <td>{pack.maintainer}</td>
                  <td>{formatCreatedAt(pack.createdAt)}</td>
                  <td>
                    <RowActions>
                      <RowDeleteAction label={t('common:delete', 'Delete')} onAction={() => onDelete(pack)} disabled={deleting} />
                    </RowActions>
                  </td>
                </tr>
              ))}
            </tbody>
          </table>
        </div>
      )}

      <ImportPackModal open={importOpen} onClose={() => setImportOpen(false)} />
    </div>
  );
}
