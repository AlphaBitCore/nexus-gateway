/**
 * LoginPage — frameless centered sign-in.
 * No card wrapper; typography + spacing create visual structure.
 */

import { FormEvent, useEffect, useMemo, useState } from 'react';
import { Link, useLocation, useNavigate, useSearchParams } from 'react-router-dom';
import { useTranslation } from 'react-i18next';
import { Input, LoadingSpinner } from '@/components/ui';
import {
  authApi,
  AuthserverError,
  type AuthserverErrorCode,
  type IdpEntry,
} from '@/api/services';
import { withPrefix } from '@/lib/deploymentPrefix';
import { useTheme } from '@/theme/useTheme';
import { useAuth } from '../context/AuthContext';
import { getAccessToken } from '../tokens/tokenStore';
import { SUPPORTED_LANGUAGES, LANGUAGE_STORAGE_KEY } from '../../i18n';
import styles from './LoginPage.module.css';

function errorKeyFor(code: AuthserverErrorCode): string {
  switch (code) {
    case 'invalid_credentials':   return 'loginErrors.invalidCredentials';
    case 'user_disabled':         return 'loginErrors.userDisabled';
    case 'authctx_expired':       return 'loginErrors.authctxExpired';
    case 'rate_limited':          return 'loginErrors.rateLimited';
    case 'internal_error':
    default:                      return 'loginErrors.internalError';
  }
}

