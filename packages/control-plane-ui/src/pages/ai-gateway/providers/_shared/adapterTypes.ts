/* ── Adapter type catalog (Provider.adapterType) ───────────────────────── */

// Mirror of the Control Plane's `ValidAdapterTypes` list — the canonical
// wire formats the AI Gateway knows how to speak. Keep this in lockstep
// with:
//   • packages/control-plane/internal/handler/provider_adapter_types.go
//   • packages/ai-gateway/internal/providers/types.go  (Format enum)
//   • docs/users/api/openapi/ai-gateway/e30-s2-provider-adapter-type.yaml
// The Control Plane rejects any write that uses a value outside this
// set, so the UI must only offer these to avoid guaranteed-to-fail
// submissions.
export const PROVIDER_ADAPTER_TYPES = [
  'openai',
  'anthropic',
  'gemini',
  'glm',
  'deepseek',
  'azure-openai',
  'minimax',
  'bedrock',
  'vertex',
  // OpenAI-compat re-users — distinct adapterType so per-vendor audit /
  // metrics / rate-limit policies can target them without name matching.
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
] as const;

export type ProviderAdapterType = (typeof PROVIDER_ADAPTER_TYPES)[number];

export function isProviderAdapterType(v: string): v is ProviderAdapterType {
  return (PROVIDER_ADAPTER_TYPES as readonly string[]).includes(v);
}
