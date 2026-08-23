import { describe, expect, it } from 'vitest';
import { mergeModelFeatureOptions, MODEL_FEATURE_OPTIONS } from '../../../../../src/pages/ai-gateway/providers/_shared/model-feature-options';

describe('mergeModelFeatureOptions', () => {
  it('returns known options when selected is empty', () => {
    expect(mergeModelFeatureOptions([])).toEqual(MODEL_FEATURE_OPTIONS);
  });

  it('appends unknown feature values from the model for editing', () => {
    const merged = mergeModelFeatureOptions(['custom_capability']);
    expect(merged).toContainEqual({ value: 'custom_capability', label: 'custom_capability' });
    expect(merged.length).toBe(MODEL_FEATURE_OPTIONS.length + 1);
  });

  it('offers every capability the router treats as eligibility', () => {
    // These three are hard filters in the gateway: `auto` refuses to pick a
    // model whose row lacks the tag the request needs. An admin who cannot
    // TICK the tag cannot describe a model the router will then decline to
    // use, and nothing in the UI explains why the model is never selected.
    //
    // `structured_outputs` is the one this test was written for. It is not a
    // stronger `json_mode`: probed 2026-08-19, gpt-4-turbo carries json_mode
    // and answers 400 to a JSON Schema, so the two are separate answers and
    // ticking one must never be read as the other.
    const values = MODEL_FEATURE_OPTIONS.map((o) => o.value);
    for (const tag of ['function_calling', 'reasoning', 'structured_outputs']) {
      expect(values).toContain(tag);
    }
  });

  it('does not offer vision — the modalities field owns that fact', () => {
    // `vision` meant "accepts images", which is what the Accepts row of the
    // modalities field says. Offering both let an admin tick one without the
    // other, and 34 production rows ended up advertising vision beside
    // inputModalities ["text"].
    expect(MODEL_FEATURE_OPTIONS.map((o) => o.value)).not.toContain('vision');
  });

  it('still renders vision for a row that has not been migrated yet', () => {
    // The value is stripped on write and derived back onto GET /v1/models, but
    // a row written before that still carries it. Dropping it from the editor
    // would delete the admin's stored value on their next unrelated save.
    const merged = mergeModelFeatureOptions(['vision']);
    expect(merged).toContainEqual({ value: 'vision', label: 'vision' });
  });
});