export function LoginPage() {
  const { t, i18n } = useTranslation();
  const { login, status } = useAuth();
  const { brand } = useTheme();
  const navigate = useNavigate();
  const location = useLocation();
  const [searchParams] = useSearchParams();
  const authctx = searchParams.get('authctx') ?? '';

  const currentLang = i18n.language;
  const changeLang = (code: string) => {
    void i18n.changeLanguage(code);
    localStorage.setItem(LANGUAGE_STORAGE_KEY, code);
  };

  const postAuthPath = useMemo(() => {
    const st = location.state as { from?: { pathname?: string; search?: string; hash?: string } } | null;
    const loc = st?.from;
    if (loc?.pathname && loc.pathname !== '/login') {
      return `${loc.pathname}${loc.search ?? ''}${loc.hash ?? ''}`;
    }
    return '/';
  }, [location.state]);

  // witchIDP=<idp_id|true> — when present, silently start SSO via the named
  // (or first available) external IdP without user interaction. Intent is
  // persisted in sessionStorage so it survives the /oauth/authorize round-trip
  // that mints a fresh authctx (the param is not in the URL on return).
  const witchIdp = searchParams.get('witchIDP') ?? '';

  const [mounted, setMounted] = useState(false);
  const [providers, setProviders] = useState<IdpEntry[]>([]);
  const [loadErrorKey, setLoadErrorKey] = useState<string | null>(null);
  const [providersLoading, setProvidersLoading] = useState(true);
  const [authctxRecoveryTriggered, setAuthctxRecoveryTriggered] = useState(false);
  const [switchIdpTriggered, setSwitchIdpTriggered] = useState(false);
  const [email, setEmail] = useState('');
  const [password, setPassword] = useState('');
  const [submitting, setSubmitting] = useState(false);
  const [formErrorKey, setFormErrorKey] = useState<string | null>(null);

  const localProvider = useMemo(
    () => providers.find((p) => p.type === 'local') ?? null,
    [providers],
  );
  const externalProviders = useMemo(
    () => providers.filter((p) => p.type !== 'local'),
    [providers],
  );

  useEffect(() => {
    // Two cases when the SPA is already authenticated:
    //
    //  1. No authctx — the operator just navigated to /login, send them on
    //     to wherever they came from (or the dashboard).
    //
    //  2. An authctx IS present — they were bounced here by /oauth/authorize
    //     (e.g. nexus-cli's PKCE flow opened a browser tab while a CP-UI
    //     session is live in the same browser). Navigating to "/" would drop
    //     the OAuth flow and leave the CLI's loopback listener hanging. The
    //     right move is to consent on the operator's behalf via the bearer
    //     session and navigate the browser to the registered redirect_uri so
    //     the CLI's loopback callback fires. On any failure (expired authctx,
    //     missing token, server error) we fall through to the normal login
    //     form rather than wedge the user on a blank screen.
    if (status !== 'authenticated') return;
    if (!authctx) {
      navigate(postAuthPath, { replace: true });
      return;
    }
    const token = getAccessToken();
    if (!token) {
      navigate(postAuthPath, { replace: true });
      return;
    }
    let cancelled = false;
    authApi.approveAuthctx(authctx, token).then(
      ({ redirectUri }) => {
        if (cancelled) return;
        window.location.assign(redirectUri);
      },
      (err) => {
        if (cancelled) return;
        // authctx no longer valid (expired / one-shot already consumed)
        // — restart the dance instead of stranding the operator on /login.
        if (err instanceof AuthserverError && err.code === 'authctx_expired') {
          void login(postAuthPath);
          return;
        }
        // Anything else: drop the authctx and send them home; they can
        // re-run `nexus login` to mint a fresh one.
        navigate(postAuthPath, { replace: true });
      },
    );
    return () => {
      cancelled = true;
    };
  }, [status, authctx, navigate, postAuthPath, login]);

  useEffect(() => {
    const id = setTimeout(() => setMounted(true), 50);
    return () => clearTimeout(id);
  }, []);

  useEffect(() => {
    if (status === 'loading' || status === 'authenticated') return;
    if (authctx) return;
    // Persist witchIDP intent before the /oauth/authorize round-trip strips it
    // from the URL. The returning /login?authctx=xxx will no longer carry the
    // param; sessionStorage bridges the gap.
    if (witchIdp) sessionStorage.setItem('_nexus_switch_idp', witchIdp);
    void login(postAuthPath);
  }, [authctx, login, postAuthPath, status, witchIdp]);

  // Auto-start SSO once authctx is live and providers are loaded.
  // Reads the witchIDP intent from sessionStorage (set above before the
  // /oauth/authorize redirect). If the stored value matches a known IdP id we
  // use that provider; otherwise we fall back to the first external provider.
  // The intent is cleared immediately so a manual page-refresh doesn't re-fire.
  useEffect(() => {
    if (!authctx || providersLoading || loadErrorKey) return;
    if (externalProviders.length === 0) return;
    if (switchIdpTriggered) return;
    const intent = sessionStorage.getItem('_nexus_switch_idp');
    if (!intent) return;
    sessionStorage.removeItem('_nexus_switch_idp');
    setSwitchIdpTriggered(true);
    const target = externalProviders.find((p) => p.id === intent) ?? externalProviders[0];
    if (!target) return;
    const url = new URL(withPrefix(`/authserver/idp/${encodeURIComponent(target.id)}/start`), window.location.origin);
    url.searchParams.set('authctx', authctx);
    window.location.assign(url.toString());
  }, [authctx, providersLoading, loadErrorKey, externalProviders, switchIdpTriggered]);

  useEffect(() => {
    if (!authctx) return;
    let cancelled = false;
    setProvidersLoading(true);
    setLoadErrorKey(null);
    authApi
      .listIdps(authctx)
      .then((res) => { if (!cancelled) setProviders(res.providers); })
      .catch((err) => {
        if (cancelled) return;
        setLoadErrorKey(err instanceof AuthserverError ? errorKeyFor(err.code) : 'loginErrors.loadProvidersFailed');
      })
      .finally(() => { if (!cancelled) setProvidersLoading(false); });
    return () => { cancelled = true; };
  }, [authctx]);

  useEffect(() => {
    if (status === 'authenticated') return;
    if (loadErrorKey !== 'loginErrors.authctxExpired') return;
    if (authctxRecoveryTriggered) return;
    setAuthctxRecoveryTriggered(true);
    void login(postAuthPath);
  }, [authctxRecoveryTriggered, loadErrorKey, login, postAuthPath, status]);

  const handleExternal = (provider: IdpEntry) => {
    const url = new URL(withPrefix(`/authserver/idp/${encodeURIComponent(provider.id)}/start`), window.location.origin);
    url.searchParams.set('authctx', authctx);
    window.location.assign(url.toString());
  };

  const handlePasswordSubmit = async (ev: FormEvent<HTMLFormElement>) => {
    ev.preventDefault();
    if (!authctx || submitting) return;
    setSubmitting(true);
    setFormErrorKey(null);
    try {
      const { redirectUri } = await authApi.submitPassword(authctx, email, password);
      window.location.assign(redirectUri);
    } catch (err) {
      // authctx expired between page load and submit: silently restart
      // the OAuth dance to mint a fresh authctx — same self-heal as the
      // listIdps path. The user lands back on /login with a new authctx
      // and retypes; no confusing "start from the top" wording shown.
      if (err instanceof AuthserverError && err.code === 'authctx_expired') {
        void login(postAuthPath);
        return;
      }
      setFormErrorKey(err instanceof AuthserverError ? errorKeyFor(err.code) : 'loginErrors.internalError');
      setSubmitting(false);
    }
  };

  const isRedirecting = !authctx || loadErrorKey === 'loginErrors.authctxExpired' || authctxRecoveryTriggered || switchIdpTriggered;
  const isLoading = isRedirecting || (!loadErrorKey && providersLoading);
  const hasBothSides = Boolean(localProvider) && externalProviders.length > 0;

  return (
    <div className={styles.page}>
      {/* Large decorative watermark — brand presence without a card.
          When the theme defines a watermark URL we render it; otherwise
          we draw the GenericLogoWatermark (neutral, doesn't impersonate
          any specific brand). */}
      {brand.logoWatermark ? (
        <img src={brand.logoWatermark} alt="" className={styles.watermark} aria-hidden="true" />
      ) : (
        <GenericLogoWatermark className={styles.watermark} />
      )}

      <div className={`${styles.authInner} ${mounted ? styles.authMounted : styles.authMounting}`}>

        {/* Brand lockup — logo + product name + tagline come from the
            active theme (brand.* fields). Themes without a logoMark fall
            back to a neutral abstract glyph (GenericLogoMark) so we never
            burn a Nexus-specific shape into another customer's UI. */}
        <div className={styles.brand}>
          {brand.logoMark ? (
            <img src={brand.logoMark} alt="" className={styles.brandMark} width={26} height={26} />
          ) : (
            <GenericLogoMark className={styles.brandMark} />
          )}
          <span className={styles.brandName}>{brand.productName}</span>
        </div>
        {brand.tagline && <p className={styles.brandSlug}>{brand.tagline}</p>}

        {isLoading && (
          <div className={styles.redirecting}>
            <LoadingSpinner size="md" message="" />
          </div>
        )}

        {!isRedirecting && loadErrorKey && loadErrorKey !== 'loginErrors.authctxExpired' && (
          <>
            <p className={styles.inlineError}>{t(loadErrorKey)}</p>
            <button type="button" className={styles.submitBtn} onClick={() => void login(postAuthPath)}>
              {t('callbackSignInAgain')}
            </button>
          </>
        )}

        {!isLoading && !loadErrorKey && (
          <>
            {externalProviders.length > 0 && (
              <div className={styles.ssoList}>
                {externalProviders.map((provider) => (
                  <button key={provider.id} type="button" className={styles.ssoBtn} onClick={() => handleExternal(provider)}>
                    {t('signInWith', { provider: provider.name })}
                  </button>
                ))}
              </div>
            )}

            {hasBothSides && (
              <div className={styles.ssoDivider}>
                <span className={styles.ssoDividerLabel}>{t('orDivider')}</span>
              </div>
            )}

            {localProvider && (
              <form className={styles.passwordForm} onSubmit={handlePasswordSubmit} noValidate aria-label={t('signIn')}>
                <div className={styles.field}>
                  <label className={styles.fieldLabel} htmlFor="login-email">
                    {t('email')}
                  </label>
                  <Input
                    id="login-email"
                    data-testid="login-email"
                    type="email"
                    autoComplete="username"
                    required
                    className={styles.fieldInput}
                    value={email}
                    onChange={(e) => setEmail(e.target.value)}
                    placeholder={t('emailPlaceholder')}
                  />
                </div>

                <div className={styles.field}>
                  <div className={styles.fieldLabelRow}>
                    <label className={styles.fieldLabel} htmlFor="login-password">
                      {t('password')}
                    </label>
                    <Link to="/forgot-password" className={styles.forgotInline}>
                      {t('forgotPassword')}
                    </Link>
                  </div>
                  <Input
                    id="login-password"
                    data-testid="login-password"
                    type="password"
                    autoComplete="current-password"
                    required
                    className={styles.fieldInput}
                    value={password}
                    onChange={(e) => setPassword(e.target.value)}
                    placeholder={t('passwordPlaceholder')}
                  />
                </div>

                {formErrorKey && (
                  <p role="alert" className={styles.inlineError}>{t(formErrorKey)}</p>
                )}

                <button
                  type="submit"
                  data-testid="login-submit"
                  className={styles.submitBtn}
                  disabled={submitting || !email || !password}
                >
                  {submitting ? t('signingIn') : t('signIn')}
                </button>
              </form>
            )}
          </>
        )}

        <div className={styles.langRow}>
          <select
            value={currentLang}
            onChange={(e) => changeLang(e.target.value)}
            className={styles.langSelect}
            aria-label={t('language')}
          >
            {SUPPORTED_LANGUAGES.map((lang) => (
              <option key={lang.code} value={lang.code}>
                {lang.flag} {lang.label}
              </option>
            ))}
          </select>
        </div>
      </div>
    </div>
  );
}

