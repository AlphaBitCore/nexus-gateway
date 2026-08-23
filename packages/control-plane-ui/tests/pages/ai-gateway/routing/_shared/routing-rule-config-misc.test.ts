import { describe, it, expect } from 'vitest';
import {
  mapLegacyStrategy, splitIdsCsv, parseRoutingConfigForForm,
  formatModelLabels, configuredInternalModelIds, emptyConditionalFormState,
} from '@/pages/ai-gateway/routing/_shared/routing-rule-config';
import type { AdminModelsByProvider } from '@/api/types';

const groups = [
  { provider: { id: 'p1', name: 'openai', displayName: 'OpenAI', enabled: true }, models: [
    { id: 'm1', providerModelId: 'gpt-4o', name: 'GPT-4o', enabled: true },
    { id: 'm2', providerModelId: 'gpt-4o-mini', name: 'Mini', enabled: true },
  ] },
] as unknown as AdminModelsByProvider[];

describe('mapLegacyStrategy', () => {
  it('maps legacy + canonical names, defaulting unknown to single', () => {
    expect(mapLegacyStrategy('priority')).toBe('single');
    expect(mapLegacyStrategy('round-robin')).toBe('loadbalance');
    expect(mapLegacyStrategy('weighted')).toBe('loadbalance');
    expect(mapLegacyStrategy('fallback')).toBe('fallback');
    expect(mapLegacyStrategy('conditional')).toBe('conditional');
    // Null, not 'single': a name the gateway cannot dispatch must not be
    // silently rewritten into one it can, which destroyed the stored config.
    expect(mapLegacyStrategy('totally-unknown')).toBeNull();
  });
});

describe('splitIdsCsv', () => {
  it('splits on commas + whitespace, trims, drops blanks', () => {
    expect(splitIdsCsv('a, b  c\nd')).toEqual(['a', 'b', 'c', 'd']);
    expect(splitIdsCsv('   ')).toEqual([]);
  });
});

describe('parseRoutingConfigForForm — loadbalance + ab_split', () => {
  it('loadbalance: weightedTargets[].node → entries with weight', () => {
    const r = parseRoutingConfigForForm('loadbalance', { type: 'loadbalance', weightedTargets: [{ weight: 70, node: { type: 'single', providerId: 'p1', modelId: 'm1' } }] }, groups);
    expect(r.entries[0]).toEqual({ provider: 'openai', model: 'gpt-4o', weight: '70' });
  });
  it('ab_split: targets[] → entries with weight', () => {
    const r = parseRoutingConfigForForm('ab_split', { type: 'ab_split', targets: [{ providerId: 'p1', modelId: 'm2', weight: '30' }] }, groups);
    expect(r.entries[0]).toEqual({ provider: 'openai', model: 'gpt-4o-mini', weight: '30' });
  });
  it('loadbalance with the wrong shape → the empty-entry default', () => {
    expect(parseRoutingConfigForForm('loadbalance', { type: 'single' }, groups).entries).toHaveLength(1);
  });
});

describe('formatModelLabels', () => {
  it('renders Provider / Model labels, falls back to the raw id, and "" for empty', () => {
    expect(formatModelLabels(groups, ['m1', 'm2'])).toBe('OpenAI / GPT-4o, OpenAI / Mini');
    expect(formatModelLabels(groups, ['ghost'])).toBe('ghost');
    expect(formatModelLabels(groups, [])).toBe('');
  });
});

describe('configuredInternalModelIds', () => {
  const e = (provider: string, model: string) => ({ provider, model, weight: '50' }) as never;
  it('policy → empty; single → the one id; multi-entry → resolved ids; conditional → delegates', () => {
    expect(configuredInternalModelIds(groups, 'policy', '', '', []).size).toBe(0);
    expect([...configuredInternalModelIds(groups, 'single', 'openai', 'gpt-4o', [])]).toEqual(['m1']);
    expect([...configuredInternalModelIds(groups, 'fallback', '', '', [e('openai', 'gpt-4o'), e('', '')])]).toEqual(['m1']);
    const form = { ...emptyConditionalFormState(), defaultProvider: 'openai', defaultModel: 'gpt-4o-mini' };
    expect([...configuredInternalModelIds(groups, 'conditional', '', '', [], form)]).toContain('m2');
  });
});
