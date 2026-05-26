/**
 * Frontend AI host registry — pattern-matches a hostname against a
 * static list of known AI service endpoints. Used by aggregate views
 * (Stats.tsx) where the data source is per-host rollup buckets.
 *
 * For per-event surfaces (Traffic.tsx) prefer `isAITraffic(event)` from
 * `@/lib/classify`, which derives the AI tag from `domainRuleId` and
 * stays in sync with the daemon's upload-filter classification.
 *
 * Patterns are plain glob with `*` as wildcard, case-insensitive and
 * exact (no substring). Add entries when an AI provider ships a new domain.
 */

const AI_HOST_PATTERNS: readonly string[] = [
  // OpenAI
  'chatgpt.com',
  '*.chatgpt.com',
  'openai.com',
  '*.openai.com',
  'oaistatic.com',
  '*.oaistatic.com',

  // Anthropic
  'claude.ai',
  '*.claude.ai',
  'anthropic.com',
  '*.anthropic.com',

  // Google AI / Gemini
  'gemini.google.com',
  'generativelanguage.googleapis.com',
  'aistudio.google.com',

  // Microsoft / Azure OpenAI
  '*.openai.azure.com',
  'copilot.microsoft.com',

  // AWS Bedrock
  '*.bedrock-runtime.*.amazonaws.com',
  'bedrock-runtime.*.amazonaws.com',

  // Mistral
  'api.mistral.ai',
  'mistral.ai',
  'chat.mistral.ai',

  // Cohere
  'api.cohere.com',
  'api.cohere.ai',

  // Perplexity
  'perplexity.ai',
  '*.perplexity.ai',

  // xAI / Grok
  'x.ai',
  '*.x.ai',
  'api.x.ai',

  // DeepSeek
  'deepseek.com',
  '*.deepseek.com',

  // Groq
  'groq.com',
  '*.groq.com',

  // Together
  'api.together.ai',
  'together.ai',

  // Cursor (uses Anthropic + OpenAI under the hood, but its own backend too)
  'cursor.sh',
  '*.cursor.sh',
  'cursor.com',
  '*.cursor.com',

  // GitHub Copilot
  '*.githubcopilot.com',
  'api.githubcopilot.com',
  'copilot-proxy.githubusercontent.com',

  // Hugging Face
  'huggingface.co',
  'api-inference.huggingface.co',

  // Replicate
  'replicate.com',
  'api.replicate.com',
];

/** Compile each glob to a case-insensitive RegExp once at module load. */
const AI_HOST_REGEX: readonly RegExp[] = AI_HOST_PATTERNS.map((p) => {
  const escaped = p
    .toLowerCase()
    .split('*')
    .map((s) => s.replace(/[.+?^${}()|[\]\\]/g, '\\$&'))
    .join('.*');
  return new RegExp(`^${escaped}$`, 'i');
});

/**
 * Returns true when host matches any AI-service pattern. Empty / null
 * / IP-literal hosts return false (we don't tag IPs since the registry
 * is hostname-keyed).
 */
export function isAIHost(host: string | null | undefined): boolean {
  if (!host) return false;
  const h = host.trim().toLowerCase();
  if (!h) return false;
  // Cheap IP-literal check — no point matching globs against an IP.
  if (/^[\d.]+$/.test(h) || h.includes(':')) return false;
  return AI_HOST_REGEX.some((re) => re.test(h));
}
