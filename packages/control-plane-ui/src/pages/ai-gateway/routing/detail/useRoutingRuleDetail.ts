import { useState, useMemo, useEffect } from 'react';
import { useTranslation } from 'react-i18next';
import { useParams, useNavigate } from 'react-router-dom';
import { routingApi, systemApi } from '@/api/services';
import type {
  RoutingRuleUpdatePayload,
  RoutingSimulateRequest,
  RoutingSimulateResponse,
} from '@/api/services';
import { useApi } from '@/hooks/useApi';
import { useMutation } from '@/hooks/useMutation';
import { useSyncFeedback } from '@/hooks/useSyncFeedback';
import { usePermission } from '@/hooks/usePermission';
import { useToast } from '@/context/ToastContext';
import { useZodForm } from '@/lib/forms';
import { useUnsavedChangesWarning } from '@/hooks/useUnsavedChangesWarning';
import { z } from 'zod';
import type { ErrorClass, RoutingRule, AdminModelsByProvider } from '@/api/types';
import {
  buildRetryPolicyPayload,
  deriveRetryPolicyInitialState,
  isRetryPolicyMaxAttemptsInvalid,
  type RetryPolicyMode,
} from '../form/RetryPolicySection';
import {
  mapLegacyStrategy,
  parseRoutingConfigForForm,
  buildRoutingApiConfig,
  buildPolicyApiConfig,
  policyConfigToFormLines,
  buildSmartConfig,
  buildFallbackChainApi,
  parseFallbackChain,
  configuredInternalModelIds,
  emptyConditionalFormState,
  hydrateConditionalEditorState,
  parseSmartConfig,
  resolveConditionalConfigFromEditor,
  DEFAULT_SMART_SYSTEM_PROMPT,
  parseMatchConditionsForm,
  buildMatchConditionsPayload,
  validateSplitWeights,
  type ConditionalEditorHydration,
  type StrategyType,
  type ProviderModelEntry,
  type SmartFormState,
  type FallbackEntry,
} from '../_shared/routing-rule-config';

const EMPTY_PROVIDER_GROUPS: AdminModelsByProvider[] = [];

const EMPTY_SMART_FORM_STATE: SmartFormState = {
  routerProvider: '',
  routerModel: '',
  systemPrompt: DEFAULT_SMART_SYSTEM_PROMPT,
  temperature: '0',
  maxTokens: '1024',
  timeoutMs: '10000',
  defaultProvider: '',
  defaultModel: '',
};

/* ── Zod schema for basic edit fields ─────────────────────────────────── */

export const routingEditSchema = z.object({
  editName: z.string().min(1),
  editDescription: z.string().optional().default(''),
  editPriority: z.coerce.number().int(),
  editEnabled: z.boolean(),
  editPipelineStage: z.string(),
  editStrategyType: z.string(),
  policyAllowM: z.array(z.string()).optional().default([]),
  policyDenyM: z.array(z.string()).optional().default([]),
  policyAllowP: z.array(z.string()).optional().default([]),
  policyDenyP: z.array(z.string()).optional().default([]),
  singleProvider: z.string().optional().default(''),
  singleModel: z.string().optional().default(''),
  matchProjectIds: z.array(z.string()).optional().default([]),
});

export type RoutingEditValues = z.infer<typeof routingEditSchema>;

