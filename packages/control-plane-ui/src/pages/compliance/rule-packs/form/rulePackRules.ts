import type { RulePackRule } from '@/api/services';

// Mirror the backend authoring contract in
// packages/shared/policy/rulepack/yaml.go (packNameRE / semverRE) so the JSON
// admin form rejects a malformed name/version inline — with a friendly hint —
// instead of round-tripping to the API and surfacing the raw 400 detail
// (`rulepack: name "test" must match <namespace>/<short-name>`).
const PACK_NAME_RE = /^[a-z][a-z0-9-]*\/[a-z][a-z0-9-]*$/;
const PACK_VERSION_RE = /^v\d+\.\d+\.\d+(?:[-+][A-Za-z0-9._-]+)?$/;

/** True when name matches "<namespace>/<short-name>" (lowercase, digits, hyphens). */
export function isValidPackName(name: string): boolean {
  return PACK_NAME_RE.test(name);
}

/** True when version is a v-prefixed semver, e.g. v1.0.0 or v1.2.3-rc1. */
export function isValidPackVersion(version: string): boolean {
  return PACK_VERSION_RE.test(version);
}

export interface RuleDraft {
  ruleId: string;
  category: string;
  severity: string;
  pattern: string;
  flags: string;
  description: string;
  labels: string;
}

export function emptyRuleDraft(): RuleDraft {
  return {
    ruleId: '',
    category: '',
    severity: 'hard',
    pattern: '',
    flags: '',
    description: '',
    labels: '',
  };
}

/** Seed JSON shown in the create form's rules editor: one example PII rule. */
export const DEFAULT_RULES_JSON = serializeRules([
  {
    ruleId: 'example-email',
    category: 'pii',
    severity: 'hard',
    pattern: '[\\w.+-]+@[\\w-]+\\.[\\w.-]+',
    description: 'Blocks email addresses',
    labels: ['pii:email'],
  },
]);

export function serializeRules(rules: RulePackRule[]): string {
  return JSON.stringify(
    rules.map((rule) => ({
      ruleId: rule.ruleId,
      category: rule.category,
      severity: rule.severity,
      pattern: rule.pattern,
      ...(rule.flags ? { flags: rule.flags } : {}),
      ...(rule.description ? { description: rule.description } : {}),
      ...(rule.labels && rule.labels.length > 0 ? { labels: rule.labels } : {}),
    })),
    null,
    2,
  );
}

export function parseRules(raw: string): { rules: RulePackRule[] | null; error: string | null } {
  const trimmed = raw.trim();
  if (trimmed === '') {
    return { rules: null, error: 'Rules JSON is required' };
  }
  try {
    const parsed = JSON.parse(trimmed);
    if (!Array.isArray(parsed)) {
      return { rules: null, error: 'Rules JSON must be an array' };
    }
    const rules: RulePackRule[] = [];
    for (let i = 0; i < parsed.length; i += 1) {
      const r = parsed[i];
      if (!r || typeof r !== 'object') {
        return { rules: null, error: `Rule #${i + 1}: must be an object` };
      }
      const rule = r as Record<string, unknown>;
      if (typeof rule.ruleId !== 'string' || rule.ruleId.trim() === '') {
        return { rules: null, error: `Rule #${i + 1}: "ruleId" required` };
      }
      if (typeof rule.category !== 'string' || rule.category.trim() === '') {
        return { rules: null, error: `Rule #${i + 1}: "category" required` };
      }
      if (typeof rule.severity !== 'string' || rule.severity.trim() === '') {
        return { rules: null, error: `Rule #${i + 1}: "severity" required` };
      }
      if (typeof rule.pattern !== 'string' || rule.pattern.trim() === '') {
        return { rules: null, error: `Rule #${i + 1}: "pattern" required` };
      }
      rules.push({
        ruleId: rule.ruleId,
        category: rule.category,
        severity: rule.severity,
        pattern: rule.pattern,
        flags: typeof rule.flags === 'string' ? rule.flags : undefined,
        description: typeof rule.description === 'string' ? rule.description : undefined,
        labels: Array.isArray(rule.labels)
          ? rule.labels.filter((l): l is string => typeof l === 'string')
          : undefined,
      });
    }
    return { rules, error: null };
  } catch (err) {
    return { rules: null, error: err instanceof Error ? err.message : String(err) };
  }
}

export function rulesToDrafts(rules: RulePackRule[]): RuleDraft[] {
  return rules.map((rule) => ({
    ruleId: rule.ruleId,
    category: rule.category,
    severity: rule.severity,
    pattern: rule.pattern,
    flags: rule.flags ?? '',
    description: rule.description ?? '',
    labels: rule.labels?.join(', ') ?? '',
  }));
}

export function draftsToRules(drafts: RuleDraft[]): { rules: RulePackRule[] | null; error: string | null } {
  const rules: RulePackRule[] = [];
  for (let i = 0; i < drafts.length; i += 1) {
    const draft = drafts[i];
    if (draft.ruleId.trim() === '') return { rules: null, error: `Rule #${i + 1}: "ruleId" required` };
    if (draft.category.trim() === '') return { rules: null, error: `Rule #${i + 1}: "category" required` };
    if (draft.severity.trim() === '') return { rules: null, error: `Rule #${i + 1}: "severity" required` };
    if (draft.pattern.trim() === '') return { rules: null, error: `Rule #${i + 1}: "pattern" required` };
    const labels = draft.labels
      .split(',')
      .map((item) => item.trim())
      .filter((item) => item.length > 0);
    rules.push({
      ruleId: draft.ruleId.trim(),
      category: draft.category.trim(),
      severity: draft.severity.trim(),
      pattern: draft.pattern,
      flags: draft.flags.trim() === '' ? undefined : draft.flags.trim(),
      description: draft.description.trim() === '' ? undefined : draft.description.trim(),
      labels: labels.length > 0 ? labels : undefined,
    });
  }
  return { rules, error: null };
}
