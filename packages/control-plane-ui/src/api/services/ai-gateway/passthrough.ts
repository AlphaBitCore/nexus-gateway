/**
 * Emergency Passthrough admin API service.
 *
 * Backs `/ai-gateway/passthrough`. The 3-tier resolution
 * (global > adapter > provider) is computed server-side; this client
 * only writes + reads tier rows.
 *
 * Constraints (enforced server-side; mirrored client-side for instant feedback):
 *   - When `enabled=true`, at least one of `bypassHooks / bypassCache /
 *     bypassNormalize` must be set.
 *   - `bypassNormalize=true` requires `bypassCache=true` (the cache key
 *     derives from the normalized payload).
 *   - `expiresAt` is required when `enabled=true` and must be ≤ NOW() + 8h.
 *   - `reason` must be ≥ 20 characters when `enabled=true`.
 */
import { api } from '../../client';

/** Maximum allowed bypass window before `expiresAt` from now. */
export const PASSTHROUGH_MAX_EXPIRY_HOURS = 8;
/** Minimum length of the reason field when enabling a bypass. */
export const PASSTHROUGH_MIN_REASON_LEN = 20;

export interface PassthroughPayload {
  enabled: boolean;
  bypassHooks?: boolean;
  bypassCache?: boolean;
  bypassNormalize?: boolean;
  expiresAt?: string | null;
  enabledBy?: string;
  reason?: string;
}

/** Tier row returned by the snapshot endpoint. Same shape across all tiers. */
export interface PassthroughTier {
  enabled: boolean;
  bypassHooks: boolean;
  bypassCache: boolean;
  bypassNormalize: boolean;
  expiresAt?: string | null;
  enabledBy?: string;
  reason?: string;
}

export interface PassthroughSnapshot {
  global: PassthroughTier;
  adapters: Record<string, PassthroughTier>;
  providers: Record<string, PassthroughTier>;
  providerNames?: Record<string, string>;
}

export const passthroughApi = {
  getSnapshot: () => api.get<PassthroughSnapshot>('/api/admin/passthrough/snapshot'),

  getGlobal: () => api.get<PassthroughPayload>('/api/admin/passthrough/global'),
  putGlobal: (body: PassthroughPayload) =>
    api.put<PassthroughPayload>('/api/admin/passthrough/global', body),

  getAdapter: (adapterType: string) =>
    api.get<PassthroughPayload>(`/api/admin/passthrough/adapter/${encodeURIComponent(adapterType)}`),
  putAdapter: (adapterType: string, body: PassthroughPayload) =>
    api.put<PassthroughPayload>(`/api/admin/passthrough/adapter/${encodeURIComponent(adapterType)}`, body),
  deleteAdapter: (adapterType: string) =>
    api.delete(`/api/admin/passthrough/adapter/${encodeURIComponent(adapterType)}`),

  getProvider: (providerId: string) =>
    api.get<PassthroughPayload>(`/api/admin/passthrough/provider/${encodeURIComponent(providerId)}`),
  putProvider: (providerId: string, body: PassthroughPayload) =>
    api.put<PassthroughPayload>(`/api/admin/passthrough/provider/${encodeURIComponent(providerId)}`, body),
  deleteProvider: (providerId: string) =>
    api.delete(`/api/admin/passthrough/provider/${encodeURIComponent(providerId)}`),

  getEffective: (providerId: string) =>
    api.get<PassthroughPayload>(`/api/admin/passthrough/effective/${encodeURIComponent(providerId)}`),
};

/**
 * Validate a payload client-side before submitting. Returns null on success or
 * a stable error code matching the server validation codes for i18n lookup.
 */
export function validatePassthroughPayload(p: PassthroughPayload): string | null {
  if (!p.enabled) return null;
  if (!p.bypassHooks && !p.bypassCache && !p.bypassNormalize) {
    return 'passthrough_no_bypass_selected';
  }
  if (p.bypassNormalize && !p.bypassCache) {
    return 'passthrough_normalize_requires_cache_bypass';
  }
  if (!p.expiresAt) return 'passthrough_invalid_expiry';
  const expires = new Date(p.expiresAt).getTime();
  if (Number.isNaN(expires)) return 'passthrough_invalid_expiry';
  const maxExpires = Date.now() + PASSTHROUGH_MAX_EXPIRY_HOURS * 60 * 60 * 1000;
  if (expires > maxExpires) return 'passthrough_invalid_expiry';
  if (expires <= Date.now()) return 'passthrough_invalid_expiry';
  if ((p.reason ?? '').trim().length < PASSTHROUGH_MIN_REASON_LEN) {
    return 'passthrough_invalid_reason';
  }
  return null;
}
