import { useState, useMemo } from 'react';
import { useNavigate } from 'react-router-dom';
import { useTranslation } from 'react-i18next';
import { useApi } from '../../../hooks/useApi';
import { organizationApi } from '@/api/services';
import { useDebouncedValue } from '../../../hooks/useDebouncedValue';
import { useMutation } from '../../../hooks/useMutation';
import { usePermission } from '../../../hooks/usePermission';
import {
  PageHeader, ListFilterToolbar, Badge, statusToVariant,
  AlertDialog, Skeleton, ErrorBanner, Button, Stack, Card,
  RowActions, RowActionIconButton, OpenActionIcon, DeleteActionIcon,
} from '@/components/ui';
import type { Organization } from '../../../api/types';
import styles from './OrganizationList.module.css';

function OrgTreeNode({ org, level = 0, searchHighlight, onDelete }: { org: Organization; level?: number; searchHighlight: string; onDelete: (o: Organization) => void }) {
  const { t } = useTranslation();
  const [expanded, setExpanded] = useState(level < 2);
  const navigate = useNavigate();

  const hasChildren = (org.children?.length ?? 0) > 0;
  const projectCount = org.projectCount ?? org._count?.projects ?? 0;
  const matchesHighlight = !searchHighlight || org.name.toLowerCase().includes(searchHighlight) || org.code.toLowerCase().includes(searchHighlight);
  const canDeleteOrg = !hasChildren && projectCount === 0;
  const cannotDeleteDetail = [hasChildren ? t('pages:organizations.subOrgsCount', { count: org.children?.length ?? 0 }) : '', projectCount > 0 ? t('pages:organizations.projectsCount', { count: projectCount }) : ''].filter(Boolean).join(t('pages:organizations.and'));

  return (
    <>
      <tr
        onClick={() => navigate(`/iam/organizations/${org.id}`)}
        className={styles.treeRow}
      >
        <td className={styles.tableCell} style={{ paddingLeft: `${16 + level * 24}px` /* dynamic: depends on tree depth */ }}>
          {hasChildren ? (
            <span
              onClick={(e) => { e.stopPropagation(); setExpanded(!expanded); }}
              className={styles.toggleButton}
            >
              {expanded ? '\u25BE' : '\u25B8'}
            </span>
          ) : (
            <span className={styles.toggleSpacer} />
          )}
          <span className={matchesHighlight && searchHighlight ? styles.highlightedName : undefined}>{org.name}</span>
        </td>
        <td className={styles.tableCell}>
          <code className={styles.codeCell}>{org.code}</code>
        </td>
        <td className={styles.tableCell}>{projectCount}</td>
        <td className={styles.tableCell}>
          <Badge variant={statusToVariant(org.enabled ? 'active' : 'disabled')}>{org.enabled ? t('pages:organizations.active') : t('pages:organizations.disabled')}</Badge>
        </td>
        <td className={`${styles.tableCell} ${styles.actionsCell}`}>
          <RowActions>
            <RowActionIconButton
              label={t('common:view', 'View')}
              onAction={() => navigate(`/iam/organizations/${org.id}`)}
            >
              <OpenActionIcon />
            </RowActionIconButton>
            <RowActionIconButton
              label={canDeleteOrg ? t('pages:organizations.deleteTitle') : t('pages:organizations.cannotDeleteTip', { detail: cannotDeleteDetail })}
              tone="danger"
              disabled={!canDeleteOrg}
              onAction={() => onDelete(org)}
            >
              <DeleteActionIcon />
            </RowActionIconButton>
          </RowActions>
        </td>
      </tr>
      {expanded && org.children?.map(child => (
        <OrgTreeNode key={child.id} org={child} level={level + 1} searchHighlight={searchHighlight} onDelete={onDelete} />
      ))}
    </>
  );
}

