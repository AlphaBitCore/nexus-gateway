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
  Skeleton,
  ErrorBanner,
  Button,
  Stack,
  Card,
  Input,
  ListPagination,
  AlertDialog,
  RowActions,
  RowActionIconButton,
  RowDeleteAction,
  EditActionIcon,
  DEFAULT_ADMIN_LIST_PAGE_SIZE,
  type AdminListPageSize,
  type DataTableColumn,
} from '@/components/ui';
import { DeviceGroupForm } from './DeviceGroupForm';
import styles from './DeviceGroupListPage.module.css';

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
      label: t('common:actions', 'Actions'),
      sortable: false,
      render: (r) =>
        canUpdate || canDelete ? (
          <RowActions>
            {canUpdate && (
              <RowActionIconButton
                label={t('common:edit', 'Edit')}
                onAction={() => {
                  setEditing(r);
                  setShowEditForm(true);
                }}
              >
                <EditActionIcon />
              </RowActionIconButton>
            )}
            {canDelete && (
              <RowDeleteAction label={t('common:delete', 'Delete')} onAction={() => setDeleting(r)} />
            )}
          </RowActions>
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
      <div className={styles.listSection}>
        <div className={styles.filterBar}>
          <div className={styles.searchBox}>
            <span className={styles.searchIcon} aria-hidden />
            <Input
              className={styles.searchInput}
              placeholder={t('pages:deviceGroups.searchPlaceholder')}
              value={search}
              onChange={(event) => onSearchChange(event.target.value)}
            />
            {search && (
              <button
                type="button"
                className={styles.clearSearchButton}
                aria-label={t('common:clearSearch', 'Clear search')}
                title={t('common:clearSearch', 'Clear search')}
                onClick={() => onSearchChange('')}
              >
                <span aria-hidden />
              </button>
            )}
          </div>
        </div>
        <p className={styles.listMeta}>
          {t('pages:deviceGroups.pageMeta', { count: items.length, total })}
        </p>
        <Card padding="none">
          <DataTable
            columns={columns}
            data={items}
            onRowClick={(r) => navigate(`/devices/groups/${r.id}`)}
            hideSearch
            frameless
            emptyMessage={t('pages:deviceGroups.noGroups')}
          />
        </Card>
        <div className={styles.paginationWrap}>
          <ListPagination
            total={total}
            offset={offset}
            limit={pageLimit}
            onOffsetChange={setOffset}
            onLimitChange={setPageLimit}
          />
        </div>
      </div>

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
