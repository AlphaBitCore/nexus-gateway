/**
 * Redirect-side orchestration of OAuth 2.1 Authorization Code + PKCE for cp-ui.
 *
 * The flow spans two page loads:
 *
 *   1. startLogin() — generate verifier + challenge + state, stash them in
 *      sessionStorage, and navigate to /oauth/authorize. The browser never
 *      comes back to the calling script — control returns only via the
 *      authserver → /auth/callback redirect.
 *
 *   2. consumeCallback() — parse the ?code=…&state=… query on the callback
 *      page, verify state matches the stashed value, POST /oauth/token with
 *      the verifier, and hand the fresh TokenPair + postAuthPath back to
 *      CallbackPage.
 *
 * The PKCE blob is stored in sessionStorage under a single key so that both
 * refresh and mid-flow tab swaps discard it — it only survives a direct
 * /authorize → /callback round trip. Every read path clears the blob before
 * returning so a replay attempt cannot re-use it.
 */

import { computeCodeChallenge, generateCodeVerifier, randomState } from '../pkce/pkce';
import type { TokenPair } from '../tokens/tokenStore';

export const OAUTH_CLIENT_ID = 'cp-ui';
export const OAUTH_SCOPES = 'admin openid profile email';
export const OAUTH_CALLBACK_PATH = '/auth/callback';
const PKCE_STATE_KEY = 'nexus_pkce_state';

interface StoredPkceState {
  verifier: string;
  state: string;
  postAuthPath: string;
}

function readStoredState(): StoredPkceState | null {
  try {
    const raw = sessionStorage.getItem(PKCE_STATE_KEY);
    if (!raw) return null;
    const parsed = JSON.parse(raw) as unknown;
    if (
      !parsed ||
      typeof parsed !== 'object' ||
      typeof (parsed as StoredPkceState).verifier !== 'string' ||
      typeof (parsed as StoredPkceState).state !== 'string' ||
      typeof (parsed as StoredPkceState).postAuthPath !== 'string'
    ) {
      return null;
    }
    return parsed as StoredPkceState;
  } catch {
    return null;
  }
}

function clearStoredState(): void {
  try {
    sessionStorage.removeItem(PKCE_STATE_KEY);
  } catch {
    /* ignore */
  }
}

/** Build the /oauth/authorize URL for the current origin. Exported for tests. */
export function buildAuthorizeUrl(params: { state: string; challenge: string; origin: string }): string {
  const u = new URL('/oauth/authorize', params.origin);
  u.searchParams.set('response_type', 'code');
  u.searchParams.set('client_id', OAUTH_CLIENT_ID);
  u.searchParams.set('redirect_uri', params.origin + OAUTH_CALLBACK_PATH);
  u.searchParams.set('scope', OAUTH_SCOPES);
  u.searchParams.set('state', params.state);
  u.searchParams.set('code_challenge', params.challenge);
  u.searchParams.set('code_challenge_method', 'S256');
  return u.toString();
}

/**
 * Start the PKCE flow by redirecting to /oauth/authorize.
 *
 * Side effect: writes verifier/state/postAuthPath to sessionStorage and
 * replaces window.location. Resolves only in tests that stub
 * window.location.assign; in production the promise never resolves because
 * the page has navigated away.
 */
export async function startLogin(postAuthPath: string): Promise<void> {
  const verifier = generateCodeVerifier();
  const challenge = await computeCodeChallenge(verifier);
  const state = randomState();

  const blob: StoredPkceState = { verifier, state, postAuthPath };
  try {
    sessionStorage.setItem(PKCE_STATE_KEY, JSON.stringify(blob));
  } catch {
    // If we cannot persist the verifier, the callback will have no way to
    // exchange the code — surface a clear error rather than redirecting into
    // a dead-end flow.
    throw new Error('Unable to persist OAuth PKCE state — sessionStorage is unavailable');
  }

  const url = buildAuthorizeUrl({ state, challenge, origin: window.location.origin });
  window.location.assign(url);
}

