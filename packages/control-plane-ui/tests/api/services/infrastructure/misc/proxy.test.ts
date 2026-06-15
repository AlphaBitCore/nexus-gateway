import { describe, it, expect, beforeEach, vi } from 'vitest';
import { api } from '../../../../../src/api/client';
import { proxyApi } from '../../../../../src/api/services/infrastructure/misc/proxy';
vi.mock('../../../../../src/api/client', () => ({ api: { get: vi.fn().mockResolvedValue({}), post: vi.fn().mockResolvedValue({}), put: vi.fn().mockResolvedValue({}), patch: vi.fn().mockResolvedValue({}), delete: vi.fn().mockResolvedValue(undefined) } }));
const m = api as unknown as Record<'get' | 'post' | 'put' | 'patch' | 'delete', ReturnType<typeof vi.fn>>;
beforeEach(() => Object.values(m).forEach((f) => f.mockClear()));
describe('proxyApi', () => {
  it('status / connections / coverage / hook-health / reject-stats', () => {
    proxyApi.getStatus();
    proxyApi.getConnections('host.example');
    proxyApi.getConnections();
    proxyApi.getComplianceCoverage('s', 'e');
    proxyApi.getHookHealth('s', 'e');
    proxyApi.getRejectStats('s', 'e');
    expect(m.get).toHaveBeenCalledWith('/api/admin/proxy/health');
    expect(m.get).toHaveBeenCalledWith('/api/admin/proxy/connections?targetHost=host.example');
    expect(m.get).toHaveBeenCalledWith('/api/admin/proxy/connections');
    expect(m.get).toHaveBeenCalledWith('/api/admin/proxy/compliance/coverage?startTime=s&endTime=e');
    expect(m.get).toHaveBeenCalledWith('/api/admin/proxy/compliance/hook-health?startTime=s&endTime=e');
    expect(m.get).toHaveBeenCalledWith('/api/admin/proxy/compliance/reject-stats?startTime=s&endTime=e');
  });
  it('buildComplianceExportUrl includes only the set optional params', () => {
    const url = proxyApi.buildComplianceExportUrl({ startTime: 's', endTime: 'e', targetHost: 'h' });
    expect(url).toContain('/api/admin/proxy/compliance/export?');
    expect(url).toContain('startTime=s');
    expect(url).toContain('targetHost=h');
    expect(url).not.toContain('sourceIp');
  });
});
