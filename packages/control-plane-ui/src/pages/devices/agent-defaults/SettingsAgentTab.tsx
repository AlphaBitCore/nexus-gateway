import { useState, useEffect, useCallback } from 'react';
import { useTranslation } from 'react-i18next';
import { Card, Stack, Button, Switch, ErrorBanner } from '@/components/ui';
import styles from './SettingsAgentTab.module.css';
import { useApi } from '@/hooks/useApi';
import { useMutation } from '@/hooks/useMutation';
import { devicesApi } from '@/api/services';
import { QuitPolicyCard } from './QuitPolicyCard';
import { ShutdownWarningCard } from './ShutdownWarningCard';
import { RuntimeDefaultsCard } from './RuntimeDefaultsCard';
import { QuicBundlesCard } from './QuicBundlesCard';
import { BypassBundlesCard } from './BypassBundlesCard';

const LOCALES = [
  { key: 'en', label: 'English' },
  { key: 'zh', label: '中文' },
  { key: 'es', label: 'Español' },
] as const;

const SHUTDOWN_WARNING_DEFAULTS: Record<string, string> = {
  en: 'Closing Nexus Agent will stop AI traffic monitoring on this device. Your IT team will be notified. Are you sure?',
  zh: '关闭 Nexus Agent 将停止本设备上的 AI 流量监控。您的 IT 团队会收到通知。确定要关闭吗？',
  es: 'Cerrar Nexus Agent detendrá la supervisión de tráfico de IA en este dispositivo. Se notificará a su equipo de TI. ¿Está seguro?',
};

interface AgentSettingsData {
  quitAllowed: boolean;
  shutdownWarning?: Record<string, string>;
  shutdownWarningEnabled?: boolean;
  // Reporting cadence (seconds). Zero = "agent falls back to YAML
  // default" (typically 60s heartbeat, 30s audit drain).
  heartbeatIntervalSec?: number;
  auditDrainIntervalSec?: number;
  configSyncIntervalSec?: number;
  auditBatchSize?: number;
  autoUpdateEnabled?: boolean;
  autoUpdateChannel?: string;
  logLevel?: string;
  trafficUploadLevel?: string;
  themeId?: string;
  forceQUICFallbackBundles?: string[];
  bypassBundles?: string[];
  attestationEnabled?: boolean;
}

/**
 * Device Defaults page — fleet-wide agent runtime knobs.
 *
 * Currently surfaces:
 *
 *   - Quit Policy ............ controls whether the macOS menu-bar exposes
 *                              Restart Agent / Quit Nexus Agent items.
 *                              Compliance always-on deployments leave this
 *                              off; dev fleets leave it on.
 *   - Shutdown Warning ....... multi-locale text shown to the user when they
 *                              attempt to quit (only relevant when
 *                              quitAllowed=true). Empty per-locale string =
 *                              fall back to the built-in default text.
 *
 * Both fields ride the same /api/admin/settings/device-defaults endpoint
 * and reconcile to the agent over the `agent_settings` shadow config key.
 *
 * The legacy auditPolicy + forensicsEnabled cards were removed in an earlier
 * sweep — those fields were stored in CP but never consumed by the agent
 * runtime. forensicsEnabled overlaps conceptually with
 * Compliance > Payload Capture, which is the modern surface that actually
 * pushes the field to agents over a shadow adapter.
 */
