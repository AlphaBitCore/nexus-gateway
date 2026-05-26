import { describe, it, expect, vi, beforeEach, afterEach, type MockInstance } from 'vitest';
import { alertsApi } from './alerts';
import * as apiClient from '../../client';

// Typing the spy holders as `MockInstance<ApiFn>` sidesteps the overload
// mismatch between `api.get` (params?: …) and `api.post` / `api.put` (body?: unknown)
// that otherwise trips up `ReturnType<typeof vi.spyOn>`.
type ApiFn = (path: string, ...rest: unknown[]) => Promise<unknown>;

describe('alertsApi', () => {
  let getSpy: MockInstance<ApiFn>;
  let postSpy: MockInstance<ApiFn>;
  let putSpy: MockInstance<ApiFn>;
  let deleteSpy: MockInstance<ApiFn>;

  beforeEach(() => {
    getSpy = vi.spyOn(apiClient.api, 'get') as unknown as MockInstance<ApiFn>;
    postSpy = vi.spyOn(apiClient.api, 'post') as unknown as MockInstance<ApiFn>;
    putSpy = vi.spyOn(apiClient.api, 'put') as unknown as MockInstance<ApiFn>;
    deleteSpy = vi.spyOn(apiClient.api, 'delete') as unknown as MockInstance<ApiFn>;
    getSpy.mockResolvedValue({});
    postSpy.mockResolvedValue({});
    putSpy.mockResolvedValue({});
    deleteSpy.mockResolvedValue(undefined);
  });
  afterEach(() => vi.restoreAllMocks());

  describe('list', () => {
    it('list() with no params GETs /api/admin/alerts without trailing "?"', async () => {
      await alertsApi.list();
      expect(getSpy).toHaveBeenCalledWith('/api/admin/alerts');
    });

    it("list({ state: ['firing', 'acknowledged'] }) appends repeated state params", async () => {
      await alertsApi.list({ state: ['firing', 'acknowledged'] });
      expect(getSpy).toHaveBeenCalledWith('/api/admin/alerts?state=firing&state=acknowledged');
    });

    it('list with severity + offset + limit builds stable querystring', async () => {
      await alertsApi.list({ severity: ['critical'], offset: 20, limit: 10 });
      // URLSearchParams preserves insertion order: severity -> offset -> limit
      expect(getSpy).toHaveBeenCalledWith('/api/admin/alerts?severity=critical&offset=20&limit=10');
    });

    it('list({ since, until }) emits since=/until= matching Hub wire format', async () => {
      await alertsApi.list({
        since: '2026-04-21T00:00:00Z',
        until: '2026-04-22T00:00:00Z',
      });
      const [url] = getSpy.mock.calls[0] as [string];
      expect(url).toContain('since=2026-04-21T00%3A00%3A00Z');
      expect(url).toContain('until=2026-04-22T00%3A00%3A00Z');
    });
  });

  describe('detail / ack / resolve', () => {
    it("detail('a-1') GETs /api/admin/alerts/a-1", async () => {
      await alertsApi.detail('a-1');
      expect(getSpy).toHaveBeenCalledWith('/api/admin/alerts/a-1');
    });

    it("ack('a-1', 'looked into it') POSTs with { reason }", async () => {
      await alertsApi.ack('a-1', 'looked into it');
      expect(postSpy).toHaveBeenCalledWith('/api/admin/alerts/a-1/ack', {
        reason: 'looked into it',
      });
    });

    it("resolve('a-1') POSTs with { reason: undefined } when no reason given", async () => {
      await alertsApi.resolve('a-1');
      expect(postSpy).toHaveBeenCalledWith('/api/admin/alerts/a-1/resolve', {
        reason: undefined,
      });
    });
  });

  describe('rules', () => {
    it("updateRule('quota.threshold', { enabled: false }) PUTs to the rule path", async () => {
      await alertsApi.updateRule('quota.threshold', { enabled: false });
      expect(putSpy).toHaveBeenCalledWith('/api/admin/alerts/rules/quota.threshold', {
        enabled: false,
      });
    });
  });

  describe('channels', () => {
    it("testChannel('c-1') POSTs {} to the test endpoint", async () => {
      await alertsApi.testChannel('c-1');
      expect(postSpy).toHaveBeenCalledWith('/api/admin/alerts/channels/c-1/test', {});
    });
  });
});
