/**
 * Unit tests for the catalog-sync diff.
 *
 * The load-bearing behaviour is the match: a provider row is matched to a
 * catalog entry by the row's upstream id, so a row the catalog has since
 * renamed away from is offered as a correction rather than duplicated.
 */
import { describe, it, expect } from 'vitest';
import {
  buildCatalogSyncDiff,
  catalogCreateInput,
  catalogUpdateInput,
  diffModelAgainstTemplate,
  resolveTemplateName,
  templateIdentityKeys,
} from '../../../../../src/pages/ai-gateway/providers/detail/catalog-sync';
import type { ApiProviderTemplate, ApiTemplateModel, Model } from '../../../../../src/api/types';

function makeModel(overrides: Partial<Model> = {}): Model {
  return {
    id: 'model-1',
    code: 'claude-haiku-4-5',
    name: 'Claude Haiku 4.5',
    providerId: 'provider-1',
    providerModelId: 'claude-haiku-4-5',
    type: 'chat',
    features: [],
    enabled: true,
    ...overrides,
  };
}

function makeTemplateModel(overrides: Partial<ApiTemplateModel> = {}): ApiTemplateModel {
  return {
    code: 'claude-haiku-4-5',
    name: 'Claude Haiku 4.5',
    description: 'Fast Claude.',
    providerModelId: 'claude-haiku-4-5',
    type: 'chat',
    features: [],
    ...overrides,
  };
}

describe('templateIdentityKeys', () => {
  it('answers to the upstream id, the local code, and every alias', () => {
    const tm = makeTemplateModel({
      code: 'azure-gpt-5',
      providerModelId: 'gpt-5',
      aliases: ['gpt-5-2025-01-01'],
    });
    expect(templateIdentityKeys(tm).sort()).toEqual(['azure-gpt-5', 'gpt-5', 'gpt-5-2025-01-01']);
  });

  it('de-duplicates when code and upstream id are the same', () => {
    expect(templateIdentityKeys(makeTemplateModel())).toEqual(['claude-haiku-4-5']);
  });

  it('treats a missing aliases field as an empty list', () => {
    const tm = makeTemplateModel();
    expect(tm.aliases).toBeUndefined();
    expect(templateIdentityKeys(tm)).toEqual(['claude-haiku-4-5']);
  });
});

describe('buildCatalogSyncDiff — matching', () => {
  it('matches a pinned provider row to the floating catalog entry via aliases, as CHANGED not NEW', () => {
    // The catalog renamed the dated id to a floating one and kept the dated id
    // as an alias. A deployment still holding the dated row is the same model.
    const model = makeModel({
      id: 'm-dated',
      code: 'claude-haiku-4-5-20251001',
      providerModelId: 'claude-haiku-4-5-20251001',
      maxOutputTokens: 65536,
    });
    const tm = makeTemplateModel({
      code: 'claude-haiku-4-5',
      providerModelId: 'claude-haiku-4-5',
      aliases: ['claude-haiku-4-5-20251001'],
      maxOutputTokens: 64000,
    });

    const diff = buildCatalogSyncDiff([model], [tm]);

    expect(diff.changedRows).toHaveLength(1);
    expect(diff.changedRows[0].model.id).toBe('m-dated');
    expect(diff.changedRows[0].template.code).toBe('claude-haiku-4-5');
    // The bug this prevents: the row read as unknown and the entry offered as
    // a new model, leaving the admin with a duplicate of what they already had.
    expect(diff.newRows).toHaveLength(0);
    expect(diff.providerOnlyRows).toHaveLength(0);
  });

  it('matches on the upstream id when the catalog code is a different local name', () => {
    // Azure entries carry code "azure-gpt-5" against upstream id "gpt-5"; the
    // wizard writes the upstream id into the row.
    const model = makeModel({ id: 'm-azure', code: 'gpt-5', providerModelId: 'gpt-5', maxOutputTokens: 100 });
    const tm = makeTemplateModel({ code: 'azure-gpt-5', providerModelId: 'gpt-5', maxOutputTokens: 128000 });

    const diff = buildCatalogSyncDiff([model], [tm]);

    expect(diff.changedRows).toHaveLength(1);
    expect(diff.newRows).toHaveLength(0);
    expect(diff.providerOnlyRows).toHaveLength(0);
  });

  it('matches a row whose providerModelId equals the catalog code', () => {
    const model = makeModel({ id: 'm-code', providerModelId: 'azure-gpt-5', maxOutputTokens: 100 });
    const tm = makeTemplateModel({ code: 'azure-gpt-5', providerModelId: 'gpt-5', maxOutputTokens: 128000 });

    expect(buildCatalogSyncDiff([model], [tm]).changedRows).toHaveLength(1);
  });

  it('matches case-insensitively and ignores surrounding whitespace', () => {
    const model = makeModel({ providerModelId: '  MiniMax-M2.7 ', maxOutputTokens: 1 });
    const tm = makeTemplateModel({ code: 'minimax-m2-7', providerModelId: 'MINIMAX-M2.7', maxOutputTokens: 8192 });

    expect(buildCatalogSyncDiff([model], [tm]).changedRows).toHaveLength(1);
  });

  it('does not match on the local code, which the admin may rename freely', () => {
    // Row renamed locally but still pointing at the same upstream model.
    const model = makeModel({ code: 'our-fast-model', providerModelId: 'claude-haiku-4-5' });
    const diff = buildCatalogSyncDiff([model], [makeTemplateModel()]);

    expect(diff.providerOnlyRows).toHaveLength(0);
    expect(diff.newRows).toHaveLength(0);
  });
});

