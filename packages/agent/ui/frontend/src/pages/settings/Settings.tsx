/**
 * Settings page — fleet-wide per-device preferences.
 *
 * Four cards:
 *   1. Theme       — light / dark / system (binds to ThemeProvider).
 *   2. Language    — en / zh / es (persists via i18n.changeLanguage).
 *   3. Pause       — 15min / 1h / 8h / Until I resume buttons.
 *   4. Sign out    — destructive action, gated behind a confirm dialog.
 */
import { useState } from 'react';
import { useTranslation } from 'react-i18next';
import { useQueryClient } from '@tanstack/react-query';
import { Button, ErrorBanner } from '@nexus-gateway/ui-shared';
import type { StatusSnapshot } from '@/api/agent';
import { agentApi } from '@/api/agent';
import { useTheme } from '@/theme/ThemeProvider';
import type { ThemeMode } from '@/theme/ThemeProvider';
import { SUPPORTED_LANGUAGES, setLanguage } from '@/i18n';
import type { LanguageCode } from '@/i18n';
import { AccountPanel, AboutFooter } from '../activity/AccountPanel';
import styles from './Settings.module.css';

const PAUSE_OPTIONS: Array<{ key: '15min' | '1h' | '8h' | 'until'; seconds: number }> = [
  { key: '15min', seconds: 15 * 60 },
  { key: '1h', seconds: 60 * 60 },
  { key: '8h', seconds: 8 * 60 * 60 },
  { key: 'until', seconds: 0 },
];

const PAUSE_LABEL_KEY = {
  '15min': 'settings.pauseDuration.15min',
  '1h': 'settings.pauseDuration.1hour',
  '8h': 'settings.pauseDuration.8hours',
  until: 'settings.pauseDuration.indefinite',
} as const;

