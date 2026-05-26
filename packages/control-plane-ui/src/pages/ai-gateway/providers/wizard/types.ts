/* ── Shared types for the provider wizard ──────────────────────────────── */

// Template types live in @/api/types — re-exported here so the wizard
// modules don't need to reach across paths. The catalog data is served
// as static JSON from public/provider-templates/ (see types.ts for the
// contract).
export type { ApiTemplateModel, ApiProviderTemplate } from '@/api/types';

// WizardModel mirrors the fields the detail-page Edit dialog exposes
// (ProviderModelsTab.tsx) so seed-from-template, manual add, and Edit
// agree on what a model row contains. Numeric fields stay as strings
// because the form inputs bind to strings; we coerce on submit.
export interface WizardModel {
  modelId: string;
  name: string;
  description: string;
  type: string;
  inputPrice: string;
  outputPrice: string;
  /** Optional per-million cache READ price. Empty string = unset (NULL on submit). */
  cachedInputReadPrice: string;
  /** Optional per-million cache WRITE price. Empty string = unset (NULL on submit). */
  cachedInputWritePrice: string;
  maxContextTokens: string;
  maxOutputTokens: string;
  features: string[];
  selected: boolean;
}

export const STEP_KEYS = ['stepTemplate', 'stepProvider', 'stepCredential', 'stepModels', 'stepReview'] as const;

/** Default picks on step 0 (choose template); "Browse more" reveals the rest. Order is display priority. */
export const FEATURED_PROVIDER_TEMPLATE_NAMES = [
  'openai',
  'anthropic',
  'google-gemini',
  'azure-openai',
  'deepseek',
  'minimax',
  'glm',
  'moonshot',
] as const;
