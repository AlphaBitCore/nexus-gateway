/**
 * API client — fetch wrapper for `/api/admin/**` that transparently attaches a
 * `Authorization: Bearer <access_token>` header, and on a 401 response, tries
 * **once** to rotate the refresh token via `POST /oauth/token` and re-dispatch
 * the original request.
 *
 * Design points:
 *
 *   - The access / refresh tokens live in `sessionStorage` (see `auth/tokenStore.ts`).
 *     This module only reads / writes tokens through that module — it never
 *     touches storage keys directly.
 *
 *   - Refresh is serialized: concurrent 401s share a single in-flight
 *     `POST /oauth/token` promise so we do not burn two refresh tokens on a
 *     burst of parallel API calls. Refresh-token rotation (server-side) means
 *     the *second* refresh with the old token would be rejected.
 *
 *   - If refresh itself fails (network / 4xx / missing refresh token), we
 *     clear tokens and redirect the browser to `/login`. From the caller's
 *     point of view the original request rejects with a 401 `ApiError` — the
 *     UI will unmount before it has a chance to render.
 *
 *   - The body-parsing and `ApiError` shape (including `forbiddenDetails`)
 *     are unchanged from the cookie-based client; ~30 service files depend on
 *     the `api.{get,post,put,patch,delete}` surface. This module is a
 *     drop-in replacement.
 */

import { getAccessToken } from '../auth/tokens/tokenStore';
import { withPrefix } from '../lib/deploymentPrefix';
import { refreshAccessToken, scheduleProactiveRefresh } from './client_refresh';

// Re-export the proactive-refresh scheduler so existing callers that import it
// from `./client` keep resolving unchanged after the refresh subsystem moved to
// `./client_refresh`.
export { scheduleProactiveRefresh };

/** IAM / RBAC denial payload from gateway 403 responses (when present). */
export interface ApiForbiddenDetails {
  action?: string;
  resource?: string;
  reason?: string;
}

export class ApiError extends Error {
  constructor(
    public status: number,
    public code: string,
    message: string,
    public errorType?: string,
    public forbiddenDetails?: ApiForbiddenDetails,
  ) {
    super(message);
    this.name = 'ApiError';
  }
}

/** Build a `Headers` object with the Content-Type and (if present) Bearer auth. */
function buildHeaders(hasBody: boolean): Record<string, string> {
  const headers: Record<string, string> = {};
  if (hasBody) headers['Content-Type'] = 'application/json';
  const token = getAccessToken();
  if (token) headers['Authorization'] = `Bearer ${token}`;
  return headers;
}

/**
 * Bearer-authenticated `fetch` for callers that need the raw `Response` —
 * SSE streams, blob downloads, and best-effort fire-and-forget POSTs (the
 * assistant native clients). Attaches the access token on top of the
 * caller's headers, and on a 401 performs the same serialized refresh-token
 * rotation as `request` and re-dispatches the call once with the fresh
 * token. If the refresh itself fails the browser is redirected to `/login`
 * (the session is gone) and the original 401 response is returned, so
 * non-throwing callers keep their falsy-result handling.
 */
export async function authorizedFetch(
  input: string,
  init: Omit<RequestInit, 'headers'> & { headers?: Record<string, string> } = {},
): Promise<Response> {
  // Prepend the deployment sub-path prefix so requests resolve correctly when
  // Nexus is served behind a reverse proxy at e.g. /nexus/ (same contract as
  // `request` above). Root-relative paths only; absolute URLs pass through.
  const url = /^https?:\/\//.test(input) ? input : withPrefix(input);
  const run = (): Promise<Response> => {
    const token = getAccessToken();
    return fetch(url, {
      ...init,
      headers: { ...init.headers, ...(token ? { Authorization: `Bearer ${token}` } : {}) },
    });
  };

  let res = await run();
  if (res.status === 401) {
    const refreshed = await refreshAccessToken();
    if (refreshed) {
      res = await run();
    } else if (typeof window !== 'undefined' && !window.location.pathname.startsWith(withPrefix('/login'))) {
      window.location.assign(withPrefix('/login'));
    }
  }
  return res;
}

/** Parse an error body shape produced by the Echo handlers: `{ error: { code, message, type, details } }`. */
async function toApiError(res: Response): Promise<ApiError> {
  const errorBody = await res.json().catch(() => ({ error: res.statusText }));
  let message =
    (errorBody as { error?: { message?: string } | string })?.error && typeof (errorBody as { error?: { message?: string } }).error === 'object'
      ? ((errorBody as { error: { message?: string } }).error.message ?? res.statusText)
      : (errorBody as { error?: string })?.error ?? res.statusText;
  const code =
    (errorBody as { error?: { code?: string } })?.error?.code ??
    (errorBody as { code?: string })?.code ??
    'UNKNOWN';
  const errorType =
    typeof (errorBody as { error?: { type?: string } })?.error?.type === 'string'
      ? (errorBody as { error: { type: string } }).error.type
      : undefined;

  let forbiddenDetails: ApiForbiddenDetails | undefined;
  const details = (errorBody as { error?: { details?: unknown } })?.error?.details;
  if (res.status === 403 && details && typeof details === 'object') {
    const d = details as Record<string, unknown>;
    forbiddenDetails = {
      action: typeof d.action === 'string' ? d.action : undefined,
      resource: typeof d.resource === 'string' ? d.resource : undefined,
      reason: typeof d.reason === 'string' ? d.reason : undefined,
    };
    const parts: string[] = [];
    if (forbiddenDetails.action) parts.push(forbiddenDetails.action);
    if (forbiddenDetails.resource) parts.push(`on ${forbiddenDetails.resource}`);
    if (parts.length > 0) message = `Access denied: ${parts.join(' ')}`;
    if (forbiddenDetails.reason && !String(message).includes(forbiddenDetails.reason)) {
      message = `${message} (${forbiddenDetails.reason})`;
    }
  }

  return new ApiError(res.status, code, String(message), errorType, forbiddenDetails);
}

