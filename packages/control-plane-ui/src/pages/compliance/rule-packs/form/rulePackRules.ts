import type { RulePackRule } from '@/api/services';

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