export function SettingsAgentTab() {
  const { t } = useTranslation();

  const [activeLocale, setActiveLocale] = useState<string>('en');

  const { data, loading, error, refetch } = useApi<AgentSettingsData>(
    () => devicesApi.getAgentSettings(),
    ['admin', 'settings', 'device-defaults'],
  );

  const [quitAllowed, setQuitAllowed] = useState(false);
  const [warnings, setWarnings] = useState<Record<string, string>>({});
  const [shutdownWarningEnabled, setShutdownWarningEnabled] = useState(false);
  // Runtime-cadence fields. Number-typed but stored in input as
  // string so empty string ≠ 0; empty string maps to undefined on
  // save (i.e. "leave the agent's local YAML default alone").
  const [heartbeat, setHeartbeat] = useState<string>('');
  const [auditDrain, setAuditDrain] = useState<string>('');
  const [configSync, setConfigSync] = useState<string>('');
  const [auditBatch, setAuditBatch] = useState<string>('');
  const [autoUpdateEnabled, setAutoUpdateEnabled] = useState(true);
  const [autoUpdateChannel, setAutoUpdateChannel] = useState<string>('stable');
  const [logLevel, setLogLevel] = useState<string>('info');
  // trafficUploadLevel = agent's enum that gates which flows reach Hub.
  // Empty value from the server means "agent default" (= processed); we
  // display 'processed' so the operator sees the effective level rather
  // than a confusing blank dropdown.
  const [trafficUploadLevel, setTrafficUploadLevel] = useState<string>('processed');
  // themeId = fleet-wide theme pack ID admin pushes to all agent Dashboards.
  // Empty value means "let each agent use its local pick" — natural starting
  // state. Setting it locks the entire fleet to one branded look.
  const [themeId, setThemeId] = useState<string>('');
  // forceQUICFallbackBundles = bundle-ID allowlist for the macOS NE
  // proxy. The chip editor binds to this array directly (not joined to
  // a textarea) so duplicates are caught at the UI layer and the user
  // sees what they typed wrong before save. Empty array (admin clears
  // every chip) propagates as []; agent disables QUIC blocking entirely.
  const [quicBundles, setQuicBundles] = useState<string[]>([]);
  const [quicInputDraft, setQuicInputDraft] = useState<string>('');
  // bypassBundles = SOURCE-app exemption list. Apps here are passed through
  // by the macOS NE WITHOUT inspection — a deliberate compliance carve-out
  // for trusted tools whose pinned TLS breaks under bump. Empty = exempt
  // nothing (inspect everything); ships empty.
  const [bypassBundles, setBypassBundles] = useState<string[]>([]);
  const [bypassInputDraft, setBypassInputDraft] = useState<string>('');
  // Fleet-wide opt-in for agent attestation (cluster default; per-agent
  // overrides are not yet supported). Defaults false so the perf optimization
  // stays gated until a security engineer explicitly enables it.
  const [attestationEnabled, setAttestationEnabled] = useState(false);
  const [dirty, setDirty] = useState(false);

  // Sync local state on every API refetch (including post-save invalidation).
  // The reducer here merges server-returned shutdownWarning over the locale
  // defaults so a partially-customized blob (e.g. only "en" set) still shows
  // sensible placeholders for the other languages.
  useEffect(() => {
    if (data) {
      setQuitAllowed(data.quitAllowed);
      setWarnings({
        ...SHUTDOWN_WARNING_DEFAULTS,
        ...(data.shutdownWarning ?? {}),
      });
      setShutdownWarningEnabled(data.shutdownWarningEnabled ?? false);
      setHeartbeat(data.heartbeatIntervalSec ? String(data.heartbeatIntervalSec) : '');
      setAuditDrain(data.auditDrainIntervalSec ? String(data.auditDrainIntervalSec) : '');
      setConfigSync(data.configSyncIntervalSec ? String(data.configSyncIntervalSec) : '');
      setAuditBatch(data.auditBatchSize ? String(data.auditBatchSize) : '');
      setAutoUpdateEnabled(data.autoUpdateEnabled ?? true);
      setAutoUpdateChannel(data.autoUpdateChannel || 'stable');
      setLogLevel(data.logLevel || 'info');
      setTrafficUploadLevel(data.trafficUploadLevel || 'processed');
      setThemeId(data.themeId || '');
      setQuicBundles(Array.isArray(data.forceQUICFallbackBundles) ? data.forceQUICFallbackBundles : []);
      setQuicInputDraft('');
      setBypassBundles(Array.isArray(data.bypassBundles) ? data.bypassBundles : []);
      setBypassInputDraft('');
      setAttestationEnabled(data.attestationEnabled ?? false);
      setDirty(false);
    }
  }, [data]);

  // Convert "" → undefined and any non-numeric to undefined so the
  // backend treats omitted fields as "don't touch". Empty string is
  // the operator's explicit "fall back to YAML default" signal.
  const num = (s: string): number | undefined => {
    if (s === '') return undefined;
    const n = Number(s);
    return Number.isFinite(n) ? n : undefined;
  };

  const { mutate: save, loading: saving } = useMutation(
    () => devicesApi.updateAgentSettings({
      quitAllowed,
      shutdownWarning: warnings,
      shutdownWarningEnabled,
      heartbeatIntervalSec: num(heartbeat),
      auditDrainIntervalSec: num(auditDrain),
      configSyncIntervalSec: num(configSync),
      auditBatchSize: num(auditBatch),
      autoUpdateEnabled,
      autoUpdateChannel,
      logLevel,
      trafficUploadLevel,
      themeId,
      forceQUICFallbackBundles: quicBundles,
      bypassBundles,
      attestationEnabled,
    }),
    {
      invalidateQueries: [['api', 'admin', 'settings', 'device-defaults']],
      successMessage: t('common:saved', 'Saved'),
      onSuccess: () => setDirty(false),
    },
  );

  const handleWarningChange = useCallback((value: string) => {
    setWarnings(prev => ({ ...prev, [activeLocale]: value }));
    setDirty(true);
  }, [activeLocale]);

  const handleQuitToggle = useCallback((next: boolean) => {
    setQuitAllowed(next);
    setDirty(true);
  }, []);

  if (error) return <ErrorBanner message={error.message} onRetry={refetch} />;

  return (
    <Stack gap="md">
      {/* Quit Policy */}
      <QuitPolicyCard
        quitAllowed={quitAllowed}
        onQuitToggle={handleQuitToggle}
        loading={loading}
      />

      {/* Agent Attestation — fleet-wide opt-in for the agent → CP
          trust-bypass path. When enabled, the agent signs every outbound
          CONNECT with its Ed25519 attestation key so the compliance-proxy
          can transparently tunnel (skip MITM + hooks) on a verified
          signature. Default off; the toggle is a perf optimization, not
          a security gate — invalid / missing signatures always fall back
          to the existing full-MITM path, never reject the request. */}
      <Card>
        <Stack gap="md">
          <h3 style={{ margin: 'var(--g-space-0)' }}>
            {t('pages:settings.attestation.title', 'Agent Attestation')}
          </h3>
          <p className={styles.helpTextSecondary}>
            {t(
              'pages:settings.attestation.desc',
              'When enabled, agents cryptographically attest that they already inspected each request, so the Compliance Proxy can transparently tunnel attested traffic (skipping its own MITM + hook pipeline). Verified attestations save ~30-50 ms per request and roughly halve CP CPU. Invalid or missing signatures always fall back to the existing full-MITM path — this is a performance optimization, not a security gate.',
            )}
          </p>

          <label style={{ display: 'flex', alignItems: 'center', gap: 'var(--g-space-3)', cursor: 'pointer' }}>
            <Switch
              checked={attestationEnabled}
              onCheckedChange={(next) => { setAttestationEnabled(next); setDirty(true); }}
              disabled={loading}
            />
            <div>
              <div style={{ fontWeight: 'var(--g-font-weight-medium)' }}>
                {t('pages:settings.attestation.enabledLabel', 'Enable agent attestation (fleet default)')}
              </div>
              <div className={styles.hintTextMuted}>
                {attestationEnabled
                  ? t('pages:settings.attestation.onHint', 'Agents will sign outbound requests; CP transparently tunnels on a valid signature.')
                  : t('pages:settings.attestation.offHint', 'Agents do not sign; every request flows through CP’s full MITM + hook pipeline (current behavior).')}
              </div>
            </div>
          </label>
        </Stack>
      </Card>

      {/* Shutdown Warning (only meaningful when quit is allowed) */}
      <ShutdownWarningCard
        quitAllowed={quitAllowed}
        shutdownWarningEnabled={shutdownWarningEnabled}
        onShutdownWarningEnabledChange={(next) => { setShutdownWarningEnabled(next); setDirty(true); }}
        locales={LOCALES}
        activeLocale={activeLocale}
        onActiveLocaleChange={setActiveLocale}
        warnings={warnings}
        onWarningChange={handleWarningChange}
        loading={loading}
        shutdownWarningData={data?.shutdownWarning}
      />

      {/* Runtime defaults — the timing / cadence / updater / log
          fields admin couldn't previously edit. Empty input means
          "use the agent's local YAML default"; non-empty sends an
          explicit override that the agent's agent_settings handler
          honours on next shadow tick. Server clamps intervals to
          [10s, 86400s] so a stray "1" doesn't DoS Hub. */}
      <RuntimeDefaultsCard
        heartbeat={heartbeat}
        setHeartbeat={setHeartbeat}
        auditDrain={auditDrain}
        setAuditDrain={setAuditDrain}
        configSync={configSync}
        setConfigSync={setConfigSync}
        auditBatch={auditBatch}
        setAuditBatch={setAuditBatch}
        logLevel={logLevel}
        setLogLevel={setLogLevel}
        autoUpdateChannel={autoUpdateChannel}
        setAutoUpdateChannel={setAutoUpdateChannel}
        trafficUploadLevel={trafficUploadLevel}
        setTrafficUploadLevel={setTrafficUploadLevel}
        themeId={themeId}
        setThemeId={setThemeId}
        autoUpdateEnabled={autoUpdateEnabled}
        setAutoUpdateEnabled={setAutoUpdateEnabled}
        setDirty={setDirty}
        loading={loading}
      />

      {/* QUIC fallback bundles — bundle-ID allowlist that the macOS NE
          extension consults to decide which apps' UDP flows to close.
          Closing UDP forces a QUIC → TCP downgrade so the agent's
          TLS-bump path can see the request. Only browsers + Electron
          AI desktop apps belong here; system processes (mdnsresponder,
          dhcp, ntp) MUST NOT be added — closing their UDP took the host
          network down (incident 2026-05-15). Empty list disables QUIC
          blocking entirely (admin's escape hatch). */}
      <QuicBundlesCard
        quicBundles={quicBundles}
        setQuicBundles={setQuicBundles}
        quicInputDraft={quicInputDraft}
        setQuicInputDraft={setQuicInputDraft}
        setDirty={setDirty}
      />

      {/* Bypass bundles — SOURCE-app exemption list. Apps named here are
          passed through by the macOS NE WITHOUT inspection (no TLS bump, no
          audit). This is a deliberate compliance carve-out for trusted tools
          whose pinned TLS breaks under bump (e.g. a developer CLI). Matching
          is by source bundle, never by host, so the same destination stays
          inspected from other apps. Empty = exempt nothing; ships empty. */}
      <BypassBundlesCard
        bypassBundles={bypassBundles}
        setBypassBundles={setBypassBundles}
        bypassInputDraft={bypassInputDraft}
        setBypassInputDraft={setBypassInputDraft}
        setDirty={setDirty}
      />

      <div>
        <Button onClick={() => save(undefined)} loading={saving} disabled={!dirty}>
          {t('common:save')}
        </Button>
      </div>
    </Stack>
  );
}
