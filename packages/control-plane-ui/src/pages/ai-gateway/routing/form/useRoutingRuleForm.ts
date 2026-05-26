import { useState, useEffect } from 'react';
import { useTranslation } from 'react-i18next';
import { useMutation } from '@/hooks/useMutation';
import { useApi } from '@/hooks/useApi';
import { routingApi, systemApi } from '@/api/services';
import type { RoutingRuleWritePayload } from '@/api/services';
import type { ErrorClass, RoutingRule, AdminModelsByProvider } from '@/api/types';
import { useToast } from '@/context/ToastContext';
import {
  buildRetryPolicyPayload,
  deriveRetryPolicyInitialState,
  isRetryPolicyMaxAttemptsInvalid,
  type RetryPolicyMode,
} from './RetryPolicySection';
import {
  mapLegacyStrategy,
  parseRoutingConfigForForm,
  buildRoutingApiConfig,
  buildPolicyApiConfig,
  policyConfigToFormLines,
  configuredInternalModelIds,
  emptyConditionalFormState,
  hydrateConditionalEditorState,
  parseSmartConfig,
  buildSmartConfig,
  parseFallbackChain,
  buildFallbackChainApi,
  resolveConditionalConfigFromEditor,
  DEFAULT_SMART_SYSTEM_PROMPT,
  parseMatchConditionsForm,
  buildMatchConditionsPayload,
  type ConditionalEditorHydration,
  type StrategyType,
  type ProviderModelEntry,
  type SmartFormState,
  type FallbackEntry,
} from '../_shared/routing-rule-config';

const EMPTY_PROVIDER_GROUPS: AdminModelsByProvider[] = [];

export interface UseRoutingRuleFormOptions {
  rule?: RoutingRule;
  onClose: () => void;
  onSaved: () => void;
}

