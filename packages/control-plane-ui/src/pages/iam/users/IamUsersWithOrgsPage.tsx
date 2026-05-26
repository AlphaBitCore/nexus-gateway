import { useState, useCallback } from 'react';
import { useNavigate, useSearchParams } from 'react-router-dom';
import { useTranslation } from 'react-i18next';
import { iamApi } from '@/api/services';
import { useApi } from '@/hooks/useApi';
import { useDebouncedValue } from '@/hooks/useDebouncedValue';
import {
  PageHeader, DataTable, ListFilterToolbar, Badge, statusToVariant,
  Skeleton, ErrorBanner, Button, Card, OrgTreeSelect,
  ListPagination, DEFAULT_ADMIN_LIST_PAGE_SIZE, type AdminListPageSize,
} from '@/components/ui';
import type { DataTableColumn } from '@/components/ui';
import type { AdminUser } from '@/api/types';
import { formatDate } from '@/lib/format';
import iamStyles from '../_shared/Iam.module.css';
import styles from './IamUsersWithOrgsPage.module.css';

/* ── User avatar cell ─────────────────────────────────────────────────── */

function AvatarCell({ name }: { name: string }) {
  const initials = name
    .split(' ')
    .filter(Boolean)
    .map((w) => w[0])
    .join('')
    .slice(0, 2)
    .toUpperCase();
  return (
    <span className={styles.avatarCell}>
      <span className={styles.avatar}>{initials}</span>
      <span>{name}</span>
    </span>
  );
}

/* ── Roles cell (max 2 visible + +N overflow tooltip) ────────────────── */

function RolesCellCompact({ roles }: { roles?: string[] }) {
  if (!roles?.length) return <span className={styles.rolesMuted}>—</span>;
  const visible = roles.slice(0, 2);
  const overflow = roles.slice(2);
  return (
    <span className={styles.rolesWrap}>
      {visible.map((r) => (
        <span key={r} className={styles.roleBadge}>{r}</span>
      ))}
      {overflow.length > 0 && (
        <span title={overflow.join(', ')} className={styles.roleBadgeOverflow}>
          +{overflow.length}
        </span>
      )}
    </span>
  );
}

/* ── Main page ────────────────────────────────────────────────────────── */

