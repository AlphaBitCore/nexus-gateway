import { useState, useMemo } from 'react';
import { useNavigate, useParams } from 'react-router-dom';
import { useTranslation } from 'react-i18next';
import { useApi } from '@/hooks/useApi';
import { useMutation } from '@/hooks/useMutation';
import { usePermission } from '@/hooks/usePermission';
import { devicesApi, fleetApi, diagModeApi } from '@/api/services';
import type { AgentDevice, AgentAuditEvent, FleetAuditEvent, DeviceAssignmentDetail } from '@/api/types';
import type { DataTableColumn, AdminListPageSize } from '@/components/ui';
import {
  PageHeader, DataTable, Badge, Button, Stack, Card, Input,
  Skeleton, ErrorBanner, Breadcrumb, Dialog, ListPagination,
  DropdownMenu, DropdownMenuTrigger, DropdownMenuContent, DropdownMenuItem, DropdownMenuSeparator,
  DEFAULT_ADMIN_LIST_PAGE_SIZE,
} from '@/components/ui';
import { ThingStatsTab } from '@/pages/infrastructure/_shared/tabs/metrics/ThingStatsTab';
import { DeviceTagEditor } from './DeviceTagEditor';
import styles from './FleetDeviceDetailPage.module.css';

type Tab = 'traffic' | 'compliance' | 'configuration' | 'system' | 'activity';

