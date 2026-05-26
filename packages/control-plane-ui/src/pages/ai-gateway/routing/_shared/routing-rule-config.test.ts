import { describe, expect, it } from 'vitest';
import {
  buildConditionalApiConfig,
  buildRoutingApiConfig,
  buildPolicyApiConfig,
  policyConfigToFormLines,
  isValidConditionalConfig,
  parseRoutingConfigForForm,
  resolveProviderModelIds,
  tryParseConditionalFormFromConfig,
  emptyConditionalFormState,
  parseMatchConditionsForm,
  buildMatchConditionsPayload,
} from './routing-rule-config';
import type { AdminModelsByProvider } from '@/api/types';

const mockGroups: AdminModelsByProvider[] = [
  {
    provider: {
      id: 'prov-1',
      name: 'anthropic',
      displayName: 'Anthropic',
      description: null,
      adapterType: 'anthropic',
      enabled: true,
      modelCount: 1,
    },
    models: [
      {
        id: 'claude-haiku',
        code: 'claude-haiku',
        name: 'Haiku',
        providerId: 'prov-1',
        providerModelId: 'claude-haiku-4-5-20251001',
        type: 'chat',
        features: [],
        enabled: true,
      },
    ],
  },
];

describe('routing-rule-config', () => {
  it('resolves provider + providerModelId to gateway ids', () => {
    const r = resolveProviderModelIds(mockGroups, 'anthropic', 'claude-haiku-4-5-20251001');
    expect(r).toEqual({ providerId: 'prov-1', modelId: 'claude-haiku' });
  });

  it('builds valid conditional config from match model ids when preserved config invalid', () => {
    const out = buildRoutingApiConfig({
      strategyType: 'conditional',
      providerGroups: mockGroups,
      singleProvider: '',
      singleModel: '',
      entries: [{ provider: '', model: '', weight: '50' }],
      matchModelIds: ['claude-haiku'],
      preservedConditionalConfig: { targets: [] },
    });
    expect(out.ok).toBe(true);
    if (!out.ok) return;
    expect(out.config).toMatchObject({ type: 'conditional' });
    const c = out.config as Record<string, unknown>;
    expect(Array.isArray(c.conditions)).toBe(true);
    expect(c.default).toMatchObject({ type: 'single', providerId: 'prov-1', modelId: 'claude-haiku' });
  });

  it('round-trips simple conditional form (single-field when + single then/default)', () => {
    const tree = {
      type: 'conditional' as const,
      conditions: [
        {
          when: { 'virtualKey.projectId': { $eq: 'marketing-project' } },
          then: { type: 'single' as const, providerId: 'prov-1', modelId: 'claude-haiku' },
        },
      ],
      default: { type: 'single' as const, providerId: 'prov-1', modelId: 'claude-haiku' },
    };
    const form = tryParseConditionalFormFromConfig(tree, mockGroups);
    expect(form).not.toBeNull();
    const built = buildConditionalApiConfig(form!, mockGroups);
    expect(built.ok).toBe(true);
    if (!built.ok) return;
    expect(built.config).toEqual(tree);
  });

  it('buildConditionalApiConfig omits branches with blank field path', () => {
    const form = emptyConditionalFormState();
    form.defaultProvider = 'anthropic';
    form.defaultModel = 'claude-haiku-4-5-20251001';
    form.branches[0].fieldPath = '';
    const out = buildConditionalApiConfig(form, mockGroups);
    expect(out.ok).toBe(true);
    if (!out.ok) return;
    expect(out.config).toMatchObject({
      type: 'conditional',
      conditions: [],
      default: { type: 'single', providerId: 'prov-1', modelId: 'claude-haiku' },
    });
  });

  it('buildRoutingApiConfig prefers conditionalForm over preserved config', () => {
    const form = emptyConditionalFormState();
    form.defaultProvider = 'anthropic';
    form.defaultModel = 'claude-haiku-4-5-20251001';
    form.branches[0].fieldPath = 'virtualKey.projectId';
    form.branches[0].value = 'p1';
    form.branches[0].thenProvider = 'anthropic';
    form.branches[0].thenModel = 'claude-haiku-4-5-20251001';
    const out = buildRoutingApiConfig({
      strategyType: 'conditional',
      providerGroups: mockGroups,
      singleProvider: '',
      singleModel: '',
      entries: [],
      matchModelIds: [],
      preservedConditionalConfig: {
        type: 'conditional',
        conditions: [{ when: { x: { $eq: 1 } }, then: { type: 'single', providerId: 'prov-1', modelId: 'claude-haiku' } }],
        default: { type: 'single', providerId: 'prov-1', modelId: 'claude-haiku' },
      },
      conditionalForm: form,
    });
    expect(out.ok).toBe(true);
    if (!out.ok) return;
    const c = out.config as Record<string, unknown>;
    expect(c.conditions).toEqual([
      {
        when: { 'virtualKey.projectId': { $eq: 'p1' } },
        then: { type: 'single', providerId: 'prov-1', modelId: 'claude-haiku' },
      },
    ]);
  });

  it('preserves valid conditional config', () => {
    const tree = {
      type: 'conditional',
      conditions: [
        {
          when: { 'requestedModel.id': { $eq: 'x' } },
          then: { type: 'single', providerId: 'prov-1', modelId: 'claude-haiku' },
        },
      ],
      default: { type: 'single', providerId: 'prov-1', modelId: 'claude-haiku' },
    };
    expect(isValidConditionalConfig(tree)).toBe(true);
    const out = buildRoutingApiConfig({
      strategyType: 'conditional',
      providerGroups: mockGroups,
      singleProvider: '',
      singleModel: '',
      entries: [],
      matchModelIds: [],
      preservedConditionalConfig: tree,
    });
    expect(out.ok).toBe(true);
    if (!out.ok) return;
    expect(out.config).toEqual(tree);
  });

  it('parses single strategy from API shape', () => {
    const p = parseRoutingConfigForForm(
      'single',
      { type: 'single', providerId: 'prov-1', modelId: 'claude-haiku' },
      mockGroups,
    );
    expect(p.singleProvider).toBe('anthropic');
    expect(p.singleModel).toBe('claude-haiku-4-5-20251001');
  });

  it('builds policy config from id arrays', () => {
    const out = buildPolicyApiConfig(['a', 'b'], [], ['p1'], []);
    expect(out.ok).toBe(true);
    if (!out.ok) return;
    expect(out.config).toEqual({ type: 'policy', allowModelIds: ['a', 'b'], allowProviderIds: ['p1'] });
  });

  it('rejects empty policy config', () => {
    const out = buildPolicyApiConfig([], [], [], []);
    expect(out.ok).toBe(false);
  });

  it('round-trips policy lines for form', () => {
    const cfg = { type: 'policy', denyModelIds: ['x', 'y'] };
    const lines = policyConfigToFormLines(cfg);
    expect(lines.denyM).toEqual(['x', 'y']);
  });

  it('parseMatchConditionsForm returns empty arrays for nullish input', () => {
    const empty = {
      models: [],
      requestedModelLiterals: [],
      modelTypes: [],
      providers: [],
      projects: [],
      virtualKeys: [],
    };
    expect(parseMatchConditionsForm(null)).toEqual(empty);
    expect(parseMatchConditionsForm(undefined)).toEqual(empty);
  });

  it('parseMatchConditionsForm reads every supported matcher field', () => {
    const mc = {
      models: ['m1', 'm2'],
      requestedModelLiterals: ['auto', 'gpt-4-*'],
      modelTypes: ['chat', 'embedding'],
      providers: ['p-a', 'p-b'],
      projects: ['proj-1', 'proj-2'],
      virtualKeys: ['team-*', 'prod-*'],
    };
    expect(parseMatchConditionsForm(mc)).toEqual({
      models: ['m1', 'm2'],
      requestedModelLiterals: ['auto', 'gpt-4-*'],
      modelTypes: ['chat', 'embedding'],
      providers: ['p-a', 'p-b'],
      projects: ['proj-1', 'proj-2'],
      virtualKeys: ['team-*', 'prod-*'],
    });
  });

  it('parseMatchConditionsForm drops the retired `organizations` key', () => {
    // The legacy organizations matcher was retired (admin API
    // rejects with HTTP 422) and Fix Group 4 deleted the engine path. The
    // parser must silently drop any leftover field in stored rule JSON
    // rather than blow up — forward-compat for any old DB rows.
    const mc = { organizations: ['org-1'] } as Record<string, unknown>;
    expect(parseMatchConditionsForm(mc)).toEqual({
      models: [],
      requestedModelLiterals: [],
      modelTypes: [],
      providers: [],
      projects: [],
      virtualKeys: [],
    });
  });

  it('buildMatchConditionsPayload omits empty arrays', () => {
    expect(
      buildMatchConditionsPayload({
        models: [],
        requestedModelLiterals: [],
        modelTypes: [],
        providers: [],
        projects: [],
        virtualKeys: [],
      }),
    ).toEqual({});
    expect(
      buildMatchConditionsPayload({
        models: ['mid'],
        requestedModelLiterals: [],
        modelTypes: [],
        providers: [],
        projects: [],
        virtualKeys: [],
      }),
    ).toEqual({ models: ['mid'] });
    expect(
      buildMatchConditionsPayload({
        models: [],
        requestedModelLiterals: ['auto'],
        modelTypes: ['chat'],
        providers: ['pid'],
        projects: ['proj-1'],
        virtualKeys: ['team-*'],
      }),
    ).toEqual({
      requestedModelLiterals: ['auto'],
      modelTypes: ['chat'],
      providers: ['pid'],
      projects: ['proj-1'],
      virtualKeys: ['team-*'],
    });
  });
});
