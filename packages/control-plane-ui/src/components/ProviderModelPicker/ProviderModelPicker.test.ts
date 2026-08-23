import { describe, expect, it } from 'vitest';
import { filterCompletableProviders } from './ProviderModelPicker';
import type { AdminModelsByProvider, Model } from '@/api/types';

function model(over: Partial<Model>): Model {
  return {
    id: 'm1',
    code: 'gpt-4o',
    name: 'GPT-4o',
    providerId: 'p1',
    providerModelId: 'gpt-4o',
    type: 'chat',
    features: [],
    enabled: true,
    status: 'active',
    ...over,
  } as Model;
}

function group(over: {
  id: string;
  enabled?: boolean;
  models: Model[];
}): AdminModelsByProvider {
  return {
    provider: {
      id: over.id,
      name: over.id,
      adapterType: 'openai',
      enabled: over.enabled ?? true,
      modelCount: over.models.length,
    },
    models: over.models,
  };
}

const nothingSelected = { providerId: null, modelId: null };

describe('filterCompletableProviders', () => {
  it('drops providers that are disabled', () => {
    const groups = [
      group({ id: 'live', models: [model({ id: 'm-live' })] }),
      group({ id: 'off', enabled: false, models: [model({ id: 'm-off', providerId: 'off' })] }),
    ];
    const got = filterCompletableProviders(groups, undefined, nothingSelected);
    expect(got.map((g) => g.provider!.id)).toEqual(['live']);
  });

  it('drops providers whose only models are withdrawn', () => {
    const groups = [
      group({ id: 'a', models: [model({ id: 'm-disabled', enabled: false })] }),
      group({ id: 'b', models: [model({ id: 'm-status', status: 'disabled' })] }),
      group({ id: 'c', models: [model({ id: 'm-ok' })] }),
    ];
    const got = filterCompletableProviders(groups, undefined, nothingSelected);
    expect(got.map((g) => g.provider!.id)).toEqual(['c']);
  });

  it('keeps deprecated and preview models selectable', () => {
    const groups = [
      group({
        id: 'a',
        models: [model({ id: 'm-dep', status: 'deprecated' }), model({ id: 'm-prev', status: 'preview' })],
      }),
    ];
    expect(filterCompletableProviders(groups, undefined, nothingSelected)).toHaveLength(1);
  });

  it('still shows a pinned pair that has since gone out of service', () => {
    // Otherwise the field silently reads as empty and an admin cannot tell a
    // broken pin from no pin at all.
    const groups = [
      group({
        id: 'off',
        enabled: false,
        models: [model({ id: 'm-pinned', providerId: 'off', enabled: false })],
      }),
    ];
    const got = filterCompletableProviders(groups, undefined, {
      providerId: 'off',
      modelId: 'm-pinned',
    });
    expect(got.map((g) => g.provider!.id)).toEqual(['off']);
  });

  it('still narrows by endpoint type', () => {
    const groups = [
      group({ id: 'a', models: [model({ id: 'm-chat', type: 'chat' })] }),
      group({ id: 'b', models: [model({ id: 'm-emb', type: 'embedding' })] }),
    ];
    const got = filterCompletableProviders(groups, 'embedding', nothingSelected);
    expect(got.map((g) => g.provider!.id)).toEqual(['b']);
  });
});
