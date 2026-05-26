import { describe, it, expect, vi, beforeEach, afterEach, type MockInstance } from 'vitest';
import { diagModeApi } from './diagmode';
import * as apiClient from '../../../client';

type ApiFn = (path: string, ...rest: unknown[]) => Promise<unknown>;

describe('diagModeApi', () => {
  let getSpy: MockInstance<ApiFn>;
  let postSpy: MockInstance<ApiFn>;
  let deleteSpy: MockInstance<ApiFn>;

  beforeEach(() => {
    getSpy = vi.spyOn(apiClient.api, 'get') as unknown as MockInstance<ApiFn>;
    postSpy = vi.spyOn(apiClient.api, 'post') as unknown as MockInstance<ApiFn>;
    deleteSpy = vi.spyOn(apiClient.api, 'delete') as unknown as MockInstance<ApiFn>;
    getSpy.mockResolvedValue({ data: [] });
    postSpy.mockResolvedValue({});
    deleteSpy.mockResolvedValue(undefined);
  });
  afterEach(() => vi.restoreAllMocks());

  it("enable('agent-1', { until, reason }) POSTs the per-agent diag-mode endpoint", async () => {
    await diagModeApi.enable('agent-1', { until: '2026-04-27T01:00:00Z', reason: 'debug session' });
    expect(postSpy).toHaveBeenCalledWith('/api/admin/agents/agent-1/diagnostic-mode', {
      until: '2026-04-27T01:00:00Z',
      reason: 'debug session',
    });
  });

  it('enable() URL-encodes nodeId before path interpolation', async () => {
    await diagModeApi.enable('agent/with slashes', { until: '2026-04-27T01:00:00Z' });
    const [url] = postSpy.mock.calls[0] as [string, unknown];
    expect(url).toBe('/api/admin/agents/agent%2Fwith%20slashes/diagnostic-mode');
  });

  it("disable('agent-1') DELETEs the per-agent diag-mode endpoint", async () => {
    await diagModeApi.disable('agent-1');
    expect(deleteSpy).toHaveBeenCalledWith('/api/admin/agents/agent-1/diagnostic-mode');
  });

  it('list() GETs and unwraps the { data } envelope', async () => {
    getSpy.mockResolvedValueOnce({
      data: [
        { id: 'w1', nodeId: 't1', startedAt: '2026-04-27T00:00:00Z', endedAt: '2026-04-27T01:00:00Z', createdAt: '2026-04-27T00:00:00Z' },
      ],
    });
    const out = await diagModeApi.list();
    expect(getSpy).toHaveBeenCalledWith('/api/admin/agents/diagnostic-mode');
    expect(out).toHaveLength(1);
    expect(out[0].nodeId).toBe('t1');
  });

  it('bulk() POSTs to the bulk endpoint with the full body', async () => {
    await diagModeApi.bulk({
      filter: { agentVersion: '1.2.3' },
      until: '2026-04-27T01:00:00Z',
      reason: 'crash repro',
    });
    expect(postSpy).toHaveBeenCalledWith('/api/admin/agents/diagnostic-mode/bulk', {
      filter: { agentVersion: '1.2.3' },
      until: '2026-04-27T01:00:00Z',
      reason: 'crash repro',
    });
  });
});