export function useRoutingRuleDetail() {
  const { t } = useTranslation();
  const { id } = useParams<{ id: string }>();
  const navigate = useNavigate();
  const [isEditing, setIsEditing] = useState(false);
  const [deleting, setDeleting] = useState(false);
  const { addToast } = useToast();
  const showSyncFeedback = useSyncFeedback();

  const canUpdate = usePermission('routing-rule:update');
  const canDelete = usePermission('routing-rule:delete');
  const canSimulate = usePermission('routing-rule:simulate');

  const { data: rule, loading, error, refetch } = useApi<RoutingRule>(
    () => routingApi.get(id!),
    ['admin', 'routing-rules', 'detail', id],
  );

  // Fetch providers + models for tree selector
  const { data: providerModelsData } = useApi<{ data: AdminModelsByProvider[] }>(
    () => systemApi.listModels({ includeEmptyProviders: 'true' }) as Promise<{ data: AdminModelsByProvider[] }>,
    ['admin', 'models', 'grouped', 'include-empty'],
  );
  const providerGroups = providerModelsData?.data ?? EMPTY_PROVIDER_GROUPS;

  const viewConfig = useMemo(() => {
    if (!rule) {
      return parseRoutingConfigForForm('single', {}, providerGroups);
    }
    return parseRoutingConfigForForm(mapLegacyStrategy(rule.strategyType), rule.config, providerGroups);
  }, [rule, providerGroups]);

  const viewSmartParsed = useMemo(() => {
    if (!rule || mapLegacyStrategy(rule.strategyType) !== 'smart') return null;
    return parseSmartConfig(rule.config, providerGroups);
  }, [rule, providerGroups]);

  // Form instance for basic edit fields
  const editForm = useZodForm<RoutingEditValues>({
    schema: routingEditSchema,
    defaultValues: {
      editName: '',
      editDescription: '',
      editPriority: 0,
      editEnabled: true,
      editPipelineStage: '1',
      editStrategyType: 'single',
      policyAllowM: [] as string[],
      policyDenyM: [] as string[],
      policyAllowP: [] as string[],
      policyDenyP: [] as string[],
      singleProvider: '',
      singleModel: '',
      matchProjectIds: [] as string[],
    },
  });

  useUnsavedChangesWarning(editForm.formState.isDirty);

  // Complex state that stays as useState
  const [entries, setEntries] = useState<ProviderModelEntry[]>([{ provider: '', model: '', weight: '50' }]);
  const [models, setModels] = useState<string[]>([]);
  const [matchProviders, setMatchProviders] = useState<string[]>([]);
  const [matchRequestedModelLiterals, setMatchRequestedModelLiterals] = useState<string[]>([]);
  const [matchModelTypes, setMatchModelTypes] = useState<string[]>([]);
  const [matchVirtualKeys, setMatchVirtualKeys] = useState<string[]>([]);
  const [smartState, setSmartState] = useState<SmartFormState>(EMPTY_SMART_FORM_STATE);
  const updateSmart = (patch: Partial<SmartFormState>) => setSmartState((prev) => ({ ...prev, ...patch }));

  // Inline fallback chain
  const [fallbackEntries, setFallbackEntries] = useState<FallbackEntry[]>([]);
  const addFallback = () => setFallbackEntries(prev => [...prev, { provider: '', model: '' }]);
  const removeFallback = (idx: number) => setFallbackEntries(prev => prev.filter((_, i) => i !== idx));
  const updateFallback = (idx: number, field: keyof FallbackEntry, value: string) =>
    setFallbackEntries(prev => prev.map((e, i) => (i === idx ? { ...e, [field]: value } : e)));

  const [conditionalUi, setConditionalUi] = useState<ConditionalEditorHydration>(() => ({
    mode: 'form',
    form: emptyConditionalFormState(),
  }));

  // Retry policy form state
  const [retryPolicyMode, setRetryPolicyMode] = useState<RetryPolicyMode>('default');
  const [retryMaxAttempts, setRetryMaxAttempts] = useState<string>('3');
  const [retryOn, setRetryOn] = useState<ErrorClass[]>(['network', 'timeout', '5xx']);

  const [simModelId, setSimModelId] = useState('');
  const [simLoading, setSimLoading] = useState(false);
  const [simData, setSimData] = useState<RoutingSimulateResponse | { error: string } | null>(null);

  useEffect(() => {
    if (!rule) return;
    setSimData(null);

    // The Routing preview's Model ID input mirrors what a real client would
    // send in `{model: "..."}`. The simulate API resolves that string against
    // Model.code (a stable customer-facing identifier such as "gpt-4o"), or
    // accepts the literal "auto" to trigger smart routing. matchConditions
    // however stores Model.id (UUID) — using it raw here would never match.
    if (mapLegacyStrategy(rule.strategyType) === 'smart') {
      setSimModelId('auto');
      return;
    }
    const mc = parseMatchConditionsForm(rule.matchConditions);
    const firstId = mc.models[0];
    if (!firstId) {
      setSimModelId('');
      return;
    }
    let code = '';
    for (const group of providerGroups) {
      const found = group.models.find((m) => m.id === firstId);
      if (found) {
        code = found.code;
        break;
      }
    }
    setSimModelId(code);
  }, [rule?.id, rule?.strategyType, rule?.matchConditions, providerGroups]);

  const { mutate: saveRule, loading: saveLoading } = useMutation(
    (data: RoutingRuleUpdatePayload) => routingApi.update(id!, data),
    {
      onSuccess: () => { showSyncFeedback('ai-gateway'); setIsEditing(false); refetch(); },
      successMessage: t('pages:routing.ruleUpdated'),
    },
  );

  const { mutate: deleteRule, loading: deleteLoading } = useMutation(
    () => routingApi.delete(id!),
    {
      onSuccess: () => navigate('/ai-gateway/routing'),
      successMessage: t('pages:routing.ruleDeleted'),
    },
  );

  const startEditing = () => {
    if (!rule) return;
    const stage = rule.pipelineStage ?? 1;
    const stageStr = String(stage);
    if (stage === 0 || rule.strategyType === 'policy') {
      const lines = policyConfigToFormLines(rule.config);
      editForm.reset({
        editName: rule.name,
        editDescription: rule.description ?? '',
        editPriority: rule.priority,
        editEnabled: rule.enabled,
        editPipelineStage: stageStr,
        editStrategyType: 'single',
        policyAllowM: lines.allowM,
        policyDenyM: lines.denyM,
        policyAllowP: lines.allowP,
        policyDenyP: lines.denyP,
        singleProvider: '',
        singleModel: '',
        matchProjectIds: [] as string[],
      });
      setConditionalUi({ mode: 'form', form: emptyConditionalFormState() });
      setEntries([{ provider: '', model: '', weight: '50' }]);
      setSmartState({ ...EMPTY_SMART_FORM_STATE });
    } else {
      const mapped = mapLegacyStrategy(rule.strategyType);
      if (mapped === 'conditional') {
        setConditionalUi(hydrateConditionalEditorState(rule.config, providerGroups));
      } else {
        setConditionalUi({ mode: 'form', form: emptyConditionalFormState() });
      }
      const parsed = parseRoutingConfigForForm(mapped, rule.config, providerGroups);
      editForm.reset({
        editName: rule.name,
        editDescription: rule.description ?? '',
        editPriority: rule.priority,
        editEnabled: rule.enabled,
        editPipelineStage: stageStr,
        editStrategyType: mapped,
        policyAllowM: [],
        policyDenyM: [],
        policyAllowP: [],
        policyDenyP: [],
        singleProvider: parsed.singleProvider,
        singleModel: parsed.singleModel,
        matchProjectIds: [] as string[],
      });
      setEntries(parsed.entries.length > 0 ? parsed.entries : [{ provider: '', model: '', weight: '50' }]);
      if (mapped === 'smart') {
        setSmartState(parseSmartConfig(rule.config, providerGroups));
      } else {
        setSmartState({ ...EMPTY_SMART_FORM_STATE });
      }
    }
    // Hydrate fallback chain
    if (rule.fallbackChain) {
      setFallbackEntries(parseFallbackChain(rule.fallbackChain, providerGroups));
    } else {
      setFallbackEntries([]);
    }
    const mc = parseMatchConditionsForm(rule.matchConditions);
    setModels(mc.models);
    setMatchProviders(mc.providers);
    setMatchRequestedModelLiterals(mc.requestedModelLiterals);
    setMatchModelTypes(mc.modelTypes);
    setMatchVirtualKeys(mc.virtualKeys);
    editForm.setValue('matchProjectIds', mc.projects);
    const rp = deriveRetryPolicyInitialState(rule.retryPolicy);
    setRetryPolicyMode(rp.mode);
    setRetryMaxAttempts(rp.maxAttempts);
    setRetryOn(rp.retryOn);
    setIsEditing(true);
  };

  const updateEntry = (index: number, field: keyof ProviderModelEntry, value: string) => {
    setEntries((prev) => prev.map((e, i) => (i === index ? { ...e, [field]: value } : e)));
  };
  const addEntry = () => setEntries((prev) => [...prev, { provider: '', model: '', weight: '50' }]);
  const removeEntry = (index: number) => setEntries((prev) => prev.filter((_, i) => i !== index));

  const handleSave = () => {
    const v = editForm.getValues();
    const editStrategyType = v.editStrategyType as StrategyType;
    const stageNum = v.editPipelineStage === '0' ? 0 : 1;

    // Resolve retry policy first so we can short-circuit on validation error.
    const rpBuilt = buildRetryPolicyPayload(retryPolicyMode, retryMaxAttempts, retryOn);
    if (!rpBuilt.ok) {
      addToast(rpBuilt.error, 'error');
      return;
    }
    // PUT semantics: explicit `null` clears the override (rule re-inherits
    // the YAML default); a present object persists verbatim.
    const retryPolicyPatch: Pick<RoutingRuleUpdatePayload, 'retryPolicy'> =
      rpBuilt.mode === 'default'
        ? { retryPolicy: null }
        : { retryPolicy: rpBuilt.value };

    if (stageNum === 0) {
      const built = buildPolicyApiConfig(v.policyAllowM, v.policyDenyM, v.policyAllowP, v.policyDenyP);
      if (!built.ok) {
        addToast(built.message, 'error');
        return;
      }
      const matchConditions = buildMatchConditionsPayload({
        models,
        requestedModelLiterals: matchRequestedModelLiterals,
        modelTypes: matchModelTypes,
        providers: matchProviders,
        projects: v.matchProjectIds ?? [],
        virtualKeys: matchVirtualKeys,
      });
      saveRule({
        name: v.editName,
        description: v.editDescription,
        strategyType: 'policy',
        priority: Number(v.editPriority),
        enabled: v.editEnabled,
        pipelineStage: 0,
        config: built.config,
        matchConditions,
        ...retryPolicyPatch,
      });
      return;
    }

    if (editStrategyType === 'ab_split' || editStrategyType === 'loadbalance') {
      const weightCheck = validateSplitWeights(entries);
      if (!weightCheck.valid) {
        addToast(t('pages:routing.weightSumError', { total: weightCheck.total }), 'error');
        return;
      }
    }

    const built =
      editStrategyType === 'conditional'
        ? resolveConditionalConfigFromEditor(conditionalUi, providerGroups)
        : editStrategyType === 'smart'
          ? buildSmartConfig(smartState, providerGroups)
          : buildRoutingApiConfig({
              strategyType: editStrategyType,
              providerGroups,
              singleProvider: v.singleProvider,
              singleModel: v.singleModel,
              entries,
              matchModelIds: models,
              preservedConditionalConfig: null,
            });
    if (!built.ok) {
      addToast(built.message, 'error');
      return;
    }
    const matchConditions = buildMatchConditionsPayload({
      models,
      requestedModelLiterals: matchRequestedModelLiterals,
      modelTypes: matchModelTypes,
      providers: matchProviders,
      projects: v.matchProjectIds ?? [],
      virtualKeys: matchVirtualKeys,
    });
    const fallbackChainApi = buildFallbackChainApi(fallbackEntries, providerGroups);
    saveRule({
      name: v.editName,
      description: v.editDescription,
      strategyType: editStrategyType,
      priority: Number(v.editPriority),
      enabled: v.editEnabled,
      pipelineStage: 1,
      config: built.config,
      matchConditions,
      ...(fallbackChainApi.length > 0 ? { fallbackChain: fallbackChainApi } : { fallbackChain: null }),
      ...retryPolicyPatch,
    });
  };

  const retryPolicyInvalid = isRetryPolicyMaxAttemptsInvalid(retryPolicyMode, retryMaxAttempts);

  const editStrategyType = editForm.watch('editStrategyType') as StrategyType;
  const editPipelineStage = editForm.watch('editPipelineStage');
  const singleProvider = editForm.watch('singleProvider');
  const singleModel = editForm.watch('singleModel');

  const showWeightColumn = editStrategyType === 'loadbalance' || editStrategyType === 'ab_split';

  const configModelIds = useMemo(() => configuredInternalModelIds(
    providerGroups,
    editPipelineStage === '0' ? 'policy' : editStrategyType,
    singleProvider,
    singleModel,
    entries,
    editStrategyType === 'conditional' && conditionalUi.mode === 'form' ? conditionalUi.form : null,
  ), [providerGroups, editPipelineStage, editStrategyType, singleProvider, singleModel, entries, conditionalUi]);

  const viewMc = useMemo(() => {
    if (!rule) return { models: [] as string[], providers: [] as string[], projects: [] as string[] };
    return parseMatchConditionsForm(rule.matchConditions);
  }, [rule]);

  const runSimulation = async () => {
    setSimLoading(true);
    try {
      const body: RoutingSimulateRequest = {
        modelId: simModelId.trim(),
        endpointType: 'chat',
      };
      if (simModelId.trim() === 'auto') {
        body.messages = [{ role: 'user', content: 'Hello' }];
      }
      const payload = await routingApi.simulate(body);
      setSimData(payload);
    } catch {
      setSimData({ error: 'Simulation request failed' });
    } finally {
      setSimLoading(false);
    }
  };

  return {
    // Route params
    id,
    navigate,

    // Data
    rule,
    loading,
    error,
    refetch,
    providerGroups,
    viewConfig,
    viewSmartParsed,
    viewMc,

    // Permissions
    canUpdate,
    canDelete,
    canSimulate,

    // Edit toggle
    isEditing,
    setIsEditing,
    startEditing,

    // Delete
    deleting,
    setDeleting,
    deleteRule,
    deleteLoading,

    // Form instance
    editForm,

    // Strategy fields (complex state)
    entries,
    updateEntry,
    addEntry,
    removeEntry,
    showWeightColumn,

    // Match conditions
    models,
    setModels,
    matchProviders,
    setMatchProviders,
    matchRequestedModelLiterals,
    setMatchRequestedModelLiterals,
    matchModelTypes,
    setMatchModelTypes,
    matchVirtualKeys,
    setMatchVirtualKeys,
    configModelIds,

    // Smart routing
    smartState,
    updateSmart,

    // Fallback chain
    fallbackEntries,
    addFallback,
    removeFallback,
    updateFallback,

    // Conditional
    conditionalUi,
    setConditionalUi,

    // Retry policy
    retryPolicyMode,
    setRetryPolicyMode,
    retryMaxAttempts,
    setRetryMaxAttempts,
    retryOn,
    setRetryOn,
    retryPolicyInvalid,

    // Save
    handleSave,
    saveLoading,

    // Simulation
    simModelId,
    setSimModelId,
    simLoading,
    simData,
    runSimulation,
  };
}

export type RoutingRuleDetailState = ReturnType<typeof useRoutingRuleDetail>;
