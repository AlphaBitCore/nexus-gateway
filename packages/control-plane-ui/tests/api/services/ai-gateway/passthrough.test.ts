import { describe, it, expect, beforeEach, vi } from 'vitest';
import { api } from '../../../../src/api/client';
import { passthroughApi, validatePassthroughPayload, PASSTHROUGH_MIN_REASON_LEN } from '../../../../src/api/services/ai-gateway/passthrough';
vi.mock('../../../../src/api/client', () => ({ api: { get: vi.fn().mockResolvedValue({}), post: vi.fn().mockResolvedValue({}), put: vi.fn().mockResolvedValue({}), patch: vi.fn().mockResolvedValue({}), delete: vi.fn().mockResolvedValue(undefined) } }));
const m = api as unknown as Record<'get' | 'post' | 'put' | 'patch' | 'delete', ReturnType<typeof vi.fn>>;
beforeEach(() => Object.values(m).forEach((f) => f.mockClear()));
describe('passthroughApi', () => {
  it('global/adapter/provider routes (adapter id encoded)', () => {
    passthroughApi.getSnapshot();
    passthroughApi.getGlobal();
    passthroughApi.putGlobal({} as never);
    passthroughApi.getAdapter('a/b');
    passthroughApi.putAdapter('a/b', {} as never);
    passthroughApi.deleteAdapter('a/b');
    passthroughApi.getProvider('p1');
    passthroughApi.putProvider('p1', {} as never);
    passthroughApi.deleteProvider('p1');
    expect(m.get).toHaveBeenCalledWith('/api/admin/passthrough/snapshot');
    expect(m.put).toHaveBeenCalledWith('/api/admin/passthrough/global', {});
    expect(m.get).toHaveBeenCalledWith('/api/admin/passthrough/adapter/a%2Fb');
    expect(m.delete).toHaveBeenCalledWith('/api/admin/passthrough/adapter/a%2Fb');
  });
});
describe('validatePassthroughPayload', () => {
  const reason = 'x'.repeat(PASSTHROUGH_MIN_REASON_LEN);
  const future = new Date(Date.now() + 60 * 60 * 1000).toISOString();
  it('returns null when disabled', () => {
    expect(validatePassthroughPayload({ enabled: false } as never)).toBeNull();
  });
  it('requires at least one bypass selected', () => {
    expect(validatePassthroughPayload({ enabled: true } as never)).toBe('passthrough_no_bypass_selected');
  });
  it('normalize bypass requires cache bypass', () => {
    expect(validatePassthroughPayload({ enabled: true, bypassNormalize: true } as never)).toBe('passthrough_normalize_requires_cache_bypass');
  });
  it('rejects missing / invalid / past / too-far expiry', () => {
    const base = { enabled: true, bypassHooks: true, reason } as never;
    expect(validatePassthroughPayload({ ...base })).toBe('passthrough_invalid_expiry');
    expect(validatePassthroughPayload({ ...base, expiresAt: 'nope' })).toBe('passthrough_invalid_expiry');
    expect(validatePassthroughPayload({ ...base, expiresAt: new Date(Date.now() - 1000).toISOString() })).toBe('passthrough_invalid_expiry');
    expect(validatePassthroughPayload({ ...base, expiresAt: new Date(Date.now() + 100 * 3600 * 1000).toISOString() })).toBe('passthrough_invalid_expiry');
  });
  it('rejects a too-short reason', () => {
    expect(validatePassthroughPayload({ enabled: true, bypassHooks: true, expiresAt: future, reason: 'short' } as never)).toBe('passthrough_invalid_reason');
  });
  it('accepts a fully valid payload', () => {
    expect(validatePassthroughPayload({ enabled: true, bypassHooks: true, expiresAt: future, reason } as never)).toBeNull();
  });
});
