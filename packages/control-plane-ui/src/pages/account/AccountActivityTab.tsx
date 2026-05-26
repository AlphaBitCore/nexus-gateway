import { useState, useCallback, useLayoutEffect, useEffect } from 'react';
import { useTranslation } from 'react-i18next';
import { ApiError } from '@/api/client';
import { useAuth } from '@/auth/context/AuthContext';
import { useApi } from '@/hooks/useApi';
import { api } from '@/api/client';
import {
  ListFilterToolbar, LoadingSpinner, Button, Card,
  ListPagination, DEFAULT_ADMIN_LIST_PAGE_SIZE, Input,
} from '@/components/ui';
import type { AdminListPageSize } from '@/components/ui';
import type { AdminAuditEntry } from '@/api/types';
import {
  DRAWER_MS,
  AdminAuditEntryDrawer,
  AdminAuditLogTable,
} from '../governance/adminAuditLogShared';
import styles from './Account.module.css';

export function AccountActivityTab() {
  const { t } = useTranslation();
  const { userId } = useAuth();

  const [offset, setOffset] = useState(0);
  const [pageLimit, setPageLimit] = useState<AdminListPageSize>(DEFAULT_ADMIN_LIST_PAGE_SIZE);

  const [actionFilter, setActionFilter] = useState('');
  const [entityTypeFilter, setEntityTypeFilter] = useState('');
  const [startTime, setStartTime] = useState('');
  const [endTime, setEndTime] = useState('');

  const [selectedEntry, setSelectedEntry] = useState<AdminAuditEntry | null>(null);
  const [drawerVisible, setDrawerVisible] = useState(false);

  const closeDrawer = useCallback(() => {
    setDrawerVisible(false);
    window.setTimeout(() => setSelectedEntry(null), DRAWER_MS);
  }, []);

  useLayoutEffect(() => {
    if (!selectedEntry) {
      setDrawerVisible(false);
      return;
    }
    setDrawerVisible(false);
    const id = window.requestAnimationFrame(() => {
      window.requestAnimationFrame(() => setDrawerVisible(true));
    });
    return () => window.cancelAnimationFrame(id);
  }, [selectedEntry?.id]);

  useEffect(() => {
    if (!selectedEntry) return;
    const onKey = (e: KeyboardEvent) => {
      if (e.key === 'Escape') closeDrawer();
    };
    window.addEventListener('keydown', onKey);
    return () => window.removeEventListener('keydown', onKey);
  }, [selectedEntry, closeDrawer]);

  const filterParams: Record<string, string> = {
    limit: String(pageLimit),
    offset: String(offset),
    ...(actionFilter && { action: actionFilter }),
    ...(entityTypeFilter && { entityType: entityTypeFilter }),
    ...(startTime && { startTime }),
    ...(endTime && { endTime }),
  };

  const { data, loading, error, refetch } = useApi<{ data: AdminAuditEntry[]; total: number }>(
    () => api.get<{ data: AdminAuditEntry[]; total: number }>('/api/my/activity', filterParams),
    ['my', 'activity', 'list', offset, pageLimit, actionFilter, entityTypeFilter, startTime, endTime],
  );

  if (loading && !data) return <LoadingSpinner />;

  if (error) {
    const ae = error instanceof ApiError ? error : null;
    if (ae?.status === 503 && ae.errorType === 'audit_not_queryable') {
      return (
        <p className={styles.errorText}>{t('pages:account.activityUnavailable')}</p>
      );
    }
    return (
      <p className={styles.errorText}>
        {error.message}
        {' '}
        <Button variant="ghost" size="sm" onClick={() => refetch()} className={styles.retryBtn}>
          {t('common:retry')}
        </Button>
      </p>
    );
  }

  const entries = data?.data ?? [];
  const total = data?.total ?? 0;

  const hasActiveFilters =
    Boolean(actionFilter) ||
    Boolean(entityTypeFilter) ||
    Boolean(startTime) ||
    Boolean(endTime);

  const clearAllFilters = () => {
    setActionFilter('');
    setEntityTypeFilter('');
    setStartTime('');
    setEndTime('');
    setOffset(0);
  };

  const actorHint = userId ? `${userId.slice(0, 8)}...` : '--';

  return (
    <div>
      <p className={styles.intro}>
        {t('pages:account.activityIntro', { actorHint })}
      </p>

      <ListFilterToolbar
        searchPlaceholder=""
        searchAriaLabel="Unused"
        searchValue=""
        onSearchChange={() => {}}
        hideSearch
        meta={<span className={styles.mutedText}>{t('pages:account.newestFirst')}</span>}
      >
        <select
          aria-label={t('pages:account.filterByAction')}
          value={actionFilter}
          onChange={(e) => { setActionFilter(e.target.value); setOffset(0); }}
          className={styles.filterSelect}
        >
          <option value="">{t('pages:account.allActions')}</option>
          <option value="create">{t('pages:account.action.create')}</option>
          <option value="update">{t('pages:account.action.update')}</option>
          <option value="delete">{t('pages:account.action.delete')}</option>
        </select>

        <select
          aria-label={t('pages:account.filterByEntityType')}
          value={entityTypeFilter}
          onChange={(e) => { setEntityTypeFilter(e.target.value); setOffset(0); }}
          className={styles.filterSelect}
        >
          <option value="">{t('pages:account.allEntityTypes')}</option>
          <option value="routingRule">{t('pages:account.entityType.routingRule')}</option>
          <option value="credential">{t('pages:account.entityType.credential')}</option>
          <option value="virtualKey">{t('pages:account.entityType.virtualKey')}</option>
          <option value="provider">{t('pages:account.entityType.provider', 'Provider')}</option>
          <option value="adminUser">{t('pages:account.entityType.adminUser')}</option>
          <option value="apiKey">{t('pages:account.entityType.apiKey', 'API Key')}</option>
        </select>

        <Input
          type="datetime-local"
          aria-label={t('pages:account.startTime')}
          value={startTime ? startTime.slice(0, 16) : ''}
          onChange={(e) => {
            setStartTime(e.target.value ? new Date(e.target.value).toISOString() : '');
            setOffset(0);
          }}
          className={styles.dateInput}
        />
        <Input
          type="datetime-local"
          aria-label={t('pages:account.endTime')}
          value={endTime ? endTime.slice(0, 16) : ''}
          onChange={(e) => {
            setEndTime(e.target.value ? new Date(e.target.value).toISOString() : '');
            setOffset(0);
          }}
          className={styles.dateInput}
        />

        {hasActiveFilters && (
          <Button variant="ghost" size="sm" onClick={clearAllFilters}>
            {t('pages:account.clearFilters')}
          </Button>
        )}
      </ListFilterToolbar>

      <Card padding="none">
        <AdminAuditLogTable
          entries={entries}
          selectedEntry={selectedEntry}
          onSelectEntry={setSelectedEntry}
          onToggleEntry={() => closeDrawer()}
          hideActorColumn
          pageSize={pageLimit}
        />
      </Card>

      {selectedEntry && (
        <AdminAuditEntryDrawer
          selectedEntry={selectedEntry}
          drawerVisible={drawerVisible}
          onClose={closeDrawer}
          titleId="account-activity-drawer-title"
          hideActor
        />
      )}

      <ListPagination
        offset={offset}
        limit={pageLimit}
        total={total}
        onOffsetChange={setOffset}
        onLimitChange={setPageLimit}
      />
    </div>
  );
}
