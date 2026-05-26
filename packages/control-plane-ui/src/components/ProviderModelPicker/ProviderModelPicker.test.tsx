import { describe, it, expect } from 'vitest';
import { screen } from '@testing-library/react';
import { renderWithProviders } from '@/test/test-utils';
import {
  ProviderModelPicker,
  filterCompletableProviders,
} from './ProviderModelPicker';
import type { AdminModelsByProvider } from '@/api/types';

// Mirrors a realistic listModels() payload: two completable providers,
// one provider with zero models (must be filtered out).
const sampleGroups: AdminModelsByProvider[] = [
  {
    provider: {
      id: 'prov-openai',
      name: 'openai',
      displayName: 'OpenAI',
      adapterType: 'openai',
      enabled: true,
      modelCount: 2,
    },
    models: [
      {
        id: 'mdl-gpt4',
        code: 'openai/gpt-4',
        providerId: 'prov-openai',
        providerModelId: 'gpt-4',
        name: 'GPT-4',
        type: 'chat',
        features: [],
        aliases: [],
        enabled: true,
      },
      {
        id: 'mdl-emb3',
        code: 'openai/text-embedding-3-small',
        providerId: 'prov-openai',
        providerModelId: 'text-embedding-3-small',
        name: 'Embedding 3',
        type: 'embedding',
        features: [],
        aliases: [],
        enabled: true,
      },
    ],
  },
  {
    provider: {
      id: 'prov-anthropic',
      name: 'anthropic',
      displayName: 'Anthropic',
      adapterType: 'anthropic',
      enabled: true,
      modelCount: 1,
    },
    models: [
      {
        id: 'mdl-sonnet',
        code: 'anthropic/claude-sonnet-4-6',
        providerId: 'prov-anthropic',
        providerModelId: 'claude-sonnet-4-6',
        name: 'Claude Sonnet 4.6',
        type: 'chat',
        features: [],
        aliases: [],
        enabled: true,
      },
    ],
  },
  {
    provider: {
      id: 'prov-empty',
      name: 'empty',
      displayName: 'Empty Provider',
      adapterType: 'openai',
      enabled: true,
      modelCount: 0,
    },
    models: [],
  },
];

describe('filterCompletableProviders', () => {
  it('drops providers with zero models', () => {
    const out = filterCompletableProviders(sampleGroups, undefined);
    expect(out.length).toBe(2);
    expect(out.find((g) => g.provider?.id === 'prov-empty')).toBeUndefined();
    expect(out.find((g) => g.provider?.id === 'prov-openai')).toBeDefined();
    expect(out.find((g) => g.provider?.id === 'prov-anthropic')).toBeDefined();
  });

  it('drops providers without any model of the requested type', () => {
    // Only providers with at least one model of type 'embedding' remain.
    // sampleGroups[0] (openai) has both chat and embedding → kept.
    // sampleGroups[1] (anthropic) has only chat → dropped.
    // sampleGroups[2] (empty) has no models → already dropped.
    const out = filterCompletableProviders(sampleGroups, 'embedding');
    expect(out.length).toBe(1);
    expect(out[0].provider?.id).toBe('prov-openai');
  });

  it('keeps every completable provider when endpointType is undefined', () => {
    // No filter: every (provider, model) pair is fair game.
    const out = filterCompletableProviders(sampleGroups, undefined);
    expect(out.length).toBe(2);
  });

  it('handles empty / null inputs gracefully', () => {
    expect(filterCompletableProviders([], undefined)).toEqual([]);
    expect(filterCompletableProviders([], 'embedding')).toEqual([]);
  });
});

describe('ProviderModelPicker (smoke)', () => {
  it('renders both selects without throwing', () => {
    renderWithProviders(
      <ProviderModelPicker
        providerGroups={sampleGroups}
        providerId={null}
        modelId={null}
        onChange={() => {}}
      />,
    );
    // Two cascading selects, each rendered as a Radix combobox trigger.
    const triggers = screen.getAllByRole('combobox');
    expect(triggers.length).toBe(2);
  });

  it('renders help text when provided', () => {
    renderWithProviders(
      <ProviderModelPicker
        providerGroups={sampleGroups}
        providerId={null}
        modelId={null}
        onChange={() => {}}
        helpText="Only embedding providers are listed."
      />,
    );
    expect(screen.getByText('Only embedding providers are listed.')).toBeDefined();
  });

  it('honours custom labels', () => {
    renderWithProviders(
      <ProviderModelPicker
        providerGroups={sampleGroups}
        providerId={null}
        modelId={null}
        onChange={() => {}}
        providerLabel="Embedding Provider"
        modelLabel="Embedding Model"
      />,
    );
    expect(screen.getByText('Embedding Provider')).toBeDefined();
    expect(screen.getByText('Embedding Model')).toBeDefined();
  });

  it('renders without crashing when providerGroups is empty', () => {
    renderWithProviders(
      <ProviderModelPicker
        providerGroups={[]}
        providerId={null}
        modelId={null}
        onChange={() => {}}
      />,
    );
    const triggers = screen.getAllByRole('combobox');
    expect(triggers.length).toBe(2);
  });

  it('survives a preselected pair whose provider was filtered out', () => {
    // Defensive guard: when admin deletes the last matching model from
    // a provider out from under a saved (providerId, modelId) pair, the
    // picker must not crash. The Selects fall back to their placeholders.
    renderWithProviders(
      <ProviderModelPicker
        providerGroups={sampleGroups}
        providerId="prov-empty" // exists in groups but has 0 models → filtered
        modelId={null}
        onChange={() => {}}
      />,
    );
    const triggers = screen.getAllByRole('combobox');
    expect(triggers.length).toBe(2);
  });
});
