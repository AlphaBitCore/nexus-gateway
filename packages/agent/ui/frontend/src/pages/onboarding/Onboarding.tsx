import { useState } from 'react';
import { useTranslation } from 'react-i18next';
import { Button } from '@nexus-gateway/ui-shared';
import type { StatusSnapshot } from '@/api/agent';
import { agentApi } from '@/api/agent';
import styles from './Onboarding.module.css';

type Mode = StatusSnapshot['agent']['deviceAuthMode'];

/**
 * First-launch onboarding. Branches on the operator-configured
 * device-auth mode reported by the daemon:
 *
 *  - "enterprise-login" → SSO: kicks off the AUTHENTICATE flow,
 *    which opens the user's default browser for PKCE OAuth.
 *  - "mtls-only"        → Token paste: a single-use enrollment
 *    token from the CP admin → ENROLL_TOKEN.
 *  - empty / unknown    → spinner; the daemon is still resolving
 *    the deployment mode via Hub's bootstrap endpoint.
 */
export interface OnboardingProps {
  status: StatusSnapshot;
  /**
   * Called the moment the user submits an enrollment attempt (token
   * paste or SSO start / confirm). The App component uses this to
   * decide when to render its `Reconnecting` grace screen during
   * the daemon's launchd respawn — without this signal, the App
   * can't tell a real user-initiated enrollment from a passive
   * "device happened to be unenrolled" state.
   */
  onEnrollmentAttempted: () => void;
}

export function Onboarding({ status, onEnrollmentAttempted }: OnboardingProps) {
  const { t } = useTranslation();
  const mode: Mode = status.agent.deviceAuthMode;

  return (
    <div className={styles.root}>
      <div className={styles.card}>
        <h1 className={styles.title}>{t('onboarding.welcomeTitle')}</h1>
        <p className={styles.subtitle}>{t('onboarding.welcomeSubtitle')}</p>

        {mode === 'enterprise-login' && <SSOBranch onEnrollmentAttempted={onEnrollmentAttempted} />}
        {mode === 'mtls-only' && <TokenBranch onEnrollmentAttempted={onEnrollmentAttempted} />}
        {!mode && <DiscoveringBranch />}
      </div>
    </div>
  );
}

function SSOBranch({ onEnrollmentAttempted }: { onEnrollmentAttempted: () => void }) {
  const { t } = useTranslation();
  const [running, setRunning] = useState(false);
  const [error, setError] = useState<string | null>(null);
  const [confirmation, setConfirmation] = useState<{ deviceID: string; message: string } | null>(null);

  async function start() {
    setError(null);
    setRunning(true);
    // Mark the moment the user kicked off enrollment — the App's
    // grace window starts now and bridges the IPC outage caused by
    // the daemon's eventual launchd-driven restart.
    onEnrollmentAttempted();
    try {
      const resp = await agentApi.authenticateSSO();
      if (resp.error) {
        setError(resp.error);
        return;
      }
      if (resp.confirmation_required) {
        setConfirmation({ deviceID: resp.device_id ?? '', message: resp.message ?? '' });
        return;
      }
      // No confirmation needed → daemon ran the flow inline. The
      // status query will pick up the new deviceID on its next
      // refetch and the App component swaps in the Shell.
    } catch (err) {
      setError(err instanceof Error ? err.message : t('onboarding.errorUnknown'));
    } finally {
      setRunning(false);
    }
  }

  async function confirm() {
    setError(null);
    setRunning(true);
    onEnrollmentAttempted(); // refresh the grace window when the user actually commits
    try {
      const resp = await agentApi.authenticateConfirm();
      if (resp.error) setError(resp.error);
    } catch (err) {
      setError(err instanceof Error ? err.message : t('onboarding.errorUnknown'));
    } finally {
      setRunning(false);
    }
  }

  async function cancel() {
    await agentApi.authenticateCancel();
    setConfirmation(null);
    setRunning(false);
  }

  return (
    <div className={styles.branch}>
      <h2 className={styles.h2}>{t('onboarding.ssoTitle')}</h2>
      <p className={styles.body}>{t('onboarding.ssoBody')}</p>
      {error && <div className={styles.error}>{error}</div>}
      {confirmation ? (
        <div className={styles.actions}>
          <Button variant="ghost" onClick={cancel}>{t('shared:actions.cancel')}</Button>
          <Button onClick={confirm} loading={running}>{t('shared:actions.continue', 'Continue')}</Button>
        </div>
      ) : (
        <Button onClick={start} loading={running}>{t('onboarding.ssoTitle')}</Button>
      )}
    </div>
  );
}

function TokenBranch({ onEnrollmentAttempted }: { onEnrollmentAttempted: () => void }) {
  const { t } = useTranslation();
  const [token, setToken] = useState('');
  const [state, setState] = useState<'idle' | 'running' | 'success' | 'error'>('idle');
  const [error, setError] = useState<string | null>(null);

  async function submit(e: React.FormEvent) {
    e.preventDefault();
    const value = token.trim();
    if (!value) return;
    setState('running');
    setError(null);
    onEnrollmentAttempted();
    try {
      const resp = await agentApi.enrollWithToken(value);
      if (resp.success) {
        setState('success');
      } else {
        setState('error');
        setError(resp.error ?? t('onboarding.errorUnknown'));
      }
    } catch (err) {
      setState('error');
      setError(err instanceof Error ? err.message : t('onboarding.errorUnknown'));
    }
  }

  if (state === 'running') {
    return (
      <div className={styles.branch}>
        <p className={styles.body}>{t('onboarding.tokenRunning')}</p>
      </div>
    );
  }
  if (state === 'success') {
    return (
      <div className={styles.branch}>
        <div className={styles.success}>{t('onboarding.tokenSuccess')}</div>
      </div>
    );
  }

  return (
    <form className={styles.branch} onSubmit={submit}>
      <h2 className={styles.h2}>{t('onboarding.tokenTitle')}</h2>
      <p className={styles.body}>{t('onboarding.tokenBody')}</p>
      {error && (
        <div className={styles.error}>
          <strong>{t('onboarding.tokenFailure')}</strong>
          <div>{error}</div>
        </div>
      )}
      <input
        type="text"
        autoComplete="off"
        autoCorrect="off"
        spellCheck={false}
        className={styles.input}
        placeholder={t('onboarding.tokenPlaceholder')}
        value={token}
        onChange={(e) => setToken(e.target.value)}
      />
      <Button type="submit" disabled={!token.trim()}>
        {t('onboarding.tokenSubmit')}
      </Button>
    </form>
  );
}

function DiscoveringBranch() {
  const { t } = useTranslation();
  return (
    <div className={styles.branch}>
      <h2 className={styles.h2}>{t('onboarding.discoveringTitle')}</h2>
      <p className={styles.body}>{t('onboarding.discoveringHint')}</p>
    </div>
  );
}
