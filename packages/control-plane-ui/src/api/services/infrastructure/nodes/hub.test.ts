import { describe, it, expect, vi, beforeEach, afterEach, type MockInstance } from 'vitest';
import { hubApi } from './hub';
import * as apiClient from '../../../client';

// Same API-fn typing trick as alerts.test.ts — sidesteps the overload
// mismatch between `api.get(path, params?)` and `api.put / api.post / api.delete(path, body?)`.
type ApiFn = (path: string, ...rest: unknown[]) => Promise<unknown>;

describe('hubApi (per-Thing override + resync surface)', () => {
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

  describe('listNodes', () => {
    it('serializes hasOverrides=true as the literal string "true"', async () => {
      await hubApi.listNodes({ hasOverrides: true });
      expect(getSpy).toHaveBeenCalledWith('/api/admin/nodes', { hasOverrides: 'true' });
    });

    it('serializes hasOverrides=false as the literal string "false"', async () => {
      await hubApi.listNodes({ hasOverrides: false });
      expect(getSpy).toHaveBeenCalledWith('/api/admin/nodes', { hasOverrides: 'false' });
    });

    it('omits the hasOverrides param entirely when not provided', async () => {
      await hubApi.listNodes({ type: 'ai-gateway' });
      expect(getSpy).toHaveBeenCalledWith('/api/admin/nodes', { type: 'ai-gateway' });
    });
  });

  describe('listOverrides', () => {
    it("GETs /api/admin/nodes/<id>/overrides with the id URL-encoded", async () => {
      await hubApi.listOverrides('node a/1');
      expect(getSpy).toHaveBeenCalledWith('/api/admin/nodes/node%20a%2F1/overrides');
    });
  });

  describe('setOverride', () => {
    it('PUTs /api/admin/nodes/<id>/overrides/<configKey> with the body', async () => {
      const body = { state: { strategy: 'sticky' }, reason: 'incident-resp' };
      await hubApi.setOverride('gw-1', 'routing_rules', body);
      expect(putSpy).toHaveBeenCalledWith(
        '/api/admin/nodes/gw-1/overrides/routing_rules',
        body,
      );
    });

    it('URL-encodes both the thingId and configKey path params', async () => {
      await hubApi.setOverride('a b', 'k/x', { state: {} });
      expect(putSpy).toHaveBeenCalledWith(
        '/api/admin/nodes/a%20b/overrides/k%2Fx',
        { state: {} },
      );
    });
  });

  describe('clearOverride', () => {
    it('DELETEs /api/admin/nodes/<id>/overrides/<configKey>', async () => {
      await hubApi.clearOverride('gw-1', 'routing_rules');
      expect(deleteSpy).toHaveBeenCalledWith(
        '/api/admin/nodes/gw-1/overrides/routing_rules',
      );
      // api.delete wrapper resolves with no body — we just confirm the call
      // shape; the server's {ok:true} envelope is intentionally discarded.
    });
  });

  describe('listGlobalOverrides', () => {
    it('forwards every filter as a stringified query param', async () => {
      await hubApi.listGlobalOverrides({
        type: 'ai-gateway',
        actor: 'admin@nexus.ai',
        hasTtl: true,
        stale: false,
        limit: 50,
        offset: 100,
      });
      expect(getSpy).toHaveBeenCalledWith('/api/admin/nodes/overrides', {
        type: 'ai-gateway',
        actor: 'admin@nexus.ai',
        hasTtl: 'true',
        stale: 'false',
        limit: '50',
        offset: '100',
      });
    });

    it('drops undefined fields and emits no params object when all are absent', async () => {
      await hubApi.listGlobalOverrides();
      expect(getSpy).toHaveBeenCalledWith('/api/admin/nodes/overrides', undefined);
    });
  });

  describe('resyncThing', () => {
    it('POSTs an empty object when no body is passed (whole-Thing resync)', async () => {
      await hubApi.resyncNodeAll('gw-1');
      expect(postSpy).toHaveBeenCalledWith('/api/admin/nodes/gw-1/resync', {});
    });

    it('POSTs the supplied configKey body for single-key resync', async () => {
      await hubApi.resyncNodeAll('gw-1', { configKey: 'routing_rules' });
      expect(postSpy).toHaveBeenCalledWith(
        '/api/admin/nodes/gw-1/resync',
        { configKey: 'routing_rules' },
      );
    });
  });
});