async function request<T>(
  method: string,
  path: string,
  body?: unknown,
  params?: Record<string, string>,
): Promise<T> {
  const url = new URL(withPrefix(path), window.location.origin);
  if (params) {
    for (const [k, v] of Object.entries(params)) {
      if (v !== undefined && v !== '') url.searchParams.set(k, v);
    }
  }

  const hasBody = body !== undefined;

  const run = async (): Promise<Response> =>
    fetch(url.toString(), {
      method,
      headers: buildHeaders(hasBody),
      body: hasBody ? JSON.stringify(body) : undefined,
    });

  let res = await run();

  if (res.status === 401) {
    // Attempt exactly one refresh + retry. If the refresh succeeds we re-run
    // the original request with the freshly-minted access token; if not, the
    // 401 propagates and the UI will redirect to /login.
    const refreshed = await refreshAccessToken();
    if (refreshed) {
      res = await run();
    } else if (typeof window !== 'undefined' && !window.location.pathname.startsWith(withPrefix('/login'))) {
      // Refresh failed — kick the user to the login page so the next click
      // starts a fresh PKCE flow. We still throw below so any in-flight
      // render can unmount cleanly.
      window.location.assign(withPrefix('/login'));
    }
  }

  if (!res.ok) {
    throw await toApiError(res);
  }

  if (res.status === 204) return undefined as T;
  return res.json() as Promise<T>;
}

/**
 * Parse the filename from a `Content-Disposition: attachment; filename="..."`
 * header. Handles both the quoted and unquoted forms and the RFC 5987
 * `filename*=UTF-8''...` extension (which browsers prefer for non-ASCII).
 * Returns null when no filename is declared so the caller can fall back.
 */
function filenameFromContentDisposition(header: string | null): string | null {
  if (!header) return null;
  const star = /filename\*\s*=\s*(?:UTF-8'')?([^;]+)/i.exec(header);
  if (star) {
    try {
      return decodeURIComponent(star[1].trim());
    } catch {
      // fall through to plain filename
    }
  }
  const plain = /filename\s*=\s*("([^"]+)"|([^;]+))/i.exec(header);
  if (plain) {
    const raw = (plain[2] ?? plain[3] ?? '').trim();
    return raw.length > 0 ? raw : null;
  }
  return null;
}

/**
 * Fetch `path` with Bearer auth (same refresh-on-401 logic as `request`) and
 * return the Blob + the server-suggested filename. Used for authenticated
 * file downloads — anchor-tag navigation cannot attach Authorization headers,
 * which is why a simple `<a href>` fails with 401 on Bearer-auth APIs.
 */
async function getBlob(
  path: string,
  params?: Record<string, string>,
): Promise<{ blob: Blob; filename: string | null }> {
  const url = new URL(withPrefix(path), window.location.origin);
  if (params) {
    for (const [k, v] of Object.entries(params)) {
      if (v !== undefined && v !== '') url.searchParams.set(k, v);
    }
  }

  const run = async (): Promise<Response> =>
    fetch(url.toString(), { method: 'GET', headers: buildHeaders(false) });

  let res = await run();
  if (res.status === 401) {
    const refreshed = await refreshAccessToken();
    if (refreshed) {
      res = await run();
    } else if (typeof window !== 'undefined' && !window.location.pathname.startsWith(withPrefix('/login'))) {
      window.location.assign(withPrefix('/login'));
    }
  }
  if (!res.ok) {
    throw await toApiError(res);
  }

  const blob = await res.blob();
  const filename = filenameFromContentDisposition(res.headers.get('Content-Disposition'));
  return { blob, filename };
}

/**
 * Authenticated file download. Fetches `path` as a Blob with the Bearer
 * token attached, then triggers a programmatic download using a synthetic
 * anchor. Falls back to `fallbackFilename` when the response has no
 * Content-Disposition header. Revokes the object URL after the click so the
 * browser can GC the blob.
 */
async function download(
  path: string,
  params?: Record<string, string>,
  fallbackFilename: string = 'download',
): Promise<void> {
  const { blob, filename } = await getBlob(path, params);
  const objectUrl = URL.createObjectURL(blob);
  const a = document.createElement('a');
  a.href = objectUrl;
  a.download = filename ?? fallbackFilename;
  document.body.appendChild(a);
  a.click();
  a.remove();
  URL.revokeObjectURL(objectUrl);
}

export const api = {
  get: <T>(path: string, params?: Record<string, string>) => request<T>('GET', path, undefined, params),
  post: <T>(path: string, body?: unknown) => request<T>('POST', path, body),
  put: <T>(path: string, body?: unknown) => request<T>('PUT', path, body),
  patch: <T>(path: string, body?: unknown) => request<T>('PATCH', path, body),
  delete: (path: string) => request<void>('DELETE', path),
  getBlob,
  download,
};

export const __testing__ = { filenameFromContentDisposition };
