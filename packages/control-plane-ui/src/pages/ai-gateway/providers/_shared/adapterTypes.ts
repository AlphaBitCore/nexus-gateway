/* ── Adapter type catalog (Provider.adapterType) ───────────────────────── */

// Mirror of the Control Plane's `ValidAdapterTypes` list — the canonical
// wire formats the AI Gateway knows how to speak. Keep this in lockstep
// with:
//   • packages/control-plane/internal/ai/providers/handler/adapter_types.go
//   • packages/ai-gateway/internal/providers/core/format.go  (Format enum)
//   • docs/users/api/openapi/control-plane/providers.yaml  (adapterType enums)
// The Control Plane rejects any write that uses a value outside its own
// set, so the UI must only offer these to avoid guaranteed-to-fail
// submissions — and must offer ALL of them, or a format the gateway can
// speak has no way in. scripts/check-adapter-type-lockstep.mjs holds all
// four lists to each other; this comment is not the enforcement.
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
  // Embeddings / rerank adapter with its own codec — not an OpenAI-compat
  // re-user, so IsOpenAIFamily() excludes it. It is still a configurable
  // provider: leaving it out of this list is what made a wire format the
  // gateway speaks unreachable from the admin UI.
  'voyage',
] as const;

export type ProviderAdapterType = (typeof PROVIDER_ADAPTER_TYPES)[number];

export function isProviderAdapterType(v: string): v is ProviderAdapterType {
  return (PROVIDER_ADAPTER_TYPES as readonly string[]).includes(v);
}