describe('buildCatalogSyncDiff — buckets', () => {
  it('reports a catalog entry the provider lacks as NEW', () => {
    const diff = buildCatalogSyncDiff([], [makeTemplateModel({ code: 'claude-opus-4-5', providerModelId: 'claude-opus-4-5' })]);

    expect(diff.newRows).toHaveLength(1);
    expect(diff.newRows[0].template.code).toBe('claude-opus-4-5');
    expect(diff.changedRows).toHaveLength(0);
  });

  it('reports a provider row absent from the catalog as PROVIDER-ONLY', () => {
    const model = makeModel({ id: 'm-custom', providerModelId: 'our-private-finetune' });
    const diff = buildCatalogSyncDiff([model], [makeTemplateModel()]);

    expect(diff.providerOnlyRows).toHaveLength(1);
    expect(diff.providerOnlyRows[0].model.id).toBe('m-custom');
    // The catalog entry itself is still unmatched, so it is offered as new.
    expect(diff.newRows).toHaveLength(1);
  });

  it('reports an identical row in no bucket at all', () => {
    const tm = makeTemplateModel({ description: 'Fast Claude.', maxOutputTokens: 64000 });
    const model = makeModel({ name: tm.name, description: tm.description, maxOutputTokens: 64000 });

    const diff = buildCatalogSyncDiff([model], [tm]);

    expect(diff.newRows).toHaveLength(0);
    expect(diff.changedRows).toHaveLength(0);
    expect(diff.providerOnlyRows).toHaveLength(0);
  });

  it('offers both rows and no duplicate when two rows match one entry', () => {
    const floating = makeModel({ id: 'm-float', providerModelId: 'claude-haiku-4-5', maxOutputTokens: 1 });
    const pinned = makeModel({ id: 'm-pin', providerModelId: 'claude-haiku-4-5-20251001', maxOutputTokens: 1 });
    const tm = makeTemplateModel({ aliases: ['claude-haiku-4-5-20251001'], maxOutputTokens: 64000 });

    const diff = buildCatalogSyncDiff([floating, pinned], [tm]);

    expect(diff.changedRows.map((r) => r.model.id).sort()).toEqual(['m-float', 'm-pin']);
    expect(diff.newRows).toHaveLength(0);
  });
});