/**
 * Neutral abstract logo — rendered when the active theme has no logoMark.
 * Deliberately brand-agnostic (a concentric square + corner dots, no
 * letterform). When a theme ships its own logoMark URL, that takes over.
 */
function GenericLogoMark({ className }: { className?: string }) {
  return (
    <svg className={className} viewBox="0 0 32 32" width="26" height="26" fill="none" xmlns="http://www.w3.org/2000/svg" aria-hidden="true">
      <rect x="1" y="1" width="30" height="30" rx="8" fill="currentColor" opacity="0.12" />
      <rect x="1" y="1" width="30" height="30" rx="8" stroke="currentColor" strokeOpacity="0.3" />
      <rect x="9" y="9" width="14" height="14" rx="3" stroke="currentColor" strokeWidth="2.25" />
      <circle cx="9" cy="9" r="1.6" fill="currentColor" />
      <circle cx="23" cy="23" r="1.6" fill="currentColor" />
    </svg>
  );
}

/** Neutral large watermark version of GenericLogoMark. */
function GenericLogoWatermark({ className }: { className?: string }) {
  return (
    <svg className={className} viewBox="0 0 32 32" fill="none" xmlns="http://www.w3.org/2000/svg" aria-hidden="true">
      <rect x="1" y="1" width="30" height="30" rx="8" fill="currentColor" />
      <rect x="9" y="9" width="14" height="14" rx="3" stroke="white" strokeWidth="2.25" />
      <circle cx="9" cy="9" r="1.6" fill="white" />
      <circle cx="23" cy="23" r="1.6" fill="white" />
    </svg>
  );
}
