import { describe, expect, it } from 'vitest';
import { mergeModelFeatureOptions, MODEL_FEATURE_OPTIONS } from './model-feature-options';

describe('mergeModelFeatureOptions', () => {
  it('returns known options when selected is empty', () => {
    expect(mergeModelFeatureOptions([])).toEqual(MODEL_FEATURE_OPTIONS);
  });

  it('appends unknown feature values from the model for editing', () => {
    const merged = mergeModelFeatureOptions(['vision', 'custom_capability']);
    expect(merged).toContainEqual({ value: 'vision', label: 'Vision' });
    expect(merged).toContainEqual({ value: 'custom_capability', label: 'custom_capability' });
    expect(merged.length).toBe(MODEL_FEATURE_OPTIONS.length + 1);
  });
});