export function OrganizationList() {
  const { t } = useTranslation();
  const navigate = useNavigate();
  const [search, setSearch] = useState('');
  const debouncedSearch = useDebouncedValue(search, 300);
  const { data: rawData, loading, error, refetch } = useApi<{ data: Organization[] }>(
    () => {
      const q = debouncedSearch.trim();
      return organizationApi.tree(q ? { q } : undefined);
    },
    ['admin', 'organizations', 'tree', debouncedSearch],
  );
  const [enabledFilter, setEnabledFilter] = useState('');
  const [deletingOrg, setDeletingOrg] = useState<Organization | null>(null);
  const canCreate = usePermission('organization:create');

  const { mutate: deleteOrg } = useMutation(
    (orgId: string) => organizationApi.delete(orgId),
    {
      invalidateQueries: [['api', 'admin', 'organizations']],
      onSuccess: () => { setDeletingOrg(null); },
      successMessage: t('pages:organizations.organizationDeleted'),
    },
  );

  const filtered = useMemo(() => {
    let items = rawData?.data ?? [];
    if (enabledFilter === 'enabled') items = filterTree(items, o => o.enabled);
    if (enabledFilter === 'disabled') items = filterTree(items, o => !o.enabled);
    return items;
  }, [rawData, enabledFilter]);

  const highlight = debouncedSearch.trim().toLowerCase();

  if (loading) return <Skeleton.ListPageSkeleton />;
  if (error) return <ErrorBanner message={error.message} onRetry={refetch} />;

  return (
    <Stack gap="md">
      <PageHeader
        title={t('pages:organizations.title')}
        subtitle={t('pages:organizations.subtitle')}
        action={
          canCreate ? (
            <Button onClick={() => navigate('/iam/organizations/new')}>
              {t('pages:organizations.createOrganization')}
            </Button>
          ) : undefined
        }
      />

      <ListFilterToolbar
        variant="boxed"
        className={styles.filterToolbar}
        searchWidth={420}
        hideClearButton
        searchPlaceholder={t('pages:organizations.searchPlaceholder')}
        searchValue={search}
        onSearchChange={setSearch}
      >
        <select aria-label={t('pages:organizations.filterByStatus')} value={enabledFilter} onChange={e => setEnabledFilter(e.target.value)} className={styles.filterSelect}>
          <option value="">{t('pages:organizations.allStatuses')}</option>
          <option value="enabled">{t('common:enabled')}</option>
          <option value="disabled">{t('common:disabled')}</option>
        </select>
      </ListFilterToolbar>

      <div className={styles.listCount}>
        {t('pages:organizations.topLevelOrgCount', { count: filtered.length })}
      </div>

      <Card padding="none">
        <div className={styles.scrollWrapper}>
          <table className={styles.fullWidthTable}>
            <thead>
              <tr>
                <th className={styles.tableHeader}>{t('pages:organizations.name')}</th>
                <th className={styles.tableHeader}>{t('pages:organizations.code')}</th>
                <th className={styles.tableHeader}>{t('pages:organizations.projects')}</th>
                <th className={styles.tableHeader}>{t('pages:organizations.status')}</th>
                <th className={styles.tableHeader}>{t('pages:organizations.actions')}</th>
              </tr>
            </thead>
            <tbody>
              {(filtered ?? []).length === 0 ? (
                <tr>
                  <td colSpan={5} className={`${styles.tableCell} ${styles.emptyRow}`}>
                    {t('pages:organizations.noOrganizationsFound')}
                  </td>
                </tr>
              ) : (
                filtered.map(org => (
                  <OrgTreeNode key={org.id} org={org} searchHighlight={highlight} onDelete={setDeletingOrg} />
                ))
              )}
            </tbody>
          </table>
        </div>
      </Card>

      <AlertDialog
        open={!!deletingOrg}
        onOpenChange={(open) => { if (!open) setDeletingOrg(null); }}
        title={t('pages:organizations.deleteOrganization')}
        description={t('pages:organizations.deleteConfirm', { name: deletingOrg?.name ?? '', code: deletingOrg?.code ?? '' })}
        confirmLabel={t('pages:organizations.delete')}
        onConfirm={() => { if (deletingOrg) deleteOrg(deletingOrg.id); }}
        variant="danger"
      />
    </Stack>
  );
}

function filterTree(orgs: Organization[], predicate: (o: Organization) => boolean): Organization[] {
  const result: Organization[] = [];
  for (const o of orgs) {
    const filteredChildren = filterTree(o.children ?? [], predicate);
    if (predicate(o) || filteredChildren.length > 0) {
      result.push({ ...o, children: filteredChildren });
    }
  }
  return result;
}
