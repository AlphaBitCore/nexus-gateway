import type { ProviderDetailState } from './useProviderDetail';
import type { ModelCapabilityJson } from '@/api/types';

/**
 * Turn the create-model form state into the API payload and submit it.
 *
 * Lives apart from the drawer because it is the only place that decides what a
 * field's absence means, which is a different job from laying the fields out.
 * The rule it encodes: a price or limit left blank is omitted rather than sent
 * as zero, and modalities left untouched are omitted rather than sent empty —
 * an empty array would tell the server this model accepts nothing, while
 * absence lets it derive the arrays from the model's type and features.
 */
export function submitCreate(detail: ProviderDetailState, capability?: ModelCapabilityJson) {
  const f = detail.newModelForm;
  const v = f.getValues();
  detail.createModel({
    name: v.modelName, providerModelId: v.modelProviderModelId, type: v.modelType,
    code: v.modelCode,
    ...(v.modelDescription && { description: v.modelDescription }),
    ...(v.modelInputPrice && { inputPricePerMillion: Number(v.modelInputPrice) }),
    ...(v.modelOutputPrice && { outputPricePerMillion: Number(v.modelOutputPrice) }),
    ...(v.modelCachedInputReadPrice && { cachedInputReadPricePerMillion: Number(v.modelCachedInputReadPrice) }),
    ...(v.modelCachedInputWritePrice && { cachedInputWritePricePerMillion: Number(v.modelCachedInputWritePrice) }),
    ...(v.modelAudioInputPrice && { audioInputPricePerMillion: Number(v.modelAudioInputPrice) }),
    ...(v.modelAudioOutputPrice && { audioOutputPricePerMillion: Number(v.modelAudioOutputPrice) }),
    ...(v.modelCachedAudioReadPrice && { cachedAudioInputReadPricePerMillion: Number(v.modelCachedAudioReadPrice) }),
    ...(v.modelMaxContext && { maxContextTokens: Number(v.modelMaxContext) }),
    ...(v.modelMaxOutput && { maxOutputTokens: Number(v.modelMaxOutput) }),
    features: v.modelSelectedFeatures,
    // Omitted when untouched. An empty array would mean "this model accepts
    // nothing"; absent means "derive it", which is what the server does.
    ...(v.modelInputModalities?.length && { inputModalities: v.modelInputModalities }),
    ...(v.modelOutputModalities?.length && { outputModalities: v.modelOutputModalities }),
    ...(v.modelRequiredModalities?.length && {
      requiredModalities: v.modelRequiredModalities,
    }),
    aliases: v.modelAliases ? v.modelAliases.split(',').map((s) => s.trim()).filter(Boolean) : [],
    ...(capability && { capabilityJson: capability }),
  });
}
