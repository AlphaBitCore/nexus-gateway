import { useState, useCallback } from 'react';
import { useNavigate } from 'react-router-dom';
import { useTranslation } from 'react-i18next';
import { iamApi } from '@/api/services';
import { useApi } from '../../../hooks/useApi';
import { useDebouncedValue } from '../../../hooks/useDebouncedValue';
import { useMutation } from '../../../hooks/useMutation';
import {
  PageHeader, DataTable, ListFilterToolbar, AlertDialog,
  Skeleton, ErrorBanner, Button, Stack, Card,
  ListPagination, DEFAULT_ADMIN_LIST_PAGE_SIZE, type AdminListPageSize,
  RowActions, RowActionIconButton, OpenActionIcon, DeleteActionIcon,
} from '@/components/ui';
import { IamRoleForm } from './IamRoleForm';
import type { IamGroup } from '../../../api/types';
import styles from './IamRoleList.module.css';

export function IamRoleList() {
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
    ['admin', 'iam', 'roles', 'list', debouncedSearch, offset, pageLimit],
  );
  const [showForm, setShowForm] = useState(false);
  const [deleting, setDeleting] = useState<IamGroup | null>(null);

  const { mutate: deleteRole } = useMutation(
    (id: string) => iamApi.deleteGroup(id),
    {
      invalidateQueries: [['api', 'admin', 'iam']],
      onSuccess: () => { setDeleting(null); },
      successMessage: 'Role deleted',
    },
  );

  const rows = data?.data ?? [];
  const total = data?.total ?? 0;

  const onSearchChange = useCallback((v: string) => {
    setSearch(v);
    setOffset(0);
  }, []);

  if (loading) return <Skeleton.ListPageSkeleton />;
  if (error) return <ErrorBanner message={error.message} onRetry={refetch} />;

  return (
    <Stack gap="md">
      <PageHeader
        title={t('pages:iam.roles')}
        subtitle={t('pages:iam.rolesSubtitle')}
        action={
          <Button onClick={() => setShowForm(true)}>{t('pages:iam.createRole')}</Button>
        }
      />

      <ListFilterToolbar
        variant="boxed"
        className={styles.filterToolbar}
        searchWidth={420}
        hideClearButton
        searchPlaceholder={t('pages:iam.searchRolesPlaceholder')}
        searchValue={search}
        onSearchChange={onSearchChange}
      />

      <div className={styles.tableSection}>
        <div className={styles.resultMeta}>
          {total === 0
            ? t('pages:iam.noRolesMatch')
            : t('pages:iam.showingRoles', { count: rows.length, total: total.toLocaleString() })}
        </div>
        <Card padding="none">
          <DataTable
            hideSearch
            frameless
            pageSize={pageLimit}
            columns={[
              { key: 'name', label: t('pages:iam.name') },
              { key: 'description', label: t('pages:iam.description'), render: (r) => r.description || '\u2014' },
              {
                key: 'actions',
                label: t('common:actions', 'Actions'),
                render: (r) => (
                  <RowActions>
                    <RowActionIconButton
                      label={t('common:view', 'View')}
                      onAction={() => navigate(`/iam/roles/${r.id}`)}
                    >
                      <OpenActionIcon />
                    </RowActionIconButton>
                    <RowActionIconButton
                      label={t('common:delete')}
                      tone="danger"
                      onAction={() => setDeleting(r)}
                    >
                      <DeleteActionIcon />
                    </RowActionIconButton>
                  </RowActions>
                ),
              },
            ]}
            data={rows}
            onRowClick={(role) => navigate(`/iam/roles/${role.id}`)}
            emptyMessage={t('pages:iam.noRolesConfigured')}
          />
        </Card>
      </div>

      <ListPagination offset={offset} limit={pageLimit} total={total} onOffsetChange={setOffset} onLimitChange={setPageLimit} />

      {showForm && (
        <IamRoleForm
          onClose={() => setShowForm(false)}
          onSaved={refetch}
        />
      )}

      <AlertDialog
        open={!!deleting}
        onOpenChange={(open) => { if (!open) setDeleting(null); }}
        title={t('pages:iam.deleteRole')}
        description={t('pages:iam.deleteRoleConfirm', { name: deleting?.name })}
        confirmLabel={t('common:delete')}
        onConfirm={() => { if (deleting) deleteRole(deleting.id); }}
        variant="danger"
      />
    </Stack>
  );
}
