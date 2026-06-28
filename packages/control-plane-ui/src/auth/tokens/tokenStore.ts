/**
 * localStorage-backed token store for the cp-ui OAuth flow.
 *
 * Tokens live in localStorage so they survive navigation and are captured by
 * Playwright's storageState for E2E testing. The XSS tradeoff is accepted
 * because a stolen refresh token can be revoked server-side.
 *
 * Every accessor guards against `localStorage` being absent (node / SSR)
 * and against quota-exceeded / private-mode write failures.
 */

const ACCESS_KEY = 'nexus_access_token';
const REFRESH_KEY = 'nexus_refresh_token';

export interface TokenPair {
  accessToken: string;
  refreshToken: string;
}

function storage(): Storage | null {
  try {
    return typeof localStorage !== 'undefined' ? localStorage : null;
  } catch {
    // Some browsers throw when accessing localStorage under strict privacy
    // settings — treat as absent.
    return null;
  }
}

export function getAccessToken(): string | undefined {
  const s = storage();
  if (!s) return undefined;
  try {
    return s.getItem(ACCESS_KEY) ?? undefined;
  } catch {
    return undefined;
  }
}

export function getRefreshToken(): string | undefined {
  const s = storage();
  if (!s) return undefined;
  try {
    return s.getItem(REFRESH_KEY) ?? undefined;
  } catch {
    return undefined;
  }
}

export function setTokens(tokens: TokenPair): void {
  const s = storage();
  if (!s) return;
  try {
    s.setItem(ACCESS_KEY, tokens.accessToken);
    s.setItem(REFRESH_KEY, tokens.refreshToken);
  } catch {
    // quota / private mode — tokens will be re-fetched next cycle.
  }
}

export function clearTokens(): void {
  const s = storage();
  if (!s) return;
  try {
    s.removeItem(ACCESS_KEY);
    s.removeItem(REFRESH_KEY);
  } catch {
    /* ignore */
  }
}
