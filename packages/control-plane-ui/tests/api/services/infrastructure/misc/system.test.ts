import { describe, it, expect, beforeEach, afterEach, vi } from 'vitest';
import { api } from '../../../../../src/api/client';
import { systemApi } from '../../../../../src/api/services/infrastructure/misc/system';

vi.mock('../../../../../src/api/client', () => ({
  api: {
    get: vi.fn().mockResolvedValue({}),
    post: vi.fn().mockResolvedValue({}),
    put: vi.fn().mockResolvedValue({}),
    delete: vi.fn().mockResolvedValue(undefined),
  },
}));
const m = api as unknown as Record<'get' | 'post' | 'put' | 'delete', ReturnType<typeof vi.fn>>;

describe('systemApi — traffic + audit', () => {
  beforeEach(() => Object.values(m).forEach((f) => f.mockClear()));

  it('traffic storage + event detail + normalized sidecar', () => {
    systemApi.getTrafficStorage();
    systemApi.getTrafficEvent('t1');
    systemApi.getTrafficEventNormalized('t1');
    expect(m.get).toHaveBeenCalledWith('/api/admin/traffic/storage');
    expect(m.get).toHaveBeenCalledWith('/api/admin/traffic/t1');
    expect(m.get).toHaveBeenCalledWith('/api/admin/traffic/t1/normalized');
  });

  it('listTrafficEvents forwards a plain-object param map as-is', () => {
    systemApi.listTrafficEvents({ limit: '20' });
    expect(m.get).toHaveBeenCalledWith('/api/admin/traffic', { limit: '20' });
  });

  it('listTrafficEvents serializes URLSearchParams (repeatable tags) into the path', () => {
    const p = new URLSearchParams();
    p.append('tag', 'a');
    p.append('tag', 'b');
    systemApi.listTrafficEvents(p);
    expect(m.get).toHaveBeenCalledWith('/api/admin/traffic?tag=a&tag=b');
  });

  it('listTrafficEvents with an empty URLSearchParams omits the query string', () => {
    systemApi.listTrafficEvents(new URLSearchParams());
    expect(m.get).toHaveBeenCalledWith('/api/admin/traffic');
  });

  it('admin-audit list/export/me-scoped routes', () => {
    systemApi.listAdminAuditLogs({ page: '1' });
    systemApi.exportAdminAuditLogs({ format: 'csv' });
    systemApi.listMyAdminAuditLogs({ page: '1' });
    expect(m.get).toHaveBeenCalledWith('/api/admin/admin-audit-logs', { page: '1' });
    expect(m.get).toHaveBeenCalledWith('/api/admin/admin-audit-logs/export', { format: 'csv' });
    expect(m.get).toHaveBeenCalledWith('/api/admin/me/admin-audit-logs', { page: '1' });
  });
});

describe('systemApi — settings + models + cache', () => {
  beforeEach(() => Object.values(m).forEach((f) => f.mockClear()));

  it('provider health + settings get/put', () => {
    systemApi.listProviderHealth();
    systemApi.getSettings();
    systemApi.updateSettings({ defaultLocale: 'en' } as never);
    expect(m.get).toHaveBeenCalledWith('/api/admin/provider-health');
    expect(m.get).toHaveBeenCalledWith('/api/admin/settings');
    expect(m.put).toHaveBeenCalledWith('/api/admin/settings', { defaultLocale: 'en' });
  });

  it('models list/flat/update/delete', () => {
    systemApi.listModels({ provider: 'openai' });
    systemApi.listModelsFlat({ q: 'gpt' });
    systemApi.updateModel('mdl1', { enabled: false } as never);
    systemApi.deleteModel('mdl1');
    expect(m.get).toHaveBeenCalledWith('/api/admin/models', { provider: 'openai' });
    expect(m.get).toHaveBeenCalledWith('/api/admin/models/flat', { q: 'gpt' });
    expect(m.put).toHaveBeenCalledWith('/api/admin/models/mdl1', { enabled: false });
    expect(m.delete).toHaveBeenCalledWith('/api/admin/models/mdl1');
  });

  it('instances + me + cache preview', () => {
    systemApi.listInstances();
    systemApi.me();
    systemApi.previewCacheNormaliser({ body: '{}' } as never);
    expect(m.get).toHaveBeenCalledWith('/api/admin/instances');
    expect(m.get).toHaveBeenCalledWith('/api/admin/me');
    expect(m.post).toHaveBeenCalledWith('/api/admin/cache/preview', { body: '{}' });
  });
});