/** Shape of the response from POST /oauth/token. */
interface TokenResponseBody {
  access_token: string;
  refresh_token?: string;
  token_type: string;
  expires_in?: number;
  scope?: string;
}

async function exchangeCodeForTokens(args: {
  code: string;
  verifier: string;
  origin: string;
}): Promise<TokenPair> {
  const body = new URLSearchParams();
  body.set('grant_type', 'authorization_code');
  body.set('code', args.code);
  body.set('code_verifier', args.verifier);
  body.set('client_id', OAUTH_CLIENT_ID);
  body.set('redirect_uri', args.origin + OAUTH_CALLBACK_PATH);

  const res = await fetch(new URL('/oauth/token', args.origin).toString(), {
    method: 'POST',
    headers: { 'Content-Type': 'application/x-www-form-urlencoded' },
    body: body.toString(),
  });
  if (!res.ok) {
    let detail = '';
    try {
      detail = await res.text();
    } catch {
      /* ignore */
    }
    throw new Error(`Token exchange failed (${res.status}): ${detail || res.statusText}`);
  }
  const json = (await res.json()) as TokenResponseBody;
  if (!json.access_token || !json.refresh_token) {
    throw new Error('Token response missing access_token or refresh_token');
  }
  return { accessToken: json.access_token, refreshToken: json.refresh_token };
}

export interface CallbackResult {
  tokens: TokenPair;
  postAuthPath: string;
}

// Module-level promise cache keyed on the callback search string. React 18
// StrictMode double-invokes mount effects in dev; without this cache the
// second run reads empty sessionStorage (the first run cleared it) and
// throws "No pending OAuth flow". Storing the promise makes consumeCallback
// idempotent per callback URL — both runs await the same exchange.
let inFlightSearch: string | null = null;
let inFlightPromise: Promise<CallbackResult> | null = null;

/**
 * Parse the callback query, validate state, exchange the code for tokens, and
 * return the fresh token pair + the path the user was heading for when login
 * started.
 *
 * Throws on:
 *  - provider error (`?error=...`)
 *  - missing code or state
 *  - mismatched or missing stored PKCE state
 *  - token exchange HTTP failure
 */
export function consumeCallback(search: string): Promise<CallbackResult> {
  if (inFlightSearch === search && inFlightPromise) {
    return inFlightPromise;
  }
  inFlightSearch = search;
  inFlightPromise = runConsumeCallback(search);
  return inFlightPromise;
}

async function runConsumeCallback(search: string): Promise<CallbackResult> {
  const params = new URLSearchParams(search.startsWith('?') ? search.slice(1) : search);

  const err = params.get('error');
  if (err) {
    clearStoredState();
    const description = params.get('error_description') ?? '';
    throw new Error(description ? `${err}: ${description}` : err);
  }

  const code = params.get('code');
  const state = params.get('state');
  if (!code || !state) {
    clearStoredState();
    throw new Error('Callback URL is missing required code or state parameter');
  }

  const stored = readStoredState();
  clearStoredState();
  if (!stored) {
    throw new Error('No pending OAuth flow — stored PKCE state was missing or expired');
  }
  if (stored.state !== state) {
    throw new Error('OAuth state mismatch — possible CSRF or stale callback');
  }

  const tokens = await exchangeCodeForTokens({
    code,
    verifier: stored.verifier,
    origin: window.location.origin,
  });
  return { tokens, postAuthPath: stored.postAuthPath };
}

/** Test helper: pre-seed the PKCE state blob. Not part of the runtime API. */
export function __setStoredStateForTests(blob: StoredPkceState): void {
  sessionStorage.setItem(PKCE_STATE_KEY, JSON.stringify(blob));
}

/**
 * Test helper: reset the module-level consumeCallback cache so each test
 * starts from a clean slate. Not part of the runtime API.
 */
export function __resetConsumeCallbackCacheForTests(): void {
  inFlightSearch = null;
  inFlightPromise = null;
}