export function FleetDeviceDetailPage() {
  const { t } = useTranslation();
  const navigate = useNavigate();
  const { id } = useParams<{ id: string }>();
  const [activeTab, setActiveTab] = useState<Tab>('traffic');
  const [revokeOpen, setRevokeOpen] = useState(false);
  const [rotateOpen, setRotateOpen] = useState(false);
  const [diagOpen, setDiagOpen] = useState(false);
  const [diagPreset, setDiagPreset] = useState<'30m' | '2h' | '8h' | null>(null);
  const [diagReason, setDiagReason] = useState('');

  // Stats are sliced inside the Traffic tab via ThingStatsTab; gated on the
  // separate observability:read because the underlying rollup API enforces it.
  const canViewStats = usePermission('observability:read');
  const canForceResync = usePermission('agent-devices:force-resync');
  const canRotateCert = usePermission('agent-devices:rotate');
  const canRevoke = usePermission('agent-devices:delete');
  const canDiag = usePermission('diagnostic-mode:update');

  const { data: device, loading, error, refetch } = useApi<AgentDevice>(
    () => devicesApi.get(id!),
    ['admin', 'devices', 'detail', id!],
  );

  const [eventsOffset, setEventsOffset] = useState(0);
  const [eventsLimit, setEventsLimit] = useState<AdminListPageSize>(DEFAULT_ADMIN_LIST_PAGE_SIZE);
  const { data: eventsData } = useApi(
    () => devicesApi.getEvents(id!, { limit: String(eventsLimit), offset: String(eventsOffset) }),
    ['admin', 'devices', 'events', id!, String(eventsOffset), String(eventsLimit)],
  );
  const allEvents: AgentAuditEvent[] = eventsData?.data ?? [];

  // Compliance tab: client-side filter of the same event stream — events
  // where the v2 hook pipeline returned anything other than "allow" (or
  // legacy MODIFY decisions). Server-side filter would scale better when
  // device traffic balloons; client-side keeps the page responsive for
  // the common case of <1k events / device.
  const complianceEvents = useMemo(
    () => allEvents.filter(e => {
      const d = (e.requestHookDecision || '').toLowerCase();
      return d !== '' && d !== 'allow';
    }),
    [allEvents],
  );

  const { data: timelineData } = useApi(
    () => fleetApi.getDeviceTimeline(id!),
    ['admin', 'devices', 'timeline', id!],
  );

  const [auditOffset, setAuditOffset] = useState(0);
  const [auditLimit, setAuditLimit] = useState<AdminListPageSize>(DEFAULT_ADMIN_LIST_PAGE_SIZE);
  const { data: auditData } = useApi(
    () => fleetApi.getDeviceAudit(id!, { limit: String(auditLimit), offset: String(auditOffset) }),
    ['admin', 'devices', 'audit', id!, String(auditOffset), String(auditLimit)],
  );

  const { data: configData } = useApi(
    () => fleetApi.getDeviceConfig(id!),
    ['admin', 'devices', 'config', id!],
  );

  // Diag-mode active window — drives the "Diag mode active" badge in the
  // header + the dropdown's current state. The list endpoint returns every
  // active window across the fleet; filter to this device.
  const { data: diagList, refetch: refetchDiag } = useApi(
    () => diagModeApi.list(),
    ['admin', 'agents', 'diagnostic-mode', 'list'],
  );
  const activeDiagWindow = useMemo(
    () => diagList?.find(w => w.nodeId === id) ?? null,
    [diagList, id],
  );

  const { mutate: forceRefresh, loading: refreshing } = useMutation(
    () => devicesApi.forceRefresh(id!),
    { successMessage: t('pages:devices.forceRefreshDone') },
  );

  const { mutate: rotateCert, loading: rotatingCert } = useMutation(
    () => devicesApi.rotateCert(id!),
    {
      onSuccess: () => { setRotateOpen(false); refetch(); },
      successMessage: t('pages:fleet.rotateCertDone'),
    },
  );

  const { mutate: revokeDevice, loading: revoking } = useMutation(
    () => devicesApi.unenroll(id!),
    {
      onSuccess: () => { setRevokeOpen(false); navigate('/devices'); },
      successMessage: t('pages:fleet.revokeDeviceDone'),
    },
  );

  const { mutate: enableDiag } = useMutation(
    () => {
      const presets: Record<'30m' | '2h' | '8h', number> = { '30m': 30 * 60, '2h': 2 * 3600, '8h': 8 * 3600 };
      const seconds = presets[diagPreset || '30m'];
      const until = new Date(Date.now() + seconds * 1000).toISOString();
      return diagModeApi.enable(id!, { until, reason: diagReason || undefined });
    },
    {
      onSuccess: () => { setDiagOpen(false); setDiagReason(''); setDiagPreset(null); refetchDiag(); },
      successMessage: t('pages:fleet.diagModeDone'),
    },
  );

  const { mutate: disableDiag } = useMutation(
    () => diagModeApi.disable(id!),
    {
      onSuccess: () => refetchDiag(),
      successMessage: t('pages:fleet.diagModeDone'),
    },
  );

  if (loading && !device) return <Skeleton.ListPageSkeleton />;
  if (error) return <ErrorBanner message={error.message} onRetry={refetch} />;
  if (!device) return <ErrorBanner message={t('pages:fleet.deviceNotFound', 'Device not found')} />;

  const copyToClipboard = (text: string) => {
    if (navigator.clipboard?.writeText) {
      void navigator.clipboard.writeText(text);
    }
  };

  // Prefer the canonical Hub-written path (`thing.metadata.staticInfo`)
  // since that's what the agent reports today via UpdateStaticInfo and
  // what `/infrastructure/nodes/agent-*` reads. The legacy
  // `thing_agent.sysinfo` column is kept as a fallback for any older
  // agent row that still carries data there. Both shapes are loose
  // JSON blobs (machineId / osName / cpuModel / cpuCores / totalMemMB /
  // networkInterfaces[]…), so typed as `any` to match the JSX below.
   
  const staticInfo: any =
    device.metadata && typeof device.metadata === 'object'
      ? (device.metadata as Record<string, unknown>).staticInfo ?? null
      : null;
   
  const legacySysinfo: any = device.sysinfo
    ? typeof device.sysinfo === 'string'
      ? JSON.parse(device.sysinfo as unknown as string)
      : device.sysinfo
    : null;
  const sysinfo = staticInfo ?? legacySysinfo;

  // Same Hub-status map as pages/devices/utils.ts and the Infra* pages so
  // online/offline/enrolled all render with the canonical color.
  const statusVariant = (s: string): 'success' | 'warning' | 'default' | 'danger' => {
    const map: Record<string, 'success' | 'warning' | 'default' | 'danger'> = {
      online: 'success',
      enrolled: 'warning',
      offline: 'default',
      'out-of-sync': 'danger',
      revoked: 'danger',
      ACTIVE: 'success',
      ENROLLED: 'warning',
      OFFLINE: 'default',
      REVOKED: 'danger',
    };
    return map[s] ?? map[s.toLowerCase()] ?? 'default';
  };

  const hookDecisionVariant = (d?: string | null) => {
    if (!d) return 'default' as const;
    const v = d.toLowerCase();
    if (v === 'allow') return 'success' as const;
    if (v === 'deny' || v === 'block' || v === 'block-hard') return 'danger' as const;
    if (v === 'redact' || v === 'modify') return 'warning' as const;
    return 'default' as const;
  };

  const trafficColumns: DataTableColumn<AgentAuditEvent>[] = [
    { key: 'timestamp', label: t('pages:fleet.timestamp'), render: (r) => new Date(r.timestamp).toLocaleString() },
    { key: 'sourceProcess', label: t('pages:fleet.source'), render: (r) => r.sourceProcess || '—' },
    { key: 'destHost', label: t('pages:fleet.targetHost'), render: (r) => r.destHost ?? '—' },
    { key: 'action', label: 'Action', render: (r) => <Badge variant="outline">{r.action || '—'}</Badge> },
    {
      key: 'requestHookDecision',
      label: t('pages:fleet.hookDecision'),
      render: (r) => r.requestHookDecision
        ? <Badge variant={hookDecisionVariant(r.requestHookDecision)}>{r.requestHookDecision}</Badge>
        : <span className={styles.dim}>—</span>,
    },
    {
      key: 'duration',
      label: 'Duration',
      render: (r) => r.duration != null ? `${r.duration}ms` : <span className={styles.dim}>—</span>,
    },
  ];

  const timelineColumns: DataTableColumn<DeviceAssignmentDetail>[] = [
    { key: 'userDisplayName', label: t('pages:fleet.displayName'), render: (r) => r.userDisplayName ?? r.userId },
    { key: 'userOsUsername', label: t('pages:fleet.osUsername'), render: (r) => r.userOsUsername ?? '—' },
    { key: 'source', label: t('pages:fleet.source') },
    { key: 'assignedAt', label: t('pages:fleet.assignedAt'), render: (r) => new Date(r.assignedAt).toLocaleString() },
    {
      key: 'releasedAt',
      label: t('pages:fleet.releasedAt'),
      render: (r) => r.releasedAt
        ? new Date(r.releasedAt).toLocaleString()
        : <Badge variant="success">{t('pages:fleet.currentAssignment')}</Badge>,
    },
  ];

  const adminAuditColumns: DataTableColumn<FleetAuditEvent>[] = [
    { key: 'timestamp', label: t('pages:fleet.timestamp'), render: (r) => new Date(r.timestamp).toLocaleString() },
    { key: 'source', label: t('pages:fleet.source') },
    { key: 'targetHost', label: t('pages:fleet.targetHost'), render: (r) => r.targetHost ?? '—' },
    { key: 'requestHookDecision', label: t('pages:fleet.requestHookDecision'), render: (r) => r.requestHookDecision ?? '—' },
  ];

  const diagActiveUntil = activeDiagWindow ? new Date(activeDiagWindow.endedAt).toLocaleString() : null;

  return (
    <Stack gap="md">
      <Breadcrumb items={[
        { label: t('pages:devices.title'), to: '/devices' },
        { label: device.hostname },
      ]} />
      <PageHeader
        title={device.hostname}
        subtitle={`${device.os === 'darwin' ? 'macOS' : device.os} ${device.osVersion} — ${device.agentVersion}`}
        action={
          <Stack direction="horizontal" gap="sm" align="center">
            <Badge variant={statusVariant(device.status)}>{device.status}</Badge>
            {activeDiagWindow && (
              <Badge variant="warning">
                {t('pages:fleet.diagModeActive')} {diagActiveUntil ? `(${t('pages:fleet.diagModeUntil', { time: diagActiveUntil })})` : ''}
              </Badge>
            )}
            {canForceResync && (
              <Button variant="secondary" size="sm" onClick={() => forceRefresh(undefined)} loading={refreshing}>
                {t('pages:devices.forceRefresh')}
              </Button>
            )}
            {(canDiag || canRotateCert || canRevoke) && (
              <DropdownMenu>
                <DropdownMenuTrigger>
                  <Button variant="secondary" size="sm">{t('common:actions', 'Actions')} ▾</Button>
                </DropdownMenuTrigger>
                <DropdownMenuContent>
                  {canDiag && [
                    <DropdownMenuItem
                      key="diag-30m"
                      onSelect={() => { setDiagPreset('30m'); setDiagOpen(true); }}
                    >
                      {t('pages:fleet.diagModeEnable30m')}
                    </DropdownMenuItem>,
                    <DropdownMenuItem
                      key="diag-2h"
                      onSelect={() => { setDiagPreset('2h'); setDiagOpen(true); }}
                    >
                      {t('pages:fleet.diagModeEnable2h')}
                    </DropdownMenuItem>,
                    <DropdownMenuItem
                      key="diag-8h"
                      onSelect={() => { setDiagPreset('8h'); setDiagOpen(true); }}
                    >
                      {t('pages:fleet.diagModeEnable8h')}
                    </DropdownMenuItem>,
                    activeDiagWindow ? (
                      <DropdownMenuItem
                        key="diag-disable"
                        onSelect={() => disableDiag(undefined)}
                      >
                        {t('pages:fleet.diagModeDisable')}
                      </DropdownMenuItem>
                    ) : null,
                    <DropdownMenuSeparator key="sep-diag" />,
                  ]}
                  {canRotateCert && (
                    <DropdownMenuItem onSelect={() => setRotateOpen(true)}>
                      {t('pages:fleet.rotateCert')}
                    </DropdownMenuItem>
                  )}
                  {canRevoke && (
                    <DropdownMenuItem onSelect={() => setRevokeOpen(true)} style={{ color: 'var(--color-danger)' }}>
                      {t('pages:fleet.revokeDevice')}
                    </DropdownMenuItem>
                  )}
                </DropdownMenuContent>
              </DropdownMenu>
            )}
          </Stack>
        }
      />

      {/* Identity card — first-class natural-key identifiers + currently
          bound user. Replaces the old simple kvGrid. */}
      <Card>
        <div className={styles.kvGrid}>
          <span className={styles.kvLabel}>{t('pages:devices.identity.hostname')}</span>
          <span className={styles.kvValue}>{device.hostname}</span>
          {device.boundUserDisplayName && (
            <>
              <span className={styles.kvLabel}>{t('pages:devices.identity.boundUser')}</span>
              <span className={styles.kvValue}>
                {device.boundUserDisplayName}
                {device.boundUserEmail && <span style={{ color: 'var(--color-text-muted)' }}>{' · '}{device.boundUserEmail}</span>}
              </span>
            </>
          )}
          {device.physicalId && (
            <>
              <span className={styles.kvLabel}>{t('pages:devices.identity.physicalId')}</span>
              <span className={styles.kvValue}>
                <code>{device.physicalId}</code>
                <button
                  type="button"
                  onClick={() => copyToClipboard(device.physicalId!)}
                  title={t('common:copy')}
                  style={{ marginLeft: 'var(--g-space-2)', padding: 'var(--g-space-0-5) var(--g-space-1-5)', border: '1px solid var(--color-border)', borderRadius: 'var(--g-radius-sm)', background: 'none', cursor: 'pointer' }}
                >⧉</button>
              </span>
            </>
          )}
          <span className={styles.kvLabel}>{t('pages:devices.identity.thingId')}</span>
          <span className={styles.kvValue}>
            <code>{device.id}</code>
            <button
              type="button"
              onClick={() => copyToClipboard(device.id)}
              title={t('common:copy')}
              style={{ marginLeft: 'var(--g-space-2)', padding: 'var(--g-space-0-5) var(--g-space-1-5)', border: '1px solid var(--color-border)', borderRadius: 'var(--g-radius-sm)', background: 'none', cursor: 'pointer' }}
            >⧉</button>
          </span>
          {device.primaryIp && (
            <>
              <span className={styles.kvLabel}>{t('pages:devices.identity.ip')}</span>
              <span className={styles.kvValue}><code>{device.primaryIp}</code></span>
            </>
          )}
          <span className={styles.kvLabel}>{t('pages:fleet.os')}</span>
          <span className={styles.kvValue}>{device.os === 'darwin' ? 'macOS' : device.os} {device.osVersion}</span>
          <span className={styles.kvLabel}>{t('pages:fleet.agentVersion')}</span>
          <span className={styles.kvValue}>{device.agentVersion}</span>
          <span className={styles.kvLabel}>{t('pages:fleet.lastHeartbeat')}</span>
          <span className={styles.kvValue}>{device.lastHeartbeat ? new Date(device.lastHeartbeat).toLocaleString() : '—'}</span>
          <span className={styles.kvLabel}>{t('pages:devices.enrolledAt')}</span>
          <span className={styles.kvValue}>{new Date(device.enrolledAt).toLocaleString()} {device.enrolledBy ? `· ${device.enrolledBy}` : ''}</span>
        </div>
        {/* Tag editor — below the kvGrid so it stays inside the Identity card */}
        <div style={{ marginTop: 'var(--g-space-4)', paddingTop: 'var(--g-space-3)', borderTop: '1px solid var(--color-border)' }}>
          <div style={{ fontSize: 'var(--g-font-size-sm)', fontWeight: 'var(--g-font-weight-semibold)', marginBottom: 'var(--g-space-2)' }}>
            {t('pages:devices.tagsLabel', 'Tags')}
          </div>
          <DeviceTagEditor
            deviceId={device.id}
            initialTags={device.tags ?? []}
            onSaved={() => refetch()}
          />
        </div>
      </Card>

      <div className={styles.tabBar}>
        {(['traffic', 'compliance', 'configuration', 'system', 'activity'] as Tab[]).map(tab => (
          <button key={tab} onClick={() => setActiveTab(tab)} className={activeTab === tab ? styles.tabActive : styles.tab}>
            {tab === 'traffic' && t('pages:fleet.tabTraffic')}
            {tab === 'compliance' && t('pages:fleet.tabCompliance')}
            {tab === 'configuration' && t('pages:fleet.tabConfiguration')}
            {tab === 'system' && t('pages:fleet.tabSystem')}
            {tab === 'activity' && t('pages:fleet.tabActivity')}
          </button>
        ))}
      </div>

      {activeTab === 'traffic' && (
        <Stack gap="md">
          {canViewStats && (
            <Card>
              <ThingStatsTab thingId={id!} thingType="agent" />
            </Card>
          )}
          <Card>
            <Stack gap="sm">
              <h4 className={styles.sectionTitle}>{t('pages:fleet.trafficEventsTitle')}</h4>
              <p className={styles.dim}>{t('pages:fleet.trafficEventsSubtitle')}</p>
              <DataTable columns={trafficColumns} data={allEvents} hideSearch />
              <ListPagination
                total={eventsData?.total ?? 0}
                offset={eventsOffset}
                limit={eventsLimit}
                onOffsetChange={setEventsOffset}
                onLimitChange={setEventsLimit}
              />
            </Stack>
          </Card>
        </Stack>
      )}

      {activeTab === 'compliance' && (
        <Card>
          <Stack gap="sm">
            <h4 className={styles.sectionTitle}>{t('pages:fleet.complianceEventsTitle')}</h4>
            {complianceEvents.length === 0 ? (
              <p className={styles.empty}>{t('pages:fleet.complianceEventsEmpty')}</p>
            ) : (
              <DataTable columns={trafficColumns} data={complianceEvents} hideSearch />
            )}
          </Stack>
        </Card>
      )}

      {activeTab === 'configuration' && (
        <Card>
          <p className={styles.configNote}>{t('pages:fleet.effectiveConfig')}</p>
          <pre className={styles.configPre}>{JSON.stringify(configData ?? {}, null, 2)}</pre>
        </Card>
      )}

      {activeTab === 'system' && (
        <Card>
          {sysinfo ? (
            <Stack gap="md">
              <div className={styles.kvGrid}>
                <span className={styles.kvLabel}>{t('pages:fleet.machineId')}</span>
                <span className={styles.kvValue}>{sysinfo.machineId ?? '—'}</span>
                <span className={styles.kvLabel}>{t('pages:fleet.osName')}</span>
                <span className={styles.kvValue}>{sysinfo.osName} {sysinfo.osVersion}</span>
                <span className={styles.kvLabel}>{t('pages:fleet.cpuModel')}</span>
                <span className={styles.kvValue}>{sysinfo.cpuModel ?? '—'}</span>
                <span className={styles.kvLabel}>{t('pages:fleet.cpuCores')}</span>
                <span className={styles.kvValue}>{sysinfo.cpuCores}</span>
                <span className={styles.kvLabel}>{t('pages:fleet.totalMemMB')}</span>
                <span className={styles.kvValue}>{sysinfo.totalMemMB?.toLocaleString() ?? '—'}</span>
                <span className={styles.kvLabel}>{t('pages:fleet.serialNumber')}</span>
                <span className={styles.kvValue}>{sysinfo.serialNumber ?? '—'}</span>
                <span className={styles.kvLabel}>{t('pages:fleet.modelName')}</span>
                <span className={styles.kvValue}>{sysinfo.modelName ?? '—'}</span>
              </div>
              {sysinfo.networkInterfaces?.length > 0 && (
                <>
                  <h4 className={styles.sectionTitle}>{t('pages:fleet.networkInterfaces')}</h4>
                  <table className={styles.table}>
                    <thead>
                      <tr>
                        <th className={styles.th}>{t('pages:fleet.ifName')}</th>
                        <th className={styles.th}>{t('pages:fleet.macAddress')}</th>
                        <th className={styles.th}>{t('pages:fleet.ips')}</th>
                      </tr>
                    </thead>
                    <tbody>
                      {sysinfo.networkInterfaces.map((nif: { name: string; macAddress: string; ips: string[] }, i: number) => (
                        <tr key={i}>
                          <td className={styles.td}>{nif.name}</td>
                          <td className={styles.td}>{nif.macAddress}</td>
                          <td className={styles.td}>{nif.ips?.join(', ')}</td>
                        </tr>
                      ))}
                    </tbody>
                  </table>
                </>
              )}
            </Stack>
          ) : (
            <p className={styles.empty}>{t('pages:fleet.noSysinfo')}</p>
          )}
        </Card>
      )}

      {activeTab === 'activity' && (
        <Stack gap="md">
          <Card>
            <Stack gap="sm">
              <h4 className={styles.sectionTitle}>{t('pages:fleet.activityAssignments')}</h4>
              <DataTable columns={timelineColumns} data={timelineData?.data ?? []} hideSearch />
            </Stack>
          </Card>
          <Card>
            <Stack gap="sm">
              <h4 className={styles.sectionTitle}>{t('pages:fleet.activityAdminActions')}</h4>
              <DataTable columns={adminAuditColumns} data={auditData?.data ?? []} hideSearch />
              <ListPagination
                total={auditData?.total ?? 0}
                offset={auditOffset}
                limit={auditLimit}
                onOffsetChange={setAuditOffset}
                onLimitChange={setAuditLimit}
              />
            </Stack>
          </Card>
        </Stack>
      )}

      <Dialog open={rotateOpen} onOpenChange={setRotateOpen} title={t('pages:fleet.rotateCertConfirmTitle')}>
        <Stack gap="md">
          <p>{t('pages:fleet.rotateCertConfirmBody')}</p>
          <Stack direction="horizontal" gap="sm" justify="end">
            <Button variant="secondary" onClick={() => setRotateOpen(false)}>{t('common:cancel')}</Button>
            <Button onClick={() => rotateCert(undefined)} loading={rotatingCert}>{t('pages:fleet.rotateCert')}</Button>
          </Stack>
        </Stack>
      </Dialog>

      <Dialog open={revokeOpen} onOpenChange={setRevokeOpen} title={t('pages:fleet.revokeDeviceConfirmTitle')}>
        <Stack gap="md">
          <p>{t('pages:fleet.revokeDeviceConfirmBody')}</p>
          <Stack direction="horizontal" gap="sm" justify="end">
            <Button variant="secondary" onClick={() => setRevokeOpen(false)}>{t('common:cancel')}</Button>
            <Button variant="danger" onClick={() => revokeDevice(undefined)} loading={revoking}>{t('pages:fleet.revokeDevice')}</Button>
          </Stack>
        </Stack>
      </Dialog>

      <Dialog open={diagOpen} onOpenChange={setDiagOpen} title={t('pages:fleet.diagMode')}>
        <Stack gap="md">
          <p>
            {diagPreset === '30m' && t('pages:fleet.diagModeEnable30m')}
            {diagPreset === '2h' && t('pages:fleet.diagModeEnable2h')}
            {diagPreset === '8h' && t('pages:fleet.diagModeEnable8h')}
          </p>
          <label className={styles.kvLabel}>{t('pages:fleet.diagModeReasonLabel')}</label>
          <Input
            placeholder={t('pages:fleet.diagModeReasonPlaceholder')}
            value={diagReason}
            onChange={(e) => setDiagReason(e.target.value)}
          />
          <Stack direction="horizontal" gap="sm" justify="end">
            <Button variant="secondary" onClick={() => setDiagOpen(false)}>{t('common:cancel')}</Button>
            <Button onClick={() => enableDiag(undefined)}>{t('pages:fleet.diagMode')}</Button>
          </Stack>
        </Stack>
      </Dialog>
    </Stack>
  );
}
