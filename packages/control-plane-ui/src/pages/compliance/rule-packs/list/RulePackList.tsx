import { useMemo, useState } from 'react';
import { useTranslation } from 'react-i18next';
import { Link, useNavigate } from 'react-router-dom';

import { rulePacksApi, type RulePackMeta } from '@/api/services';
import { AlertDialog, Button, PageHeader, RowActions, RowDeleteAction } from '@/components/ui';
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
  const [deletingPack, setDeletingPack] = useState<RulePackMeta | null>(null);
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
      onSuccess: () => setDeletingPack(null),
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

  return (
    <div className={styles.page}>
      <div className={styles.header}>
        <PageHeader
          title={t('pages:hooks.rulePacks.listTitle', 'Rule Packs')}
          subtitle={t(
            'pages:hooks.rulePacks.listSubtitle',
            'Author, import, and bind rule packs that power the unified hooks evaluation engine.',
          )}
          subtitleClassName={styles.headerSubtitle}
          action={(
            <div className={styles.toolbarActions}>
              <Button variant="secondary" onClick={() => setImportOpen(true)}>
                <span className={styles.importButtonContent}>
                  <svg className={styles.importIcon} width="14" height="14" viewBox="0 0 16 16" fill="none" aria-hidden>
                    <path d="M8 2v7" stroke="currentColor" strokeWidth="1.6" strokeLinecap="round" />
                    <path d="M5.25 6.75 8 9.5l2.75-2.75" stroke="currentColor" strokeWidth="1.6" strokeLinecap="round" strokeLinejoin="round" />
                    <path d="M3 10.5v2.25c0 .69.56 1.25 1.25 1.25h7.5c.69 0 1.25-.56 1.25-1.25V10.5" stroke="currentColor" strokeWidth="1.6" strokeLinecap="round" />
                  </svg>
                  {t('pages:hooks.rulePacks.importButton', 'Import YAML')}
                </span>
              </Button>
              <Button onClick={() => navigate('/compliance/rule-packs/create')}>
                {t('pages:hooks.rulePacks.createButton', 'Create pack')}
              </Button>
            </div>
          )}
        />
      </div>

      <div className={styles.toolbar}>
        <label className={styles.filterField}>
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
                <th>{t('pages:hooks.rulePacks.colActions', 'Actions')}</th>
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
                      <RowDeleteAction label={t('common:delete', 'Delete')} onAction={() => setDeletingPack(pack)} disabled={deleting} />
                    </RowActions>
                  </td>
                </tr>
              ))}
            </tbody>
          </table>
        </div>
      )}

      <ImportPackModal open={importOpen} onClose={() => setImportOpen(false)} />
      <AlertDialog
        open={deletingPack != null}
        onOpenChange={(open) => { if (!open) setDeletingPack(null); }}
        title={t('pages:hooks.rulePacks.deleteTitle', 'Delete rule pack?')}
        description={t(
          'pages:hooks.rulePacks.deleteConfirm',
          'Delete rule pack "{{name}}" {{version}}? Installs referencing this pack must be removed first.',
          { name: deletingPack?.name ?? '', version: deletingPack?.version ?? '' },
        )}
        confirmLabel={t('common:delete', 'Delete')}
        onConfirm={() => { if (deletingPack) deletePack(deletingPack.id); }}
        variant="danger"
      />
    </div>
  );
}
