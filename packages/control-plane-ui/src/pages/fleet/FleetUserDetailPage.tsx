import { useState } from 'react';
import { useParams, useNavigate } from 'react-router-dom';
import { useTranslation } from 'react-i18next';
import { useApi } from '@/hooks/useApi';
import { useMutation } from '@/hooks/useMutation';
import { fleetApi } from '@/api/services';
import type { AgentUserDetail, FleetUserDevice, FleetAuditEvent } from '@/api/types';
import type { DataTableColumn, AdminListPageSize } from '@/components/ui';
import {
  PageHeader, DataTable, Badge, Button, Stack, Card,
  Skeleton, ErrorBanner, Breadcrumb, AlertDialog, ListPagination,
  DEFAULT_ADMIN_LIST_PAGE_SIZE,
} from '@/components/ui';
import styles from './FleetUserDetailPage.module.css';

type Tab = 'devices' | 'audit';

export function FleetUserDetailPage() {
  const { t } = useTranslation();
  const { id } = useParams<{ id: string }>();
  const navigate = useNavigate();
  const [activeTab, setActiveTab] = useState<Tab>('devices');
  const [confirmAction, setConfirmAction] = useState<'suspend' | 'activate' | null>(null);

  const { data: user, loading, error, refetch } = useApi<AgentUserDetail>(
    () => fleetApi.getAgentUser(id!),
    ['admin', 'agent-users', id!],
  );

  const [devOffset, setDevOffset] = useState(0);
  const [devLimit, setDevLimit] = useState<AdminListPageSize>(DEFAULT_ADMIN_LIST_PAGE_SIZE);

  const { data: devicesData } = useApi(
    () => fleetApi.getUserDevices(id!, { limit: String(devLimit), offset: String(devOffset) }),
    ['admin', 'agent-users', id!, 'devices', String(devOffset), String(devLimit)],
  );

  const [auditOffset, setAuditOffset] = useState(0);
  const [auditLimit, setAuditLimit] = useState<AdminListPageSize>(DEFAULT_ADMIN_LIST_PAGE_SIZE);

  const { data: auditData } = useApi(
    () => fleetApi.getUserAudit(id!, { limit: String(auditLimit), offset: String(auditOffset) }),
    ['admin', 'agent-users', id!, 'audit', String(auditOffset), String(auditLimit)],
  );

  const { mutate: suspendUser } = useMutation(
    () => fleetApi.suspendUser(id!),
    { invalidateQueries: [['api', 'admin', 'agent-users']], onSuccess: () => { setConfirmAction(null); refetch(); }, successMessage: t('pages:fleet.userSuspended') },
  );

  const { mutate: activateUser } = useMutation(
    () => fleetApi.activateUser(id!),
    { invalidateQueries: [['api', 'admin', 'agent-users']], onSuccess: () => { setConfirmAction(null); refetch(); }, successMessage: t('pages:fleet.userActivated') },
  );

  if (loading && !user) return <Skeleton.ListPageSkeleton />;
  if (error) return <ErrorBanner message={error.message} onRetry={refetch} />;
  if (!user) return <ErrorBanner message={t('pages:fleet.userNotFound', 'User not found')} />;

  const deviceColumns: DataTableColumn<FleetUserDevice>[] = [
    { key: 'hostname', label: t('pages:fleet.hostname') },
    { key: 'os', label: t('pages:fleet.os'), render: (r) => r.os === 'darwin' ? 'macOS' : r.os === 'windows' ? 'Windows' : r.os },
    { key: 'status', label: t('pages:fleet.status'), render: (r) => <Badge variant={r.status === 'ACTIVE' ? 'success' : r.status === 'OFFLINE' ? 'warning' : 'default'}>{r.status}</Badge> },
    { key: 'agentVersion', label: t('pages:fleet.agentVersion') },
    { key: 'lastHeartbeat', label: t('pages:fleet.lastHeartbeat'), render: (r) => r.lastHeartbeat ? new Date(r.lastHeartbeat).toLocaleString() : '—' },
    { key: 'assignedAt', label: t('pages:fleet.assignedAt'), render: (r) => new Date(r.assignedAt).toLocaleDateString() },
  ];

  const auditColumns: DataTableColumn<FleetAuditEvent>[] = [
    { key: 'timestamp', label: t('pages:fleet.timestamp'), render: (r) => new Date(r.timestamp).toLocaleString() },
    { key: 'source', label: t('pages:fleet.source') },
    { key: 'targetHost', label: t('pages:fleet.targetHost'), render: (r) => r.targetHost ?? '—' },
    { key: 'requestHookDecision', label: t('pages:fleet.requestHookDecision'), render: (r) => r.requestHookDecision ?? '—' },
  ];

  const isSuspended = user.status === 'suspended';

  return (
    <Stack gap="md">
      <Breadcrumb items={[
        { label: t('pages:fleet.usersTitle'), to: '/fleet/users' },
        { label: user.displayName },
      ]} />
      <PageHeader
        title={user.displayName}
        action={
          isSuspended
            ? <Button onClick={() => setConfirmAction('activate')}>{t('pages:fleet.activateUser')}</Button>
            : <Button variant="danger" onClick={() => setConfirmAction('suspend')}>{t('pages:fleet.suspendUser')}</Button>
        }
      />
      <Card>
        <div className={styles.kvGrid}>
          <span className={styles.kvLabel}>{t('pages:fleet.osUsername')}</span>
          <span className={styles.kvValue}>{user.osUsername ?? '—'}</span>
          <span className={styles.kvLabel}>{t('pages:fleet.osDomain')}</span>
          <span className={styles.kvValue}>{user.osDomain ?? '—'}</span>
          <span className={styles.kvLabel}>{t('pages:fleet.status')}</span>
          <span className={styles.kvValue}>
            <Badge variant={isSuspended ? 'warning' : 'success'}>{user.status}</Badge>
          </span>
          <span className={styles.kvLabel}>{t('pages:fleet.createdAt')}</span>
          <span className={styles.kvValue}>{new Date(user.createdAt).toLocaleDateString()}</span>
        </div>
      </Card>

      <div className={styles.tabBar}>
        {(['devices', 'audit'] as Tab[]).map(tab => (
          <button key={tab} onClick={() => setActiveTab(tab)} className={activeTab === tab ? styles.tabActive : styles.tab}>
            {tab === 'devices' && t('pages:fleet.devices')}
            {tab === 'audit' && t('pages:fleet.audit')}
          </button>
        ))}
      </div>

      {activeTab === 'devices' && (
        <Card>
          <DataTable columns={deviceColumns} data={devicesData?.data ?? []} onRowClick={(r) => navigate(`/devices/${r.id}`)} hideSearch />
          <ListPagination total={devicesData?.total ?? 0} offset={devOffset} limit={devLimit} onOffsetChange={setDevOffset} onLimitChange={setDevLimit} />
        </Card>
      )}

      {activeTab === 'audit' && (
        <Card>
          <DataTable columns={auditColumns} data={auditData?.data ?? []} hideSearch />
          <ListPagination total={auditData?.total ?? 0} offset={auditOffset} limit={auditLimit} onOffsetChange={setAuditOffset} onLimitChange={setAuditLimit} />
        </Card>
      )}

      <AlertDialog
        open={confirmAction === 'suspend'}
        onOpenChange={(open) => { if (!open) setConfirmAction(null); }}
        title={t('pages:fleet.suspendConfirmTitle')}
        description={t('pages:fleet.suspendConfirmDescription', { name: user.displayName })}
        confirmLabel={t('pages:fleet.suspendUser')}
        onConfirm={() => suspendUser(undefined as never)}
        variant="danger"
      />
      <AlertDialog
        open={confirmAction === 'activate'}
        onOpenChange={(open) => { if (!open) setConfirmAction(null); }}
        title={t('pages:fleet.activateConfirmTitle')}
        description={t('pages:fleet.activateConfirmDescription', { name: user.displayName })}
        confirmLabel={t('pages:fleet.activateUser')}
        onConfirm={() => activateUser(undefined as never)}
      />
    </Stack>
  );
}
