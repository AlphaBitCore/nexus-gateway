import { useState, useEffect, useCallback } from 'react';
import { useTranslation } from 'react-i18next';
import clsx from 'clsx';
import { Card, Stack, Button, Switch, ErrorBanner, Input, Select, FormField } from '@/components/ui';
import styles from './SettingsAgentTab.module.css';
import { useApi } from '@/hooks/useApi';
import { useMutation } from '@/hooks/useMutation';
import { devicesApi } from '@/api/services';

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
      <Card>
        <Stack gap="md">
          <h3 style={{ margin: 'var(--g-space-0)' }}>{t('pages:settings.quitPolicyTitle', 'Agent Quit Policy')}</h3>
          <p className={styles.helpTextSecondary}>
            {t('pages:settings.quitPolicyDesc', 'Controls whether the agent menu bar exposes Restart Agent and Quit Nexus Agent items. Turn off for compliance always-on deployments — users cannot quit the agent process.')}
          </p>

          <label style={{ display: 'flex', alignItems: 'center', gap: 'var(--g-space-3)', cursor: 'pointer' }}>
            <Switch
              checked={quitAllowed}
              onCheckedChange={handleQuitToggle}
              disabled={loading}
            />
            <div>
              <div style={{ fontWeight: 'var(--g-font-weight-medium)' }}>
                {t('pages:settings.quitAllowedLabel', 'Allow users to quit the agent')}
              </div>
              <div className={styles.hintTextMuted}>
                {quitAllowed
                  ? t('pages:settings.quitAllowedOnHint', 'Restart Agent and Quit Nexus Agent menu items are visible.')
                  : t('pages:settings.quitAllowedOffHint', 'Restart Agent and Quit Nexus Agent menu items are hidden — only Restart App is available.')}
              </div>
            </div>
          </label>
        </Stack>
      </Card>

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
      <Card>
        <Stack gap="md">
          <h3 style={{ margin: 'var(--g-space-0)' }}>{t('pages:settings.agentShutdownWarningTitle')}</h3>
          <p className={styles.helpTextSecondary}>
            {quitAllowed
              ? t('pages:settings.agentShutdownWarningDesc')
              : t('pages:settings.shutdownWarningDisabledHint', 'This text only appears when "Allow users to quit the agent" is turned on. Edit it now so the message is ready when quit is enabled.')}
          </p>

          {/* shutdownWarningEnabled gate. Admin can prepare the
              warning text but suppress the dialog (text saved but
              not shown). Mirrors the field the agent's
              agent_settings handler now reads (see #83). */}
          <label style={{ display: 'flex', alignItems: 'center', gap: 'var(--g-space-3)', cursor: 'pointer' }}>
            <Switch
              checked={shutdownWarningEnabled}
              onCheckedChange={(next) => { setShutdownWarningEnabled(next); setDirty(true); }}
              disabled={loading}
            />
            <div style={{ fontWeight: 'var(--g-font-weight-medium)' }}>
              {t('pages:settings.shutdownWarningEnabledLabel', 'Show this warning when the user clicks Quit')}
            </div>
          </label>

          {/* Locale tabs */}
          <div className={styles.tabRow}>
            {LOCALES.map(loc => (
              <button
                key={loc.key}
                onClick={() => setActiveLocale(loc.key)}
                className={clsx(styles.localeTab, activeLocale === loc.key && styles.localeTabActive)}
              >
                {loc.label}
              </button>
            ))}
          </div>

          <textarea
            value={warnings[activeLocale] ?? ''}
            onChange={e => handleWarningChange(e.target.value)}
            rows={4}
            className={styles.warningTextarea}
            placeholder={t('pages:settings.agentShutdownWarningPlaceholder')}
            disabled={loading}
          />

          {/* When the live blob has no shutdownWarning field yet (or this
              locale is empty after the merge) tell the user we're showing a
              default placeholder so a "Save" click doesn't seem surprising. */}
          {!data?.shutdownWarning?.[activeLocale] && (
            <p className={styles.helpTextSecondarySmall}>
              {t('pages:settings.agentWarningUsingDefault')}
            </p>
          )}
        </Stack>
      </Card>

      {/* Runtime defaults — the timing / cadence / updater / log
          fields admin couldn't previously edit. Empty input means
          "use the agent's local YAML default"; non-empty sends an
          explicit override that the agent's agent_settings handler
          honours on next shadow tick. Server clamps intervals to
          [10s, 86400s] so a stray "1" doesn't DoS Hub. */}
      <Card>
        <Stack gap="md">
          <h3 style={{ margin: 'var(--g-space-0)' }}>{t('pages:settings.agentRuntimeTitle', 'Runtime defaults')}</h3>
          <p className={styles.helpTextSecondary}>
            {t('pages:settings.agentRuntimeDesc', 'Fleet-wide reporting cadence, updater channel, and log level. Leave a field empty to fall back to the agent\'s YAML default.')}
          </p>

          <div style={{ display: 'grid', gridTemplateColumns: '1fr 1fr', gap: 'var(--g-space-3)' }}>
            <FormField label={t('pages:settings.heartbeatIntervalSec')}>
              <Input
                type="number"
                value={heartbeat}
                onChange={(e) => { setHeartbeat(e.target.value); setDirty(true); }}
                placeholder="60"
                min={10}
                max={86400}
              />
            </FormField>
            <FormField label={t('pages:settings.auditDrainIntervalSec')}>
              <Input
                type="number"
                value={auditDrain}
                onChange={(e) => { setAuditDrain(e.target.value); setDirty(true); }}
                placeholder="30"
                min={10}
                max={86400}
              />
            </FormField>
            <FormField label={t('pages:settings.configSyncIntervalSec')}>
              <Input
                type="number"
                value={configSync}
                onChange={(e) => { setConfigSync(e.target.value); setDirty(true); }}
                placeholder="300"
                min={10}
                max={86400}
              />
            </FormField>
            <FormField label={t('pages:settings.auditBatchSize')}>
              <Input
                type="number"
                value={auditBatch}
                onChange={(e) => { setAuditBatch(e.target.value); setDirty(true); }}
                placeholder="100"
                min={1}
                max={10000}
              />
            </FormField>
            <FormField label={t('pages:settings.logLevel')}>
              <Select
                value={logLevel}
                onValueChange={(v) => { setLogLevel(v); setDirty(true); }}
                options={[
                  { value: 'debug', label: 'debug' },
                  { value: 'info', label: 'info' },
                  { value: 'warn', label: 'warn' },
                  { value: 'error', label: 'error' },
                ]}
              />
            </FormField>
            <FormField label={t('pages:settings.autoUpdateChannel')}>
              <Select
                value={autoUpdateChannel}
                onValueChange={(v) => { setAutoUpdateChannel(v); setDirty(true); }}
                options={[
                  { value: 'stable', label: 'stable' },
                  { value: 'beta', label: 'beta' },
                ]}
              />
            </FormField>
            <FormField
              label={t('pages:settings.trafficUploadLevel.label', 'Traffic upload level')}
              helpText={
                trafficUploadLevel === 'all'
                  ? t('pages:settings.trafficUploadLevel.helpAll', 'Every captured flow is uploaded — including untracked hosts and inspect-but-passthrough rows. Highest cost; useful for audit windows.')
                  : trafficUploadLevel === 'blocked'
                    ? t('pages:settings.trafficUploadLevel.helpBlocked', 'Only blocked / denied / bump-failed flows reach Hub. Silent operation; compliance evidence preserved.')
                    : t('pages:settings.trafficUploadLevel.helpProcessed', 'Processed (hooks ran), Blocked, and Bump-failed flows reach Hub. Untracked hosts and Inspect-only rows (matched but admin set passthrough) stay local. Recommended for production.')
              }
            >
              <Select
                value={trafficUploadLevel}
                onValueChange={(v) => { setTrafficUploadLevel(v); setDirty(true); }}
                options={[
                  { value: 'all', label: t('pages:settings.trafficUploadLevel.optAll', 'All flows') },
                  { value: 'processed', label: t('pages:settings.trafficUploadLevel.optProcessed', 'Processed / Blocked / Bump-failed (recommended)') },
                  { value: 'blocked', label: t('pages:settings.trafficUploadLevel.optBlocked', 'Blocked / Bump-failed only') },
                ]}
              />
            </FormField>
            <FormField
              label={t('pages:settings.agentThemeId.label', 'Agent Dashboard theme')}
              helpText={t(
                'pages:settings.agentThemeId.help',
                'Forces every agent Dashboard in the fleet to render with this theme pack. Empty means each user keeps their own local pick. Unknown IDs fall back to the bundled default theme.',
              )}
            >
              <Select
                value={themeId}
                onValueChange={(v) => { setThemeId(v); setDirty(true); }}
                options={[
                  { value: '', label: t('pages:settings.agentThemeId.optUserPick', 'Let each user choose (no fleet override)') },
                  { value: 'default', label: t('pages:settings.agentThemeId.optDefault', 'Default (monochrome, Geist)') },
                  { value: 'morningstar', label: 'Morningstar' },
                  { value: 'rbc', label: 'RBC' },
                ]}
              />
            </FormField>
          </div>

          <label style={{ display: 'flex', alignItems: 'center', gap: 'var(--g-space-3)', cursor: 'pointer' }}>
            <Switch
              checked={autoUpdateEnabled}
              onCheckedChange={(next) => { setAutoUpdateEnabled(next); setDirty(true); }}
              disabled={loading}
            />
            <div style={{ fontWeight: 'var(--g-font-weight-medium)' }}>
              {t('pages:settings.autoUpdateEnabledLabel', 'Auto-install signed updates')}
            </div>
          </label>
        </Stack>
      </Card>

      {/* QUIC fallback bundles — bundle-ID allowlist that the macOS NE
          extension consults to decide which apps' UDP flows to close.
          Closing UDP forces a QUIC → TCP downgrade so the agent's
          TLS-bump path can see the request. Only browsers + Electron
          AI desktop apps belong here; system processes (mdnsresponder,
          dhcp, ntp) MUST NOT be added — closing their UDP took the host
          network down (incident 2026-05-15). Empty list disables QUIC
          blocking entirely (admin's escape hatch). */}
      <Card>
        <Stack gap="md">
          <h3 style={{ margin: 'var(--g-space-0)' }}>
            {t('pages:settings.quicFallback.title', 'QUIC fallback bundles (macOS)')}
          </h3>
          <p className={styles.helpTextSecondary}>
            {t('pages:settings.quicFallback.desc', 'macOS bundle IDs of apps whose UDP flows the agent will close to force HTTP/3 → HTTP/2 fallback. Browsers and Electron AI clients prefer h3 to QUIC-friendly endpoints (ChatGPT, Claude.ai, Cloudflare-fronted services); without this list the agent\'s TCP path never sees their requests. Add Chromium-based desktop apps you also want intercepted (Cursor, Claude Desktop, etc.). NEVER add system processes (com.apple.mDNSResponder, dhcpcd, etc.) — that breaks DNS and takes the host network down.')}
          </p>

          <div style={{ display: 'flex', flexWrap: 'wrap', gap: 'var(--g-space-2)' }}>
            {quicBundles.map((b) => (
              <span
                key={b}
                style={{
                  display: 'inline-flex',
                  alignItems: 'center',
                  gap: 'var(--g-space-2)',
                  padding: 'var(--g-space-1) var(--g-space-3)',
                  background: 'var(--color-surface-2)',
                  border: '1px solid var(--color-border)',
                  borderRadius: 'var(--radius-pill)',
                  fontSize: 'var(--font-size-sm)',
                  fontFamily: 'var(--g-font-family-mono)',
                }}
              >
                {b}
                <button
                  type="button"
                  aria-label={t('pages:settings.quicFallback.remove', 'Remove')}
                  onClick={() => {
                    setQuicBundles((prev) => prev.filter((x) => x !== b));
                    setDirty(true);
                  }}
                  style={{
                    background: 'transparent',
                    border: 'none',
                    cursor: 'pointer',
                    padding: 'var(--g-space-0)',
                    color: 'var(--color-text-muted)',
                    fontSize: 'var(--font-size-md)',
                    lineHeight: 1,
                  }}
                >
                  ×
                </button>
              </span>
            ))}
            {quicBundles.length === 0 && (
              <span className={styles.hintTextMuted}>
                {t('pages:settings.quicFallback.empty', 'No bundles configured — agent will not close any UDP flows.')}
              </span>
            )}
          </div>

          <div style={{ display: 'flex', gap: 'var(--g-space-2)', alignItems: 'center' }}>
            <Input
              type="text"
              value={quicInputDraft}
              onChange={(e) => setQuicInputDraft(e.target.value)}
              onKeyDown={(e) => {
                if (e.key === 'Enter') {
                  e.preventDefault();
                  const v = quicInputDraft.trim();
                  if (v && !quicBundles.includes(v)) {
                    setQuicBundles((prev) => [...prev, v]);
                    setDirty(true);
                  }
                  setQuicInputDraft('');
                }
              }}
              placeholder={t('pages:settings.quicFallback.placeholder', 'com.example.MyBrowser — press Enter to add')}
              style={{ flex: 1 }}
            />
            <Button
              variant="secondary"
              onClick={() => {
                const v = quicInputDraft.trim();
                if (v && !quicBundles.includes(v)) {
                  setQuicBundles((prev) => [...prev, v]);
                  setDirty(true);
                }
                setQuicInputDraft('');
              }}
              disabled={!quicInputDraft.trim()}
            >
              {t('pages:settings.quicFallback.add', 'Add')}
            </Button>
          </div>
          <p className={styles.helpTextSecondarySmall}>
            {t('pages:settings.quicFallback.howToFind', 'Find a Mac app\'s bundle ID with: defaults read /Applications/SomeApp.app/Contents/Info.plist CFBundleIdentifier')}
          </p>
        </Stack>
      </Card>

      <div>
        <Button onClick={() => save(undefined)} loading={saving} disabled={!dirty}>
          {t('common:save')}
        </Button>
      </div>
    </Stack>
  );
}