export function Settings({ status }: { status: StatusSnapshot }) {
  const { t, i18n } = useTranslation();
  const queryClient = useQueryClient();
  const { mode, setMode } = useTheme();

  const [busy, setBusy] = useState(false);
  const [error, setError] = useState<string | null>(null);
  const [confirmSignOut, setConfirmSignOut] = useState(false);
  const [signOutInFlight, setSignOutInFlight] = useState(false);
  const [updateCheck, setUpdateCheck] = useState<{ checking: boolean; result?: string }>({ checking: false });

  const paused = status.paused;
  const pausedUntil = status.pausedUntil;

  async function doPause(seconds: number) {
    setBusy(true);
    setError(null);
    try {
      const resp = await agentApi.pauseProtection(seconds);
      if (resp.error) setError(resp.error);
    } catch (err) {
      setError(err instanceof Error ? err.message : String(err));
    } finally {
      setBusy(false);
      queryClient.invalidateQueries({ queryKey: ['agent', 'status'] });
    }
  }

  async function doResume() {
    setBusy(true);
    setError(null);
    try {
      const resp = await agentApi.resumeProtection();
      if (resp.error) setError(resp.error);
    } catch (err) {
      setError(err instanceof Error ? err.message : String(err));
    } finally {
      setBusy(false);
      queryClient.invalidateQueries({ queryKey: ['agent', 'status'] });
    }
  }

  async function doUpdateCheck() {
    setUpdateCheck({ checking: true });
    setError(null);
    try {
      const resp = await agentApi.checkUpdate();
      if (resp.error) {
        setError(resp.error);
        setUpdateCheck({ checking: false });
        return;
      }
      setUpdateCheck({
        checking: false,
        result: resp.available
          ? t('settings.updates.foundNew', { version: resp.version ?? '—' })
          : t('settings.updates.upToDate'),
      });
      queryClient.invalidateQueries({ queryKey: ['agent', 'status'] });
    } catch (err) {
      setError(err instanceof Error ? err.message : String(err));
      setUpdateCheck({ checking: false });
    }
  }

  async function doSignOut() {
    setSignOutInFlight(true);
    setError(null);
    try {
      const resp = await agentApi.unenroll();
      if (!resp.acknowledged) {
        setError(t('settings.signOutFailed', { error: resp.error ?? 'unknown' }));
        setConfirmSignOut(false);
        return;
      }
      // Daemon will exit ~200ms after the ack lands. The App.tsx
      // polling layer will see the dropped socket and switch to the
      // AgentNotRunning / Reconnecting screen; the launchd respawn
      // brings the agent back in pre-enrollment mode, which the App
      // then routes to Onboarding. We don't navigate manually here —
      // the existing branching does the right thing.
      queryClient.invalidateQueries({ queryKey: ['agent', 'status'] });
    } catch (err) {
      setError(t('settings.signOutFailed', {
        error: err instanceof Error ? err.message : String(err),
      }));
    } finally {
      setSignOutInFlight(false);
      setConfirmSignOut(false);
    }
  }

  const THEME_OPTIONS: Array<{ value: ThemeMode; labelKey: string }> = [
    { value: 'light', labelKey: 'settings.themeLight' },
    { value: 'dark', labelKey: 'settings.themeDark' },
    { value: 'system', labelKey: 'settings.themeSystem' },
  ];

  return (
    <div className={styles.root}>
      <header>
        <h1 className={styles.title}>{t('settings.title')}</h1>
        <p className={styles.subtitle}>{t('settings.subtitle')}</p>
      </header>

      {error && (
        <ErrorBanner
          message={error}
          onDismiss={() => setError(null)}
          dismissLabel={t('shared:actions.dismiss')}
        />
      )}

      <h2 className={styles.groupTitle}>{t('settings.groupAccount')}</h2>
      <AccountPanel status={status} />

      <h2 className={styles.groupTitle}>{t('settings.groupPreferences')}</h2>
      <section className={styles.card}>
        <h3 className={styles.cardTitle}>{t('settings.themeTitle')}</h3>
        <p className={styles.cardDesc}>{t('settings.themeDesc')}</p>
        <div className={styles.segmentedRow}>
          {THEME_OPTIONS.map((opt) => (
            <Button
              key={opt.value}
              variant={mode === opt.value ? 'primary' : 'secondary'}
              onClick={() => setMode(opt.value)}
            >
              {t(opt.labelKey)}
            </Button>
          ))}
        </div>
      </section>
      <section className={styles.card}>
        <h3 className={styles.cardTitle}>{t('settings.languageTitle')}</h3>
        <p className={styles.cardDesc}>{t('settings.languageDesc')}</p>
        <div className={styles.segmentedRow}>
          {SUPPORTED_LANGUAGES.map((lang) => (
            <Button
              key={lang.code}
              variant={i18n.language === lang.code ? 'primary' : 'secondary'}
              onClick={() => setLanguage(lang.code as LanguageCode)}
            >
              {lang.label}
            </Button>
          ))}
        </div>
      </section>

      {/* Group: Protection. Pause / Resume is the only operator
          toggle that changes daemon behaviour, so it gets its own
          group above the destructive Danger zone. */}
      <h2 className={styles.groupTitle}>{t('settings.groupProtection')}</h2>
      <section className={styles.card}>
        <h3 className={styles.cardTitle}>
          {paused ? t('settings.resume') : t('settings.pause')}
        </h3>
        {paused ? (
          <div className={styles.pausedRow}>
            <Button onClick={doResume} loading={busy} variant="primary">
              {t('settings.resume')}
            </Button>
            {pausedUntil && (
              <span className={styles.muted}>
                {t('settings.pausedUntil', {
                  time: new Date(pausedUntil).toLocaleString(),
                })}
              </span>
            )}
          </div>
        ) : (
          <div className={styles.pauseGrid}>
            {PAUSE_OPTIONS.map(({ key, seconds }) => (
              <Button
                key={key}
                variant="secondary"
                onClick={() => doPause(seconds)}
                loading={busy}
              >
                {t(PAUSE_LABEL_KEY[key])}
              </Button>
            ))}
          </div>
        )}
      </section>

      <section className={styles.card}>
        <h3 className={styles.cardTitle}>{t('settings.updates.title')}</h3>
        <p className={styles.cardDesc}>{t('settings.updates.desc')}</p>
        <div className={styles.segmentedRow}>
          <Button
            variant="secondary"
            onClick={doUpdateCheck}
            loading={updateCheck.checking}
          >
            {t('settings.updates.checkNow')}
          </Button>
          {updateCheck.result && (
            <span className={styles.muted}>{updateCheck.result}</span>
          )}
        </div>
      </section>

      {/* Group: Danger zone — Sign Out is destructive (loses
          enrollment). The dedicated section header + the .dangerCard
          variant put a 2-step visual barrier between the user and
          the action. */}
      <h2 className={styles.groupTitle}>{t('settings.groupDanger')}</h2>
      <section className={styles.dangerCard}>
        <h2 className={styles.cardTitle}>{t('settings.signOutTitle')}</h2>
        <p className={styles.cardDesc}>{t('settings.signOutDesc')}</p>
        {!confirmSignOut ? (
          <div>
            <Button variant="danger" onClick={() => setConfirmSignOut(true)}>
              {t('settings.signOutAction')}
            </Button>
          </div>
        ) : (
          <>
            <p style={{ margin: 'var(--g-space-0)', fontWeight: 'var(--g-font-weight-medium)' }}>
              {t('settings.signOutConfirmTitle')}
            </p>
            <p className={styles.cardDesc}>
              {t('settings.signOutConfirmBody')}
            </p>
            <div className={styles.segmentedRow}>
              <Button
                variant="danger"
                onClick={doSignOut}
                loading={signOutInFlight}
              >
                {signOutInFlight
                  ? t('settings.signOutInProgress')
                  : t('settings.signOutConfirmAction')}
              </Button>
              <Button
                variant="ghost"
                onClick={() => setConfirmSignOut(false)}
                disabled={signOutInFlight}
              >
                {t('settings.signOutConfirmCancel')}
              </Button>
            </div>
          </>
        )}
      </section>

      <AboutFooter status={status} />
    </div>
  );
}
