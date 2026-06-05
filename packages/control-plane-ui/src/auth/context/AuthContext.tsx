/**
 * OAuth-backed authentication context for cp-ui.
 *
 * State machine:
 *
 *   loading  → (bootstrap check)
 *     ├─ no tokens → unauthenticated
 *     └─ tokens exist → fetch GET /api/admin/me
 *         ├─ 200 → authenticated
 *         └─ 401 (after refresh retry in api/client) → unauthenticated
 *
 * Public API (shape preserved for RequireAuth, RequireRole, useIdleTimeout,
 * and ~30 service files that consume `useAuth()`):
 *
 *   status, roles, keyName, userId, principalType
 *   login(postAuthPath?)       — start a fresh PKCE flow (redirects to /oauth/authorize)
 *   logout()                   — clear tokens client-side + redirect to /login
 *   completeLogin(tokens)      — called by CallbackPage after /oauth/token succeeds
 *   refreshSession()           — re-pull /api/admin/me (e.g. after profile edit)
 *
 * Intentionally NOT here (vs the previous cookie-session design):
 *   - `/api/admin/whoami` periodic heartbeat (we rely on the Bearer 401 →
 *     refresh-token retry path in `api/client`; a token-rotation failure
 *     surfaces via the normal API-call error, and `useIdleTimeout` still
 *     enforces inactivity logout).
 *   - CSRF storage — JWT + Bearer header is not CSRF-exposed the way cookie
 *     sessions were.
 */

import { createContext, useCallback, useContext, useEffect, useReducer, useState } from 'react';
import { useTranslation } from 'react-i18next';
import { api, ApiError, scheduleProactiveRefresh } from '../../api/client';
import styles from './AuthContext.module.css';
import type { WhoAmI } from '../../api/types';
import { clearTokens, getAccessToken, setTokens, type TokenPair } from '../tokens/tokenStore';
import { decodeAccessToken } from '../tokens/claims';
import { startLogin } from '../pkce/pkceFlow';
import { useIdleTimeout } from '../context/useIdleTimeout';
import { setDisplayTZ } from '@/lib/format';

interface AuthState {
  status: 'loading' | 'authenticated' | 'unauthenticated';
  roles: string[];
  /** IAM action strings the caller is allowed to perform. Populated from
   * GET /api/admin/me/permissions on login. Used by usePermission() and
   * Sidebar to drive feature visibility without group-name heuristics. */
  permissions: string[];
  keyName?: string;
  userId?: string;
  principalType?: string;
  /** Present for `admin_user` principals after GET /api/admin/me. */
  email?: string | null;
}

type AuthAction =
  | { type: 'AUTH_OK'; me: WhoAmI; permissions: string[] }
  | { type: 'AUTH_GONE' };

function authReducer(_state: AuthState, action: AuthAction): AuthState {
  switch (action.type) {
    case 'AUTH_OK':
      return {
        status: 'authenticated',
        roles: action.me.roles ?? [],
        permissions: action.permissions,
        keyName: action.me.keyName,
        userId: action.me.keyId,
        principalType: action.me.authPrincipalType,
        email: action.me.email,
      };
    case 'AUTH_GONE':
      return { status: 'unauthenticated', roles: [], permissions: [] };
  }
}

interface AuthContextValue extends AuthState {
  /**
   * Start a fresh PKCE authorization-code flow. Never resolves in production
   * because the browser navigates away to /oauth/authorize.
   */
  login: (postAuthPath?: string) => Promise<void>;
  /** Clear local tokens and redirect to /login. No server round-trip. */
  logout: () => void;
  /**
   * Called by CallbackPage once /oauth/token has minted a fresh TokenPair.
   * Persists tokens + fetches /api/admin/me + flips status to authenticated.
   */
  completeLogin: (tokens: TokenPair) => Promise<void>;
  /** Re-sync identity + roles from GET /api/admin/me (e.g. after profile edit). */
  refreshSession: () => Promise<void>;
}

const AuthContext = createContext<AuthContextValue | null>(null);

/** Fetch the current principal; return null when unauthenticated. */
async function fetchMe(): Promise<WhoAmI | null> {
  try {
    const me = await api.get<WhoAmI>('/api/admin/me');
    // Apply the user's display TZ to the global formatter as soon as
    // we know it, so the very first render already reflects their preference.
    // Empty / unset falls back to browser TZ via setDisplayTZ's null path.
    setDisplayTZ(me.preferredTimezone || null);
    return me;
  } catch (e) {
    if (e instanceof ApiError && e.status === 401) return null;
    // Network / 5xx / anything else — treat as "session unknown" and let the
    // caller render unauthenticated rather than crash the whole app.
    return null;
  }
}

