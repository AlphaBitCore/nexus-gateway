import { useState, useCallback } from 'react';
import { useNavigate } from 'react-router-dom';
import { useTranslation } from 'react-i18next';
import { useApi } from '../../../hooks/useApi';
import { projectApi } from '@/api/services';
import { useDebouncedValue } from '../../../hooks/useDebouncedValue';
import { useMutation } from '../../../hooks/useMutation';
import { usePermission } from '../../../hooks/usePermission';
import {
  PageHeader, DataTable, ListFilterToolbar, Badge, statusToVariant,
  AlertDialog, Skeleton, ErrorBanner, Button, Stack, Card,
  ListPagination, DEFAULT_ADMIN_LIST_PAGE_SIZE, type AdminListPageSize,
  RowActions, RowActionIconButton, OpenActionIcon, DeleteActionIcon,
} from '@/components/ui';
import type { DataTableColumn } from '@/components/ui';
import type { Project } from '../../../api/types';
import styles from './ProjectList.module.css';

export function ProjectList() {
  const { t } = useTranslation();
  const navigate = useNavigate();
  const [search, setSearch] = useState('');
  const debouncedSearch = useDebouncedValue(search, 300);
  const [statusFilter, setStatusFilter] = useState('');
  const [offset, setOffset] = useState(0);
  const [pageLimit, setPageLimit] = useState<AdminListPageSize>(DEFAULT_ADMIN_LIST_PAGE_SIZE);
  const { data, loading, error, refetch } = useApi<{ data: Project[]; total: number }>(
    () => {
      const params: Record<string, string> = {
        limit: String(pageLimit),
        offset: String(offset),
      };
      const q = debouncedSearch.trim();
      if (q) params.q = q;
      if (statusFilter) params.status = statusFilter;
      return projectApi.list(params);
    },
    ['admin', 'projects', 'list', debouncedSearch, statusFilter, offset, pageLimit],
  );
  const [deleting, setDeleting] = useState<Project | null>(null);
  const canCreate = usePermission('project:create');
  const canDelete = usePermission('project:delete');

  const { mutate: deleteProject } = useMutation(
    (id: string) => projectApi.delete(id),
    {
      invalidateQueries: [['api', 'admin', 'projects']],
      onSuccess: () => { setDeleting(null); },
      successMessage: t('pages:projects.projectDeleted'),
    },
  );

  const projects = data?.data ?? [];
  const total = data?.total ?? 0;

  const onSearchChange = useCallback((v: string) => {
    setSearch(v);
    setOffset(0);
  }, []);

  const onStatusFilterChange = useCallback((e: React.ChangeEvent<HTMLSelectElement>) => {
    setStatusFilter(e.target.value);
    setOffset(0);
  }, []);

  if (loading) return <Skeleton.ListPageSkeleton />;
  if (error) return <ErrorBanner message={error.message} onRetry={refetch} />;

  const columns: DataTableColumn<Project>[] = [
    { key: 'name', label: t('pages:projects.name') },
    { key: 'code', label: t('pages:projects.code'), render: (r) => <code className={styles.codeCell}>{r.code}</code> },
    { key: 'organization', label: t('pages:projects.organization'), render: (r) => r.organization?.name ?? '—' },
    { key: '_count', label: t('pages:projects.colVirtualKeys'), render: (r) => String(r._count?.virtualKeys ?? 0) },
    { key: 'status', label: t('pages:projects.status'), render: (r) => <Badge variant={statusToVariant(r.status)}>{r.status}</Badge> },
    {
      key: 'actions', label: '', render: (r) => (
        <RowActions>
          <RowActionIconButton
            label={t('common:view', 'View')}
            onAction={() => navigate(`/iam/projects/${r.id}`)}
          >
            <OpenActionIcon />
          </RowActionIconButton>
          {canDelete && (() => {
            const vkCount = r._count?.virtualKeys ?? 0;
            const canDel = vkCount === 0;
            return (
              <RowActionIconButton
                label={canDel ? t('pages:projects.deleteTitle') : t('pages:projects.cannotDeleteTitle', { count: vkCount })}
                tone="danger"
                disabled={!canDel}
                onAction={() => setDeleting(r)}
              >
                <DeleteActionIcon />
              </RowActionIconButton>
            );
          })()}
        </RowActions>
      ),
    },
  ];

  return (
    <Stack gap="md">
      <PageHeader
        title={t('pages:projects.title')}
        subtitle={t('pages:projects.subtitle')}
        action={
          canCreate ? (
            <Button onClick={() => navigate('/iam/projects/new')}>
              {t('pages:projects.createProject')}
            </Button>
          ) : undefined
        }
      />

      <ListFilterToolbar
        variant="boxed"
        searchWidth={420}
        hideClearButton
        searchPlaceholder={t('pages:projects.searchPlaceholder')}
        searchValue={search}
        onSearchChange={onSearchChange}
      >
        <select aria-label={t('pages:projects.filterByStatus')} value={statusFilter} onChange={onStatusFilterChange} className={styles.filterSelect}>
          <option value="">{t('pages:projects.allStatuses')}</option>
          <option value="active">{t('pages:projects.active')}</option>
          <option value="archived">{t('pages:projects.archived')}</option>
        </select>
      </ListFilterToolbar>

      <div className={styles.tableSection}>
        <div className={styles.resultMeta}>
          {total === 0
            ? t('pages:projects.noProjectsMatch')
            : t('pages:projects.showingProjects', { count: projects.length, total: total.toLocaleString() })}
        </div>
        <Card padding="none">
          <DataTable<Project>
            hideSearch
            frameless
            pageSize={pageLimit}
            onRowClick={(row) => navigate(`/iam/projects/${row.id}`)}
            columns={columns}
            data={projects}
            emptyMessage={t('pages:projects.noProjectsFound')}
          />
        </Card>
      </div>

      <ListPagination offset={offset} limit={pageLimit} total={total} onOffsetChange={setOffset} onLimitChange={setPageLimit} />

      <AlertDialog
        open={!!deleting}
        onOpenChange={(open) => { if (!open) setDeleting(null); }}
        title={t('pages:projects.deleteProject')}
        description={t('pages:projects.deleteConfirm', { name: deleting?.name ?? '', code: deleting?.code ?? '' })}
        confirmLabel={t('pages:projects.delete')}
        onConfirm={() => { if (deleting) deleteProject(deleting.id); }}
        variant="danger"
      />

    </Stack>
  );
}
