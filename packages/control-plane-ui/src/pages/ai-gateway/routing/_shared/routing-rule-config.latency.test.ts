import { describe, it, expect } from 'vitest';

import type { AdminModelsByProvider } from '@/api/types';
import {
  mapLegacyStrategy,
  buildRoutingApiConfig,
  parseRoutingConfigForForm,
  type ProviderModelEntry,
} from './routing-rule-config';

const groups = [
  {
    provider: { id: 'prov-a-id', name: 'ProvA' },
    models: [{ id: 'model-a', providerModelId: 'gpt-4o', name: 'GPT-4o (A)' }],
  },
  {
    provider: { id: 'prov-b-id', name: 'ProvB' },
    models: [{ id: 'model-b', providerModelId: 'gpt-4o', name: 'GPT-4o (B)' }],
  },
] as unknown as AdminModelsByProvider[];

const entry = (provider: string, model: string): ProviderModelEntry => ({ provider, model, weight: '50' });

describe('latency strategy — CP-UI config mapping', () => {
  it('maps the stored "latency" strategy string to the latency strategy, not single (regression guard)', () => {
    // Before this feature, mapLegacyStrategy('latency') collapsed to 'single',
    // so a saved latency rule displayed and re-saved as single.
    expect(mapLegacyStrategy('latency')).toBe('latency');
  });

  it('builds a weightless {type:"latency",targets:[…]} config from resolved entries', () => {
    const result = buildRoutingApiConfig({
      strategyType: 'latency',
      providerGroups: groups,
      singleProvider: '',
      singleModel: '',
      entries: [entry('ProvA', 'gpt-4o'), entry('ProvB', 'gpt-4o')],
      matchModelIds: [],
    });
    expect(result.ok).toBe(true);
    if (!result.ok) return;
    expect(result.config).toEqual({
      type: 'latency',
      targets: [
        { providerId: 'prov-a-id', modelId: 'model-a' },
        { providerId: 'prov-b-id', modelId: 'model-b' },
      ],
    });
    // No weight key is emitted — ordering is p95-derived, not weighted.
    for (const t of (result.config as { targets: Record<string, unknown>[] }).targets) {
      expect(t).not.toHaveProperty('weight');
    }
  });

  it('rejects a latency config with no resolvable targets', () => {
    const result = buildRoutingApiConfig({
      strategyType: 'latency',
      providerGroups: groups,
      singleProvider: '',
      singleModel: '',
      entries: [entry('', '')],
      matchModelIds: [],
    });
    expect(result.ok).toBe(false);
  });

  it('parses a stored latency config back into provider/model form entries', () => {
    const parsed = parseRoutingConfigForForm(
      'latency',
      { type: 'latency', targets: [{ providerId: 'prov-a-id', modelId: 'model-a' }] },
      groups,
    );
    expect(parsed.entries).toHaveLength(1);
    expect(parsed.entries[0]).toMatchObject({ provider: 'ProvA', model: 'gpt-4o' });
  });

  it('round-trips build → parse without drift', () => {
    const built = buildRoutingApiConfig({
      strategyType: 'latency',
      providerGroups: groups,
      singleProvider: '',
      singleModel: '',
      entries: [entry('ProvA', 'gpt-4o'), entry('ProvB', 'gpt-4o')],
      matchModelIds: [],
    });
    expect(built.ok).toBe(true);
    if (!built.ok) return;
    const parsed = parseRoutingConfigForForm('latency', built.config, groups);
    expect(parsed.entries.map((e) => `${e.provider}/${e.model}`)).toEqual(['ProvA/gpt-4o', 'ProvB/gpt-4o']);
  });
});
