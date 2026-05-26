import { useState } from 'react';
import { useTranslation } from 'react-i18next';
import { Button } from '@nexus-gateway/ui-shared';
import styles from './AgentNotRunning.module.css';

/**
 * Full-window fallback shown when the Dashboard cannot reach the
 * agent daemon over the local socket. Triggered by:
 *  - the Wails bridge is missing (Dashboard launched outside of
 *    the bundled .app)
 *  - the socket dial fails (daemon stopped / crashed / not yet
 *    spawned by launchd)
 *  - the daemon answers but returns malformed JSON (unlikely)
 *
 * Retry re-invokes the React Query refetch — on success the App
 * shell takes over within ~one socket round-trip.
 *
 * The troubleshoot block surfaces the manual recovery command for
 * the one scenario launchd's KeepAlive cannot self-recover from:
 * a deliberate `sudo launchctl bootout`. Auto-recovery is not implemented; keep this minimal: tell the user
 * the exact command and let them copy it to clipboard.
 */
export function AgentNotRunning({ onRetry }: { onRetry: () => void }) {
  const { t } = useTranslation();
  const command = t('agentNotRunning.troubleshootCommand');
  const [copied, setCopied] = useState(false);

  async function copy() {
    try {
      await navigator.clipboard.writeText(command);
      setCopied(true);
      // Clear after 2s so the user sees the success acknowledgement.
      setTimeout(() => setCopied(false), 2000);
    } catch {
      // Clipboard API can fail in restricted contexts. Falls back to
      // the user manually selecting the pre block.
    }
  }

  return (
    <div className={styles.root}>
      <div className={styles.card}>
        <div className={styles.icon}>⚠️</div>
        <h1 className={styles.title}>{t('agentNotRunning.title')}</h1>
        <p className={styles.body}>{t('agentNotRunning.body')}</p>
        <Button onClick={onRetry}>{t('agentNotRunning.retry')}</Button>
        <div className={styles.troubleshoot}>
          <span>{t('agentNotRunning.troubleshootHint')}</span>
          <div className={styles.commandRow}>
            <pre className={styles.command}>{command}</pre>
            <button type="button" className={styles.copyButton} onClick={copy}>
              {copied
                ? t('agentNotRunning.copyCommandSuccess')
                : t('agentNotRunning.copyCommand')}
            </button>
          </div>
        </div>
      </div>
    </div>
  );
}