describe('diffModelAgainstTemplate', () => {
  it('reports the corrected ceiling as a per-field yours-to-catalog move', () => {
    const model = makeModel({ maxOutputTokens: 131072, maxContextTokens: 200000 });
    const tm = makeTemplateModel({ maxOutputTokens: 128000, maxContextTokens: 200000 });

    const diffs = diffModelAgainstTemplate(model, tm);

    expect(diffs).toContainEqual({ field: 'maxOutputTokens', current: 131072, catalog: 128000 });
    // A field that already agrees is not a diff.
    expect(diffs.find((d) => d.field === 'maxContextTokens')).toBeUndefined();
  });

  it('reports a field the row has no value for at all', () => {
    const model = makeModel({ maxOutputTokens: undefined });
    const diffs = diffModelAgainstTemplate(model, makeTemplateModel({ maxOutputTokens: 64000 }));

    expect(diffs).toContainEqual({ field: 'maxOutputTokens', current: undefined, catalog: 64000 });
  });

  it('reports a hand-typed price that drifted from the catalog', () => {
    const model = makeModel({ inputPricePerMillion: 2 });
    const diffs = diffModelAgainstTemplate(model, makeTemplateModel({ inputPricePerMillion: 1 }));

    expect(diffs).toContainEqual({ field: 'inputPricePerMillion', current: 2, catalog: 1 });
  });

  it('never proposes erasing a value the catalog has no fact for', () => {
    // The generator omits null numerics and empty lists entirely.
    const model = makeModel({ maxOutputTokens: 4096, features: ['vision'], description: 'ours' });
    const tm = makeTemplateModel({ description: '', features: [] });
    expect(tm.maxOutputTokens).toBeUndefined();

    expect(diffModelAgainstTemplate(model, tm)).toEqual([]);
  });

  it('never proposes changing the local code or the upstream id', () => {
    const model = makeModel({ code: 'renamed-locally', providerModelId: 'claude-haiku-4-5' });
    const tm = makeTemplateModel({ code: 'claude-haiku-4-5', providerModelId: 'claude-haiku-4-5' });

    const fields = diffModelAgainstTemplate(model, tm).map((d) => d.field);
    expect(fields).not.toContain('code');
    expect(fields).not.toContain('providerModelId');
  });

  it('compares features as a set, ignoring order', () => {
    const tm = makeTemplateModel({ features: ['vision', 'streaming'] });
    const model = makeModel({ description: tm.description, features: ['streaming', 'vision'] });

    expect(diffModelAgainstTemplate(model, tm)).toEqual([]);
  });

  it('reports a feature the catalog added', () => {
    const model = makeModel({ features: ['vision'] });
    const diffs = diffModelAgainstTemplate(model, makeTemplateModel({ features: ['vision', 'thinking'] }));

    expect(diffs).toContainEqual({ field: 'features', current: ['vision'], catalog: ['vision', 'thinking'] });
  });
});

describe('catalogUpdateInput', () => {
  it('sends only the fields that differ, leaving everything else untouched', () => {
    const payload = catalogUpdateInput([
      { field: 'maxOutputTokens', current: 131072, catalog: 128000 },
      { field: 'inputPricePerMillion', current: 2, catalog: 1 },
    ]);

    expect(payload).toEqual({ maxOutputTokens: 128000, inputPricePerMillion: 1 });
  });
});

describe('catalogCreateInput', () => {
  it('carries the catalog code so a shared upstream id cannot collide across providers', () => {
    const input = catalogCreateInput(makeTemplateModel({ code: 'azure-gpt-5', providerModelId: 'gpt-5' }));

    expect(input.code).toBe('azure-gpt-5');
    expect(input.providerModelId).toBe('gpt-5');
  });

  it('carries the catalog facts and omits what the catalog does not know', () => {
    const input = catalogCreateInput(makeTemplateModel({
      maxOutputTokens: 64000,
      inputPricePerMillion: 1,
      features: ['vision'],
      aliases: ['claude-haiku-4-5-20251001'],
    }));

    expect(input).toMatchObject({
      name: 'Claude Haiku 4.5',
      type: 'chat',
      maxOutputTokens: 64000,
      inputPricePerMillion: 1,
      features: ['vision'],
      aliases: ['claude-haiku-4-5-20251001'],
    });
    expect('maxContextTokens' in input).toBe(false);
    expect('outputPricePerMillion' in input).toBe(false);
  });
});

describe('resolveTemplateName', () => {
  const templates: ApiProviderTemplate[] = [
    { name: 'anthropic', displayName: 'Anthropic', description: '', baseUrl: 'https://api.anthropic.com', adapterType: 'anthropic' },
    { name: 'openai', displayName: 'OpenAI', description: '', baseUrl: 'https://api.openai.com', adapterType: 'openai' },
  ];

  it('resolves by name, which the wizard seeds from the template', () => {
    const p = { name: 'anthropic', adapterType: 'anthropic', baseUrl: 'https://api.anthropic.com' };
    expect(resolveTemplateName(p, templates)).toBe('anthropic');
  });

  it('resolves a renamed provider by adapter plus base URL', () => {
    const p = { name: 'anthropic-prod', adapterType: 'anthropic', baseUrl: 'https://api.anthropic.com/' };
    expect(resolveTemplateName(p, templates)).toBe('anthropic');
  });

  it('refuses an OpenAI-compatible endpoint that merely speaks the protocol', () => {
    // A self-hosted runtime would otherwise resolve to OpenAI's catalog and be
    // offered that vendor's entire model list as new.
    const p = { name: 'local-vllm', adapterType: 'openai', baseUrl: 'http://10.0.0.5:8000' };
    expect(resolveTemplateName(p, templates)).toBeNull();
  });

  it('returns null when nothing matches', () => {
    const p = { name: 'mystery', adapterType: 'cohere', baseUrl: 'https://example.invalid' };
    expect(resolveTemplateName(p, templates)).toBeNull();
  });
});

