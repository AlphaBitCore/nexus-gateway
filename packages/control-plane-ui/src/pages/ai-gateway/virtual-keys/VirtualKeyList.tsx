import { useState, useMemo, useCallback } from 'react';
import { useTranslation } from 'react-i18next';
import { useNavigate } from 'react-router-dom';
import { useApi } from '@/hooks/useApi';
import { virtualKeyApi, projectApi } from '@/api/services';
import { useDebouncedValue } from '@/hooks/useDebouncedValue';
import { useMutation } from '@/hooks/useMutation';
import { usePermission } from '@/hooks/usePermission';
import {
  PageHeader, DataTable, ListFilterToolbar, Badge,
  AlertDialog, Skeleton, ErrorBanner, Button, Stack, Card, Dialog,
  ListPagination, DEFAULT_ADMIN_LIST_PAGE_SIZE, type AdminListPageSize,
  ListEnabledSwitchCell,
  RowActions, RowActionIconButton, RowDeleteAction, RevokeActionIcon,
} from '@/components/ui';
import type { DataTableColumn } from '@/components/ui';
import type { Project, VirtualKey } from '@/api/types';
import { ADMIN_LIST_FULL_PAGE_PARAMS } from '@/constants/admin-api';
import styles from './VirtualKeyList.module.css';

/* -- VK status badge ------------------------------------------------------- */

function vkStatusVariant(status?: string): 'warning' | 'success' | 'default' | 'danger' {
  switch (status) {
    case 'pending': return 'warning';
    case 'active': return 'success';
    case 'expired': return 'default';
    case 'rejected':
    case 'revoked': return 'danger';
    default: return 'default';
  }
}

/* -- Main component -------------------------------------------------------- */

