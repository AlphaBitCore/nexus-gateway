import { useTranslation } from 'react-i18next';
import clsx from 'clsx';
import { Card, Stack, Switch } from '@/components/ui';
import styles from './SettingsAgentTab.module.css';

interface ShutdownWarningCardProps {
  quitAllowed: boolean;
  shutdownWarningEnabled: boolean;
  onShutdownWarningEnabledChange: (next: boolean) => void;
  locales: ReadonlyArray<{ key: string; label: string }>;
  activeLocale: string;
  onActiveLocaleChange: (key: string) => void;
  warnings: Record<string, string>;
  onWarningChange: (value: string) => void;
  loading: boolean;
  shutdownWarningData: Record<string, string> | undefined;
}

export function ShutdownWarningCard({
  quitAllowed,
  shutdownWarningEnabled,
  onShutdownWarningEnabledChange,
  locales,
  activeLocale,
  onActiveLocaleChange,
  warnings,
  onWarningChange,
  loading,
  shutdownWarningData,
}: ShutdownWarningCardProps) {
  const { t } = useTranslation();

  return (
    <div className={styles.settingsSection}>
      <div className={styles.sectionHeader}>
        <h3 className={styles.sectionTitle}>{t('pages:settings.agentShutdownWarningTitle')}</h3>
        <p className={styles.helpTextSecondary}>
          {quitAllowed
            ? t('pages:settings.agentShutdownWarningDesc')
            : t('pages:settings.shutdownWarningDisabledHint', 'This text only appears when "Allow users to turn off the agent" is turned on. Edit it now so the message is ready.')}
        </p>
      </div>

      <Card>
        <Stack gap="md">
        {/* shutdownWarningEnabled gate. Admin can prepare the
            warning text but suppress the dialog (text saved but
            not shown). Mirrors the field the agent's
            agent_settings handler now reads (see #83). */}
        <label style={{ display: 'flex', alignItems: 'center', gap: 'var(--g-space-3)', cursor: 'pointer' }}>
          <Switch
            checked={shutdownWarningEnabled}
            onCheckedChange={onShutdownWarningEnabledChange}
            disabled={loading}
          />
          <div style={{ fontWeight: 'var(--g-font-weight-medium)' }}>
            {t('pages:settings.shutdownWarningEnabledLabel', 'Show this warning when the user clicks Quit')}
          </div>
        </label>

        {/* Locale tabs */}
        <div className={styles.tabRow}>
          {locales.map(loc => (
            <button
              key={loc.key}
              onClick={() => onActiveLocaleChange(loc.key)}
              className={clsx(styles.localeTab, activeLocale === loc.key && styles.localeTabActive)}
            >
              {loc.label}
            </button>
          ))}
        </div>

        <textarea
          value={warnings[activeLocale] ?? ''}
          onChange={e => onWarningChange(e.target.value)}
          rows={4}
          className={styles.warningTextarea}
          placeholder={t('pages:settings.agentShutdownWarningPlaceholder')}
          disabled={loading}
        />

        {/* When the live blob has no shutdownWarning field yet (or this
            locale is empty after the merge) tell the user we're showing a
            default placeholder so a "Save" click doesn't seem surprising. */}
        {!shutdownWarningData?.[activeLocale] && (
          <p className={styles.helpTextSecondarySmall}>
            {t('pages:settings.agentWarningUsingDefault')}
          </p>
        )}
        </Stack>
      </Card>
    </div>
  );
}
