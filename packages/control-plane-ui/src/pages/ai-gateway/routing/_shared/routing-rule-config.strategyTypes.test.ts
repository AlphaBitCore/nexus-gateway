import { describe, it, expect } from 'vitest';

import { STRATEGY_TYPES } from './routing-rule-config';

// STRATEGY_TYPES is the single source of truth the Strategy Type picker
// (create/edit) and the strategy filter (list page) both render. This test locks
// the canonical set so the two surfaces can never drift again — the original bug
// was the list filter deriving its options from whichever strategy values happened
// to appear in the current page of rules, silently omitting Fallback Chain and A/B
// Split and showing raw enum values.
describe('STRATEGY_TYPES', () => {
  it('is exactly the six user-selectable strategies, in canonical display order', () => {
    expect(STRATEGY_TYPES).toEqual([
      'single',
      'fallback',
      'loadbalance',
      'conditional',
      'ab_split',
      'smart',
    ]);
  });

  it('excludes the stage-0 pipeline type "policy" (chosen via the pipeline-stage control, not the strategy picker)', () => {
    expect(STRATEGY_TYPES).not.toContain('policy');
  });

  it('has no duplicate entries', () => {
    expect(new Set(STRATEGY_TYPES).size).toBe(STRATEGY_TYPES.length);
  });
});
