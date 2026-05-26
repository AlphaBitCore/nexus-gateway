/**
 * IAM policy document validation and form ↔ JSON conversion.
 * Mirrors packages/control-plane/internal/iam/ — keep in sync when server rules change.
 */

export const DEFAULT_IAM_POLICY_VERSION = '2026-03-28';

const MAX_STATEMENTS = 50;
const VALID_CONDITION_OPERATORS = [
  'StringEquals', 'StringNotEquals', 'StringLike',
  'IpAddress', 'NotIpAddress',
  'NumericLessThan', 'NumericGreaterThan', 'NumericEquals',
  'DateLessThan', 'DateGreaterThan',
];

export interface StatementEntry {
  sid: string;
  effect: 'Allow' | 'Deny';
  actions: string;
  resources: string;
  /** Stringified JSON object for optional Condition block; empty if none */
  conditionJson: string;
}

export function validateIamPolicyDocument(doc: unknown): { valid: boolean; errors: string[] } {
  const errors: string[] = [];

  if (!doc || typeof doc !== 'object') {
    return { valid: false, errors: ['Policy document must be an object'] };
  }

  const d = doc as Record<string, unknown>;

  if (!d.Version || typeof d.Version !== 'string') {
    errors.push('Version is required and must be a string');
  }

  if (!Array.isArray(d.Statement)) {
    errors.push('Statement must be an array');
    return { valid: false, errors };
  }

  if (d.Statement.length === 0) {
    errors.push('Statement array must contain at least one statement');
  }

  if (d.Statement.length > MAX_STATEMENTS) {
    errors.push(`Statement array exceeds maximum of ${MAX_STATEMENTS} statements`);
  }

  for (let i = 0; i < d.Statement.length; i++) {
    const stmt = d.Statement[i] as Record<string, unknown>;
    const prefix = `Statement[${i}]`;

    if (!stmt || typeof stmt !== 'object') {
      errors.push(`${prefix}: must be an object`);
      continue;
    }

    if (stmt.Effect !== 'Allow' && stmt.Effect !== 'Deny') {
      errors.push(`${prefix}.Effect must be "Allow" or "Deny"`);
    }

    // AWS allows Action and Resource as either a single string OR an
    // array of strings. The Go-side iam.StringList type accepts both,
    // and statementsToDocument now emits the canonical form (bare
    // string for length==1). The validator must accept either shape.
    const validateStringOrArray = (value: unknown, label: string) => {
      let entries: unknown[];
      if (typeof value === 'string') {
        entries = [value];
      } else if (Array.isArray(value)) {
        entries = value;
      } else {
        errors.push(`${prefix}.${label} must be a string or non-empty array of strings`);
        return;
      }
      if (entries.length === 0) {
        errors.push(`${prefix}.${label} must be a string or non-empty array of strings`);
        return;
      }
      for (const e of entries) {
        if (typeof e !== 'string' || e.length === 0) {
          errors.push(`${prefix}.${label} entries must be non-empty strings`);
          break;
        }
        if (e.includes('**')) {
          errors.push(`${prefix}.${label}: consecutive wildcards not allowed in "${e}"`);
        }
      }
    };
    validateStringOrArray(stmt.Action, 'Action');
    validateStringOrArray(stmt.Resource, 'Resource');

    if (stmt.Condition !== undefined) {
      if (typeof stmt.Condition !== 'object' || stmt.Condition === null) {
        errors.push(`${prefix}.Condition must be an object if provided`);
      } else {
        for (const [op, conds] of Object.entries(stmt.Condition as Record<string, unknown>)) {
          if (!VALID_CONDITION_OPERATORS.includes(op)) {
            errors.push(`${prefix}.Condition: unknown operator "${op}"`);
          }
          if (typeof conds !== 'object' || conds === null) {
            errors.push(`${prefix}.Condition.${op} must be an object`);
          }
        }
      }
    }
  }

  return { valid: errors.length === 0, errors };
}

function stringifyCondition(cond: unknown): string {
  if (cond === undefined || cond === null) return '';
  try {
    return JSON.stringify(cond, null, 2);
  } catch {
    return '';
  }
}

export function documentToStatements(doc: unknown): { version: string; statements: StatementEntry[] } {
  if (!doc || typeof doc !== 'object') {
    return {
      version: DEFAULT_IAM_POLICY_VERSION,
      statements: [{
        sid: '',
        effect: 'Allow',
        actions: 'admin:*.read',
        resources: 'nrn:nexus:*:*:*/*',
        conditionJson: '',
      }],
    };
  }
  const d = doc as Record<string, unknown>;
  const version = typeof d.Version === 'string' && d.Version.trim() ? d.Version : DEFAULT_IAM_POLICY_VERSION;
  const stmts = Array.isArray(d.Statement) ? d.Statement : [];
  if (stmts.length === 0) {
    return {
      version,
      statements: [{
        sid: '',
        effect: 'Allow',
        actions: 'admin:*.read',
        resources: 'nrn:nexus:*:*:*/*',
        conditionJson: '',
      }],
    };
  }
  return {
    version,
    statements: stmts.map((s: Record<string, unknown>) => ({
      sid: String(s.Sid ?? ''),
      effect: s.Effect === 'Deny' ? 'Deny' as const : 'Allow' as const,
      actions: Array.isArray(s.Action)
        ? (s.Action as string[]).join('\n')
        : String(s.Action ?? ''),
      resources: Array.isArray(s.Resource)
        ? (s.Resource as string[]).join('\n')
        : String(s.Resource ?? ''),
      conditionJson: stringifyCondition(s.Condition),
    })),
  };
}

// AWS-canonical normalization: a one-element StringList serializes as a
// bare string, longer lists as an array. Mirrors
// packages/control-plane/internal/iam.StringList.MarshalJSON so a
// document round-tripped frontend → backend → frontend stays
// byte-identical (vs. always-array which would drift on length==1).
function awsCanonical(list: string[]): string | string[] {
  return list.length === 1 ? list[0] : list;
}

export function statementsToDocument(version: string, statements: StatementEntry[]): Record<string, unknown> {
  return {
    Version: version.trim() || DEFAULT_IAM_POLICY_VERSION,
    Statement: statements.map((s) => {
      const actions = s.actions.split('\n').map((a) => a.trim()).filter(Boolean);
      const resources = s.resources.split('\n').map((r) => r.trim()).filter(Boolean);
      const row: Record<string, unknown> = {
        Effect: s.effect,
        Action: awsCanonical(actions),
        Resource: awsCanonical(resources),
      };
      if (s.sid.trim()) row.Sid = s.sid.trim();
      if (s.conditionJson.trim()) {
        try {
          row.Condition = JSON.parse(s.conditionJson) as unknown;
        } catch {
          /* caller should validate condition JSON before build */
        }
      }
      return row;
    }),
  };
}

/** Parse full document from JSON string; returns errors for invalid JSON or failed validation */
export function parsePolicyDocumentJson(jsonText: string): { ok: true; document: Record<string, unknown> } | { ok: false; errors: string[] } {
  let parsed: unknown;
  try {
    parsed = JSON.parse(jsonText);
  } catch (e) {
    return { ok: false, errors: [(e as Error).message] };
  }
  const v = validateIamPolicyDocument(parsed);
  if (!v.valid) return { ok: false, errors: v.errors };
  return { ok: true, document: parsed as Record<string, unknown> };
}
