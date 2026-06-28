import { useTranslation } from 'react-i18next';
import { Card, Switch } from '@/components/ui';
import styles from './SettingsAgentTab.module.css';

interface QuitPolicyCardProps {
  quitAllowed: boolean;
  onQuitToggle: (next: boolean) => void;
  loading: boolean;
}

export function QuitPolicyCard({ quitAllowed, onQuitToggle, loading }: QuitPolicyCardProps) {
  const { t } = useTranslation();

  return (
    <div className={styles.settingsSection}>
      <div className={styles.sectionHeader}>
        <h3 className={styles.sectionTitle}>{t('pages:settings.quitPolicyTitle', 'Agent Quit Policy')}</h3>
        <p className={styles.helpTextSecondary}>
          {t('pages:settings.quitPolicyDesc', "Controls whether employees can turn protection off on their device. Turn off for compliance always-on deployments: the user cannot quit, pause, or sign out of the agent, so monitoring can't be disabled. Signing in always works.")}
        </p>
      </div>
      <Card>
        <label style={{ display: 'flex', alignItems: 'center', gap: 'var(--g-space-3)', cursor: 'pointer' }}>
          <Switch
            checked={quitAllowed}
            onCheckedChange={onQuitToggle}
            disabled={loading}
          />
          <div>
            <div style={{ fontWeight: 'var(--g-font-weight-medium)' }}>
              {t('pages:settings.quitAllowedLabel', 'Allow users to turn off the agent')}
            </div>
            <div className={styles.hintTextMuted}>
              {quitAllowed
                ? t('pages:settings.quitAllowedOnHint', 'Quit, Pause, and Sign Out are available to the user.')
                : t('pages:settings.quitAllowedOffHint', "Always-on: Quit, Pause, and Sign Out are hidden — the user can't turn protection off. Signing in still works.")}
            </div>
          </div>
        </label>
      </Card>
    </div>
  );
}
