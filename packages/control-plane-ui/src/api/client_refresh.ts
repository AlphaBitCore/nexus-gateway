/**
 * Access-token refresh subsystem for the admin API client.
 *
 * Owns the serialized refresh-token rotation (`refreshAccessToken`) shared by
 * every authenticated request path in `client.ts`, plus the proactive
 * scheduler (`scheduleProactiveRefresh`) that rotates the token shortly before
 * it expires. Split out of `client.ts` purely to keep that file under the
 * file-size ratchet — there is no behavior change and the public surface
 * (`scheduleProactiveRefresh`) is re-exported from `client.ts` unchanged.
 *
 *   - Refresh is serialized: concurrent 401s share a single in-flight
 *     `POST /oauth/token` promise so we do not burn two refresh tokens on a
 *     burst of parallel API calls. Refresh-token rotation (server-side) means
 *     the *second* refresh with the old token would be rejected.
 *
 *   - If refresh itself fails (network / 4xx / missing refresh token), we
 *     clear tokens. The caller then redirects the browser to `/login`.
 */

import { clearTokens, getAccessToken, getRefreshToken, setTokens } from '../auth/tokens/tokenStore';
import { OAUTH_CLIENT_ID } from '../auth/pkce/pkceFlow';
import { withPrefix } from '../lib/deploymentPrefix';

/** Shape of a successful POST /oauth/token response. */
interface TokenResponseBody {
  access_token: string;
  refresh_token?: string;
  token_type: string;
  expires_in?: number;
}

/** Serialize concurrent refreshes. Null when no refresh is currently in flight. */
let refreshInFlight: Promise<boolean> | null = null;

/**
 * Rotate the refresh token exactly once (serialized across concurrent callers).
 *
 * Returns `true` on success (new access + refresh tokens stored), `false` on
 * any failure. On failure, also clears both tokens so subsequent API calls
 * short-circuit without hitting the wire again.
 */
export async function refreshAccessToken(): Promise<boolean> {
  if (refreshInFlight) return refreshInFlight;

  // Resolve the no-refresh-token case BEFORE assigning `refreshInFlight`.
  // That path returns synchronously (no `await`), so if it ran inside the
  // IIFE below its `finally { refreshInFlight = null }` would execute *before*
  // the outer `refreshInFlight = (…)()` assignment completed — the assignment
  // would then re-latch the resolved-false promise and it would never be
  // nulled. The result: a single no-refresh-token 401 permanently disables
  // refresh for the page's lifetime, so even after the user re-authenticates
  // every later 401 short-circuits at the guard above. Keeping the check out
  // here means the slot is never latched on the synchronous failure path.
  const refreshToken = getRefreshToken();
  if (!refreshToken) {
    clearTokens();
    return false;
  }

  refreshInFlight = (async (): Promise<boolean> => {
    try {
      const body = new URLSearchParams();
      body.set('grant_type', 'refresh_token');
      body.set('refresh_token', refreshToken);
      body.set('client_id', OAUTH_CLIENT_ID);
      const res = await fetch(new URL(withPrefix('/oauth/token'), window.location.origin).toString(), {
        method: 'POST',
        headers: { 'Content-Type': 'application/x-www-form-urlencoded' },
        body: body.toString(),
      });
      if (!res.ok) {
        clearTokens();
        return false;
      }
      const json = (await res.json()) as TokenResponseBody;
      if (!json.access_token || !json.refresh_token) {
        clearTokens();
        return false;
      }
      setTokens({ accessToken: json.access_token, refreshToken: json.refresh_token });
      return true;
    } catch {
      clearTokens();
      return false;
    } finally {
      // Release the serialization slot for subsequent 401s. The `await fetch`
      // above guarantees this runs after the assignment below, not before it.
      refreshInFlight = null;
    }
  })();
  return refreshInFlight;
}

/** Decode the `exp` claim (Unix seconds) from a JWT without verifying the signature. */
function getTokenExp(token: string): number | null {
  try {
    const part = token.split('.')[1].replace(/-/g, '+').replace(/_/g, '/');
    const payload = JSON.parse(atob(part)) as Record<string, unknown>;
    return typeof payload.exp === 'number' ? payload.exp : null;
  } catch {
    return null;
  }
}

/**
 * Proactively refresh the access token 60 s before it expires.
 * Self-reschedules after each successful rotation. Returns a cleanup
 * function that cancels the pending timer (call it on logout / unmount).
 */
export function scheduleProactiveRefresh(): () => void {
  let timer: ReturnType<typeof setTimeout> | undefined;

  function schedule(): void {
    const token = getAccessToken();
    if (!token) return;
    const exp = getTokenExp(token);
    if (!exp) return;
    const msUntilRefresh = exp * 1000 - Date.now() - 600_000;
    const runRefresh = () => {
      refreshAccessToken().then((ok) => {
        if (ok) schedule();
      });
    };
    if (msUntilRefresh <= 0) {
      runRefresh();
      return;
    }
    timer = setTimeout(runRefresh, msUntilRefresh);
  }

  schedule();
  return () => clearTimeout(timer);
}
