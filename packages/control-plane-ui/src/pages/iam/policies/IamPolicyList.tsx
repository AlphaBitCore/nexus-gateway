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
} from '@/components/ui';
import type { DataTableColumn } from '@/components/ui';
import type { IamPolicy } from '../../../api/types';
import styles from '../_shared/Iam.module.css';

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
        <span className={r.type === 'managed' ? styles.typeBadgeManaged : styles.typeBadgeCustom}>
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
      key: 'actions', label: t('pages:iam.actions'),
      render: (r) => (
        <Stack direction="horizontal" gap="xs" onClick={e => e.stopPropagation()}>
          <Button
            variant="secondary"
            size="sm"
            onClick={(e) => { e.stopPropagation(); navigate(`/iam/policies/${r.id}/edit`); }}
            disabled={r.type === 'managed'}
          >
            {t('common:edit')}
          </Button>
          {r.type !== 'managed' && (
            <Button
              variant="danger"
              size="sm"
              onClick={(e) => { e.stopPropagation(); setDeleting(r); }}
            >
              {t('common:delete')}
            </Button>
          )}
        </Stack>
      ),
    },
  ];

  return (
    <Stack gap="lg">
      <PageHeader
        title={t('pages:iam.policies')}
        subtitle={t('pages:iam.policiesSubtitle')}
        action={
          <Button onClick={() => navigate('/iam/policies/new')}>{t('pages:iam.createPolicy')}</Button>
        }
      />

      <ListFilterToolbar
        searchPlaceholder={t('pages:iam.searchPoliciesPlaceholder')}
        searchValue={search}
        onSearchChange={onSearchChange}
        meta={
          total === 0
            ? t('pages:iam.noPoliciesMatch')
            : t('pages:iam.showingPolicies', { count: rows.length, total: total.toLocaleString() })
        }
      >
        <select aria-label={t('pages:iam.filterByType')} value={typeFilter} onChange={onTypeFilterChange} className={styles.filterSelect}>
          <option value="">{t('pages:iam.allTypes')}</option>
          <option value="managed">{t('pages:iam.managed')}</option>
          <option value="custom">{t('pages:iam.custom')}</option>
        </select>
        <select aria-label={t('pages:iam.filterByState')} value={enabledFilter} onChange={onEnabledFilterChange} className={styles.filterSelect}>
          <option value="">{t('pages:iam.allStates')}</option>
          <option value="enabled">{t('pages:iam.enabledOnly')}</option>
          <option value="disabled">{t('pages:iam.disabledOnly')}</option>
        </select>
      </ListFilterToolbar>

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
