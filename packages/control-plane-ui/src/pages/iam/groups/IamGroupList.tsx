import { useState, useCallback } from 'react';
import { useNavigate } from 'react-router-dom';
import { useTranslation } from 'react-i18next';
import { iamApi } from '@/api/services';
import { useApi } from '../../../hooks/useApi';
import { useDebouncedValue } from '../../../hooks/useDebouncedValue';
import { useMutation } from '../../../hooks/useMutation';
import {
  PageHeader, DataTable, ListFilterToolbar, AlertDialog, Skeleton,
  ErrorBanner, Button, Stack, Card, ListPagination,
  DEFAULT_ADMIN_LIST_PAGE_SIZE, type AdminListPageSize,
} from '@/components/ui';
import { IamGroupForm } from './IamGroupForm';
import type { IamGroup } from '../../../api/types';
import type { DataTableColumn } from '@/components/ui';

export function IamGroupList() {
  const { t } = useTranslation();
  const navigate = useNavigate();
  const [search, setSearch] = useState('');
  const debouncedSearch = useDebouncedValue(search, 300);
  const [offset, setOffset] = useState(0);
  const [pageLimit, setPageLimit] = useState<AdminListPageSize>(DEFAULT_ADMIN_LIST_PAGE_SIZE);
  const { data, loading, error, refetch } = useApi<{ data: IamGroup[]; total: number }>(
    () => {
      const params: Record<string, string> = {
        limit: String(pageLimit),
        offset: String(offset),
      };
      const q = debouncedSearch.trim();
      if (q) params.q = q;
      return iamApi.listGroups(params);
    },
    ['admin', 'iam', 'groups', 'list', debouncedSearch, offset, pageLimit],
  );
  const [showForm, setShowForm] = useState(false);
  const [editing, setEditing] = useState<IamGroup | null>(null);
  const [deleting, setDeleting] = useState<IamGroup | null>(null);

  const rows = data?.data ?? [];
  const total = data?.total ?? 0;

  const onSearchChange = useCallback((v: string) => {
    setSearch(v);
    setOffset(0);
  }, []);

  const { mutate: deleteGroup } = useMutation(
    (id: string) => iamApi.deleteGroup(id),
    {
      invalidateQueries: [['api', 'admin', 'iam']],
      onSuccess: () => { setDeleting(null); },
      successMessage: 'Group deleted',
    },
  );

  if (loading) return <Skeleton.ListPageSkeleton />;
  if (error) return <ErrorBanner message={error.message} onRetry={refetch} />;

  const columns: DataTableColumn<IamGroup>[] = [
    { key: 'name', label: t('pages:iam.name') },
    { key: 'description', label: t('pages:iam.description'), render: (r) => r.description || '\u2014' },
    {
      key: 'actions',
      label: t('pages:iam.actions'),
      render: (r) => (
        <Stack direction="horizontal" gap="xs" onClick={e => e.stopPropagation()}>
          <Button
            variant="secondary"
            size="sm"
            onClick={(e) => { e.stopPropagation(); setEditing(r); }}
          >
            {t('common:edit')}
          </Button>
          <Button
            variant="danger"
            size="sm"
            onClick={(e) => { e.stopPropagation(); setDeleting(r); }}
          >
            {t('common:delete')}
          </Button>
        </Stack>
      ),
    },
  ];

  return (
    <Stack gap="lg">
      <PageHeader
        title={t('pages:iam.groups')}
        subtitle={t('pages:iam.groupsSubtitle')}
        action={
          <Button onClick={() => setShowForm(true)}>{t('pages:iam.createGroup')}</Button>
        }
      />

      <ListFilterToolbar
        searchPlaceholder={t('pages:iam.searchGroupsPlaceholder')}
        searchValue={search}
        onSearchChange={onSearchChange}
        meta={
          total === 0
            ? t('pages:iam.noGroupsMatch')
            : t('pages:iam.showingGroups', { count: rows.length, total: total.toLocaleString() })
        }
      />

      <Card padding="none">
        <DataTable
          hideSearch
          frameless
          pageSize={pageLimit}
          columns={columns}
          data={rows}
          onRowClick={(group) => navigate(`/iam/groups/${group.id}`)}
          emptyMessage={t('pages:iam.noGroupsConfigured')}
        />
      </Card>

      <ListPagination offset={offset} limit={pageLimit} total={total} onOffsetChange={setOffset} onLimitChange={setPageLimit} />

      {(showForm || editing) && (
        <IamGroupForm
          group={editing ?? undefined}
          onClose={() => { setShowForm(false); setEditing(null); }}
          onSaved={refetch}
        />
      )}

      <AlertDialog
        open={!!deleting}
        onOpenChange={(open) => { if (!open) setDeleting(null); }}
        title={t('pages:iam.deleteGroup')}
        description={t('pages:iam.deleteGroupConfirm', { name: deleting?.name })}
        confirmLabel={t('common:delete')}
        onConfirm={() => { if (deleting) deleteGroup(deleting.id); }}
        variant="danger"
      />
    </Stack>
  );
}
