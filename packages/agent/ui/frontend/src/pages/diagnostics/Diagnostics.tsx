import { useState } from 'react';
import { useQuery, useQueryClient } from '@tanstack/react-query';
import { useTranslation } from 'react-i18next';
import { agentApi } from '@/api/agent';
import styles from '../overview/Overview.module.css';
import page from '../_shared/pageStyles.module.css';

/**
 * Diagnostics page — runs the GET_DIAGNOSTICS IPC every 5 s. Renders:
 *   1. Recovery actions: Restart Daemon + Copy support bundle.
 *      Reinstall Network Extension lives on the menu-bar app
 *      (needs the OSSystemExtensionRequest entitlement the Wails
 *      process doesn't carry) and is reachable from the menu's
 *      Diagnostics submenu in a follow-up.
 *   2. Status table: gateway reachability, cert path, interception
 *      mechanism.
 *   3. Last ~50 lines of the agent log.
 *
 * Recovery actions show inline confirmation toasts and refetch
 * status so the user sees the daemon coming back up.
 */
export function Diagnostics() {
  const { t } = useTranslation();
  const queryClient = useQueryClient();
  const [toast, setToast] = useState<string | null>(null);

  const { data, isLoading } = useQuery({
    queryKey: ['agent', 'diagnostics'],
    queryFn: () => agentApi.getDiagnostics(),
    refetchInterval: 5_000,
  });

  // Toast tone distinguishes "blocked by policy" (error) from "kicked off" (success).
  const [toastTone, setToastTone] = useState<'success' | 'error' | 'info'>('info');
  const flashToast = (msg: string, tone: 'success' | 'error' | 'info' = 'info') => {
    setToast(msg);
    setToastTone(tone);
    window.setTimeout(() => setToast(null), 4_500);
  };

  // Restart daemon: SHUTDOWN IPC. launchd respawns within ~10s;
  // the Dashboard's reconnect banner covers the gap. Flash an info
  // toast immediately on click before the confirm dialog opens so the
  // user always sees visual feedback regardless of confirm outcome.
  const onRestartDaemon = async () => {
    flashToast(t('diagnostics.restartDaemon.clicked', 'Restart daemon clicked — confirm to proceed…'), 'info');
    // window.confirm is synchronous + blocking; the toast above renders
    // first because React flushes state before the alert opens.
    if (!window.confirm(t('diagnostics.restartDaemon.confirm'))) {
      flashToast(t('diagnostics.restartDaemon.cancelled', 'Restart daemon cancelled'), 'info');
      return;
    }
    flashToast(t('diagnostics.restartDaemon.inflight', 'Restarting daemon…'), 'info');
    try {
      const resp = await agentApi.restartDaemon();
      if (!resp.acknowledged) {
        flashToast(t('diagnostics.restartDaemon.blocked', { error: resp.error ?? 'unknown' }), 'error');
        return;
      }
      flashToast(t('diagnostics.restartDaemon.success'), 'success');
      window.setTimeout(() => {
        queryClient.invalidateQueries({ queryKey: ['agent'] });
      }, 12_000);
    } catch (err) {
      flashToast(t('diagnostics.restartDaemon.failed', { error: String(err) }), 'error');
    }
  };

  // Copy support bundle: status snapshot + log tail + applied config to clipboard.
  const onCopySupportBundle = async () => {
    const status = await agentApi.getStatus().catch(() => null);
    const diag = data;
    const policies = await agentApi.getAppliedConfig().catch(() => null);
    const bundle = {
      collectedAt: new Date().toISOString(),
      agent: status?.agent ?? null,
      state: status?.state ?? null,
      gatewayConnected: status?.gatewayConnected ?? null,
      interceptionMode: diag?.interceptionMode ?? null,
      hubReachable: diag?.hubReachable ?? null,
      sync: policies?.sync ?? null,
      logTail: diag?.logTail ?? [],
    };
    const text = JSON.stringify(bundle, null, 2);
    try {
      await navigator.clipboard.writeText(text);
      flashToast(t('diagnostics.copyBundle.success', { size: text.length }));
    } catch {
      flashToast(t('diagnostics.copyBundle.failed'));
    }
  };

  // Copy the log path to clipboard. The Wails webview can't open files
  // directly; the user pastes into Finder / tail / Console.
  const onCopyLogPath = async () => {
    // Daemon doesn't expose the log path via GET_DIAGNOSTICS; use the
    // canonical macOS path. A cross-platform agent should surface
    // platform.DefaultPaths.LogFile on the status snapshot instead.
    const path = '/Library/Logs/com.nexus-gateway.agent/agent.log';
    try {
      await navigator.clipboard.writeText(path);
      flashToast(t('diagnostics.copyLogPath.success'));
    } catch {
      flashToast(t('diagnostics.copyLogPath.failed'));
    }
  };

  return (
    <div className={styles.root}>
      <header>
        <h1 className={styles.title}>{t('diagnostics.title')}</h1>
        <p className={styles.subtitle}>{t('diagnostics.subtitle')}</p>
      </header>

      <section className={styles.card}>
        <header className={styles.cardHeader}>
          <h2 className={styles.h2}>{t('diagnostics.actions.title')}</h2>
          <p className={styles.subtitle}>{t('diagnostics.actions.subtitle')}</p>
        </header>
        <div className={page.row}>
          <button type="button" onClick={onRestartDaemon} className={page.formInput}>
            {t('diagnostics.restartDaemon.button')}
          </button>
          <button type="button" onClick={onCopySupportBundle} className={page.formInput}>
            {t('diagnostics.copyBundle.button')}
          </button>
          <button type="button" onClick={onCopyLogPath} className={page.formInput}>
            {t('diagnostics.copyLogPath.button')}
          </button>
        </div>
        {toast && (
          <div
            role="status"
            aria-live="polite"
            style={{
              marginTop: 'var(--g-space-3)',
              padding: 'var(--g-space-3) var(--g-space-4)',
              borderRadius: 'var(--g-radius-md)',
              border: '1px solid var(--color-border)',
              background: toastTone === 'error'
                ? 'var(--color-danger-bg)'
                : toastTone === 'success'
                  ? 'var(--color-success-bg)'
                  : 'var(--color-info-bg)',
              color: toastTone === 'error'
                ? 'var(--color-danger)'
                : toastTone === 'success'
                  ? 'var(--color-success)'
                  : 'var(--color-info)',
              fontWeight: 'var(--g-font-weight-semibold)',
              fontSize: 'var(--g-font-size-base)',
              display: 'flex',
              alignItems: 'center',
              gap: 'var(--g-space-2)',
            }}
          >
            <span aria-hidden="true">
              {toastTone === 'error' ? '✕' : toastTone === 'success' ? '✓' : '⟳'}
            </span>
            <span>{toast}</span>
          </div>
        )}
      </section>

      <table className={styles.table}>
        <tbody>
          <tr>
            <td className={page.cellLabel}>{t('diagnostics.hubReachable')}</td>
            <td>{isLoading ? '…' : data?.hubReachable ? '✅ Yes' : '❌ No'}</td>
          </tr>
          <tr>
            <td className={page.cellLabel}>{t('diagnostics.certPath')}</td>
            <td className={page.cellValue}>{data?.certPath || '—'}</td>
          </tr>
          <tr>
            <td className={page.cellLabel}>{t('diagnostics.interceptionMode.label')}</td>
            <td>
              {data?.interceptionMode ? (
                <>
                  <span className={page.cellValue}>{data.interceptionMode}</span>
                  {data.interceptionMode === 'SystemProxyFallback' && (
                    <span
                      title={t('diagnostics.interceptionMode.fallbackWarning') ?? ''}
                      className={page.warnIcon}
                    >
                      ⚠
                    </span>
                  )}
                </>
              ) : (
                <span className={page.mutedSmall}>—</span>
              )}
            </td>
          </tr>
        </tbody>
      </table>

      <section>
        <h2 className={page.h2}>{t('diagnostics.logTail')}</h2>
        <pre className={page.logTail}>
          {(data?.logTail ?? []).join('\n') || '—'}
        </pre>
      </section>
    </div>
  );
}
