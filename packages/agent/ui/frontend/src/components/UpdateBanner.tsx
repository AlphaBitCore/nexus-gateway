import { useEffect, useState } from 'react';
import { useTranslation } from 'react-i18next';
import type { StatusSnapshot } from '@/api/agent';
import styles from './UpdateBanner.module.css';

/**
 * UpdateBanner — yellow page-top banner shown when the daemon's
 * updater reports a newer build available on Hub. Drives manual
 * install in deployments where auto-install is disabled (no
 * Ed25519 public key configured, admin-disabled, etc.) and serves
 * as a visible confirmation that updates were detected for
 * deployments where auto-install IS enabled.
 *
 * Dismiss state lives in localStorage with a 24h cooldown so a user
 * who explicitly clicks "Remind tomorrow" doesn't see the banner
 * again until the next day. "Install" opens the canonical agent
 * download URL — the actual install flow varies per platform (.pkg
 * on macOS, .deb on Linux, etc.) and is the user's choice.
 */
export function UpdateBanner({ status }: { status: StatusSnapshot | undefined }) {
  const { t } = useTranslation();
  const [dismissedUntil, setDismissedUntil] = useState<number>(() => {
    const v = localStorage.getItem('updateBanner.dismissedUntil');
    return v ? Number(v) : 0;
  });

  // Re-evaluate dismissal every minute so the banner reappears the
  // moment the cooldown expires, without forcing a page reload.
  useEffect(() => {
    const id = window.setInterval(() => {
      const v = localStorage.getItem('updateBanner.dismissedUntil');
      setDismissedUntil(v ? Number(v) : 0);
    }, 60_000);
    return () => window.clearInterval(id);
  }, []);

  if (!status?.agent?.updateAvailable) return null;
  if (Date.now() < dismissedUntil) return null;

  // Daemon-served URL — composed from the operator-configured cpURL
  // in agent.yaml + a platform-specific path suffix. Empty string
  // when cpURL is unset; hide the install button rather than open a
  // broken link.
  const downloadURL = status.agent.downloadURL;
  if (!downloadURL) return null;

  const onInstall = () => {
    window.open(downloadURL, '_blank');
  };

  const onDismiss = () => {
    // 24h cooldown — long enough to not nag, short enough that the
    // user actually returns to a working install path the next day.
    const until = Date.now() + 24 * 60 * 60 * 1000;
    localStorage.setItem('updateBanner.dismissedUntil', String(until));
    setDismissedUntil(until);
  };

  return (
    <div className={styles.banner} role="alert">
      <span className={styles.icon} aria-hidden>↓</span>
      <span className={styles.message}>{t('updateBanner.message')}</span>
      <div className={styles.actions}>
        <button type="button" className={styles.primary} onClick={onInstall}>
          {t('updateBanner.install')}
        </button>
        <button type="button" className={styles.secondary} onClick={onDismiss}>
          {t('updateBanner.dismiss')}
        </button>
      </div>
    </div>
  );
}