export function VirtualKeyListPage() {
  const { t } = useTranslation();
  const navigate = useNavigate();
  const [search, setSearch] = useState('');
  const debouncedSearch = useDebouncedValue(search, 300);
  const [projectFilter, setProjectFilter] = useState('');
  const [enabledFilter, setEnabledFilter] = useState('');
  const [offset, setOffset] = useState(0);
  const [pageLimit, setPageLimit] = useState<AdminListPageSize>(DEFAULT_ADMIN_LIST_PAGE_SIZE);
  const { data, loading, error, refetch } = useApi<{ data: VirtualKey[]; total: number }>(
    () => {
      const params: Record<string, string> = {
        limit: String(pageLimit),
        offset: String(offset),
      };
      const q = debouncedSearch.trim();
      if (q) params.q = q;
      if (projectFilter) params.projectId = projectFilter;
      if (enabledFilter === 'enabled') params.enabled = 'true';
      if (enabledFilter === 'disabled') params.enabled = 'false';
      params.vkType = 'application';
      return virtualKeyApi.list(params);
    },
    ['admin', 'virtual-keys', 'list', debouncedSearch, projectFilter, enabledFilter, offset, pageLimit],
  );
  const { data: projectsData } = useApi<{ data: Project[] }>(
    () => projectApi.list({ ...ADMIN_LIST_FULL_PAGE_PARAMS }),
    ['admin', 'projects', 'list', 'vk-list'],
  );
  const projectsMap = useMemo(() => {
    const map = new Map<string, Project>();
    for (const p of projectsData?.data ?? []) map.set(p.id, p);
    return map;
  }, [projectsData]);

  const [deleting, setDeleting] = useState<VirtualKey | null>(null);
  const [rejecting, setRejecting] = useState<VirtualKey | null>(null);
  const [rejectReason, setRejectReason] = useState('');
  const canCreate = usePermission('virtual-key:create');
  const canUpdate = usePermission('virtual-key:update');
  const canDelete = usePermission('virtual-key:delete');
  const canApprove = usePermission('virtualKey:approve');

  const { mutate: toggleKey, loading: togglingVkEnabled } = useMutation(
    (payload: { id: string; enabled: boolean }) => virtualKeyApi.update(payload.id, { enabled: payload.enabled }),
    { invalidateQueries: [['api', 'admin', 'virtual-keys']], successMessage: t('pages:virtualKeys.updated') },
  );

  const { mutate: deleteKey } = useMutation(
    (id: string) => virtualKeyApi.delete(id),
    {
      invalidateQueries: [['api', 'admin', 'virtual-keys']],
      onSuccess: () => { setDeleting(null); },
      successMessage: t('pages:virtualKeys.deleted'),
    },
  );

  const { mutate: approveKey } = useMutation(
    (id: string) => virtualKeyApi.approve(id),
    {
      onSuccess: () => refetch(),
      successMessage: t('pages:virtualKeys.approved'),
    },
  );

  const { mutate: rejectKey } = useMutation(
    (payload: { id: string; reason: string }) => virtualKeyApi.reject(payload.id, payload.reason),
    {
      onSuccess: () => { setRejecting(null); setRejectReason(''); refetch(); },
      successMessage: t('pages:virtualKeys.rejected'),
    },
  );

  const { mutate: revokeKey } = useMutation(
    (id: string) => virtualKeyApi.revoke(id),
    {
      onSuccess: () => refetch(),
      successMessage: t('pages:virtualKeys.revoked'),
    },
  );

  const rows = data?.data ?? [];
  const total = data?.total ?? 0;

  const onSearchChange = useCallback((v: string) => {
    setSearch(v);
    setOffset(0);
  }, []);

  const onProjectFilterChange = useCallback((e: React.ChangeEvent<HTMLSelectElement>) => {
    setProjectFilter(e.target.value);
    setOffset(0);
  }, []);

  const onEnabledFilterChange = useCallback((e: React.ChangeEvent<HTMLSelectElement>) => {
    setEnabledFilter(e.target.value);
    setOffset(0);
  }, []);

  if (loading) return <Skeleton.ListPageSkeleton />;
  if (error) return <ErrorBanner message={error.message} onRetry={refetch} />;

  const columns: DataTableColumn<VirtualKey>[] = [
    { key: 'name', label: t('pages:virtualKeys.name') },
    {
      key: 'projectId',
      label: t('pages:virtualKeys.project'),
      render: (r) => {
        const p = r.projectId ? projectsMap.get(r.projectId) : null;
        if (!p) return <span>—</span>;
        const orgName = p.organization?.name;
        return (
          <div className={styles.projectCell}>
            <div className={styles.projectName}>{p.name}</div>
            {orgName && <div className={styles.projectOrg}>{orgName}</div>}
          </div>
        );
      },
    },
    {
      key: 'vkStatus' as keyof VirtualKey,
      label: t('pages:virtualKeys.vkStatus'),
      render: (r) => {
        const status = (r as unknown as Record<string, unknown>).vkStatus as string | undefined;
        if (!status) return '-';
        return <Badge variant={vkStatusVariant(status)}>{status}</Badge>;
      },
    },
    {
      key: 'expiresAt' as keyof VirtualKey,
      label: t('pages:virtualKeys.expiresAt'),
      render: (r) => {
        const expiresAt = (r as unknown as Record<string, unknown>).expiresAt as string | undefined;
        if (!expiresAt) return <span>—</span>;
        const expired = new Date(expiresAt) < new Date();
        return (
          <span>
            {new Date(expiresAt).toLocaleDateString()}
            {' '}
            {expired ? <Badge variant="danger">{t('pages:credentials.expiresOverdue')}</Badge> : null}
          </span>
        );
      },
    },
    {
      key: 'enabled',
      label: t('pages:virtualKeys.status'),
      render: (r) => (
        <ListEnabledSwitchCell
          enabled={r.enabled}
          canToggle={canUpdate}
          toggleDisabled={togglingVkEnabled}
          ariaLabel={t('common:listToggleEnabledAria', { name: r.name })}
          onToggle={(enabled) => { void toggleKey({ id: r.id, enabled }); }}
        />
      ),
    },
    {
      key: 'actions',
      label: t('pages:virtualKeys.actions'),
      render: (r) => {
        const vkStatus = (r as unknown as Record<string, unknown>).vkStatus as string | undefined;
        const vkType = (r as unknown as Record<string, unknown>).vkType as string | undefined;
        const isApplication = vkType === 'application';

        return (
          <RowActions>
            {/* Approve/Reject for pending application VKs */}
            {isApplication && vkStatus === 'pending' && canApprove && (
              <>
                <Button
                  variant="secondary"
                  size="sm"
                  onClick={(e) => { e.stopPropagation(); approveKey(r.id); }}
                >
                  {t('pages:virtualKeys.approve')}
                </Button>
                <Button
                  variant="danger"
                  size="sm"
                  onClick={(e) => { e.stopPropagation(); setRejecting(r); }}
                >
                  {t('pages:virtualKeys.reject')}
                </Button>
              </>
            )}
            {/* Revoke for active application VKs */}
            {isApplication && vkStatus === 'active' && canApprove && (
              <RowActionIconButton label={t('pages:virtualKeys.revoke')} tone="danger" onAction={() => revokeKey(r.id)}>
                <RevokeActionIcon />
              </RowActionIconButton>
            )}
            {canDelete && (
              <RowDeleteAction label={t('common:delete')} onAction={() => setDeleting(r)} />
            )}
          </RowActions>
        );
      },
    },
  ];

  return (
    <Stack gap="lg">
      <PageHeader
        title={t('pages:virtualKeys.title')}
        subtitle={t('pages:virtualKeys.listSubtitle')}
        action={
          canCreate ? (
            <Button onClick={() => navigate('/ai-gateway/virtual-keys/new')}>{t('pages:virtualKeys.createVirtualKey')}</Button>
          ) : undefined
        }
      />

      <ListFilterToolbar
        variant="boxed"
        hideClearButton
        searchPlaceholder={t('pages:virtualKeys.searchPlaceholder')}
        searchValue={search}
        onSearchChange={onSearchChange}
        meta={
          total === 0
            ? t('pages:virtualKeys.noVirtualKeysMatch')
            : t('pages:virtualKeys.showingVirtualKeys', { count: rows.length, total: total.toLocaleString() })
        }
      >
        <select aria-label={t('pages:virtualKeys.filterByProject')} value={projectFilter} onChange={onProjectFilterChange} className={styles.filterSelect}>
          <option value="">{t('pages:virtualKeys.allProjects')}</option>
          {(projectsData?.data ?? []).map(p => <option key={p.id} value={p.id}>{p.name}</option>)}
        </select>
        <select aria-label={t('pages:virtualKeys.filterByStatus')} value={enabledFilter} onChange={onEnabledFilterChange} className={styles.filterSelect}>
          <option value="">{t('pages:virtualKeys.allStatuses')}</option>
          <option value="enabled">{t('common:enabled')}</option>
          <option value="disabled">{t('common:disabled')}</option>
        </select>
      </ListFilterToolbar>

      <Card padding="none">
        <DataTable
          hideSearch
          frameless
          pageSize={pageLimit}
          columns={columns}
          data={rows}
          emptyMessage={t('pages:virtualKeys.noVirtualKeysConfigured')}
          onRowClick={(row) => navigate(`/ai-gateway/virtual-keys/${row.id}`)}
        />
      </Card>

      <ListPagination variant="plain" offset={offset} limit={pageLimit} total={total} onOffsetChange={setOffset} onLimitChange={setPageLimit} />

      <AlertDialog
        open={!!deleting}
        onOpenChange={(open) => { if (!open) setDeleting(null); }}
        title={t('pages:virtualKeys.deleteVirtualKey')}
        description={t('pages:virtualKeys.deleteConfirm', { name: deleting?.name })}
        confirmLabel={t('common:delete')}
        onConfirm={() => { if (deleting) deleteKey(deleting.id); }}
        variant="danger"
      />

      {/* Reject dialog with reason input */}
      <Dialog
        open={!!rejecting}
        onOpenChange={(open) => { if (!open) { setRejecting(null); setRejectReason(''); } }}
        title={t('pages:virtualKeys.rejectVk')}
        size="sm"
      >
        <Stack gap="md">
          <p>{t('pages:virtualKeys.rejectConfirm', { name: rejecting?.name })}</p>
          <label>
            {t('pages:virtualKeys.rejectReason')}
            <textarea
              value={rejectReason}
              onChange={(e) => setRejectReason(e.target.value)}
              className={styles.rejectTextarea}
              rows={3}
            />
          </label>
          <Stack direction="horizontal" gap="sm" className={styles.justifyEnd}>
            <Button variant="secondary" onClick={() => { setRejecting(null); setRejectReason(''); }}>
              {t('common:cancel')}
            </Button>
            <Button
              variant="danger"
              onClick={() => { if (rejecting) rejectKey({ id: rejecting.id, reason: rejectReason }); }}
            >
              {t('pages:virtualKeys.reject')}
            </Button>
          </Stack>
        </Stack>
      </Dialog>
    </Stack>
  );
}
