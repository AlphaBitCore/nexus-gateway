import { describe, it, expect } from 'vitest';
import { formatModelLabels, smartLiteralsProblem } from '../../../../../src/pages/ai-gateway/routing/_shared/routing-rule-config';
import type { AdminModelsByProvider } from '@/api/types';

describe('routing-rule-config safety', () => {
  it('formatModelLabels handles groups with undefined provider', () => {
    const groups = [
      { provider: undefined, models: [{ id: 'm1', name: 'GPT-4' }] },
    ] as unknown as AdminModelsByProvider[];

    // Should not crash — renders with "?" fallback for missing provider
    const result = formatModelLabels(groups, ['m1']);
    expect(result).toBe('? / GPT-4');
  });

  it('formatModelLabels handles empty groups', () => {
    expect(formatModelLabels([], ['m1'])).toBe('m1');
  });

  it('formatModelLabels handles empty modelIds', () => {
    expect(formatModelLabels([], [])).toBe('');
  });

  it('formatModelLabels resolves valid provider + model', () => {
    const groups = [
      {
        provider: { id: 'p1', name: 'openai', displayName: 'OpenAI' },
        models: [{ id: 'm1', name: 'GPT-4', providerModelId: 'gpt-4' }],
      },
    ] as unknown as AdminModelsByProvider[];

    expect(formatModelLabels(groups, ['m1'])).toBe('OpenAI / GPT-4');
  });
});

// smartLiteralsProblem is the wizard's copy of the Go guard
// (validateSmartRuleMatchConditions / rejectSmartLiteral). The two must agree,
// or the wizard lets an operator finish a rule the API then refuses — or blocks
// one the API would accept. Each case below mirrors an arm of the Go table in
// packages/control-plane/internal/ai/routing/handler/validate_test.go.
describe('smartLiteralsProblem — the wizard copy of the smart-rule guard', () => {
  it('accepts a keyword that is not "auto"', () => {
    expect(smartLiteralsProblem(['route-me'])).toBeNull();
  });

  it('accepts several keywords — the point of the rule', () => {
    expect(smartLiteralsProblem(['auto', 'fast', 'cheap'])).toBeNull();
  });

  it('accepts a bounded glob', () => {
    expect(smartLiteralsProblem(['gpt-4-*'])).toBeNull();
  });

  it('refuses a rule that pins no keyword at all', () => {
    expect(smartLiteralsProblem([])).toEqual({ kind: 'none-pinned' });
  });

  it('refuses the everything-glob, naming the offending entry', () => {
    expect(smartLiteralsProblem(['*'])).toEqual({ kind: 'catch-all', literal: '*' });
  });

  it('refuses an all-stars glob however many stars it has', () => {
    expect(smartLiteralsProblem(['**'])).toEqual({ kind: 'catch-all', literal: '**' });
  });

  it('refuses one everything-glob hidden among real keywords', () => {
    expect(smartLiteralsProblem(['auto', 'fast', '*'])).toEqual({ kind: 'catch-all', literal: '*' });
  });

  it('refuses an empty entry — no request can carry an empty model', () => {
    expect(smartLiteralsProblem([''])).toEqual({ kind: 'blank', literal: '' });
  });

  it('refuses a whitespace-only entry, reporting it verbatim', () => {
    expect(smartLiteralsProblem(['auto', '   '])).toEqual({ kind: 'blank', literal: '   ' });
  });

  it('leaves a glob that carries a literal character alone', () => {
    // "?*" is bounded: MatchGlob quotes the "?", so it needs a real prefix.
    expect(smartLiteralsProblem(['?*'])).toBeNull();
  });
});
