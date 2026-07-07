import { describe, it, expect, beforeEach, vi } from 'vitest';
import { api } from '../../../../src/api/client';
import { personalVKApi } from '../../../../src/api/services/ai-gateway/personalVirtualKeys';
vi.mock('../../../../src/api/client', () => ({ api: { get: vi.fn().mockResolvedValue({}), post: vi.fn().mockResolvedValue({}), put: vi.fn().mockResolvedValue({}), patch: vi.fn().mockResolvedValue({}), delete: vi.fn().mockResolvedValue(undefined) } }));
const m = api as unknown as Record<'get' | 'post' | 'put' | 'patch' | 'delete', ReturnType<typeof vi.fn>>;
beforeEach(() => Object.values(m).forEach((f) => f.mockClear()));
describe('personalVKApi', () => {
  it('hits /api/my/virtual-keys', () => {
    personalVKApi.list();
    personalVKApi.list({ q: 'my-key' });
    personalVKApi.create({} as never);
    personalVKApi.update('v1', {});
    personalVKApi.delete('v1');
    personalVKApi.regenerate('v1');
    expect(m.get).toHaveBeenCalledWith('/api/my/virtual-keys', undefined);
    expect(m.get).toHaveBeenCalledWith('/api/my/virtual-keys', { q: 'my-key' });
    expect(m.post).toHaveBeenCalledWith('/api/my/virtual-keys', {});
    expect(m.put).toHaveBeenCalledWith('/api/my/virtual-keys/v1', {});
    expect(m.delete).toHaveBeenCalledWith('/api/my/virtual-keys/v1');
    expect(m.post).toHaveBeenCalledWith('/api/my/virtual-keys/v1/regenerate', {});
  });
});
