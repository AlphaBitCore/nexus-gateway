/**
 * IAM type smoke tests — verify interfaces are correctly defined and usable.
 * Component rendering tests require jsdom + testing-library (not yet configured).
 */

import { describe, it, expect } from 'vitest';
import type {
  IamPolicy,
  IamPolicyDocument,
  IamStatement,
  IamGroup,
  IamGroupDetail,
  IamGroupMembership,
  IamPolicyAttachment,
  IamSimulationRequest,
  IamSimulationResponse,
  IamUserView,
} from '../../src/api/types';

describe('IAM types', () => {
  it('IamPolicyDocument has correct structure', () => {
    const doc: IamPolicyDocument = {
      Version: '2026-03-28',
      Statement: [
        {
          Sid: 'AllowRead',
          Effect: 'Allow',
          Action: ['admin:ReadProvider'],
          Resource: ['nrn:nexus:gateway:*:provider/*'],
        },
      ],
    };
    expect(doc.Version).toBe('2026-03-28');
    expect(doc.Statement).toHaveLength(1);
    expect(doc.Statement[0].Effect).toBe('Allow');
  });

  it('IamStatement supports conditions', () => {
    const stmt: IamStatement = {
      Effect: 'Deny',
      Action: ['gateway:InvokeModel'],
      Resource: ['nrn:nexus:gateway:*:model/gpt-4o'],
      Condition: {
        StringNotEquals: { 'nexus:Department': 'Research' },
        IpAddress: { 'nexus:SourceIp': '10.0.0.0/8' },
      },
    };
    expect(stmt.Condition).toBeDefined();
    expect(stmt.Condition!['StringNotEquals']['nexus:Department']).toBe('Research');
  });

  it('IamPolicy has all required fields', () => {
    const policy: IamPolicy = {
      id: 'pol-1',
      name: 'TestPolicy',
      description: 'A test policy',
      type: 'custom',
      document: { Version: '2026-03-28', Statement: [] },
      enabled: true,
      createdBy: 'admin',
      createdAt: '2026-03-28T00:00:00Z',
      updatedAt: '2026-03-28T00:00:00Z',
    };
    expect(policy.type).toBe('custom');
    expect(policy.enabled).toBe(true);
  });

  it('IamGroup has count fields', () => {
    const group: IamGroup = {
      id: 'grp-1',
      name: 'super-admins',
      description: 'Super admin group',
      memberCount: 3,
      policyCount: 1,
      createdAt: '2026-03-28T00:00:00Z',
      updatedAt: '2026-03-28T00:00:00Z',
    };
    expect(group.memberCount).toBe(3);
    expect(group.policyCount).toBe(1);
  });

  it('IamGroupDetail extends IamGroup with members and policies', () => {
    const detail: IamGroupDetail = {
      id: 'grp-1',
      name: 'super-admins',
      description: null,
      createdAt: '2026-03-28T00:00:00Z',
      updatedAt: '2026-03-28T00:00:00Z',
      members: [
        { id: 'm-1', groupId: 'grp-1', principalType: 'api_key', principalId: 'key-1', createdAt: '2026-03-28T00:00:00Z' },
      ],
      policyAttachments: [
        { id: 'a-1', policyId: 'pol-1', policy: { id: 'pol-1', name: 'NexusSuperAdmin', type: 'managed' }, createdAt: '2026-03-28T00:00:00Z' },
      ],
    };
    expect(detail.members).toHaveLength(1);
    expect(detail.policyAttachments[0].policy.name).toBe('NexusSuperAdmin');
  });

  it('IamSimulationRequest and Response have correct structure', () => {
    const req: IamSimulationRequest = {
      principal: { type: 'api_key', id: 'key-1' },
      action: 'admin:DeleteProvider',
      resource: 'nrn:nexus:gateway:*:provider/openai',
      context: { 'nexus:SourceIp': '10.0.1.5' },
    };
    expect(req.principal.type).toBe('api_key');

    const res: IamSimulationResponse = {
      decision: 'Deny',
      matchedStatements: [
        { policyId: 'pol-1', policyName: 'DenyProd', sid: 'BlockDelete', effect: 'Deny', source: 'direct' },
      ],
      reason: 'Explicit Deny in matched policy',
    };
    expect(res.decision).toBe('Deny');
    expect(res.matchedStatements).toHaveLength(1);
  });

  it('IamPolicyAttachment distinguishes direct vs group', () => {
    const direct: IamPolicyAttachment = {
      id: 'att-1',
      policyId: 'pol-1',
      source: 'direct',
      createdAt: '2026-03-28T00:00:00Z',
    };
    const group: IamPolicyAttachment = {
      id: 'att-2',
      policyId: 'pol-2',
      source: 'group',
      groupName: 'super-admins',
      createdAt: '2026-03-28T00:00:00Z',
    };
    expect(direct.source).toBe('direct');
    expect(group.groupName).toBe('super-admins');
  });

  it('IamUserView extends AdminApiKey with IAM data', () => {
    const user: IamUserView = {
      id: 'key-1',
      name: 'My Key',
      keyPrefix: 'nxk_abc',
      role: 'super_admin',
      enabled: true,
      createdAt: '2026-03-28T00:00:00Z',
      groups: [
        { id: 'grp-1', name: 'super-admins', membershipId: 'm-1' },
        { id: 'grp-2', name: 'compliance-team', membershipId: 'm-2' },
      ],
      directPolicyCount: 2,
    };
    expect(user.groups).toHaveLength(2);
    expect(user.directPolicyCount).toBe(2);
    expect(user.role).toBe('super_admin');
    expect(user.keyPrefix).toBe('nxk_abc');
  });
});
