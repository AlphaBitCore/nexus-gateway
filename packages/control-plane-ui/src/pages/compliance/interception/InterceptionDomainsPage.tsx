/**
 * InterceptionDomainsPage — admin CRUD over
 * /api/admin/interception-domains. Follows the Hooks / Policies list
 * pattern: PageHeader + filter toolbar + paginated DataTable + confirm
 * dialog for delete + shared form dialog for create.
 *
 * Edits happen on the detail page (:id) — the list row "Edit" action
 * routes there rather than popping a modal in-place, because the detail
 * page is where nested paths are managed.
 */
import { useState, useCallback } from 'react';
import { useTranslation } from 'react-i18next';
import { useNavigate } from 'react-router-dom';

import { useApi } from '@/hooks/useApi';
import { useMutation } from '@/hooks/useMutation';
import { useDebouncedValue } from '@/hooks/useDebouncedValue';
import { interceptionDomainApi } from '@/api/services';
import type {
  InterceptionDomain,
  InterceptionDomainCreatePayload,
  InterceptionDomainListResponse,
  InterceptionDomainUpdatePayload,
} from '@/api/services';
import {
  AlertDialog,
  Button,
  Card,
  DataTable,
  DEFAULT_ADMIN_LIST_PAGE_SIZE,
  ErrorBanner,
  Input,
  ListEnabledSwitchCell,
  ListPagination,
  OpenActionIcon,
  PageHeader,
  RowActions,
  RowActionIconButton,
  RowDeleteAction,
  Skeleton,
  Stack,
  type AdminListPageSize,
  type DataTableColumn,
} from '@/components/ui';
import { InterceptionDomainForm } from './InterceptionDomainForm';
import styles from './InterceptionDomainsPage.module.css';

