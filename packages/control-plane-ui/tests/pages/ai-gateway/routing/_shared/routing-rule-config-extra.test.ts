import { describe, it, expect } from 'vitest';
import type { AdminModelsByProvider } from '@/api/types';
import {
  mapLegacyStrategy,
  splitIdsCsv,
  parseSmartConfig,
  buildSmartConfig,
  parseFallbackChain,
  buildFallbackChainApi,
  DEFAULT_SMART_SYSTEM_PROMPT,
  type SmartFormState,
} from '../../../../../src/pages/ai-gateway/routing/_shared/routing-rule-config';

// Provider groups: UI uses provider.name + model.providerModelId; the API
// payload uses provider.id + model.id. The helpers translate between them.
const groups = [
  {
    provider: { id: 'prov-1', name: 'anthropic', displayName: 'Anthropic', adapterType: 'anthropic', enabled: true, modelCount: 1 },
    models: [
      { id: 'm-haiku', providerModelId: 'claude-3-haiku', name: 'Haiku', enabled: true },
      { id: 'm-sonnet', providerModelId: 'claude-3-sonnet', name: 'Sonnet', enabled: true },
    ],
  },
] as unknown as AdminModelsByProvider[];

describe('mapLegacyStrategy', () => {
  it('maps legacy strategy names to the current strategy types', () => {
    expect(mapLegacyStrategy('priority')).toBe('single');
    expect(mapLegacyStrategy('round-robin')).toBe('loadbalance');
    expect(mapLegacyStrategy('weighted')).toBe('loadbalance');
    // 'latency' is a first-class strategy since e89-s3 — it no longer
    // collapses to 'single'. See routing-rule-config.latency.test.ts.
    expect(mapLegacyStrategy('latency')).toBe('latency');
    expect(mapLegacyStrategy('cost')).toBe('single');
    expect(mapLegacyStrategy('fallback')).toBe('fallback');
    expect(mapLegacyStrategy('smart')).toBe('smart');
    expect(mapLegacyStrategy('policy')).toBe('policy');
  });
  it('defaults unknown strategies to single', () => {
    expect(mapLegacyStrategy('totally-unknown')).toBe('single');
  });
});

describe('splitIdsCsv', () => {
  it('splits on commas + whitespace and trims empties', () => {
    expect(splitIdsCsv('a, b  c,,d')).toEqual(['a', 'b', 'c', 'd']);
    expect(splitIdsCsv('  x \n y ')).toEqual(['x', 'y']);
    expect(splitIdsCsv('')).toEqual([]);
  });
});

describe('parseSmartConfig', () => {
  it('returns the empty default for non-objects / non-smart configs', () => {
    expect(parseSmartConfig(null, groups).systemPrompt).toBe(DEFAULT_SMART_SYSTEM_PROMPT);
    expect(parseSmartConfig({ type: 'single' }, groups).routerProvider).toBe('');
  });
  it('resolves router + default UI names from a smart config', () => {
    const s = parseSmartConfig(
      {
        type: 'smart',
        routerProviderId: 'prov-1',
        routerModelId: 'm-haiku',
        defaultProviderId: 'prov-1',
        defaultModelId: 'm-sonnet',
        systemPrompt: 'pick wisely',
        temperature: 0.5,
        maxTokens: 2048,
        timeoutMs: 5000,
      },
      groups,
    );
    expect(s.routerProvider).toBe('anthropic');
    expect(s.routerModel).toBe('claude-3-haiku');
    expect(s.defaultModel).toBe('claude-3-sonnet');
    expect(s.systemPrompt).toBe('pick wisely');
    expect(s.temperature).toBe('0.5');
    expect(s.maxTokens).toBe('2048');
  });
});

describe('buildSmartConfig', () => {
  const base: SmartFormState = {
    routerProvider: 'anthropic', routerModel: 'claude-3-haiku',
    systemPrompt: 'route it', temperature: '0.2', maxTokens: '1000', timeoutMs: '8000',
    defaultProvider: '', defaultModel: '',
  };
  it('fails when the router provider/model cannot be resolved', () => {
    const r = buildSmartConfig({ ...base, routerModel: 'nope' }, groups);
    expect(r.ok).toBe(false);
  });
  it('fails when the system prompt is blank', () => {
    const r = buildSmartConfig({ ...base, systemPrompt: '   ' }, groups);
    expect(r.ok).toBe(false);
  });
  it('builds a smart config with numeric coercion + optional default', () => {
    const r = buildSmartConfig({ ...base, defaultProvider: 'anthropic', defaultModel: 'claude-3-sonnet' }, groups);
    expect(r.ok).toBe(true);
    if (!r.ok) return;
    expect(r.config).toMatchObject({
      type: 'smart',
      routerProviderId: 'prov-1',
      routerModelId: 'm-haiku',
      temperature: 0.2,
      defaultProviderId: 'prov-1',
      defaultModelId: 'm-sonnet',
    });
  });
});

describe('fallback chain', () => {
  it('parseFallbackChain maps API entries → UI names, dropping malformed/non-array', () => {
    expect(parseFallbackChain('nope', groups)).toEqual([]);
    const out = parseFallbackChain(
      [{ providerId: 'prov-1', modelId: 'm-haiku' }, { providerId: 'prov-1' }, { providerId: 'x', modelId: 'y' }],
      groups,
    );
    expect(out).toEqual([{ provider: 'anthropic', model: 'claude-3-haiku' }]);
  });
  it('buildFallbackChainApi resolves UI names → API ids, dropping blanks/unresolvable', () => {
    const out = buildFallbackChainApi(
      [{ provider: 'anthropic', model: 'claude-3-sonnet' }, { provider: '', model: '' }, { provider: 'anthropic', model: 'ghost' }],
      groups,
    );
    expect(out).toEqual([{ providerId: 'prov-1', modelId: 'm-sonnet' }]);
  });
});
