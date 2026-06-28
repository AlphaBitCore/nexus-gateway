import { useState, useCallback, useEffect, useMemo, useRef } from 'react';
import { useNavigate } from 'react-router-dom';
import { useTranslation } from 'react-i18next';
import { providerApi, credentialApi, systemApi } from '@/api/services';
import { useToast } from '@/context/ToastContext';
import { useSyncFeedback } from '@/hooks/useSyncFeedback';
import { useApi } from '@/hooks/useApi';
import type { ApiProviderTemplate, WizardModel } from './types';
import { FEATURED_PROVIDER_TEMPLATE_NAMES } from './types';
import { featuredTemplatesFirst } from './helpers';
import { useFetchModels } from './useFetchModels';

const DEFAULT_VISIBLE_TEMPLATE_COUNT = 16;

export function useProviderWizard() {
  const { t } = useTranslation();
  const navigate = useNavigate();
  const { addToast } = useToast();
  const showSyncFeedback = useSyncFeedback();

  /* ── Template fetching ──────────────────────────────────────────────── */

  const { data: templatesRes, loading: templatesLoading, error: templatesError, refetch: refetchTemplates } = useApi<{
    data: ApiProviderTemplate[];
  }>(() => providerApi.getTemplates(), ['wizard', 'provider-templates', 'index']);

  const templates = templatesRes?.data ?? [];

  /* ── Template filtering & grid ──────────────────────────────────────── */

  const [templateQuery, setTemplateQuery] = useState('');
  const filteredTemplates = useMemo(() => {
    const q = templateQuery.trim().toLowerCase();
    if (!q) return templates;
    return templates.filter(
      (t) =>
        t.name.toLowerCase().includes(q) ||
        t.displayName.toLowerCase().includes(q) ||
        t.description.toLowerCase().includes(q) ||
        t.adapterType.toLowerCase().includes(q),
    );
  }, [templates, templateQuery]);

  const [browseAllTemplates, setBrowseAllTemplates] = useState(false);

  const defaultCollapsedTemplates = useMemo(() => {
    const featured = featuredTemplatesFirst(filteredTemplates);
    const ordered = featured.length > 0 ? featured : filteredTemplates;
    return ordered.slice(0, DEFAULT_VISIBLE_TEMPLATE_COUNT);
  }, [filteredTemplates]);

  const templatesForGrid = useMemo(() => {
    if (templateQuery.trim() || browseAllTemplates) return filteredTemplates;
    return defaultCollapsedTemplates;
  }, [filteredTemplates, templateQuery, browseAllTemplates, defaultCollapsedTemplates]);

  const collapsedHiddenCount = useMemo(
    () => Math.max(0, filteredTemplates.length - defaultCollapsedTemplates.length),
    [filteredTemplates, defaultCollapsedTemplates],
  );

  /* ── Wizard step state ──────────────────────────────────────────────── */

  const [step, setStep] = useState(0);
  const [submitting, setSubmitting] = useState(false);
  const [error, setError] = useState<string | null>(null);

  /* ── Step 0: template selection ─────────────────────────────────────── */

  const [selectedTemplate, setSelectedTemplate] = useState<string | null>(null);
  const [isCustom, setIsCustom] = useState(false);

  /* ── Step 1: provider fields ────────────────────────────────────────── */

  const [name, setName] = useState('');
  const [displayName, setDisplayName] = useState('');
  const [baseUrl, setBaseUrl] = useState('');
  // Default the wizard's adapter selection to the broadest compatible
  // wire format. The user must still pick explicitly on step 1 (custom
  // template) — the default just keeps the Select showing a valid
  // option until then. A template selection overwrites this.
  const [adapterType, setAdapterType] = useState('openai');
  const [description, setDescription] = useState('');

  // Provider.name has a unique constraint (it's the routing path slug
  // /<name>/...). Pre-check it as the user types so the conflict surfaces
  // on Step 1 rather than at submit on Step 5. The check is debounced and
  // uses providerApi.list({q}) — when the response contains an exact
  // case-insensitive name match, we set nameError. canNext blocks step 1.
  const [nameError, setNameError] = useState<string | null>(null);
  const [nameChecking, setNameChecking] = useState(false);
  const nameCheckSeqRef = useRef(0);

  useEffect(() => {
    const trimmed = name.trim();
    if (!trimmed) {
      setNameError(null);
      setNameChecking(false);
      return;
    }
    const seq = ++nameCheckSeqRef.current;
    setNameChecking(true);
    const handle = setTimeout(async () => {
      try {
        const res = await providerApi.list({ q: trimmed, limit: 50 });
        if (seq !== nameCheckSeqRef.current) return; // stale
        const hit = (res.data ?? []).some((p) => p.name.toLowerCase() === trimmed.toLowerCase());
        setNameError(hit ? t('pages:providers.nameAlreadyExists', 'A provider with this name already exists') : null);
      } catch {
        // On lookup failure, don't block submit — backend is the final guard.
        if (seq === nameCheckSeqRef.current) setNameError(null);
      } finally {
        if (seq === nameCheckSeqRef.current) setNameChecking(false);
      }
    }, 350);
    return () => clearTimeout(handle);
  }, [name, t]);

  /* ── Step 2: credential ─────────────────────────────────────────────── */

  const [credName, setCredName] = useState('');
  const [apiKey, setApiKey] = useState('');
  const [skipCredential, setSkipCredential] = useState(false);

  // Credential.name is globally unique (Credential_name_key). Pre-check it as
  // the user types so the conflict surfaces on Step 2 rather than at submit on
  // Step 5 — mirrors the provider-name check above. The check is debounced and
  // uses credentialApi.list({q}); an exact case-insensitive match sets
  // credNameError, which canNext blocks. Skipped entirely when skipCredential.
  const [credNameError, setCredNameError] = useState<string | null>(null);
  const [credNameChecking, setCredNameChecking] = useState(false);
  const credNameCheckSeqRef = useRef(0);

  useEffect(() => {
    if (skipCredential) {
      setCredNameError(null);
      setCredNameChecking(false);
      return;
    }
    const trimmed = credName.trim();
    if (!trimmed) {
      setCredNameError(null);
      setCredNameChecking(false);
      return;
    }
    const seq = ++credNameCheckSeqRef.current;
    setCredNameChecking(true);
    const handle = setTimeout(async () => {
      try {
        const res = await credentialApi.list({ q: trimmed, limit: '50' });
        if (seq !== credNameCheckSeqRef.current) return; // stale
        const hit = (res.data ?? []).some((cr) => cr.name.toLowerCase() === trimmed.toLowerCase());
        setCredNameError(hit ? t('pages:providers.credNameAlreadyExists', 'A credential with this name already exists — credential names are globally unique') : null);
      } catch {
        // On lookup failure, don't block submit — backend is the final guard.
        if (seq === credNameCheckSeqRef.current) setCredNameError(null);
      } finally {
        if (seq === credNameCheckSeqRef.current) setCredNameChecking(false);
      }
    }, 350);
    return () => clearTimeout(handle);
  }, [credName, skipCredential, t]);

  /* ── Step 3: models ─────────────────────────────────────────────────── */

  const [models, setModels] = useState<WizardModel[]>([]);
  const [manualMode, setManualMode] = useState(false);

  // Fetch-from-/v1/models capability — isolated in its own hook to keep
  // this file within the line-count ratchet (500 lines).
  const { fetchModels, fetchingModels, fetchModelsError, fetchModelsUnsupported, fetchModelsCount } =
    useFetchModels({ adapterType, baseUrl, apiKey, setModels });

  const [newModelId, setNewModelId] = useState('');
  const [newModelName, setNewModelName] = useState('');
  const [newModelDescription, setNewModelDescription] = useState('');
  const [newModelType, setNewModelType] = useState('chat');
  const [newModelInputPrice, setNewModelInputPrice] = useState('');
  const [newModelOutputPrice, setNewModelOutputPrice] = useState('');
  const [newModelCachedInputReadPrice, setNewModelCachedInputReadPrice] = useState('');
  const [newModelCachedInputWritePrice, setNewModelCachedInputWritePrice] = useState('');
  const [newModelMaxContext, setNewModelMaxContext] = useState('');
  const [newModelMaxOutput, setNewModelMaxOutput] = useState('');
  const [newModelFeatures, setNewModelFeatures] = useState<string[]>([]);

  // Existing global model codes, fetched once. Model.code is globally unique
  // (Model_code_key), so a wizard model whose code (its modelId) collides with
  // an already-registered model would 409 at submit. We flag it on this step
  // and block Next instead — the conflict surfaces before "Add provider".
  const [existingModelCodes, setExistingModelCodes] = useState<Set<string>>(new Set());
  useEffect(() => {
    let cancelled = false;
    (async () => {
      try {
        const res = await systemApi.listModelsFlat({ limit: '1000' });
        if (cancelled) return;
        setExistingModelCodes(new Set((res.data ?? []).map((m) => m.code.toLowerCase())));
      } catch {
        // Non-fatal — backend stays the final guard.
      }
    })();
    return () => { cancelled = true; };
  }, []);

  const modelCodeConflicts = useMemo(
    () => models
      .filter((m) => m.selected && existingModelCodes.has(m.modelId.trim().toLowerCase()))
      .map((m) => m.modelId),
    [models, existingModelCodes],
  );

  // Inline-edit a wizard model's id (= its code) so a collision can be renamed
  // in place rather than only deselected — closes the dead-end where template
  // models were selection-only.
  const updateModelId = useCallback((idx: number, value: string) => {
    setModels((prev) => prev.map((m, i) => (i === idx ? { ...m, modelId: value } : m)));
  }, []);

  const resetManualModelForm = useCallback(() => {
    setNewModelId(''); setNewModelName(''); setNewModelDescription('');
    setNewModelType('chat');
    setNewModelInputPrice(''); setNewModelOutputPrice('');
    setNewModelCachedInputReadPrice(''); setNewModelCachedInputWritePrice('');
    setNewModelMaxContext(''); setNewModelMaxOutput('');
    setNewModelFeatures([]);
  }, []);

  /* ── Actions ────────────────────────────────────────────────────────── */

  // index.json entries don't carry the models array — we fetch
  // `/provider-templates/<name>.json` on selection to get the full detail.
  // If the detail fetch fails we still populate the other fields so the user
  // can continue and add models manually.
  const selectFromApiTemplate = useCallback(async (tpl: ApiProviderTemplate) => {
    setIsCustom(false);
    setSelectedTemplate(tpl.name);
    setName(tpl.name);
    setDisplayName(tpl.displayName);
    setBaseUrl(tpl.baseUrl);
    setAdapterType(tpl.adapterType);
    setDescription(tpl.description);
    setCredName(`${tpl.name}-default`);
    setModels([]);
    setManualMode(false);
    try {
      const detail = await providerApi.getTemplateDetail(tpl.name);
      setModels(
        (detail.models ?? []).map((m) => ({
          modelId: m.providerModelId || m.code,
          name: m.name,
          description: m.description ?? '',
          type: m.type,
          // Pre-fill numeric fields so the user can accept or tweak the catalog values.
          inputPrice: m.inputPricePerMillion != null ? String(m.inputPricePerMillion) : '',
          outputPrice: m.outputPricePerMillion != null ? String(m.outputPricePerMillion) : '',
          cachedInputReadPrice: m.cachedInputReadPricePerMillion != null ? String(m.cachedInputReadPricePerMillion) : '',
          cachedInputWritePrice: m.cachedInputWritePricePerMillion != null ? String(m.cachedInputWritePricePerMillion) : '',
          maxContextTokens: m.maxContextTokens != null ? String(m.maxContextTokens) : '',
          maxOutputTokens: m.maxOutputTokens != null ? String(m.maxOutputTokens) : '',
          features: [...(m.features ?? [])],
          selected: true,
        })),
      );
    } catch (err) {
      addToast(
        t('pages:providers.templateDetailLoadFailed', 'Failed to load model catalog for this template; you can add models manually.'),
        'error',
      );
      // Non-fatal — let the user continue without the catalog.
      // eslint-disable-next-line no-console
      console.warn('[wizard] loadTemplateDetail failed', err);
    }
  }, [addToast, t]);

  const selectCustom = useCallback(() => {
    setIsCustom(true);
    setSelectedTemplate(null);
    setName('');
    setDisplayName('');
    setBaseUrl('');
    setAdapterType('openai');
    setDescription('');
    setCredName('');
    setModels([]);
    setManualMode(false);
  }, []);

  const canNext = () => {
    switch (step) {
      case 0: return selectedTemplate !== null || isCustom;
      case 1: return !!name?.trim() && !!baseUrl?.trim() && !nameError && !nameChecking;
      case 2: return skipCredential || (!!credName?.trim() && !!apiKey && !credNameError && !credNameChecking);
      case 3: return modelCodeConflicts.length === 0;
      case 4: return true;
      default: return false;
    }
  };

  const addManualModel = () => {
    if (!newModelId) return;
    setModels((prev) => [
      ...prev,
      {
        modelId: newModelId,
        name: newModelName || newModelId,
        description: newModelDescription,
        type: newModelType,
        inputPrice: newModelInputPrice,
        outputPrice: newModelOutputPrice,
        cachedInputReadPrice: newModelCachedInputReadPrice,
        cachedInputWritePrice: newModelCachedInputWritePrice,
        maxContextTokens: newModelMaxContext,
        maxOutputTokens: newModelMaxOutput,
        features: [...newModelFeatures],
        selected: true,
      },
    ]);
    resetManualModelForm();
  };

  const toggleModel = (idx: number) => {
    setModels((prev) => prev.map((m, i) => (i === idx ? { ...m, selected: !m.selected } : m)));
  };

  const removeModel = (idx: number) => {
    setModels((prev) => prev.filter((_, i) => i !== idx));
  };

  const handleSubmit = async () => {
    setSubmitting(true);
    setError(null);
    try {
      // Single atomic request: POST /providers with models + credential
      // inline. Backend wraps provider + models + credential inserts in
      // one transaction — a duplicate provider name or a duplicate
      // (providerId, providerModelId) triggers a rollback and the DB
      // stays clean.
      const selectedModels = models.filter((m) => m.selected);
      const payloadModels = selectedModels.map((m) => ({
        providerModelId: m.modelId,
        name: m.name,
        type: m.type,
        ...(m.description.trim() && { description: m.description.trim() }),
        ...(m.inputPrice && { inputPricePerMillion: parseFloat(m.inputPrice) }),
        ...(m.outputPrice && { outputPricePerMillion: parseFloat(m.outputPrice) }),
        ...(m.cachedInputReadPrice && { cachedInputReadPricePerMillion: parseFloat(m.cachedInputReadPrice) }),
        ...(m.cachedInputWritePrice && { cachedInputWritePricePerMillion: parseFloat(m.cachedInputWritePrice) }),
        ...(m.maxContextTokens && { maxContextTokens: parseInt(m.maxContextTokens, 10) }),
        ...(m.maxOutputTokens && { maxOutputTokens: parseInt(m.maxOutputTokens, 10) }),
        features: m.features,
      }));

      const payloadCredential = !skipCredential && credName && apiKey
        ? { name: credName, apiKey }
        : undefined;

      const created = await providerApi.create({
        name, displayName, description, baseUrl, adapterType, enabled: true,
        ...(payloadModels.length > 0 && { models: payloadModels }),
        ...(payloadCredential && { credential: payloadCredential }),
      });

      addToast(t('pages:providers.providerCreatedSuccess'), 'success');
      showSyncFeedback('ai-gateway');
      navigate(`/ai-gateway/providers/${created.id}`);
    } catch (err) {
      const message = err instanceof Error ? err.message : t('pages:providers.providerCreateFailed');
      setError(message);
      addToast(message, 'error');
    } finally {
      setSubmitting(false);
    }
  };

  const goBack = () => {
    if (step === 0) navigate('/ai-gateway/providers');
    else setStep((s) => s - 1);
  };

  const goNext = () => setStep((s) => s + 1);

  const clearError = () => setError(null);

  const handleTemplateQueryChange = (value: string) => {
    setTemplateQuery(value);
    if (!value.trim()) setBrowseAllTemplates(false);
  };

  return {
    /* translation */
    t,
    navigate,

    /* template data */
    templates,
    templatesLoading,
    templatesError,
    refetchTemplates,
    templateQuery,
    handleTemplateQueryChange,
    filteredTemplates,
    browseAllTemplates,
    setBrowseAllTemplates,
    templatesForGrid,
    collapsedHiddenCount,

    /* wizard step */
    step,
    submitting,
    error,
    clearError,

    /* step 0 */
    selectedTemplate,
    isCustom,
    selectFromApiTemplate,
    selectCustom,

    /* step 1 */
    name, setName,
    nameError, nameChecking,
    displayName, setDisplayName,
    baseUrl, setBaseUrl,
    adapterType, setAdapterType,
    description, setDescription,

    /* step 2 */
    credName, setCredName,
    credNameError, credNameChecking,
    apiKey, setApiKey,
    skipCredential, setSkipCredential,

    /* step 3 */
    models,
    manualMode, setManualMode,
    newModelId, setNewModelId,
    newModelName, setNewModelName,
    newModelDescription, setNewModelDescription,
    newModelType, setNewModelType,
    newModelInputPrice, setNewModelInputPrice,
    newModelOutputPrice, setNewModelOutputPrice,
    newModelCachedInputReadPrice, setNewModelCachedInputReadPrice,
    newModelCachedInputWritePrice, setNewModelCachedInputWritePrice,
    newModelMaxContext, setNewModelMaxContext,
    newModelMaxOutput, setNewModelMaxOutput,
    newModelFeatures, setNewModelFeatures,
    resetManualModelForm,
    addManualModel,
    toggleModel,
    removeModel,
    existingModelCodes,
    modelCodeConflicts,
    updateModelId,
    fetchModels,
    fetchingModels,
    fetchModelsError,
    fetchModelsUnsupported,
    fetchModelsCount,

    /* navigation */
    canNext,
    goBack,
    goNext,
    handleSubmit,
  } as const;
}

export type ProviderWizardHook = ReturnType<typeof useProviderWizard>;
