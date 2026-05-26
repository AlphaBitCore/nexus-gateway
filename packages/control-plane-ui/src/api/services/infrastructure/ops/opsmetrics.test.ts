import { describe, it, expect, vi, beforeEach, afterEach, type MockInstance } from 'vitest';
import { opsMetricsApi } from './opsmetrics';
import * as apiClient from '../../../client';

type ApiFn = (path: string, ...rest: unknown[]) => Promise<unknown>;

describe('opsMetricsApi', () => {
  let getSpy: MockInstance<ApiFn>;

  beforeEach(() => {
    getSpy = vi.spyOn(apiClient.api, 'get') as unknown as MockInstance<ApiFn>;
    getSpy.mockResolvedValue({ data: [] });
  });
  afterEach(() => vi.restoreAllMocks());

  describe('current', () => {
    it('current() with no params hits /api/admin/ops-metrics/current cleanly', async () => {
      await opsMetricsApi.current();
      expect(getSpy).toHaveBeenCalledWith('/api/admin/ops-metrics/current');
    });

    it('current({ nodeType, nodeId }) appends both filters', async () => {
      await opsMetricsApi.current({ nodeType: 'agent', nodeId: 'node-abc' });
      expect(getSpy).toHaveBeenCalledWith(
        '/api/admin/ops-metrics/current?nodeType=agent&nodeId=node-abc',
      );
    });

    it('current() unwraps the { data } envelope to a flat sample list', async () => {
      const sample = {
        sampledAt: '2026-04-27T00:00:00Z',
        nodeId: 't1',
        nodeType: 'service',
        metricName: 'requests_total',
        metricKind: 'counter',
        dimensionKey: '{}',
        value: 42,
      };
      getSpy.mockResolvedValueOnce({ data: [sample] });
      const out = await opsMetricsApi.current();
      expect(out).toEqual([sample]);
    });
  });

  describe('timeseries', () => {
    it('timeseries() emits all required params in URL', async () => {
      await opsMetricsApi.timeseries({
        nodeId: 't1',
        metric: 'requests_total',
        from: '2026-04-26T00:00:00Z',
        to: '2026-04-27T00:00:00Z',
        granularity: '1h',
      });
      const [url] = getSpy.mock.calls[0] as [string];
      expect(url.startsWith('/api/admin/ops-metrics/timeseries?')).toBe(true);
      expect(url).toContain('nodeId=t1');
      expect(url).toContain('metric=requests_total');
      expect(url).toContain('from=2026-04-26T00%3A00%3A00Z');
      expect(url).toContain('to=2026-04-27T00%3A00%3A00Z');
      expect(url).toContain('granularity=1h');
    });

    it('timeseries() omits granularity when undefined (server picks auto)', async () => {
      await opsMetricsApi.timeseries({
        nodeId: 't1',
        metric: 'requests_total',
        from: '2026-04-26T00:00:00Z',
        to: '2026-04-27T00:00:00Z',
      });
      const [url] = getSpy.mock.calls[0] as [string];
      expect(url).not.toContain('granularity');
    });

    it('timeseries() forwards optional dim filter', async () => {
      await opsMetricsApi.timeseries({
        nodeId: 't1',
        metric: 'requests_total',
        from: '2026-04-26T00:00:00Z',
        to: '2026-04-27T00:00:00Z',
        dim: 'status="500"',
      });
      const [url] = getSpy.mock.calls[0] as [string];
      expect(url).toContain('dim=status%3D%22500%22');
    });
  });

  describe('fleet', () => {
    it('fleet() requires nodeType + metric + from/to', async () => {
      await opsMetricsApi.fleet({
        nodeType: 'agent',
        metric: 'agent_active_total',
        from: '2026-04-26T00:00:00Z',
        to: '2026-04-27T00:00:00Z',
      });
      const [url] = getSpy.mock.calls[0] as [string];
      expect(url).toContain('nodeType=agent');
      expect(url).toContain('metric=agent_active_total');
      expect(url).toContain('from=2026-04-26T00%3A00%3A00Z');
      expect(url).toContain('to=2026-04-27T00%3A00%3A00Z');
    });
  });
});
