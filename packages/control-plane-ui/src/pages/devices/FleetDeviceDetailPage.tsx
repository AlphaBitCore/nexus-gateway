import { useState, useMemo } from 'react';
import { Link, useNavigate, useParams } from 'react-router-dom';
import { useTranslation } from 'react-i18next';
import { useApi } from '@/hooks/useApi';
import { useMutation } from '@/hooks/useMutation';
import { usePermission } from '@/hooks/usePermission';
import { devicesApi, fleetApi, diagModeApi } from '@/api/services';
import type { AgentDevice, AgentAuditEvent } from '@/api/types';
import type { AdminListPageSize } from '@/components/ui';
import {
  Badge, Button, Stack,
  Skeleton, ErrorBanner,
  DropdownMenu, DropdownMenuTrigger, DropdownMenuContent, DropdownMenuItem, DropdownMenuSeparator,
  DEFAULT_ADMIN_LIST_PAGE_SIZE,
} from '@/components/ui';
import { thingStatusVariant } from '@/lib/thingStatus';
import styles from './FleetDeviceDetailPage.module.css';
import { IdentityCard } from './fleet-device-detail/IdentityCard';
import { TrafficTab } from './fleet-device-detail/TrafficTab';
import { ComplianceTab } from './fleet-device-detail/ComplianceTab';
import { ConfigurationTab } from './fleet-device-detail/ConfigurationTab';
import { SystemTab } from './fleet-device-detail/SystemTab';
import { ActivityTab } from './fleet-device-detail/ActivityTab';
import { RotateCertDialog } from './fleet-device-detail/RotateCertDialog';
import { RevokeDeviceDialog } from './fleet-device-detail/RevokeDeviceDialog';
import { DiagModeDialog } from './fleet-device-detail/DiagModeDialog';

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

  const diagActiveUntil = activeDiagWindow ? new Date(activeDiagWindow.endedAt).toLocaleString() : null;

  return (
    <Stack gap="md">
      <section className={styles.detailHeader}>
        <div className={styles.headerTitleRow}>
          <Link to="/devices" className={styles.backLink} aria-label={t('common:back', 'Back')}>
            <svg className={styles.backIcon} width="20" height="20" viewBox="0 0 20 20" fill="none" aria-hidden="true">
              <path d="M8.33333 5L3.33333 10L8.33333 15" stroke="currentColor" strokeWidth="1.66667" strokeLinecap="round" strokeLinejoin="round" />
              <path d="M4.16667 10H13.3333C15.1743 10 16.6667 11.4924 16.6667 13.3333V15" stroke="currentColor" strokeWidth="1.66667" strokeLinecap="round" strokeLinejoin="round" />
            </svg>
          </Link>
          <div className={styles.headerTextBlock}>
            <div className={styles.titleRow}>
              <h1 className={styles.detailTitle}>{device.hostname}</h1>
              <Badge className={styles.statusBadge} variant={thingStatusVariant(device.status)}>{device.status}</Badge>
            </div>
            <p className={styles.detailSubtitle}>
              {`${device.os === 'darwin' ? 'macOS' : device.os} ${device.osVersion} — ${device.agentVersion}`}
            </p>
          </div>
          <Stack direction="horizontal" gap="sm" align="center" className={styles.headerActions}>
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
                  <Button variant="primary" size="sm">{t('common:actions', 'Actions')} ▾</Button>
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
        </div>
      </section>

      <IdentityCard
        device={device}
        copyToClipboard={copyToClipboard}
        onTagsSaved={() => refetch()}
      />

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
        <TrafficTab
          id={id!}
          canViewStats={canViewStats}
          allEvents={allEvents}
          eventsTotal={eventsData?.total ?? 0}
          eventsOffset={eventsOffset}
          eventsLimit={eventsLimit}
          onOffsetChange={setEventsOffset}
          onLimitChange={setEventsLimit}
        />
      )}

      {activeTab === 'compliance' && (
        <ComplianceTab complianceEvents={complianceEvents} />
      )}

      {activeTab === 'configuration' && (
        <ConfigurationTab configData={configData} />
      )}

      {activeTab === 'system' && (
        <SystemTab sysinfo={sysinfo} />
      )}

      {activeTab === 'activity' && (
        <ActivityTab
          timelineRows={timelineData?.data ?? []}
          auditRows={auditData?.data ?? []}
          auditTotal={auditData?.total ?? 0}
          auditOffset={auditOffset}
          auditLimit={auditLimit}
          onAuditOffsetChange={setAuditOffset}
          onAuditLimitChange={setAuditLimit}
        />
      )}

      <RotateCertDialog
        open={rotateOpen}
        onOpenChange={setRotateOpen}
        onConfirm={() => rotateCert(undefined)}
        loading={rotatingCert}
      />

      <RevokeDeviceDialog
        open={revokeOpen}
        onOpenChange={setRevokeOpen}
        onConfirm={() => revokeDevice(undefined)}
        loading={revoking}
      />

      <DiagModeDialog
        open={diagOpen}
        onOpenChange={setDiagOpen}
        diagPreset={diagPreset}
        diagReason={diagReason}
        onReasonChange={setDiagReason}
        onConfirm={() => enableDiag(undefined)}
      />
    </Stack>
  );
}
