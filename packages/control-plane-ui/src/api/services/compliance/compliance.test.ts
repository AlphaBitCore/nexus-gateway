import { describe, it, expect, vi, beforeEach, afterEach, type MockInstance } from 'vitest';
import { complianceApi } from './compliance';
import * as apiClient from '../../client';

type ApiFn = (path: string, ...rest: unknown[]) => Promise<unknown>;

describe('complianceApi', () => {
  let getSpy: MockInstance<ApiFn>;
  let putSpy: MockInstance<ApiFn>;
  let postSpy: MockInstance<ApiFn>;
  let deleteSpy: MockInstance<ApiFn>;

  beforeEach(() => {
    getSpy = vi.spyOn(apiClient.api, 'get') as unknown as MockInstance<ApiFn>;
    putSpy = vi.spyOn(apiClient.api, 'put') as unknown as MockInstance<ApiFn>;
    postSpy = vi.spyOn(apiClient.api, 'post') as unknown as MockInstance<ApiFn>;
    deleteSpy = vi.spyOn(apiClient.api, 'delete') as unknown as MockInstance<ApiFn>;
    getSpy.mockResolvedValue({});
    putSpy.mockResolvedValue({});
    postSpy.mockResolvedValue({});
    deleteSpy.mockResolvedValue(undefined);
  });
  afterEach(() => vi.restoreAllMocks());

  it('setKillSwitch POSTs /api/admin/compliance/killswitch with {engaged}', async () => {
    await complianceApi.setKillSwitch({ engaged: true });
    expect(postSpy).toHaveBeenCalledWith('/api/admin/compliance/killswitch', { engaged: true });
  });

  it('listExemptions GETs /api/admin/compliance/exemption-grants with tab + paging', async () => {
    await complianceApi.listExemptions({ tab: 'all', limit: 20, offset: 0 });
    expect(getSpy).toHaveBeenCalledWith(
      '/api/admin/compliance/exemption-grants?tab=all&limit=20&offset=0',
    );
  });
  it('listExemptions GETs without query when called with no params (server defaults)', async () => {
    await complianceApi.listExemptions();
    expect(getSpy).toHaveBeenCalledWith('/api/admin/compliance/exemption-grants');
  });
  it('getExemption GETs /api/admin/compliance/exemptions/:id', async () => {
    await complianceApi.getExemption('e1');
    expect(getSpy).toHaveBeenCalledWith('/api/admin/compliance/exemptions/e1');
  });
  it('listExemptions threads the pending tab through to the server', async () => {
    await complianceApi.listExemptions({ tab: 'pending', limit: 10, offset: 5 });
    expect(getSpy).toHaveBeenCalledWith(
      '/api/admin/compliance/exemption-grants?tab=pending&limit=10&offset=5',
    );
  });
  it('createExemptionGrant POSTs /api/admin/compliance/exemption-grants', async () => {
    await complianceApi.createExemptionGrant({
      sourceIp: '1.2.3.4',
      targetHost: 'x',
      durationMinutes: 60,
      reason: 'reason',
    });
    expect(postSpy).toHaveBeenCalledWith('/api/admin/compliance/exemption-grants', {
      sourceIP: '1.2.3.4',
      targetHost: 'x',
      durationMinutes: 60,
      reason: 'reason',
    });
  });
  it('patchExemptionGrant PATCHes /api/admin/compliance/exemption-grants/:id', async () => {
    const patchSpy = vi.spyOn(apiClient.api, 'patch') as unknown as MockInstance<ApiFn>;
    patchSpy.mockResolvedValue({});
    await complianceApi.patchExemptionGrant('e1', { inactive: true });
    expect(patchSpy).toHaveBeenCalledWith('/api/admin/compliance/exemption-grants/e1', { inactive: true });
  });
  it('createPendingExemptionRequest POSTs /api/admin/exemption-requests', async () => {
    await complianceApi.createPendingExemptionRequest({
      transactionId: 'tx-1',
      sourceIp: '10.0.0.1',
      targetHost: 'api.example.com',
      reason: 'need access',
      durationMinutes: 120,
      requestedBy: 'alice@nexus.ai',
    });
    expect(postSpy).toHaveBeenCalledWith('/api/admin/exemption-requests', {
      transactionId: 'tx-1',
      sourceIp: '10.0.0.1',
      targetHost: 'api.example.com',
      reason: 'need access',
      durationMinutes: 120,
      requestedBy: 'alice@nexus.ai',
    });
  });
  it('deleteExemptionGrant DELETEs /api/admin/compliance/exemption-grants/:id', async () => {
    await complianceApi.deleteExemptionGrant('e1');
    expect(deleteSpy).toHaveBeenCalledWith('/api/admin/compliance/exemption-grants/e1');
  });
  it('approveExemption POSTs /api/admin/compliance/exemptions/:id/approve', async () => {
    await complianceApi.approveExemption('e1');
    expect(postSpy).toHaveBeenCalledWith('/api/admin/compliance/exemptions/e1/approve', {});
  });
  it('rejectExemption POSTs /api/admin/compliance/exemptions/:id/reject', async () => {
    await complianceApi.rejectExemption('e1', 'bad');
    expect(postSpy).toHaveBeenCalledWith('/api/admin/compliance/exemptions/e1/reject', { reason: 'bad' });
  });
});
