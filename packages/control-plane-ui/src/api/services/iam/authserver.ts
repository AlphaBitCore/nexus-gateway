/**
 * authApi — SPA client for the unauthenticated login endpoints at
 * /authserver/*. These sit outside the bearer-token path used by the
 * admin API client, so we issue plain fetch calls and parse the typed
 * error codes defined by docs/users/api/openapi/auth/authserver-login.yaml.
 */
import { withPrefix } from '@/lib/deploymentPrefix';

export type AuthserverErrorCode =
  | 'invalid_credentials'
  | 'user_disabled'
  | 'authctx_expired'
  | 'rate_limited'
  | 'internal_error';

/** Thrown when the authserver responds with a typed error body. */
export class AuthserverError extends Error {
  constructor(
    public status: number,
    public code: AuthserverErrorCode,
  ) {
    super(code);
    this.name = 'AuthserverError';
  }
}

export type IdpType = 'local' | 'oidc' | 'saml';

export interface IdpEntry {
  id: string;
  type: IdpType;
  name: string;
}

export interface IdpListResponse {
  providers: IdpEntry[];
}

export interface PasswordSubmitResponse {
  redirectUri: string;
}

async function parseError(res: Response): Promise<never> {
  let code: AuthserverErrorCode = 'internal_error';
  try {
    const body = (await res.json()) as { error?: string };
    if (body && typeof body.error === 'string') {
      code = body.error as AuthserverErrorCode;
    }
  } catch {
    // Leave code as internal_error when the body is missing or malformed.
  }
  throw new AuthserverError(res.status, code);
}

export const authApi = {
  async listIdps(authctx: string): Promise<IdpListResponse> {
    const url = new URL(withPrefix('/authserver/idps'), window.location.origin);
    url.searchParams.set('authctx', authctx);
    const res = await fetch(url.toString(), { headers: { Accept: 'application/json' } });
    if (!res.ok) await parseError(res);
    return (await res.json()) as IdpListResponse;
  },

  async submitPassword(
    authctx: string,
    email: string,
    password: string,
  ): Promise<PasswordSubmitResponse> {
    const res = await fetch(new URL(withPrefix('/authserver/password'), window.location.origin).toString(), {
      method: 'POST',
      headers: { 'Content-Type': 'application/json', Accept: 'application/json' },
      body: JSON.stringify({ authctx, email, password }),
    });
    if (!res.ok) await parseError(res);
    return (await res.json()) as PasswordSubmitResponse;
  },

  /**
   * Complete a pending OAuth authorize using the caller's existing session
   * (bearer token in localStorage). Used when an operator lands on /login with
   * an authctx already signed in — e.g. nexus-cli's PKCE flow opening the
   * browser tab while the SPA session is live. Without this the page would
   * navigate to "/" and the CLI's loopback listener would hang.
   */
  async approveAuthctx(authctx: string, accessToken: string): Promise<PasswordSubmitResponse> {
    const res = await fetch(new URL(withPrefix('/authserver/approve'), window.location.origin).toString(), {
      method: 'POST',
      headers: {
        'Content-Type': 'application/json',
        Accept: 'application/json',
        Authorization: `Bearer ${accessToken}`,
      },
      body: JSON.stringify({ authctx }),
    });
    if (!res.ok) await parseError(res);
    return (await res.json()) as PasswordSubmitResponse;
  },
};
