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
  const [tip, setTip] = useState<{ text: string; x: number; y: number } | null>(null);
  const canCreate = usePermission('project:create');
  const canDelete = usePermission('project:delete');

  const showTip = useCallback((text: string, e: React.MouseEvent) => {
    const rect = (e.currentTarget as HTMLElement).getBoundingClientRect();
    setTip({ text, x: rect.left, y: rect.top - 6 });
    setTimeout(() => setTip(null), 3000);
  }, []);

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
        <Stack direction="horizontal" gap="xs" onClick={e => e.stopPropagation()}>
          <Button
            variant="secondary"
            size="sm"
            onClick={() => navigate(`/iam/projects/${r.id}`)}
          >
            {t('pages:projects.edit')}
          </Button>
          {canDelete && (() => {
            const vkCount = r._count?.virtualKeys ?? 0;
            const canDel = vkCount === 0;
            return (
              <Button
                variant="danger"
                size="sm"
                onClick={(e) => {
                  if (canDel) { setDeleting(r); }
                  else { showTip(t('pages:projects.cannotDeleteTip', { count: vkCount }), e); }
                }}
                title={canDel ? t('pages:projects.deleteTitle') : t('pages:projects.cannotDeleteTitle', { count: vkCount })}
                className={canDel ? undefined : styles.disabledDelete}
              >
                {t('pages:projects.delete')}
              </Button>
            );
          })()}
        </Stack>
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
        searchPlaceholder={t('pages:projects.searchPlaceholder')}
        searchValue={search}
        onSearchChange={onSearchChange}
        meta={
          total === 0
            ? t('pages:projects.noProjectsMatch')
            : t('pages:projects.showingProjects', { count: projects.length, total: total.toLocaleString() })
        }
      >
        <select aria-label={t('pages:projects.filterByStatus')} value={statusFilter} onChange={onStatusFilterChange} className={styles.filterSelect}>
          <option value="">{t('pages:projects.allStatuses')}</option>
          <option value="active">{t('pages:projects.active')}</option>
          <option value="archived">{t('pages:projects.archived')}</option>
        </select>
      </ListFilterToolbar>

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

      {tip && (
        <div className={styles.tip} style={{ left: tip.x, top: tip.y }}>
          {tip.text}
        </div>
      )}
    </Stack>
  );
}
