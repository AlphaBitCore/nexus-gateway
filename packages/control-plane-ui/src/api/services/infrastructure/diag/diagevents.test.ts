import { describe, it, expect, vi, beforeEach, afterEach, type MockInstance } from 'vitest';
import { diagEventsApi } from './diagevents';
import * as apiClient from '../../../client';

type ApiFn = (path: string, ...rest: unknown[]) => Promise<unknown>;

describe('diagEventsApi', () => {
  let getSpy: MockInstance<ApiFn>;

  beforeEach(() => {
    getSpy = vi.spyOn(apiClient.api, 'get') as unknown as MockInstance<ApiFn>;
    getSpy.mockResolvedValue({ data: [], nextCursor: '' });
  });
  afterEach(() => vi.restoreAllMocks());

  describe('list', () => {
    it('list() with no params hits /api/admin/diag-events without trailing "?"', async () => {
      await diagEventsApi.list();
      expect(getSpy).toHaveBeenCalledWith('/api/admin/diag-events');
    });

    it('list({ level, limit }) emits both query params', async () => {
      await diagEventsApi.list({ level: 'error', limit: 5 });
      expect(getSpy).toHaveBeenCalledWith('/api/admin/diag-events?level=error&limit=5');
    });

    it('list({ nodeId, q, source, from, to, cursor }) builds full URL', async () => {
      await diagEventsApi.list({
        nodeId: 'agent-1',
        q: 'panic',
        source: 'agent',
        from: '2026-04-26T00:00:00Z',
        to: '2026-04-27T00:00:00Z',
        cursor: 'b64opaque',
      });
      const [url] = getSpy.mock.calls[0] as [string];
      expect(url).toContain('nodeId=agent-1');
      expect(url).toContain('q=panic');
      expect(url).toContain('source=agent');
      expect(url).toContain('from=2026-04-26T00%3A00%3A00Z');
      expect(url).toContain('cursor=b64opaque');
    });

    it('list() returns the {data, nextCursor} envelope as-is', async () => {
      const event = {
        id: 'e-1',
        nodeId: 't1',
        nodeType: 'agent',
        occurredAt: '2026-04-27T00:00:00Z',
        receivedAt: '2026-04-27T00:00:01Z',
        level: 'error',
        eventType: 'agent.error',
        source: 'agent',
        message: 'boom',
        messageHash: 'abc123',
        repeatCount: 1,
      };
      getSpy.mockResolvedValueOnce({ data: [event], nextCursor: 'next' });
      const out = await diagEventsApi.list();
      expect(out.data).toEqual([event]);
      expect(out.nextCursor).toBe('next');
    });
  });

  describe('groups', () => {
    it('groups({ from, to }) emits both required params', async () => {
      getSpy.mockResolvedValueOnce({ data: [] });
      await diagEventsApi.groups({
        from: '2026-04-26T00:00:00Z',
        to: '2026-04-27T00:00:00Z',
      });
      const [url] = getSpy.mock.calls[0] as [string];
      expect(url.startsWith('/api/admin/diag-events/groups?')).toBe(true);
      expect(url).toContain('from=2026-04-26T00%3A00%3A00Z');
      expect(url).toContain('to=2026-04-27T00%3A00%3A00Z');
    });

    it('groups() unwraps the { data } envelope', async () => {
      getSpy.mockResolvedValueOnce({
        data: [{ level: 'error', messageHash: 'h1', sampleMessage: 'x', affectedNodes: 1, totalOccurrences: 5, lastSeenAt: '2026-04-27T00:00:00Z' }],
      });
      const out = await diagEventsApi.groups({
        from: '2026-04-26T00:00:00Z',
        to: '2026-04-27T00:00:00Z',
      });
      expect(out).toHaveLength(1);
      expect(out[0].messageHash).toBe('h1');
    });
  });

  describe('crashCohorts', () => {
    it('crashCohorts({ from, to }) emits both params and unwraps data', async () => {
      getSpy.mockResolvedValueOnce({ data: [] });
      await diagEventsApi.crashCohorts({
        from: '2026-04-26T00:00:00Z',
        to: '2026-04-27T00:00:00Z',
      });
      const [url] = getSpy.mock.calls[0] as [string];
      expect(url.startsWith('/api/admin/diag-events/crash-cohorts?')).toBe(true);
    });
  });
});
