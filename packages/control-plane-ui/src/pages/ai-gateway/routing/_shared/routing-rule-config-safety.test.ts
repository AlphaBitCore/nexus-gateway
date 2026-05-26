import { describe, it, expect } from 'vitest';
import { formatModelLabels } from './routing-rule-config';
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
