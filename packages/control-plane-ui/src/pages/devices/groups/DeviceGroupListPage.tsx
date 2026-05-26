import { useState, useCallback } from 'react';
import { useNavigate } from 'react-router-dom';
import { useTranslation } from 'react-i18next';
import { useApi } from '@/hooks/useApi';
import { useDebouncedValue } from '@/hooks/useDebouncedValue';
import { usePermission } from '@/hooks/usePermission';
import { useMutation } from '@/hooks/useMutation';
import { deviceGroupsApi, type DeviceGroupListItem, type DeviceGroup } from '@/api/services';
import {
  PageHeader,
  DataTable,
  ListFilterToolbar,
  Skeleton,
  ErrorBanner,
  Button,
  Stack,
  Card,
  ListPagination,
  AlertDialog,
  DEFAULT_ADMIN_LIST_PAGE_SIZE,
  type AdminListPageSize,
  type DataTableColumn,
} from '@/components/ui';
import { DeviceGroupForm } from './DeviceGroupForm';

export function DeviceGroupListPage() {
  const { t } = useTranslation();
  const navigate = useNavigate();
  const canCreate = usePermission('device-groups:create');
  const canUpdate = usePermission('device-groups:update');
  const canDelete = usePermission('device-groups:delete');

  const [editing, setEditing] = useState<DeviceGroup | null>(null);
  const [showEditForm, setShowEditForm] = useState(false);
  const [deleting, setDeleting] = useState<DeviceGroupListItem | null>(null);

  const [search, setSearch] = useState('');
  const debouncedSearch = useDebouncedValue(search, 300);
  const [offset, setOffset] = useState(0);
  const [pageLimit, setPageLimit] = useState<AdminListPageSize>(DEFAULT_ADMIN_LIST_PAGE_SIZE);

  const { data, loading, error, refetch } = useApi<{ data: DeviceGroupListItem[]; total: number }>(
    () => {
      const params: Record<string, string> = { limit: String(pageLimit), offset: String(offset) };
      const q = debouncedSearch.trim();
      if (q) params.q = q;
      return deviceGroupsApi.list(params);
    },
    ['admin', 'device-groups', 'list', debouncedSearch, String(offset), String(pageLimit)],
  );

  const { mutate: deleteGroup } = useMutation(
    (groupId: string) => deviceGroupsApi.delete(groupId),
    {
      invalidateQueries: [['admin', 'device-groups']],
      successMessage: t('pages:deviceGroups.deleteSuccess'),
      onSuccess: () => {
        setDeleting(null);
        void refetch();
      },
    },
  );

  const onSearchChange = useCallback((v: string) => {
    setSearch(v);
    setOffset(0);
  }, []);

  if (loading && !data) return <Skeleton.ListPageSkeleton />;
  if (error) return <ErrorBanner message={error.message} onRetry={refetch} />;

  const items = data?.data ?? [];
  const total = data?.total ?? 0;

  const columns: DataTableColumn<DeviceGroupListItem>[] = [
    { key: 'name', label: t('pages:deviceGroups.name') },
    { key: 'description', label: t('pages:deviceGroups.description'), render: (r) => r.description ?? '\u2014' },
    { key: 'members', label: t('pages:deviceGroups.members'), render: (r) => r.memberCount ?? 0 },
    {
      key: 'createdAt',
      label: t('pages:deviceGroups.created'),
      render: (r) => new Date(r.createdAt).toLocaleDateString(),
    },
    {
      key: 'actions',
      label: '',
      sortable: false,
      render: (r) =>
        canUpdate || canDelete ? (
          <Stack direction="horizontal" gap="xs" onClick={(e) => e.stopPropagation()}>
            {canUpdate && (
              <Button
                size="sm"
                variant="secondary"
                onClick={() => {
                  setEditing(r);
                  setShowEditForm(true);
                }}
              >
                {t('common:edit')}
              </Button>
            )}
            {canDelete && (
              <Button size="sm" variant="danger" onClick={() => setDeleting(r)}>
                {t('common:delete')}
              </Button>
            )}
          </Stack>
        ) : null,
    },
  ];

  return (
    <Stack gap="lg">
      <PageHeader
        title={t('pages:deviceGroups.title')}
        subtitle={t('pages:deviceGroups.subtitle')}
        action={
          canCreate ? (
            <Button variant="primary" onClick={() => navigate('/devices/groups/new')}>
              {t('pages:deviceGroups.create')}
            </Button>
          ) : undefined
        }
      />
      <ListFilterToolbar
        searchPlaceholder={t('pages:deviceGroups.searchPlaceholder')}
        searchValue={search}
        onSearchChange={onSearchChange}
        meta={t('pages:deviceGroups.pageMeta', { count: items.length, total })}
      />
      <Card padding="none">
        <DataTable
          columns={columns}
          data={items}
          onRowClick={(r) => navigate(`/devices/groups/${r.id}`)}
          hideSearch
          frameless
          emptyMessage={t('pages:deviceGroups.noGroups')}
        />
        <ListPagination
          total={total}
          offset={offset}
          limit={pageLimit}
          onOffsetChange={setOffset}
          onLimitChange={setPageLimit}
        />
      </Card>

      <DeviceGroupForm
        open={showEditForm}
        group={editing}
        onClose={() => {
          setShowEditForm(false);
          setEditing(null);
        }}
        onSaved={() => {
          void refetch();
        }}
      />

      {deleting && (
        <AlertDialog
          open={!!deleting}
          onOpenChange={(open) => {
            if (!open) setDeleting(null);
          }}
          title={t('pages:deviceGroups.deleteTitle')}
          description={t('pages:deviceGroups.deleteDescription', {
            name: deleting.name,
            members: deleting.memberCount ?? 0,
          })}
          confirmLabel={t('common:delete')}
          cancelLabel={t('common:cancel')}
          variant="danger"
          onConfirm={() => {
            void deleteGroup(deleting.id);
          }}
        />
      )}
    </Stack>
  );
}
