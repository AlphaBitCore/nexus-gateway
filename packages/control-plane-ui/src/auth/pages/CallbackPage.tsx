/**
 * OAuth callback landing page.
 *
 * Rendered only at `/auth/callback`. On mount:
 *   1. Parse `?code=...&state=...` (or `?error=...`) from the URL.
 *   2. Validate state against the verifier blob in sessionStorage.
 *   3. POST /oauth/token with the verifier + code.
 *   4. Hand the fresh TokenPair to AuthContext.completeLogin() which also
 *      populates user identity from /api/admin/me.
 *   5. Navigate to the path the user was heading for when /login started.
 *
 * On any error (provider rejection, state mismatch, network failure) render an
 * inline error panel with a "Sign in again" action that restarts a fresh
 * PKCE flow — preserving the intended postAuthPath where possible.
 */
import { useEffect, useState } from 'react';
import { useLocation, useNavigate } from 'react-router-dom';
import { useTranslation } from 'react-i18next';
import { LoadingSpinner } from '@/components/ui';
import { useTheme } from '@/theme/useTheme';
import { useAuth } from '../context/AuthContext';
import { consumeCallback } from '../pkce/pkceFlow';
import styles from './LoginPage.module.css';

type CallbackState = { status: 'pending' } | { status: 'error'; message: string };

export function CallbackPage() {
  const { t } = useTranslation('common');
  const { brand } = useTheme();
  const navigate = useNavigate();
  const location = useLocation();
  const { completeLogin, login } = useAuth();
  const [state, setState] = useState<CallbackState>({ status: 'pending' });

  useEffect(() => {
    let cancelled = false;
    (async () => {
      try {
        const { tokens, postAuthPath } = await consumeCallback(location.search);
        if (cancelled) return;
        await completeLogin(tokens);
        if (cancelled) return;
        // `replace: true` so Back doesn't return to the callback URL (the code
        // has already been consumed and would fail on replay).
        navigate(postAuthPath || '/', { replace: true });
      } catch (err) {
        if (cancelled) return;
        const message = err instanceof Error ? err.message : String(err);
        setState({ status: 'error', message });
      }
    })();
    return () => {
      cancelled = true;
    };
  }, [location.search, completeLogin, navigate]);

  if (state.status === 'pending') {
    return (
      <div className={styles.page} role="status" aria-live="polite" aria-busy="true">
        <div className={styles.card}>
          <div className={styles.header}>
            <h1 className={styles.title}>{brand.productName}</h1>
            <p className={styles.subtitle}>{t('callbackCompletingSignIn')}</p>
          </div>
          <div style={{ display: 'flex', justifyContent: 'center', padding: 'var(--g-space-4) 0' }}>
            <LoadingSpinner size="lg" />
          </div>
        </div>
      </div>
    );
  }

  return (
    <div className={styles.page}>
      <div className={styles.card}>
        <div className={styles.header}>
          <h1 className={styles.title}>{brand.productName}</h1>
          <p className={styles.subtitle}>{t('callbackFailedTitle')}</p>
        </div>
        <p className={styles.error} role="alert">
          {state.message}
        </p>
        <button
          type="button"
          className={styles.submitBtn}
          onClick={() => {
            void login('/');
          }}
        >
          {t('callbackSignInAgain')}
        </button>
      </div>
    </div>
  );
}

export default CallbackPage;
