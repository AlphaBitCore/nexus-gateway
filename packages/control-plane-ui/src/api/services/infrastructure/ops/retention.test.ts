import { describe, it, expect, vi, beforeEach, afterEach, type MockInstance } from 'vitest';
import { retentionApi } from './retention';
import * as apiClient from '../../../client';

type ApiFn = (path: string, ...rest: unknown[]) => Promise<unknown>;

describe('retentionApi', () => {
  let getSpy: MockInstance<ApiFn>;
  let putSpy: MockInstance<ApiFn>;

  beforeEach(() => {
    getSpy = vi.spyOn(apiClient.api, 'get') as unknown as MockInstance<ApiFn>;
    putSpy = vi.spyOn(apiClient.api, 'put') as unknown as MockInstance<ApiFn>;
    getSpy.mockResolvedValue({ retention: {} });
    putSpy.mockResolvedValue({ ok: true, updated: 0 });
  });
  afterEach(() => vi.restoreAllMocks());

  it('get() GETs /api/admin/observability/retention', async () => {
    await retentionApi.get();
    expect(getSpy).toHaveBeenCalledWith('/api/admin/observability/retention');
  });

  it('get() preserves the { retention } envelope (nested layer rows)', async () => {
    getSpy.mockResolvedValueOnce({
      retention: {
        runtime_raw: { value: 7, min: 1, max: 30 },
        diag_fatal: { value: 365, min: 90, max: 1825 },
      },
    });
    const out = await retentionApi.get();
    expect(out.retention.runtime_raw.value).toBe(7);
    expect(out.retention.diag_fatal.max).toBe(1825);
  });

  it('put() PUTs the body verbatim and returns { ok, updated }', async () => {
    putSpy.mockResolvedValueOnce({ ok: true, updated: 2 });
    const out = await retentionApi.put({ runtime_raw: 14, business_raw: 30 });
    expect(putSpy).toHaveBeenCalledWith('/api/admin/observability/retention', {
      runtime_raw: 14,
      business_raw: 30,
    });
    expect(out).toEqual({ ok: true, updated: 2 });
  });
});
