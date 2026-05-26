/**
 * providerApi contract tests — verify that provider writes carry
 * `adapterType` (the canonical wire-format slug) and never the legacy
 * `type` field. The Control Plane handler rejects `type` and requires
 * `adapterType` on create, so a UI regression here would mean every
 * wizard and detail edit fails at submit time.
 */
import { describe, it, expect, vi, beforeEach, afterEach, type MockInstance } from 'vitest';
import { providerApi } from './providers';
import * as apiClient from '../../client';

type ApiFn = (path: string, ...rest: unknown[]) => Promise<unknown>;

describe('providerApi', () => {
  let postSpy: MockInstance<ApiFn>;
  let putSpy: MockInstance<ApiFn>;

  beforeEach(() => {
    postSpy = vi.spyOn(apiClient.api, 'post') as unknown as MockInstance<ApiFn>;
    putSpy = vi.spyOn(apiClient.api, 'put') as unknown as MockInstance<ApiFn>;
    postSpy.mockResolvedValue({});
    putSpy.mockResolvedValue({});
  });
  afterEach(() => vi.restoreAllMocks());

  it('create POSTs /api/admin/providers with adapterType and no legacy type field', async () => {
    await providerApi.create({
      name: 'openai',
      displayName: 'OpenAI',
      baseUrl: 'https://api.openai.com',
      adapterType: 'openai',
      enabled: true,
    });
    expect(postSpy).toHaveBeenCalledTimes(1);
    const [path, body] = postSpy.mock.calls[0] as [string, Record<string, unknown>];
    expect(path).toBe('/api/admin/providers');
    expect(body.adapterType).toBe('openai');
    expect(body).not.toHaveProperty('type');
  });

  it('update PUTs /api/admin/providers/:id and forwards adapterType when operator changes it', async () => {
    await providerApi.update('prov-1', { adapterType: 'anthropic' });
    expect(putSpy).toHaveBeenCalledTimes(1);
    const [path, body] = putSpy.mock.calls[0] as [string, Record<string, unknown>];
    expect(path).toBe('/api/admin/providers/prov-1');
    expect(body.adapterType).toBe('anthropic');
    expect(body).not.toHaveProperty('type');
  });

  it('testConnection POSTs /api/admin/providers/test-connection with adapterType (draft wizard flow)', async () => {
    await providerApi.testConnection({
      name: 'custom',
      adapterType: 'openai',
      baseUrl: 'https://example.com',
      apiKey: 'sk-xxx',
    });
    expect(postSpy).toHaveBeenCalledTimes(1);
    const [path, body] = postSpy.mock.calls[0] as [string, Record<string, unknown>];
    expect(path).toBe('/api/admin/providers/test-connection');
    expect(body.adapterType).toBe('openai');
    expect(body).not.toHaveProperty('type');
  });
});