describe('systemApi — observability / payload / streaming / sso / siem / rollups', () => {
  beforeEach(() => Object.values(m).forEach((f) => f.mockClear()));

  it('observability + payload-capture + streaming-compliance get/put', () => {
    systemApi.getObservabilityConfig();
    systemApi.updateObservabilityConfig({ otelEnabled: true, samplingRate: 0.5, traceViewerUrl: 'u' });
    systemApi.getPayloadCaptureConfig();
    systemApi.updatePayloadCaptureConfig({ enabled: true } as never);
    systemApi.getStreamingComplianceConfig();
    systemApi.updateStreamingComplianceConfig({ mode: 'buffer' } as never);
    expect(m.get).toHaveBeenCalledWith('/api/admin/settings/observability');
    expect(m.put).toHaveBeenCalledWith('/api/admin/settings/observability', { otelEnabled: true, samplingRate: 0.5, traceViewerUrl: 'u' });
    expect(m.get).toHaveBeenCalledWith('/api/admin/settings/payload-capture');
    expect(m.put).toHaveBeenCalledWith('/api/admin/settings/payload-capture', { enabled: true });
    expect(m.get).toHaveBeenCalledWith('/api/admin/settings/streaming-compliance');
    expect(m.put).toHaveBeenCalledWith('/api/admin/settings/streaming-compliance', { mode: 'buffer' });
  });

  it('SSO config + token test + saml metadata fetch + providers', () => {
    systemApi.getSsoConfig();
    systemApi.updateSsoConfig({ enabled: true });
    systemApi.testSsoToken('jwt');
    systemApi.fetchSamlMetadata('https://idp/metadata');
    systemApi.getSsoProviders();
    expect(m.get).toHaveBeenCalledWith('/api/admin/settings/sso');
    expect(m.put).toHaveBeenCalledWith('/api/admin/settings/sso', { enabled: true });
    expect(m.post).toHaveBeenCalledWith('/api/admin/settings/sso/test', { token: 'jwt' });
    expect(m.post).toHaveBeenCalledWith('/api/admin/settings/sso/saml/fetch-metadata', { url: 'https://idp/metadata' });
    expect(m.get).toHaveBeenCalledWith('/api/admin/auth/sso/providers');
  });

  it('rollup jobs list + trigger interpolates the job name', () => {
    systemApi.listRollupJobs();
    systemApi.triggerRollupJob('cost-daily');
    expect(m.get).toHaveBeenCalledWith('/api/admin/rollup-jobs');
    expect(m.post).toHaveBeenCalledWith('/api/admin/rollup-jobs/cost-daily/trigger');
  });

  it('SIEM config get/put + test + event-types', () => {
    systemApi.getSiemConfig();
    systemApi.updateSiemConfig({ enabled: true });
    systemApi.sendSiemTestEvent();
    systemApi.listSiemEventTypes();
    expect(m.get).toHaveBeenCalledWith('/api/admin/settings/siem');
    expect(m.put).toHaveBeenCalledWith('/api/admin/settings/siem', { enabled: true });
    expect(m.post).toHaveBeenCalledWith('/api/admin/settings/siem/test');
    expect(m.get).toHaveBeenCalledWith('/api/admin/settings/siem/event-types');
  });
});

describe('systemApi.checkReady — readiness probe (raw fetch, tolerates 503)', () => {
  let fetchMock: ReturnType<typeof vi.fn>;
  beforeEach(() => {
    fetchMock = vi.fn();
    vi.stubGlobal('fetch', fetchMock);
  });
  afterEach(() => vi.unstubAllGlobals());

  it('returns the parsed not_ready body on a 503 with checks', async () => {
    fetchMock.mockResolvedValue({
      ok: false,
      status: 503,
      json: () => Promise.resolve({ status: 'not_ready', checks: { db: 'down' } }),
    });
    await expect(systemApi.checkReady()).resolves.toEqual({ status: 'not_ready', checks: { db: 'down' } });
  });

  it('returns the ready body on 200', async () => {
    fetchMock.mockResolvedValue({
      ok: true,
      status: 200,
      json: () => Promise.resolve({ status: 'ready', checks: { db: 'ok' } }),
    });
    await expect(systemApi.checkReady()).resolves.toEqual({ status: 'ready', checks: { db: 'ok' } });
  });

  it('returns status=unknown on a non-ok response without a structured body', async () => {
    fetchMock.mockResolvedValue({
      ok: false,
      status: 500,
      json: () => Promise.reject(new Error('not json')),
    });
    await expect(systemApi.checkReady()).resolves.toEqual({ status: 'unknown' });
  });
});
