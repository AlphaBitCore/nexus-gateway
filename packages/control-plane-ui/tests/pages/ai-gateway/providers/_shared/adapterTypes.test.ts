import { describe, it, expect } from 'vitest';
import {
  PROVIDER_ADAPTER_TYPES,
  isProviderAdapterType,
} from '../../../../../src/pages/ai-gateway/providers/_shared/adapterTypes';

/**
 * This file does NOT restate the list.
 *
 * It used to, and that made a sixth copy of a set already written in
 * format.go, adapter_types.go, four spec enums and the module under test — so
 * the assertion could only ever confirm that two copies in the same repo
 * agreed, which is what a copy is for. It could not see the drift that
 * mattered: `voyage` reached every Go list and the published spec and never
 * reached the picker, leaving a wire format the gateway speaks with no way in
 * from the admin UI.
 *
 * Set equality now belongs to scripts/check-adapter-type-lockstep.mjs, which
 * can read all four sources. What is left here is what a unit test can know on
 * its own: the shape of the list, and that membership is exact.
 */
describe('PROVIDER_ADAPTER_TYPES', () => {
  it('is a non-empty list of lowercase slugs with no duplicates', () => {
    expect(PROVIDER_ADAPTER_TYPES.length).toBeGreaterThan(0);
    expect(new Set(PROVIDER_ADAPTER_TYPES).size).toBe(PROVIDER_ADAPTER_TYPES.length);
    for (const v of PROVIDER_ADAPTER_TYPES) {
      expect(v).toMatch(/^[a-z][a-z0-9-]*$/);
    }
  });

  it('excludes generic-jsonpath, which is traffic-only and not a Provider adapter', () => {
    expect((PROVIDER_ADAPTER_TYPES as readonly string[]).includes('generic-jsonpath')).toBe(false);
  });

  it('excludes openai-responses, an ingress format that is never a Provider adapter', () => {
    expect((PROVIDER_ADAPTER_TYPES as readonly string[]).includes('openai-responses')).toBe(false);
  });
});

describe('isProviderAdapterType', () => {
  it('returns true for every canonical value', () => {
    for (const v of PROVIDER_ADAPTER_TYPES) {
      expect(isProviderAdapterType(v)).toBe(true);
    }
  });

  it('matches exactly — no case folding, no trimming, no legacy aliases', () => {
    for (const v of [
      '',
      'builtin',
      'openai-compatible',
      'generic-jsonpath',
      'OpenAI',
      'openai ',
      ' openai',
      'unknown',
    ]) {
      expect(isProviderAdapterType(v)).toBe(false);
    }
  });
});