export function IamUsersWithOrgsPage() {
  const { t } = useTranslation();
  const navigate = useNavigate();
  const [searchParams, setSearchParams] = useSearchParams();

  /* ── URL-persisted filter state ── */
  const selectedOrgId = searchParams.get('orgId') ?? '';
  const statusFilter = searchParams.get('status') ?? '';
  const consoleAccessFilter = searchParams.get('consoleAccess') ?? '';

  /* ── Local pagination state (consistent with all other list pages) ── */
  const [offset, setOffset] = useState(0);
  const [pageLimit, setPageLimit] = useState<AdminListPageSize>(DEFAULT_ADMIN_LIST_PAGE_SIZE);

  /* ── Local search state ── */
  const [search, setSearch] = useState('');
  const debouncedSearch = useDebouncedValue(search, 300);

  /* ── User list data ── */
  const {
    data: usersData,
    loading: usersLoading,
    error: usersError,
    refetch: refetchUsers,
  } = useApi<{ data: AdminUser[]; total: number }>(
    () => {
      const params: Record<string, string> = {
        limit: String(pageLimit),
        offset: String(offset),
      };
      const q = debouncedSearch.trim();
      if (q) params.q = q;
      if (statusFilter === 'active') params.enabled = 'true';
      if (statusFilter === 'suspended') params.enabled = 'false';
      if (selectedOrgId) params.organizationId = selectedOrgId;
      if (consoleAccessFilter === 'yes') params.canAccessControlPlane = 'true';
      if (consoleAccessFilter === 'no') params.canAccessControlPlane = 'false';
      return iamApi.listUsers(params);
    },
    ['admin', 'iam', 'users', 'list', debouncedSearch, statusFilter, consoleAccessFilter, offset, pageLimit, selectedOrgId],
  );

  const rows = usersData?.data ?? [];
  const total = usersData?.total ?? 0;

  /* ── URL-update helpers ── */
  const handleOrgChange = useCallback((v: string | string[]) => {
    const id = Array.isArray(v) ? (v[0] ?? '') : v;
    setSearchParams((p) => {
      const next = new URLSearchParams(p);
      if (id) next.set('orgId', id); else next.delete('orgId');
      return next;
    }, { replace: true });
    setOffset(0);
  }, [setSearchParams]);

  const onSearchChange = useCallback((v: string) => {
    setSearch(v);
    setOffset(0);
  }, []);

  const onStatusChange = useCallback((e: React.ChangeEvent<HTMLSelectElement>) => {
    const val = e.target.value;
    setSearchParams((p) => {
      const next = new URLSearchParams(p);
      if (val) next.set('status', val); else next.delete('status');
      return next;
    }, { replace: true });
    setOffset(0);
  }, [setSearchParams]);

  const onConsoleAccessChange = useCallback((e: React.ChangeEvent<HTMLSelectElement>) => {
    const val = e.target.value;
    setSearchParams((p) => {
      const next = new URLSearchParams(p);
      if (val) next.set('consoleAccess', val); else next.delete('consoleAccess');
      return next;
    }, { replace: true });
    setOffset(0);
  }, [setSearchParams]);

  /* ── User table columns ── */
  const columns: DataTableColumn<AdminUser>[] = [
    {
      key: 'displayName',
      label: t('pages:iam.displayName'),
      render: (r) => <AvatarCell name={r.displayName} />,
    },
    { key: 'email', label: t('pages:iam.email'), render: (r) => r.email || '—' },
    {
      key: 'roles',
      label: t('pages:iam.roles'),
      render: (r) => <RolesCellCompact roles={r.roles} />,
    },
    {
      key: 'status',
      label: t('pages:iam.status'),
      render: (r) => <Badge variant={statusToVariant(r.status)}>{r.status}</Badge>,
    },
    {
      key: 'canAccessControlPlane',
      label: t('pages:iam.consoleAccess'),
      render: (r) => (
        <Badge variant={r.canAccessControlPlane ? 'success' : 'default'}>
          {r.canAccessControlPlane ? t('common:yes') : t('common:no')}
        </Badge>
      ),
    },
    {
      key: 'source',
      label: t('pages:iam.source'),
      render: (r) => {
        if (!r.source || r.source === 'local') return <span style={{ color: 'var(--color-text-muted)' }}>—</span>;
        const label = r.source === 'oidc' ? 'SSO' : r.source === 'scim' ? 'SCIM' : r.source;
        return <Badge variant="info">{label}</Badge>;
      },
    },
    {
      key: 'organizationName',
      label: t('pages:iam.organization'),
      render: (r) => r.organizationName
        ? (
          <button
            className={styles.orgLink}
            onClick={(e) => { e.stopPropagation(); navigate(`/iam/organizations/${r.organizationId}`); }}
          >
            {r.organizationName}
          </button>
        )
        : <span style={{ color: 'var(--color-text-muted)' }}>—</span>,
    },
    {
      key: 'lastLoginAt',
      label: t('pages:iam.lastLogin'),
      render: (r) => r.lastLoginAt ? formatDate(r.lastLoginAt) : t('pages:iam.never'),
    },
    {
      key: 'actions',
      label: '',
      render: (r) => (
        <Button
          variant="secondary"
          size="sm"
          onClick={(e) => { e.stopPropagation(); navigate(`/iam/users/${r.id}`); }}
        >
          {t('pages:iam.view')}
        </Button>
      ),
    },
  ];

  return (
    <div style={{ display: 'flex', flexDirection: 'column', gap: 'var(--g-space-5)' }}>
      <PageHeader
        title={t('pages:usersOrgs.title')}
        subtitle={t('pages:usersOrgs.subtitle')}
        action={
          <Button onClick={() => navigate('/iam/users/new')}>
            {t('pages:iam.createUser')}
          </Button>
        }
      />

      <ListFilterToolbar
        searchPlaceholder={t('pages:iam.searchUsersPlaceholder')}
        searchValue={search}
        onSearchChange={onSearchChange}
        meta={
          total === 0
            ? t('pages:iam.noUsersMatch')
            : t('pages:iam.showingUsers', { count: rows.length, total: total.toLocaleString() })
        }
      >
        <div className={styles.orgFilter}>
          <OrgTreeSelect
            mode="single"
            value={selectedOrgId}
            onChange={handleOrgChange}
            allowClear
            placeholder={t('pages:usersOrgs.filterByOrg')}
          />
        </div>
        <select
          aria-label={t('pages:iam.filterByStatus')}
          value={statusFilter}
          onChange={onStatusChange}
          className={iamStyles.filterSelect}
        >
          <option value="">{t('pages:iam.allAccounts')}</option>
          <option value="active">{t('pages:iam.activeOnly')}</option>
          <option value="suspended">{t('pages:iam.suspendedOnly')}</option>
        </select>
        <select
          aria-label={t('pages:iam.filterByConsoleAccess')}
          value={consoleAccessFilter}
          onChange={onConsoleAccessChange}
          className={iamStyles.filterSelect}
        >
          <option value="">{t('pages:iam.allConsoleAccess')}</option>
          <option value="yes">{t('pages:iam.consoleAccessYes')}</option>
          <option value="no">{t('pages:iam.consoleAccessNo')}</option>
        </select>
      </ListFilterToolbar>

      {usersLoading && <Skeleton.ListPageSkeleton />}
      {usersError && <ErrorBanner message={usersError.message} onRetry={refetchUsers} />}

      {!usersLoading && !usersError && (
        <>
          <Card padding="none">
            <DataTable
              hideSearch
              frameless
              serverPaginated
              pageSize={pageLimit}
              columns={columns}
              data={rows}
              onRowClick={(r) => navigate(`/iam/users/${r.id}`)}
              emptyMessage={t('pages:iam.noUsersFound')}
            />
          </Card>

          <ListPagination
            offset={offset}
            limit={pageLimit}
            total={total}
            onOffsetChange={setOffset}
            onLimitChange={setPageLimit}
          />
        </>
      )}
    </div>
  );
}
