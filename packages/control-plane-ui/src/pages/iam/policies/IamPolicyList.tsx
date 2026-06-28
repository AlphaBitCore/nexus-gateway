import { useState, useCallback } from 'react';
import { useNavigate } from 'react-router-dom';
import { useTranslation } from 'react-i18next';
import { iamApi } from '@/api/services';
import { useApi } from '../../../hooks/useApi';
import { useDebouncedValue } from '../../../hooks/useDebouncedValue';
import { useMutation } from '../../../hooks/useMutation';
import {
  PageHeader, DataTable, ListFilterToolbar, Badge, statusToVariant,
  AlertDialog, Skeleton, ErrorBanner, Button, Stack, Card,
  ListPagination, DEFAULT_ADMIN_LIST_PAGE_SIZE, type AdminListPageSize,
  RowActions, RowActionIconButton, OpenActionIcon, EditActionIcon, DeleteActionIcon,
} from '@/components/ui';
import type { DataTableColumn } from '@/components/ui';
import type { IamPolicy } from '../../../api/types';
import iamStyles from '../_shared/Iam.module.css';
import styles from './IamPolicyList.module.css';

export function IamPolicyList() {
  const { t } = useTranslation();
  const navigate = useNavigate();
  const [search, setSearch] = useState('');
  const debouncedSearch = useDebouncedValue(search, 300);
  const [typeFilter, setTypeFilter] = useState('');
  const [enabledFilter, setEnabledFilter] = useState('');
  const [offset, setOffset] = useState(0);
  const [pageLimit, setPageLimit] = useState<AdminListPageSize>(DEFAULT_ADMIN_LIST_PAGE_SIZE);
  const { data, loading, error, refetch } = useApi<{ data: IamPolicy[]; total: number }>(
    () => {
      const params: Record<string, string> = {
        limit: String(pageLimit),
        offset: String(offset),
      };
      const q = debouncedSearch.trim();
      if (q) params.q = q;
      if (typeFilter) params.type = typeFilter;
      if (enabledFilter === 'enabled') params.enabled = 'true';
      if (enabledFilter === 'disabled') params.enabled = 'false';
      return iamApi.listPolicies(params);
    },
    ['admin', 'iam', 'policies', 'list', 'page', debouncedSearch, typeFilter, enabledFilter, offset, pageLimit],
  );
  const [deleting, setDeleting] = useState<IamPolicy | null>(null);

  const { mutate: deletePolicy } = useMutation(
    (id: string) => iamApi.deletePolicy(id),
    {
      invalidateQueries: [['api', 'admin', 'iam']],
      onSuccess: () => { setDeleting(null); },
      successMessage: 'IAM policy deleted',
    },
  );

  const rows = data?.data ?? [];
  const total = data?.total ?? 0;

  const onSearchChange = useCallback((v: string) => {
    setSearch(v);
    setOffset(0);
  }, []);

  const onTypeFilterChange = useCallback((e: React.ChangeEvent<HTMLSelectElement>) => {
    setTypeFilter(e.target.value);
    setOffset(0);
  }, []);

  const onEnabledFilterChange = useCallback((e: React.ChangeEvent<HTMLSelectElement>) => {
    setEnabledFilter(e.target.value);
    setOffset(0);
  }, []);

  if (loading) return <Skeleton.ListPageSkeleton />;
  if (error) return <ErrorBanner message={error.message} onRetry={refetch} />;

  const columns: DataTableColumn<IamPolicy>[] = [
    { key: 'name', label: t('pages:iam.name') },
    {
      key: 'type', label: t('pages:iam.type'),
      render: (r) => (
        <span className={r.type === 'managed' ? iamStyles.typeBadgeManaged : iamStyles.typeBadgeCustom}>
          {r.type}
        </span>
      ),
    },
    { key: 'description', label: t('pages:iam.description') },
    {
      key: 'statements', label: t('pages:iam.statements'),
      render: (r) => <span>{r.document?.Statement?.length ?? 0}</span>,
    },
    {
      key: 'enabled', label: t('pages:iam.status'),
      render: (r) => <Badge variant={statusToVariant(r.enabled ? 'enabled' : 'disabled')}>{r.enabled ? t('common:enabled') : t('common:disabled')}</Badge>,
    },
    {
      key: 'actions', label: t('common:actions', 'Actions'),
      render: (r) => (
        <RowActions>
          <RowActionIconButton
            label={t('common:view', 'View')}
            onAction={() => navigate(`/iam/policies/${r.id}`)}
          >
            <OpenActionIcon />
          </RowActionIconButton>
          {r.type !== 'managed' && (
            <>
              <RowActionIconButton
                label={t('common:edit')}
                onAction={() => navigate(`/iam/policies/${r.id}/edit`)}
              >
                <EditActionIcon />
              </RowActionIconButton>
              <RowActionIconButton
                label={t('common:delete')}
                tone="danger"
                onAction={() => setDeleting(r)}
              >
                <DeleteActionIcon />
              </RowActionIconButton>
            </>
          )}
        </RowActions>
      ),
    },
  ];

  return (
    <Stack gap="md">
      <PageHeader
        title={t('pages:iam.policies')}
        subtitle={t('pages:iam.policiesSubtitle')}
        action={
          <Button onClick={() => navigate('/iam/policies/new')}>{t('pages:iam.createPolicy')}</Button>
        }
      />

      <ListFilterToolbar
        variant="boxed"
        className={styles.filterToolbar}
        searchWidth={420}
        hideClearButton
        searchPlaceholder={t('pages:iam.searchPoliciesPlaceholder')}
        searchValue={search}
        onSearchChange={onSearchChange}
      >
        <select aria-label={t('pages:iam.filterByType')} value={typeFilter} onChange={onTypeFilterChange} className={iamStyles.filterSelect}>
          <option value="">{t('pages:iam.allTypes')}</option>
          <option value="managed">{t('pages:iam.managed')}</option>
          <option value="custom">{t('pages:iam.custom')}</option>
        </select>
        <select aria-label={t('pages:iam.filterByState')} value={enabledFilter} onChange={onEnabledFilterChange} className={iamStyles.filterSelect}>
          <option value="">{t('pages:iam.allStates')}</option>
          <option value="enabled">{t('pages:iam.enabledOnly')}</option>
          <option value="disabled">{t('pages:iam.disabledOnly')}</option>
        </select>
      </ListFilterToolbar>

      <div className={styles.tableSection}>
        <div className={styles.resultMeta}>
          {total === 0
            ? t('pages:iam.noPoliciesMatch')
            : t('pages:iam.showingPolicies', { count: rows.length, total: total.toLocaleString() })}
        </div>
        <Card padding="none">
          <DataTable
            hideSearch
            frameless
            pageSize={pageLimit}
            onRowClick={(row) => navigate(`/iam/policies/${row.id}`)}
            columns={columns}
            data={rows}
            emptyMessage={t('pages:iam.noPoliciesConfigured')}
          />
        </Card>
      </div>

      <ListPagination offset={offset} limit={pageLimit} total={total} onOffsetChange={setOffset} onLimitChange={setPageLimit} />

      <AlertDialog
        open={!!deleting}
        onOpenChange={(open) => { if (!open) setDeleting(null); }}
        title={t('pages:iam.deleteIamPolicy')}
        description={t('pages:iam.deletePolicyConfirm', { name: deleting?.name })}
        confirmLabel={t('common:delete')}
        onConfirm={() => { if (deleting) deletePolicy(deleting.id); }}
        variant="danger"
      />
    </Stack>
  );
}
