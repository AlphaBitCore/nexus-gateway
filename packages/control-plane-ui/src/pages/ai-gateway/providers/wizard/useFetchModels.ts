/**
 * useFetchModels — thin hook that wraps the "Fetch from /v1/models" workflow.
 *
 * Extracted from useProviderWizard to keep that file under the file-size ratchet
 * (500 lines). All state is owned here; useProviderWizard spreads the return
 * value into its own return object so callers (StepModels) stay unchanged.
 */

import { useState, useCallback } from 'react';
import { useTranslation } from 'react-i18next';
import { providerApi } from '@/api/services';
import type { WizardModel } from './types';

export interface UseFetchModelsParams {
  adapterType: string;
  baseUrl: string;
  apiKey: string;
  setModels: React.Dispatch<React.SetStateAction<WizardModel[]>>;
}

export function useFetchModels({ adapterType, baseUrl, apiKey, setModels }: UseFetchModelsParams) {
  const { t } = useTranslation();
  const [fetchingModels, setFetchingModels] = useState(false);
  const [fetchModelsError, setFetchModelsError] = useState<string | null>(null);
  const [fetchModelsUnsupported, setFetchModelsUnsupported] = useState(false);
  const [fetchModelsCount, setFetchModelsCount] = useState<number | null>(null);

  /**
   * Fetch models from the provider's /v1/models endpoint via the backend
   * discover-models proxy. On success, new model rows are merged into the
   * existing list (deduplicated by providerModelId). Uses the same WizardModel
   * shape as selectFromApiTemplate so the rest of the wizard works identically.
   */
  const fetchModels = useCallback(async () => {
    setFetchingModels(true);
    setFetchModelsError(null);
    setFetchModelsUnsupported(false);
    setFetchModelsCount(null);
    try {
      const result = await providerApi.discoverModels({ adapterType, baseUrl, apiKey });
      if (!result.success) {
        if (result.code === 'discovery_unsupported') {
          setFetchModelsUnsupported(true);
        } else {
          setFetchModelsError(result.error);
        }
        return;
      }
      setModels((prev) => {
        const existingCodes = new Set(prev.map((m) => m.modelId.trim().toLowerCase()));
        const newRows: WizardModel[] = (result.models ?? [])
          .filter((m) => !existingCodes.has(m.id.trim().toLowerCase()))
          .map((m) => ({
            modelId: m.id,
            name: m.id,
            description: '',
            type: m.suggestedType,
            inputPrice: '',
            outputPrice: '',
            cachedInputReadPrice: '',
            cachedInputWritePrice: '',
            maxContextTokens: '',
            maxOutputTokens: '',
            features: [],
            selected: false,
          }));
        setFetchModelsCount(newRows.length);
        return [...prev, ...newRows];
      });
    } catch (err) {
      setFetchModelsError(err instanceof Error ? err.message : t('pages:providers.wizardModelsFetchError', 'Failed to fetch models'));
    } finally {
      setFetchingModels(false);
    }
  }, [adapterType, baseUrl, apiKey, setModels, t]);

  return { fetchModels, fetchingModels, fetchModelsError, fetchModelsUnsupported, fetchModelsCount };
}
