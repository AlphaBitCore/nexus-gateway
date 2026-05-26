import { useState, useCallback, useLayoutEffect, useEffect } from 'react';
import { useTranslation } from 'react-i18next';
import { ApiError } from '../../api/client';
import { useApi } from '../../hooks/useApi';
import { systemApi, iamApi } from '@/api/services';
import { usePermission } from '../../hooks/usePermission';
import { useToast } from '../../context/ToastContext';
import {
  PageHeader, ListFilterToolbar, LoadingSpinner, ErrorBanner,
  Button, Card, Stack, SearchableCombobox,
  ListPagination, DEFAULT_ADMIN_LIST_PAGE_SIZE, type AdminListPageSize,
  Input,
} from '@/components/ui';
import type { AdminAuditEntry } from '../../api/types';
import {
  DRAWER_MS,
  AdminAuditEntryDrawer,
  AdminAuditLogTable,
} from './adminAuditLogShared';
import styles from './AuditLogPage.module.css';

export function AuditLogPage() {
  const { t } = useTranslation();
  const { addToast } = useToast();
  const canExport = usePermission('audit:export');
  const [offset, setOffset] = useState(0);
  const [pageLimit, setPageLimit] = useState<AdminListPageSize>(DEFAULT_ADMIN_LIST_PAGE_SIZE);

  const [actionFilter, setActionFilter] = useState('');
  const [entityTypeFilter, setEntityTypeFilter] = useState('');
  const [userFilterId, setUserFilterId] = useState('');
  const [userFilterLabel, setUserFilterLabel] = useState('');
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
    ...(userFilterLabel && { actorLabel: userFilterLabel }),
    ...(startTime && { startTime }),
    ...(endTime && { endTime }),
  };

  const { data, loading, error, refetch } = useApi<{ data: AdminAuditEntry[]; total: number }>(
    () => systemApi.listAdminAuditLogs(filterParams),
    [
      'admin',
      'audit',
      'admin-logs',
      offset,
      pageLimit,
      actionFilter,
      entityTypeFilter,
      userFilterLabel,
      startTime,
      endTime,
    ],
  );

  if (loading && !data) return <LoadingSpinner />;
  if (error) {
    return (
      <ErrorBanner
        message={error.message}
        detail={error instanceof ApiError ? error.forbiddenDetails?.reason : undefined}
        onRetry={refetch}
      />
    );
  }

  const entries = data?.data ?? [];
  const total = data?.total ?? 0;

  const hasActiveFilters =
    Boolean(actionFilter) ||
    Boolean(entityTypeFilter) ||
    Boolean(userFilterId) ||
    Boolean(startTime) ||
    Boolean(endTime);

  const clearAllFilters = () => {
    setActionFilter('');
    setEntityTypeFilter('');
    setUserFilterId('');
    setUserFilterLabel('');
    setStartTime('');
    setEndTime('');
    setOffset(0);
  };

  const handleExport = async () => {
    const exportParams: Record<string, string> = {
      ...(actionFilter && { action: actionFilter }),
      ...(entityTypeFilter && { entityType: entityTypeFilter }),
      ...(userFilterLabel && { actorLabel: userFilterLabel }),
      ...(startTime && { startTime }),
      ...(endTime && { endTime }),
    };
    try {
      const exportData = await systemApi.exportAdminAuditLogs(exportParams);
      const blob = new Blob([JSON.stringify(exportData, null, 2)], { type: 'application/json' });
      const url = URL.createObjectURL(blob);
      const el = document.createElement('a');
      el.href = url;
      el.download = `admin-audit-export-${new Date().toISOString().slice(0, 19).replace(/[:]/g, '-')}.json`;
      el.click();
      URL.revokeObjectURL(url);
      if (exportData.truncated) {
        addToast(t('pages:audit.exportTruncated'), 'warning');
      }
    } catch (err) {
      const msg = err instanceof Error ? err.message : t('pages:audit.exportFailed');
      addToast(msg, 'error');
    }
  };

  return (
    <Stack gap="md">
      <PageHeader
        title={t('pages:audit.title')}
        subtitle={t('pages:audit.subtitle')}
        action={
          canExport ? (
            <Button variant="secondary" onClick={() => void handleExport()}>
              {t('pages:audit.export')}
            </Button>
          ) : undefined
        }
      />

      <ListFilterToolbar
        searchPlaceholder=""
        searchValue=""
        onSearchChange={() => {}}
        hideSearch
        meta={
          <span className={styles.metaSubline}>
            {t('pages:audit.metaSubline')}
          </span>
        }
      >
        <div className={styles.userCombobox}>
          <SearchableCombobox
            ariaLabel={t('pages:audit.searchAriaLabel')}
            placeholder={t('pages:audit.searchPlaceholder')}
            valueId={userFilterId}
            valueLabel={userFilterLabel}
            allowEmptyQueryFetch
            fetchOptions={async (q) => {
              const params: Record<string, string> = { limit: '100' };
              if (q.trim()) params.q = q.trim();
              const res = await iamApi.listUsers(params);
              const rows = res.data ?? [];
              return rows.map((u) => ({
                id: u.id,
                label: u.displayName + (u.email ? ` (${u.email})` : ''),
              }));
            }}
            onSelect={(opt) => {
              setUserFilterId(opt?.id ?? '');
              setUserFilterLabel(opt ? opt.label.split(' (')[0] : '');
              setOffset(0);
            }}
          />
        </div>

        <select
          aria-label={t('pages:audit.allActions')}
          value={actionFilter}
          onChange={(e) => { setActionFilter(e.target.value); setOffset(0); }}
          className={styles.filterSelect}
        >
          <option value="">{t('pages:audit.allActions')}</option>
          <option value="create">{t('pages:audit.actionCreate')}</option>
          <option value="update">{t('pages:audit.actionUpdate')}</option>
          <option value="delete">{t('pages:audit.actionDelete')}</option>
          <option value="reset">{t('pages:audit.actionReset')}</option>
          <option value="simulate">{t('pages:audit.actionSimulate')}</option>
        </select>

        <select
          aria-label={t('pages:audit.allEntityTypes')}
          value={entityTypeFilter}
          onChange={(e) => { setEntityTypeFilter(e.target.value); setOffset(0); }}
          className={styles.filterSelect}
        >
          <option value="">{t('pages:audit.allEntityTypes')}</option>
          <option value="routingRule">{t('pages:audit.entityRoutingRule')}</option>
          <option value="credential">{t('pages:audit.entityCredential')}</option>
          <option value="virtualKey">{t('pages:audit.entityVirtualKey')}</option>
          <option value="quota">{t('pages:audit.entityQuota')}</option>
          <option value="model">{t('pages:audit.entityModel')}</option>
          <option value="provider">{t('pages:audit.entityProvider')}</option>
          <option value="iamRole">{t('pages:audit.entityIamRole')}</option>
        </select>

        <Input
          type="datetime-local"
          aria-label={t('pages:audit.startTimeTitle')}
          value={startTime ? startTime.slice(0, 16) : ''}
          onChange={(e) => {
            setStartTime(e.target.value ? new Date(e.target.value).toISOString() : '');
            setOffset(0);
          }}
          className={styles.dateInput}
          title={t('pages:audit.startTimeTitle')}
        />
        <Input
          type="datetime-local"
          aria-label={t('pages:audit.endTimeTitle')}
          value={endTime ? endTime.slice(0, 16) : ''}
          onChange={(e) => {
            setEndTime(e.target.value ? new Date(e.target.value).toISOString() : '');
            setOffset(0);
          }}
          className={styles.dateInput}
          title={t('pages:audit.endTimeTitle')}
        />

        {hasActiveFilters && (
          <Button variant="ghost" size="sm" onClick={clearAllFilters} aria-label={t('pages:audit.clearFilters')}>
            {t('pages:audit.clearFilters')}
          </Button>
        )}
      </ListFilterToolbar>

      <Card padding="none">
        <AdminAuditLogTable
          entries={entries}
          selectedEntry={selectedEntry}
          onSelectEntry={setSelectedEntry}
          onToggleEntry={() => closeDrawer()}
          pageSize={pageLimit}
        />
      </Card>

      {selectedEntry && (
        <AdminAuditEntryDrawer
          selectedEntry={selectedEntry}
          drawerVisible={drawerVisible}
          onClose={closeDrawer}
        />
      )}

      <ListPagination
        offset={offset}
        limit={pageLimit}
        total={total}
        onOffsetChange={setOffset}
        onLimitChange={setPageLimit}
      />
    </Stack>
  );
}
