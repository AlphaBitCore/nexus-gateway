import { z } from 'zod';

/**
 * Field shapes for the provider-detail forms.
 *
 * Separate from the hook because these describe what the forms hold, while the
 * hook describes what happens when they are submitted — and because the four
 * schemas together are longer than the behaviour they feed.
 */

export const providerEditSchema = z.object({
  name: z.string().min(1),
  displayName: z.string().optional().default(''),
  description: z.string().optional().default(''),
  baseUrl: z.string().min(1),
  adapterType: z.string().min(1),
  region: z.string().optional().default(''),
  apiVersion: z.string().optional().default(''),
  enabled: z.boolean(),
});
export type ProviderEditValues = z.infer<typeof providerEditSchema>;

export const newCredentialSchema = z.object({
  credName: z.string().min(1),
  credApiKey: z.string().min(1),
  newCredEnabled: z.boolean(),
  credExpiresAt: z.string().optional().default(''),
});
export type NewCredentialValues = z.infer<typeof newCredentialSchema>;

export const editCredentialSchema = z.object({
  editCredName: z.string().min(1),
  editCredApiKey: z.string().optional().default(''),
  editCredEnabled: z.boolean(),
  editCredExpiresAt: z.string().optional().default(''),
});
export type EditCredentialValues = z.infer<typeof editCredentialSchema>;

export const newModelSchema = z.object({
  modelName: z.string().min(1),
  modelProviderModelId: z.string().min(1),
  modelCode: z.string().optional().default(''),
  modelType: z.string().min(1),
  modelDescription: z.string().optional().default(''),
  modelInputPrice: z.string().optional().default(''),
  modelOutputPrice: z.string().optional().default(''),
  modelCachedInputReadPrice: z.string().optional().default(''),
  modelCachedInputWritePrice: z.string().optional().default(''),
  modelAudioInputPrice: z.string().optional().default(''),
  modelAudioOutputPrice: z.string().optional().default(''),
  modelCachedAudioReadPrice: z.string().optional().default(''),
  modelMaxContext: z.string().optional().default(''),
  modelMaxOutput: z.string().optional().default(''),
  modelSelectedFeatures: z.array(z.string()),
  modelInputModalities: z.array(z.string()),
  modelOutputModalities: z.array(z.string()),
  // The floor: what a request must carry for this model to serve it.
  // Independent of the ceiling above — a model can accept audio without
  // requiring it, and one that requires audio refuses a plain text chat.
  modelRequiredModalities: z.array(z.string()),
  modelAliases: z.string().optional().default(''),
});
export type NewModelValues = z.infer<typeof newModelSchema>;

export const editModelSchema = z.object({
  editModelCode: z.string().min(1),
  editModelProviderModelId: z.string().min(1),
  editModelName: z.string().min(1),
  editModelDescription: z.string().optional().default(''),
  editModelInputPrice: z.string().optional().default(''),
  editModelOutputPrice: z.string().optional().default(''),
  editModelCachedInputReadPrice: z.string().optional().default(''),
  editModelCachedInputWritePrice: z.string().optional().default(''),
  editModelAudioInputPrice: z.string().optional().default(''),
  editModelAudioOutputPrice: z.string().optional().default(''),
  editModelCachedAudioReadPrice: z.string().optional().default(''),
  editModelMaxContext: z.string().optional().default(''),
  editModelMaxOutput: z.string().optional().default(''),
  editModelFeatures: z.array(z.string()),
  editModelInputModalities: z.array(z.string()),
  editModelOutputModalities: z.array(z.string()),
  editModelRequiredModalities: z.array(z.string()),
  editModelType: z.string().min(1),
  editModelStatus: z.string().min(1),
  editModelAliases: z.string().optional().default(''),
  editModelEnabled: z.boolean(),
  editModelDeprecationDate: z.string().optional().default(''),
  editModelReplacedBy: z.string().optional().default(''),
});
export type EditModelValues = z.infer<typeof editModelSchema>;