/**
 * The field set the diff covers.
 *
 * These were added after a measurement: 49 of 30 days' production 400s were
 * `MODEL_MODALITY_MISMATCH`, which is what a stale modality list looks like
 * from the caller's side, and the sync that exists to correct catalog drift
 * compared neither those nor the audio prices. The compile-time classification
 * in catalog-sync.ts stops the list going stale again; these assert it is
 * wired to actual behaviour rather than merely exhaustive.
 */
describe('diffModelAgainstTemplate — every catalog-owned field', () => {
  it('offers the modality lists the gateway routes on', () => {
    const model = makeModel({ inputModalities: ['text'], outputModalities: ['text'] });
    const tm = makeTemplateModel({
      inputModalities: ['text', 'image'],
      outputModalities: ['text'],
      requiredModalities: ['audio'],
    });
    const fields = diffModelAgainstTemplate(model, tm).map((d) => d.field);
    expect(fields).toContain('inputModalities');
    expect(fields).toContain('requiredModalities');
    // outputModalities already agrees — an agreeing field is not a diff.
    expect(fields).not.toContain('outputModalities');
  });

  it('offers the audio prices, which no other surface shows', () => {
    const model = makeModel({ audioInputPricePerMillion: 40 });
    const tm = makeTemplateModel({
      audioInputPricePerMillion: 32,
      audioOutputPricePerMillion: 64,
      cachedAudioInputReadPricePerMillion: 0.4,
    });
    const diffs = diffModelAgainstTemplate(model, tm);
    expect(diffs.find((d) => d.field === 'audioInputPricePerMillion')).toEqual({
      field: 'audioInputPricePerMillion',
      current: 40,
      catalog: 32,
    });
    expect(diffs.map((d) => d.field)).toEqual(
      expect.arrayContaining(['audioOutputPricePerMillion', 'cachedAudioInputReadPricePerMillion']),
    );
  });

  it('compares modality lists as sets, so a reordering is not a change', () => {
    const model = makeModel({ inputModalities: ['image', 'text'] });
    const tm = makeTemplateModel({ inputModalities: ['text', 'image'] });
    const fields = diffModelAgainstTemplate(model, tm).map((d) => d.field);
    expect(fields).not.toContain('inputModalities');
  });

  it('never proposes overwriting the identity fields', () => {
    const model = makeModel({ code: 'my-own-name', aliases: ['local-alias'] });
    const tm = makeTemplateModel({ code: 'catalog-name', aliases: ['catalog-alias'] });
    const fields = diffModelAgainstTemplate(model, tm).map((d) => d.field);
    expect(fields).not.toContain('code');
    expect(fields).not.toContain('providerModelId');
    expect(fields).not.toContain('aliases');
  });
});

describe('catalogCreateInput — a row created from the catalog is complete', () => {
  it('carries modalities and audio prices, not just the chat fields', () => {
    const input = catalogCreateInput(
      makeTemplateModel({
        inputModalities: ['text', 'audio'],
        outputModalities: ['audio'],
        requiredModalities: ['audio'],
        audioInputPricePerMillion: 32,
        audioOutputPricePerMillion: 64,
        cachedAudioInputReadPricePerMillion: 0.4,
      }),
    );
    expect(input.inputModalities).toEqual(['text', 'audio']);
    expect(input.outputModalities).toEqual(['audio']);
    expect(input.requiredModalities).toEqual(['audio']);
    expect(input.audioInputPricePerMillion).toBe(32);
    expect(input.audioOutputPricePerMillion).toBe(64);
    expect(input.cachedAudioInputReadPricePerMillion).toBe(0.4);
  });
});
