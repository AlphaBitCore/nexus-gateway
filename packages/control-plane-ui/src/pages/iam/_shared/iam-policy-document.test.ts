import { describe, it, expect } from 'vitest';
import {
  documentToStatements,
  statementsToDocument,
} from '../_shared/iam-policy-document';

/**
 * iam-policy-document round-trip tests — exercise the AWS-shape
 * tolerance the Control Plane must guarantee:
 *
 *   1. Parse(string Action / Resource) — pasting a vendor AWS policy
 *      that uses the single-string form for one-element lists.
 *   2. Parse(array Action / Resource) — the form authored via the
 *      Visual Editor's chip input.
 *   3. Serialize-then-parse round-trip preserves chip-list contents.
 *   4. Length-1 chip list serializes back to bare string
 *      (AWS-canonical, matches the Go-side iam.StringList).
 */

describe('iam-policy-document — AWS shape parity', () => {
  it('parses Action / Resource as single string (AWS form)', () => {
    const doc = {
      Version: '2012-10-17',
      Statement: [
        { Effect: 'Allow', Action: '*', Resource: '*' },
      ],
    };
    const { statements } = documentToStatements(doc);
    expect(statements).toHaveLength(1);
    expect(statements[0].effect).toBe('Allow');
    expect(statements[0].actions).toBe('*');
    expect(statements[0].resources).toBe('*');
  });

  it('parses Action / Resource as array', () => {
    const doc = {
      Version: '2026-03-28',
      Statement: [
        { Effect: 'Allow', Action: ['admin:provider.read', 'admin:provider.create'], Resource: ['nrn:nexus:gateway:*:provider/*'] },
      ],
    };
    const { statements } = documentToStatements(doc);
    expect(statements[0].actions).toBe('admin:provider.read\nadmin:provider.create');
    expect(statements[0].resources).toBe('nrn:nexus:gateway:*:provider/*');
  });

  it('serializes length-1 list as bare string (AWS canonical)', () => {
    const out = statementsToDocument('2026-03-28', [
      { sid: '', effect: 'Allow', actions: 'admin:provider.read', resources: 'nrn:nexus:gateway:*:provider/*', conditionJson: '' },
    ]) as { Statement: Array<Record<string, unknown>> };
    const stmt = out.Statement[0];
    expect(stmt.Action).toBe('admin:provider.read');
    expect(stmt.Resource).toBe('nrn:nexus:gateway:*:provider/*');
  });

  it('serializes length>1 list as array', () => {
    const out = statementsToDocument('2026-03-28', [
      { sid: '', effect: 'Allow', actions: 'admin:provider.read\nadmin:provider.create', resources: 'a\nb', conditionJson: '' },
    ]) as { Statement: Array<Record<string, unknown>> };
    const stmt = out.Statement[0];
    expect(stmt.Action).toEqual(['admin:provider.read', 'admin:provider.create']);
    expect(stmt.Resource).toEqual(['a', 'b']);
  });

  it('round-trip: parse → serialize → parse preserves all fields', () => {
    const input = {
      Version: '2012-10-17',
      Statement: [
        { Sid: 'AllowRead', Effect: 'Allow', Action: 's3:GetObject', Resource: ['arn:aws:s3:::bucket/*'] },
        { Effect: 'Allow', Action: ['s3:PutObject', 's3:DeleteObject'], Resource: '*' },
      ],
    };
    const { version, statements } = documentToStatements(input);
    const serialized = statementsToDocument(version, statements);
    const { statements: round } = documentToStatements(serialized);
    expect(round).toEqual(statements);
  });
});

describe('iam-policy-document — real AWS policy samples', () => {
  // Three real AWS console-exported policies used in the
  // request. Each must (a) parse without errors, (b) round-trip
  // through serialize+parse with identical chip-list contents.
  const samples: Record<string, unknown> = {
    'Vercel marketplace (mixed services + conditions)': {
      Version: '2012-10-17',
      Statement: [
        {
          Effect: 'Allow',
          Action: ['account:CloseAccount', 'ce:GetCostAndUsage', 'iam:ListSAMLProviders', 'freetier:GetFreeTierUsage'],
          Resource: '*',
        },
        {
          Effect: 'Allow',
          Action: ['iam:UpdateSamlProvider', 'iam:GetSamlProvider'],
          Resource: '*',
          Condition: { StringEquals: { 'aws:ResourceTag/VercelInstallId': '${aws:PrincipalTag/VercelInstallId}' } },
        },
        {
          Sid: 'ManageServiceRole',
          Effect: 'Allow',
          Action: ['iam:GetRole', 'iam:CreateRole', 'iam:AttachRolePolicy'],
          Resource: 'arn:aws:iam::*:role/Vercel/Service_2026_04_16',
          Condition: {
            StringEquals: {
              'aws:ResourceTag/VercelInstallId': '${aws:PrincipalTag/VercelInstallId}',
              'iam:PermissionsBoundary': ['arn:aws:iam::partner:policy/permissions-boundary/vercel.com/VercelMarketplaceServiceRoleBoundary_2026_04_16'],
            },
          },
        },
      ],
    },
    'Full admin wildcard (Action="*", Resource="*")': {
      Version: '2012-10-17',
      Statement: [
        { Effect: 'Allow', Action: '*', Resource: '*' },
      ],
    },
    'AIOps + SSO (verb-prefix wildcards)': {
      Version: '2012-10-17',
      Statement: [
        {
          Sid: 'AIOpsReadOnlyAccess',
          Effect: 'Allow',
          Action: ['aiops:Get*', 'aiops:List*', 'aiops:ValidateInvestigationGroup'],
          Resource: '*',
        },
        {
          Sid: 'SSOManagementAccess',
          Effect: 'Allow',
          Action: ['identitystore:DescribeUser', 'sso:DescribeInstance', 'sso-directory:DescribeUsers'],
          Resource: '*',
        },
      ],
    },
  };

  for (const [name, sample] of Object.entries(samples)) {
    it(`round-trips: ${name}`, () => {
      const { version, statements } = documentToStatements(sample);
      expect(version).toBeTruthy();
      expect(statements.length).toBeGreaterThan(0);

      // Re-serialize and re-parse. The reconstructed StatementEntry
      // values must equal the originals (chip-list contents preserved
      // byte-for-byte modulo whitespace).
      const serialized = statementsToDocument(version, statements);
      const { statements: round } = documentToStatements(serialized);
      expect(round).toEqual(statements);
    });
  }

  it('Vercel sample: Condition object preserves nested arrays + variables verbatim', () => {
    const sample = samples['Vercel marketplace (mixed services + conditions)'] as { Statement: Array<{ Condition?: Record<string, unknown> }> };
    const { statements } = documentToStatements(sample);
    // Statement 2 has a single-key StringEquals; the chip layer
    // stores it as a JSON string so it round-trips when re-serialized.
    expect(statements[1].conditionJson).toContain('aws:ResourceTag/VercelInstallId');
    expect(statements[1].conditionJson).toContain('aws:PrincipalTag/VercelInstallId');
    // Statement 3 has the nested array form.
    expect(statements[2].conditionJson).toContain('iam:PermissionsBoundary');
    expect(statements[2].conditionJson).toContain('VercelMarketplaceServiceRoleBoundary_2026_04_16');
  });
});
