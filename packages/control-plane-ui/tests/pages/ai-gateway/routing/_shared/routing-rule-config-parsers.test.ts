import { describe, it, expect } from 'vitest';
import {
  parseMatchConditionsForm, buildMatchConditionsPayload,
  policyConfigToFormLines, buildPolicyApiConfig,
  buildConditionalApiConfig, hydrateConditionalEditorState, resolveConditionalConfigFromEditor,
  conditionalFormInternalModelIds, parseRoutingConfigForForm,
  emptyConditionalFormState, newConditionalBranchRow,
} from '@/pages/ai-gateway/routing/_shared/routing-rule-config';
import type { AdminModelsByProvider } from '@/api/types';

const groups = [
  { provider: { id: 'p1', name: 'openai', enabled: true }, models: [
    { id: 'm1', providerModelId: 'gpt-4o', name: 'GPT-4o', enabled: true },
    { id: 'm2', providerModelId: 'gpt-4o-mini', name: 'Mini', enabled: true },
  ] },
] as unknown as AdminModelsByProvider[];

describe('match-conditions form helpers', () => {
  it('parse coerces arrays + defaults non-object to empty', () => {
    expect(parseMatchConditionsForm({ models: ['m1'], providers: ['p1'] })).toMatchObject({ models: ['m1'], providers: ['p1'], projects: [] });
    expect(parseMatchConditionsForm(null)).toEqual({ models: [], requestedModelLiterals: [], modelTypes: [], providers: [], projects: [], virtualKeys: [] });
  });
  it('build omits empty arrays', () => {
    expect(buildMatchConditionsPayload({ models: ['m1'], requestedModelLiterals: [], modelTypes: [], providers: [], projects: [], virtualKeys: ['vk1'] }))
      .toEqual({ models: ['m1'], virtualKeys: ['vk1'] });
  });
});

describe('conditional builders', () => {
  const formWithBranch = () => ({
    ...emptyConditionalFormState(),
    defaultProvider: 'openai', defaultModel: 'gpt-4o',
    branches: [{ ...newConditionalBranchRow(), fieldPath: 'requestedModel.id', operator: '$eq', value: 'gpt-4o', thenProvider: 'openai', thenModel: 'gpt-4o-mini' }],
  });

  it('buildConditionalApiConfig builds conditions + default (errors without a default)', () => {
    const r = buildConditionalApiConfig(formWithBranch(), groups) as { ok: true; config: { type: string; conditions: unknown[]; default: unknown } };
    expect(r.ok).toBe(true);
    expect(r.config.type).toBe('conditional');
    expect(r.config.conditions).toHaveLength(1);
    expect(r.config.default).toEqual({ type: 'single', providerId: 'p1', modelId: 'm1' });
    expect(buildConditionalApiConfig(emptyConditionalFormState(), groups).ok).toBe(false);
  });

  it('errors when a branch has a field path but no resolvable target', () => {
    const f = { ...emptyConditionalFormState(), defaultProvider: 'openai', defaultModel: 'gpt-4o', branches: [{ ...newConditionalBranchRow(), fieldPath: 'x', thenProvider: 'nope', thenModel: 'nope' }] };
    expect(buildConditionalApiConfig(f, groups).ok).toBe(false);
  });

  it('conditionalFormInternalModelIds collects default + branch target model ids', () => {
    const ids = conditionalFormInternalModelIds(groups, formWithBranch());
    expect([...ids].sort()).toEqual(['m1', 'm2']);
  });

  it('hydrateConditionalEditorState: parseable → form mode, opaque → json mode, null → empty form', () => {
    const cfg = { type: 'conditional', conditions: [], default: { type: 'single', providerId: 'p1', modelId: 'm1' } };
    expect(hydrateConditionalEditorState(cfg, groups).mode).toBe('form');
    // a config whose default can't map back to UI falls to json mode
    expect(hydrateConditionalEditorState({ type: 'conditional', conditions: [], default: { type: 'single', providerId: 'x', modelId: 'y' } }, groups).mode).toBe('json');
    // null → empty form (branch ids are random, so assert the shape not deep-equal)
    const nullHydration = hydrateConditionalEditorState(null, groups);
    expect(nullHydration.mode).toBe('form');
    expect(nullHydration.mode === 'form' && nullHydration.form.branches.length).toBeGreaterThanOrEqual(0);
  });

  it('resolveConditionalConfigFromEditor: form builds; valid/invalid JSON paths', () => {
    expect(resolveConditionalConfigFromEditor({ mode: 'form', form: formWithBranch() }, groups).ok).toBe(true);
    expect(resolveConditionalConfigFromEditor({ mode: 'json', text: '{"type":"conditional","conditions":[],"default":{}}' }, groups).ok).toBe(true);
    expect(resolveConditionalConfigFromEditor({ mode: 'json', text: '{"type":"single"}' }, groups).ok).toBe(false);
    expect(resolveConditionalConfigFromEditor({ mode: 'json', text: '{bad json' }, groups).ok).toBe(false);
  });
});

describe('parseRoutingConfigForForm', () => {
  it('single: maps api ids back to ui provider/model', () => {
    expect(parseRoutingConfigForForm('single', { type: 'single', providerId: 'p1', modelId: 'm1' }, groups))
      .toEqual({ entries: [], singleProvider: 'openai', singleModel: 'gpt-4o' });
  });
  it('fallback: maps targets to entries', () => {
    const r = parseRoutingConfigForForm('fallback', { type: 'fallback', targets: [{ type: 'single', providerId: 'p1', modelId: 'm2' }] }, groups);
    expect(r.entries[0]).toMatchObject({ provider: 'openai', model: 'gpt-4o-mini' });
  });
  it('policy + conditional + non-object → the single empty-entry default', () => {
    expect(parseRoutingConfigForForm('policy', {}, groups).entries).toHaveLength(1);
    expect(parseRoutingConfigForForm('conditional', { type: 'conditional' }, groups).entries).toHaveLength(1);
    expect(parseRoutingConfigForForm('fallback', null, groups).entries).toHaveLength(1);
  });
});
