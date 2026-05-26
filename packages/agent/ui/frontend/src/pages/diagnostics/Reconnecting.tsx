import { useTranslation } from 'react-i18next';
import styles from './Reconnecting.module.css';

/**
 * Transient "we just enrolled, daemon is respawning" screen. Shown
 * for the ENROLLMENT_GRACE_MS window after Onboarding submits a
 * token / completes SSO. If the daemon doesn't come back inside
 * the window the App falls through to AgentNotRunning so the user
 * has a real action.
 */
export function Reconnecting() {
  const { t } = useTranslation();
  return (
    <div className={styles.root}>
      <div className={styles.card}>
        <div className={styles.icon}>⏳</div>
        <h1 className={styles.title}>{t('reconnecting.title', 'Finishing setup')}</h1>
        <p className={styles.body}>{t('reconnecting.body', 'The agent is restarting with your new device certificate. This usually takes a few seconds.')}</p>
      </div>
    </div>
  );
}