export function useRoutingRuleForm({ rule, onClose, onSaved }: UseRoutingRuleFormOptions) {
  const { t } = useTranslation();
  const { addToast } = useToast();

  // ── Basic fields ──────────────────────────────────────────────────
  const [name, setName] = useState(rule?.name ?? '');
  const [description, setDescription] = useState(rule?.description ?? '');
  const [strategyType, setStrategyType] = useState<StrategyType>(
    mapLegacyStrategy(rule?.strategyType ?? 'single'),
  );
  const [priority, setPriority] = useState(String(rule?.priority ?? 0));
  const [pipelineStage, setPipelineStage] = useState(String(rule?.pipelineStage ?? 1));
  const [enabled, setEnabled] = useState(rule?.enabled ?? true);

  // ── Policy fields ─────────────────────────────────────────────────
  const [policyAllowM, setPolicyAllowM] = useState<string[]>([]);
  const [policyDenyM, setPolicyDenyM] = useState<string[]>([]);
  const [policyAllowP, setPolicyAllowP] = useState<string[]>([]);
  const [policyDenyP, setPolicyDenyP] = useState<string[]>([]);

  // ── Single-provider fields ────────────────────────────────────────
  const [singleProvider, setSingleProvider] = useState('');
  const [singleModel, setSingleModel] = useState('');

  // ── Multi-entry fields (fallback/loadbalance/ab_split) ────────────
  const [entries, setEntries] = useState<ProviderModelEntry[]>([
    { provider: '', model: '', weight: '50' },
  ]);

  // ── Smart strategy state ──────────────────────────────────────────
  const [smartState, setSmartState] = useState<SmartFormState>({
    routerProvider: '',
    routerModel: '',
    systemPrompt: DEFAULT_SMART_SYSTEM_PROMPT,
    temperature: '0',
    maxTokens: '1024',
    timeoutMs: '10000',
    defaultProvider: '',
    defaultModel: '',
  });
  const updateSmart = (patch: Partial<SmartFormState>) =>
    setSmartState(prev => ({ ...prev, ...patch }));

  // ── Inline fallback chain ─────────────────────────────────────────
  const [fallbackEntries, setFallbackEntries] = useState<FallbackEntry[]>([]);
  const addFallback = () =>
    setFallbackEntries(prev => [...prev, { provider: '', model: '' }]);
  const removeFallback = (idx: number) =>
    setFallbackEntries(prev => prev.filter((_, i) => i !== idx));
  const updateFallback = (idx: number, field: keyof FallbackEntry, value: string) =>
    setFallbackEntries(prev =>
      prev.map((e, i) => (i === idx ? { ...e, [field]: value } : e)),
    );

  // ── Retry policy ──────────────────────────────────────────────────
  const rpInit = deriveRetryPolicyInitialState(rule?.retryPolicy);
  const [retryPolicyMode, setRetryPolicyMode] = useState<RetryPolicyMode>(rpInit.mode);
  const [retryMaxAttempts, setRetryMaxAttempts] = useState<string>(rpInit.maxAttempts);
  const [retryOn, setRetryOn] = useState<ErrorClass[]>(rpInit.retryOn);

  // ── Match conditions ──────────────────────────────────────────────
  const mcInit = parseMatchConditionsForm(rule?.matchConditions);
  const [models, setModels] = useState(mcInit.models);
  const [matchProviders, setMatchProviders] = useState(mcInit.providers);
  const [matchProjectIds, setMatchProjectIds] = useState<string[]>(mcInit.projects);
  const [matchRequestedModelLiterals, setMatchRequestedModelLiterals] = useState<string[]>(
    mcInit.requestedModelLiterals,
  );
  const [matchModelTypes, setMatchModelTypes] = useState<string[]>(mcInit.modelTypes);
  const [matchVirtualKeys, setMatchVirtualKeys] = useState<string[]>(mcInit.virtualKeys);

  // ── Conditional routing ───────────────────────────────────────────
  const [conditionalUi, setConditionalUi] = useState<ConditionalEditorHydration>(() => ({
    mode: 'form',
    form: emptyConditionalFormState(),
  }));

  // ── Fetch providers + models ──────────────────────────────────────
  const { data: providerModelsData } = useApi<{ data: AdminModelsByProvider[] }>(
    () => systemApi.listModels({ includeEmptyProviders: 'true' }) as Promise<{ data: AdminModelsByProvider[] }>,
    ['admin', 'models', 'grouped', 'include-empty'],
  );
  const providerGroups = providerModelsData?.data ?? EMPTY_PROVIDER_GROUPS;

  // ── Hydrate form from existing rule ───────────────────────────────
  useEffect(() => {
    if (!rule || providerGroups.length === 0) return;
    const stage = rule.pipelineStage ?? 1;
    setPipelineStage(String(stage));
    if (stage === 0 || rule.strategyType === 'policy') {
      const lines = policyConfigToFormLines(rule.config);
      setPolicyAllowM(lines.allowM);
      setPolicyDenyM(lines.denyM);
      setPolicyAllowP(lines.allowP);
      setPolicyDenyP(lines.denyP);
      setStrategyType('single');
      setConditionalUi({ mode: 'form', form: emptyConditionalFormState() });
      const p = parseRoutingConfigForForm('policy', rule.config, providerGroups);
      setSingleProvider(p.singleProvider);
      setSingleModel(p.singleModel);
      setEntries(p.entries.length > 0 ? p.entries : [{ provider: '', model: '', weight: '50' }]);
    } else {
      setPolicyAllowM([]);
      setPolicyDenyM([]);
      setPolicyAllowP([]);
      setPolicyDenyP([]);
      const mapped = mapLegacyStrategy(rule.strategyType);
      const p = parseRoutingConfigForForm(mapped, rule.config, providerGroups);
      setStrategyType(mapped);
      if (mapped === 'conditional') {
        setConditionalUi(hydrateConditionalEditorState(rule.config, providerGroups));
      } else {
        setConditionalUi({ mode: 'form', form: emptyConditionalFormState() });
      }
      setSingleProvider(p.singleProvider);
      setSingleModel(p.singleModel);
      setEntries(p.entries.length > 0 ? p.entries : [{ provider: '', model: '', weight: '50' }]);
      if (mapped === 'smart') {
        setSmartState(parseSmartConfig(rule.config, providerGroups));
      }
    }
    if (rule.fallbackChain) {
      setFallbackEntries(parseFallbackChain(rule.fallbackChain, providerGroups));
    }
    const mc = parseMatchConditionsForm(rule.matchConditions);
    setModels(mc.models);
    setMatchProviders(mc.providers);
    setMatchProjectIds(mc.projects);
    setMatchRequestedModelLiterals(mc.requestedModelLiterals);
    setMatchModelTypes(mc.modelTypes);
    setMatchVirtualKeys(mc.virtualKeys);
    const rp = deriveRetryPolicyInitialState(rule.retryPolicy);
    setRetryPolicyMode(rp.mode);
    setRetryMaxAttempts(rp.maxAttempts);
    setRetryOn(rp.retryOn);
  }, [rule?.id, providerGroups]);

  // ── Mutation ──────────────────────────────────────────────────────
  const { mutate, loading } = useMutation(
    (data: RoutingRuleWritePayload) =>
      rule ? routingApi.update(rule.id, data) : routingApi.create(data),
    {
      onSuccess: () => { onSaved(); onClose(); },
      successMessage: rule ? t('pages:routing.ruleUpdated') : t('pages:routing.ruleCreated'),
    },
  );

  // ── Entry helpers ─────────────────────────────────────────────────
  const updateEntry = (index: number, field: keyof ProviderModelEntry, value: string) => {
    setEntries(prev => prev.map((e, i) => (i === index ? { ...e, [field]: value } : e)));
  };
  const addEntry = () =>
    setEntries(prev => [...prev, { provider: '', model: '', weight: '50' }]);
  const removeEntry = (index: number) =>
    setEntries(prev => prev.filter((_, i) => i !== index));

  // ── Strategy change handler ───────────────────────────────────────
  const handleStrategyChange = (next: StrategyType) => {
    if (strategyType !== 'conditional' && next === 'conditional') {
      setConditionalUi(hydrateConditionalEditorState(null, providerGroups));
    }
    setStrategyType(next);
  };

  // ── Submit handler ────────────────────────────────────────────────
  /**
   * Build the wire value for `retryPolicy`.
   *   create (POST): omit field for default mode → backend leaves column NULL;
   *   update (PUT):  send `null` for default mode → backend clears the column
   *                  (rule re-inherits YAML default).
   */
  const resolveRetryPolicyForPayload = ():
    | { include: false }
    | { include: true; value: import('@/api/types').RetryPolicy | null } => {
    const built = buildRetryPolicyPayload(retryPolicyMode, retryMaxAttempts, retryOn);
    if (!built.ok) {
      addToast(built.error, 'error');
      return { include: true, value: null }; // sentinel; caller short-circuits below
    }
    if (built.mode === 'default') {
      return rule ? { include: true, value: null } : { include: false };
    }
    return { include: true, value: built.value };
  };

  const handleSubmit = () => {
    // Validate retry policy form first so we can short-circuit cleanly.
    const rpBuilt = buildRetryPolicyPayload(retryPolicyMode, retryMaxAttempts, retryOn);
    if (!rpBuilt.ok) {
      addToast(rpBuilt.error, 'error');
      return;
    }
    const rpResolved = resolveRetryPolicyForPayload();
    const retryPolicyPatch: Partial<RoutingRuleWritePayload> = rpResolved.include
      ? { retryPolicy: rpResolved.value }
      : {};

    const stageNum = pipelineStage === '0' ? 0 : 1;
    if (stageNum === 0) {
      const built = buildPolicyApiConfig(policyAllowM, policyDenyM, policyAllowP, policyDenyP);
      if (!built.ok) {
        addToast(built.message, 'error');
        return;
      }
      const matchConditions = buildMatchConditionsPayload({
        models,
        requestedModelLiterals: matchRequestedModelLiterals,
        modelTypes: matchModelTypes,
        providers: matchProviders,
        projects: matchProjectIds,
        virtualKeys: matchVirtualKeys,
      });
      mutate({
        name,
        description,
        strategyType: 'policy',
        priority: Number(priority),
        enabled,
        pipelineStage: 0,
        config: built.config,
        matchConditions,
        ...retryPolicyPatch,
      });
      return;
    }

    const built =
      strategyType === 'conditional'
        ? resolveConditionalConfigFromEditor(conditionalUi, providerGroups)
        : strategyType === 'smart'
          ? buildSmartConfig(smartState, providerGroups)
          : buildRoutingApiConfig({
              strategyType,
              providerGroups,
              singleProvider,
              singleModel,
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
      projects: matchProjectIds,
      virtualKeys: matchVirtualKeys,
    });

    const fallbackChainApi = buildFallbackChainApi(fallbackEntries, providerGroups);
    mutate({
      name,
      description,
      strategyType,
      priority: Number(priority),
      enabled,
      pipelineStage: 1,
      config: built.config,
      matchConditions,
      ...(fallbackChainApi.length > 0 ? { fallbackChain: fallbackChainApi } : { fallbackChain: null }),
      ...retryPolicyPatch,
    });
  };

  const retryPolicyInvalid = isRetryPolicyMaxAttemptsInvalid(retryPolicyMode, retryMaxAttempts);

  // ── Derived values ────────────────────────────────────────────────
  const showWeightColumn = strategyType === 'loadbalance' || strategyType === 'ab_split';

  const configModelIds = configuredInternalModelIds(
    providerGroups,
    pipelineStage === '0' ? 'policy' : strategyType,
    singleProvider,
    singleModel,
    entries,
    strategyType === 'conditional' && conditionalUi.mode === 'form' ? conditionalUi.form : null,
  );

  return {
    // Basic fields
    name, setName,
    description, setDescription,
    strategyType, handleStrategyChange,
    priority, setPriority,
    pipelineStage,
    enabled, setEnabled,

    // Policy fields
    policyAllowM, setPolicyAllowM,
    policyDenyM, setPolicyDenyM,
    policyAllowP, setPolicyAllowP,
    policyDenyP, setPolicyDenyP,

    // Single-provider
    singleProvider, setSingleProvider,
    singleModel, setSingleModel,

    // Multi-entry
    entries, updateEntry, addEntry, removeEntry,

    // Smart
    smartState, updateSmart,

    // Fallback chain
    fallbackEntries, addFallback, removeFallback, updateFallback,

    // Match conditions
    models, setModels,
    matchProviders, setMatchProviders,
    matchProjectIds, setMatchProjectIds,
    matchRequestedModelLiterals, setMatchRequestedModelLiterals,
    matchModelTypes, setMatchModelTypes,
    matchVirtualKeys, setMatchVirtualKeys,

    // Conditional
    conditionalUi, setConditionalUi,

    // Retry policy
    retryPolicyMode, setRetryPolicyMode,
    retryMaxAttempts, setRetryMaxAttempts,
    retryOn, setRetryOn,
    retryPolicyInvalid,

    // Provider data
    providerGroups,

    // Derived
    showWeightColumn,
    configModelIds,

    // Actions
    handleSubmit,
    loading,
  };
}