export function InterceptionDomainsPage() {
  const { t } = useTranslation();
  const navigate = useNavigate();

  const [search, setSearch] = useState('');
  const debouncedSearch = useDebouncedValue(search, 300);
  const [enabledFilter, setEnabledFilter] = useState<'' | 'enabled' | 'disabled'>('');
  const [offset, setOffset] = useState(0);
  const [pageLimit, setPageLimit] = useState<AdminListPageSize>(
    DEFAULT_ADMIN_LIST_PAGE_SIZE,
  );
  const [showCreate, setShowCreate] = useState(false);
  const [deleting, setDeleting] = useState<InterceptionDomain | null>(null);

  const { data, loading, error, refetch } = useApi<InterceptionDomainListResponse>(
    () =>
      interceptionDomainApi.list({
        search: debouncedSearch.trim() || undefined,
        enabled:
          enabledFilter === 'enabled'
            ? 'true'
            : enabledFilter === 'disabled'
              ? 'false'
              : undefined,
        limit: pageLimit,
        offset,
      }),
    [
      'admin',
      'interception-domains',
      'list',
      debouncedSearch,
      enabledFilter,
      offset,
      pageLimit,
    ],
  );

  const { mutate: createDomain } = useMutation(
    (payload: InterceptionDomainCreatePayload) =>
      interceptionDomainApi.create(payload),
    {
      invalidateQueries: [['api', 'admin', 'interception-domains']],
      successMessage: 'Interception domain created',
    },
  );

  const { mutate: deleteDomain } = useMutation(
    (id: string) => interceptionDomainApi.delete(id),
    {
      invalidateQueries: [['api', 'admin', 'interception-domains']],
      successMessage: 'Interception domain deleted',
      onSuccess: () => setDeleting(null),
    },
  );

  const { mutate: patchDomainEnabled, loading: togglingDomainEnabled } = useMutation(
    ({ id, enabled }: { id: string; enabled: boolean }) =>
      interceptionDomainApi.update(id, { enabled } satisfies InterceptionDomainUpdatePayload),
    {
      invalidateQueries: [['api', 'admin', 'interception-domains']],
      successMessage: t('pages:interceptionDomains.updateSuccess', 'Interception domain updated'),
    },
  );

  const onSearchChange = useCallback((v: string) => {
    setSearch(v);
    setOffset(0);
  }, []);

  const onEnabledFilterChange = useCallback(
    (e: React.ChangeEvent<HTMLSelectElement>) => {
      setEnabledFilter(e.target.value as '' | 'enabled' | 'disabled');
      setOffset(0);
    },
    [],
  );

  if (loading && !data) return <Skeleton.ListPageSkeleton />;
  if (error) return <ErrorBanner message={error.message} onRetry={refetch} />;

  const rows = data?.data ?? [];
  const total = data?.total ?? 0;

  const columns: DataTableColumn<InterceptionDomain>[] = [
    { key: 'name', label: t('pages:interceptionDomains.name', 'Name') },
    {
      key: 'hostPattern',
      label: t('pages:interceptionDomains.hostPattern', 'Host pattern'),
      render: (r) => <code>{r.hostPattern}</code>,
    },
    {
      key: 'hostMatchType',
      label: t('pages:interceptionDomains.hostMatchType', 'Match'),
      render: (r) =>
        t(`pages:interceptionDomains.enums.${r.hostMatchType}`, r.hostMatchType),
    },
    {
      key: 'adapterId',
      label: t('pages:interceptionDomains.adapterId', 'Adapter'),
      render: (r) => <code>{r.adapterId}</code>,
    },
    { key: 'priority', label: t('pages:interceptionDomains.priority', 'Priority') },
    {
      key: 'enabled',
      label: t('pages:interceptionDomains.enabled', 'Enabled'),
      render: (r) => (
        <ListEnabledSwitchCell
          enabled={r.enabled}
          canToggle
          toggleDisabled={togglingDomainEnabled}
          ariaLabel={t('pages:interceptionDomains.toggleDomainEnabledAria', {
            name: r.name,
          })}
          onToggle={(enabled) => {
            void patchDomainEnabled({ id: r.id, enabled });
          }}
        />
      ),
    },
    {
      key: 'pathsCount',
      label: t('pages:interceptionDomains.pathsCount', 'Paths'),
      render: (r) => r.paths?.length ?? 0,
    },
    {
      key: 'updatedAt',
      label: t('pages:interceptionDomains.updatedAt', 'Updated'),
      render: (r) =>
        r.updatedAt ? new Date(r.updatedAt).toLocaleString() : '-',
    },
    {
      key: 'actions',
      label: '',
      sortable: false,
      render: (r) => (
        <RowActions>
          <RowActionIconButton label={t('common:edit', 'Edit')} onAction={() => navigate(`/compliance/interception-domains/${r.id}`)}>
            <OpenActionIcon />
          </RowActionIconButton>
          <RowDeleteAction label={t('common:delete', 'Delete')} onAction={() => setDeleting(r)} />
        </RowActions>
      ),
    },
  ];

  return (
    <Stack gap="lg">
      <div className={styles.pageHeader}>
        <PageHeader
          title={t('pages:interceptionDomains.title', 'Interception Domains')}
          subtitle={t(
            'pages:interceptionDomains.subtitle',
            'Hosts + path rules the compliance proxy intercepts and passes through the traffic adapter pipeline.',
          )}
          subtitleClassName={styles.headerSubtitle}
          action={
            <Button variant="primary" onClick={() => setShowCreate(true)}>
              {t('pages:interceptionDomains.create', 'Create')}
            </Button>
          }
        />
      </div>

      <div className={styles.listSection}>
        <div className={styles.filterBar}>
          <div className={styles.searchBox}>
            <span className={styles.searchIcon} aria-hidden />
            <Input
              type="text"
              className={styles.searchInput}
              placeholder={t(
                'pages:interceptionDomains.searchPlaceholder',
                'Search name, host pattern, adapter…',
              )}
              value={search}
              onChange={(event) => onSearchChange(event.target.value)}
            />
            {search.trim().length > 0 && (
              <button
                type="button"
                onClick={() => onSearchChange('')}
                className={styles.clearSearchButton}
                aria-label={t('common:clear')}
                title={t('common:clear')}
              >
                <span aria-hidden="true" />
              </button>
            )}
          </div>
          <select
            className={styles.filterSelect}
            aria-label={t('pages:interceptionDomains.filterByStatus', 'Filter by status')}
            value={enabledFilter}
            onChange={onEnabledFilterChange}
          >
            <option value="">{t('pages:interceptionDomains.filterAll', 'All')}</option>
            <option value="enabled">
              {t('pages:interceptionDomains.filterEnabled', 'Enabled only')}
            </option>
            <option value="disabled">
              {t('pages:interceptionDomains.filterDisabled', 'Disabled only')}
            </option>
          </select>
        </div>

        <p className={styles.listMeta}>
          {t(
            'pages:interceptionDomains.pageMeta',
            'Showing {{count}} of {{total}}',
            { count: rows.length, total },
          )}
        </p>

        <Card padding="none">
          <DataTable
            hideSearch
            frameless
            pageSize={pageLimit}
            columns={columns}
            data={rows}
            emptyMessage={t(
              'pages:interceptionDomains.noDomains',
              'No interception domains configured',
            )}
            onRowClick={(r) => navigate(`/compliance/interception-domains/${r.id}`)}
          />
        </Card>
      </div>

      <ListPagination
        offset={offset}
        limit={pageLimit}
        total={total}
        onOffsetChange={setOffset}
        onLimitChange={setPageLimit}
      />

      <InterceptionDomainForm
        open={showCreate}
        mode="create"
        initial={null}
        onClose={() => setShowCreate(false)}
        onSubmit={async (payload) => {
          await createDomain(payload as InterceptionDomainCreatePayload);
        }}
      />

      <AlertDialog
        open={deleting !== null}
        onOpenChange={(open) => {
          if (!open) setDeleting(null);
        }}
        title={t('pages:interceptionDomains.deleteTitle', 'Delete interception domain?')}
        description={
          deleting
            ? t(
                'pages:interceptionDomains.confirmDeleteDomain',
                'Delete domain "{{name}}" and all {{n}} of its paths? This cannot be undone.',
                {
                  name: deleting.name,
                  n: deleting.paths?.length ?? 0,
                },
              )
            : ''
        }
        confirmLabel={t('common:delete', 'Delete')}
        cancelLabel={t('common:cancel', 'Cancel')}
        onConfirm={() => {
          if (deleting) void deleteDomain(deleting.id);
        }}
        variant="danger"
      />
    </Stack>
  );
}
