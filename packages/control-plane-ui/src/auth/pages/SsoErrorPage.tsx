/**
 * SsoErrorPage — terminal landing page for an external-IdP sign-in failure.
 *
 * Rendered at `/auth/sso-error`. The OIDC callback
 * (`/authserver/oidc/callback`) redirects here with `?code=<oauth-error>` when
 * the IdP rejects the authorize request (e.g. Auth0 "parameter organization is
 * required"). The page does NOT auto-redirect — it shows the failure and waits
 * for the operator to click back to sign-in.
 *
 * Security: only the bounded OAuth error code reaches the browser, and it is
 * pattern-gated to a short lowercase token before display. The IdP's free-text
 * `error_description` never leaves the server (it is logged at WARN by the
 * callback) — reflecting it on this unauthenticated page would be a phishing
 * vector. The detailed reason lives in the Control Plane logs.
 */
import { useNavigate, useSearchParams } from 'react-router-dom';
import { useTranslation } from 'react-i18next';
import { useTheme } from '@/theme/useTheme';
import styles from './LoginPage.module.css';

// Standard OAuth error codes are short lowercase tokens (RFC 6749 §4.1.2.1).
// Anything outside this shape is shown as "unknown" so a crafted callback URL
// can't reflect arbitrary text onto this unauthenticated page.
const SAFE_CODE = /^[a-z_]{1,40}$/;

export function SsoErrorPage() {
  const { t } = useTranslation('common');
  const { brand } = useTheme();
  const navigate = useNavigate();
  const [params] = useSearchParams();
  const raw = params.get('code') ?? '';
  const code = SAFE_CODE.test(raw) ? raw : 'unknown';

  return (
    <div className={styles.page}>
      <div className={styles.card}>
        <div className={styles.header}>
          <h1 className={styles.title}>{brand.productName}</h1>
          <p className={styles.subtitle}>{t('callbackFailedTitle')}</p>
        </div>
        <p className={styles.error} role="alert">
          {t('loginErrors.ssoFailed', { code })}
        </p>
        <button
          type="button"
          className={styles.submitBtn}
          // `replace` so Back doesn't return to this terminal page.
          onClick={() => navigate('/login', { replace: true })}
        >
          {t('callbackSignInAgain')}
        </button>
      </div>
    </div>
  );
}

export default SsoErrorPage;
