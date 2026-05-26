import { describe, it, expect } from 'vitest';
import { PROVIDER_ADAPTER_TYPES, isProviderAdapterType } from './adapterTypes';

describe('PROVIDER_ADAPTER_TYPES', () => {
  it('lists the canonical Provider adapter types from the AI Gateway format enum', () => {
    expect([...PROVIDER_ADAPTER_TYPES]).toEqual([
      'openai',
      'anthropic',
      'gemini',
      'glm',
      'deepseek',
      'azure-openai',
      'minimax',
      'bedrock',
      'vertex',
      'cohere',
      'huggingface',
      'replicate',
      'mistral',
      'xai',
      'groq',
      'perplexity',
      'together',
      'fireworks',
      'moonshot',
    ]);
  });

  it('excludes generic-jsonpath, which is traffic-only and not a Provider adapter', () => {
    expect((PROVIDER_ADAPTER_TYPES as readonly string[]).includes('generic-jsonpath')).toBe(false);
  });
});

describe('isProviderAdapterType', () => {
  it('returns true for every canonical value', () => {
    for (const v of PROVIDER_ADAPTER_TYPES) {
      expect(isProviderAdapterType(v)).toBe(true);
    }
  });

  it('returns false for unknown or legacy values', () => {
    for (const v of ['', 'builtin', 'openai-compatible', 'generic-jsonpath', 'OpenAI', 'unknown']) {
      expect(isProviderAdapterType(v)).toBe(false);
    }
  });
});