/** Fetch the caller's allowed IAM actions from the backend.
 * The backend invalidates the policy cache before evaluating, so this always
 * reflects the latest group memberships. Returns [] on any error. */
async function fetchPermissions(): Promise<string[]> {
  try {
    const data = await api.get<{ actions: string[] }>('/api/admin/me/permissions');
    return data.actions ?? [];
  } catch {
    return [];
  }
}

export function AuthProvider({ children }: { children: React.ReactNode }) {
  const [state, dispatch] = useReducer(authReducer, { status: 'loading', roles: [], permissions: [] });

  // Bootstrap: on mount, try to restore a session from sessionStorage tokens.
  useEffect(() => {
    let cancelled = false;
    (async () => {
      if (!getAccessToken()) {
        if (!cancelled) dispatch({ type: 'AUTH_GONE' });
        return;
      }
      const [me, permissions] = await Promise.all([fetchMe(), fetchPermissions()]);
      if (cancelled) return;
      if (me) {
        dispatch({ type: 'AUTH_OK', me, permissions });
      } else {
        // Tokens present but /me returned 401 (and the refresh-retry in
        // api/client didn't recover). Wipe and go to unauthenticated; the
        // client already redirected to /login in the 401 path.
        clearTokens();
        dispatch({ type: 'AUTH_GONE' });
      }
    })();
    return () => {
      cancelled = true;
    };
  }, []);

  const login = useCallback(async (postAuthPath?: string): Promise<void> => {
    // startLogin redirects the browser to /oauth/authorize; this promise never
    // resolves in a real browser session, only in tests that stub
    // window.location.assign.
    await startLogin(postAuthPath ?? '/');
  }, []);

  const completeLogin = useCallback(async (tokens: TokenPair): Promise<void> => {
    setTokens(tokens);
    const [me, permissions] = await Promise.all([fetchMe(), fetchPermissions()]);
    if (me) {
      dispatch({ type: 'AUTH_OK', me, permissions });
    } else {
      // /me shouldn't 401 immediately after /oauth/token success. If it does,
      // the only safe move is to drop the tokens and go back to login.
      clearTokens();
      dispatch({ type: 'AUTH_GONE' });
    }
  }, []);

  const logout = useCallback(() => {
    // For a federated (SSO) session, route through the backend RP-initiated
    // logout so the IdP ends its own session too — otherwise re-clicking the SSO
    // button silently re-authenticates against the still-live IdP session. The
    // backend 302s to the IdP's end_session_endpoint then back to /login; for a
    // local / password session (or an IdP without end_session) it just lands on
    // /login. Read the claim BEFORE clearing the token.
    const claims = decodeAccessToken(getAccessToken());
    const ssoIdp = claims?.amr?.includes('sso') ? claims?.idp : undefined;
    clearTokens();
    dispatch({ type: 'AUTH_GONE' });
    if (typeof window === 'undefined') return;
    if (ssoIdp) {
      window.location.assign(`/authserver/idp/${encodeURIComponent(ssoIdp)}/logout`);
    } else if (!window.location.pathname.startsWith('/login')) {
      window.location.assign('/login');
    }
  }, []);

  const refreshSession = useCallback(async (): Promise<void> => {
    const [me, permissions] = await Promise.all([fetchMe(), fetchPermissions()]);
    if (me) dispatch({ type: 'AUTH_OK', me, permissions });
  }, []);

  const { t } = useTranslation('common');
  const [idleWarning, setIdleWarning] = useState(false);

  useIdleTimeout({
    timeoutMs: 15 * 60 * 1000,
    warningMs: 13 * 60 * 1000,
    onWarning: () => setIdleWarning(true),
    onActivity: () => setIdleWarning(false),
    onTimeout: () => {
      setIdleWarning(false);
      logout();
    },
    enabled: state.status === 'authenticated',
  });

  // Proactively refresh the access token ~60 s before it expires so the user
  // never hits a 401 mid-session. Cancel the scheduler on logout / unmount.
  useEffect(() => {
    if (state.status !== 'authenticated') return;
    return scheduleProactiveRefresh();
  }, [state.status]);

  return (
    <AuthContext.Provider value={{ ...state, login, logout, completeLogin, refreshSession }}>
      {idleWarning && (
        <div className={styles.idleWarningBanner}>
          {t('sessionExpiringSoon')}
        </div>
      )}
      {children}
    </AuthContext.Provider>
  );
}

export function useAuth(): AuthContextValue {
  const ctx = useContext(AuthContext);
  if (!ctx) throw new Error('useAuth must be used within AuthProvider');
  return ctx;
}
