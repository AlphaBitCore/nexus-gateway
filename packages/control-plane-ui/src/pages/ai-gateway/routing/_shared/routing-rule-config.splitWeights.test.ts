import { describe, it, expect } from 'vitest';

import { validateSplitWeights, type ProviderModelEntry } from './routing-rule-config';

const entry = (weight: string): ProviderModelEntry => ({ provider: 'p', model: 'm', weight });

// validateSplitWeights backs the sum-to-100 gate on ab_split "Split %" and
// loadbalance "Weight" targets. The resolvers treat weights as relative
// (weighted random over the sum), so the UI enforces 100 to keep the entered
// numbers truthful and the split predictable.
describe('validateSplitWeights', () => {
  it('accepts weights that sum to exactly 100', () => {
    expect(validateSplitWeights([entry('70'), entry('30')])).toEqual({ valid: true, total: 100 });
    expect(validateSplitWeights([entry('50'), entry('25'), entry('25')])).toEqual({ valid: true, total: 100 });
    expect(validateSplitWeights([entry('100')])).toEqual({ valid: true, total: 100 });
  });

  it('rejects the 70 + 50 = 120 case and returns the running total', () => {
    expect(validateSplitWeights([entry('70'), entry('50')])).toEqual({ valid: false, total: 120 });
  });

  it('rejects sums below 100', () => {
    expect(validateSplitWeights([entry('50')])).toEqual({ valid: false, total: 50 });
    expect(validateSplitWeights([entry('40'), entry('40')])).toEqual({ valid: false, total: 80 });
  });

  it('rejects any target outside 0..100 even when the total is 100', () => {
    // 150 + -50 = 100 but individual values are out of range → invalid.
    expect(validateSplitWeights([entry('150'), entry('-50')]).valid).toBe(false);
    expect(validateSplitWeights([entry('101'), entry('-1')]).valid).toBe(false);
  });

  it('treats non-numeric weights as invalid', () => {
    expect(validateSplitWeights([entry(''), entry('100')]).valid).toBe(false);
    expect(validateSplitWeights([entry('abc'), entry('100')]).valid).toBe(false);
  });
});
