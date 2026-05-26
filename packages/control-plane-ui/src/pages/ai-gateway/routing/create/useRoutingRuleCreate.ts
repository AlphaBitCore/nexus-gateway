import { useState } from 'react';
import { useTranslation } from 'react-i18next';
import { useNavigate } from 'react-router-dom';
import { routingApi, systemApi } from '@/api/services';
import type { RoutingRuleWritePayload } from '@/api/services';
import { useApi } from '@/hooks/useApi';
import { useMutation } from '@/hooks/useMutation';
import { useSyncFeedback } from '@/hooks/useSyncFeedback';
import type { AdminModelsByProvider } from '@/api/types';
import { useToast } from '@/context/ToastContext';
import {
  buildPolicyApiConfig,
  buildRoutingApiConfig,
  buildSmartConfig,
  buildFallbackChainApi,
  configuredInternalModelIds,
  emptyConditionalFormState,
  hydrateConditionalEditorState,
  resolveConditionalConfigFromEditor,
  DEFAULT_SMART_SYSTEM_PROMPT,
  buildMatchConditionsPayload,
  type ConditionalEditorHydration,
  type StrategyType,
  type ProviderModelEntry,
  type SmartFormState,
  type FallbackEntry,
} from '../_shared/routing-rule-config';

const EMPTY_PROVIDER_GROUPS: AdminModelsByProvider[] = [];

export function useRoutingRuleCreate() {
  const { t } = useTranslation();
  const navigate = useNavigate();
  const { addToast } = useToast();
  const showSyncFeedback = useSyncFeedback();

  // ── Basic fields ──────────────────────────────────────────────────
  const [name, setName] = useState('');
  const [description, setDescription] = useState('');
  const [pipelineStage, setPipelineStage] = useState('1');
  const [policyAllowM, setPolicyAllowM] = useState<string[]>([]);
  const [policyDenyM, setPolicyDenyM] = useState<string[]>([]);
  const [policyAllowP, setPolicyAllowP] = useState<string[]>([]);
  const [policyDenyP, setPolicyDenyP] = useState<string[]>([]);
  const [strategyType, setStrategyType] = useState<StrategyType>('single');
  const [priority, setPriority] = useState('0');
  const [enabled, setEnabled] = useState(true);
  const [singleProvider, setSingleProvider] = useState('');
  const [singleModel, setSingleModel] = useState('');
  const [entries, setEntries] = useState<ProviderModelEntry[]>([{ provider: '', model: '', weight: '50' }]);
  const [models, setModels] = useState<string[]>([]);
  const [matchProviders, setMatchProviders] = useState<string[]>([]);
  const [matchProjectIds, setMatchProjectIds] = useState<string[]>([]);
  const [matchRequestedModelLiterals, setMatchRequestedModelLiterals] = useState<string[]>([]);
  const [matchModelTypes, setMatchModelTypes] = useState<string[]>([]);
  const [matchVirtualKeys, setMatchVirtualKeys] = useState<string[]>([]);

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
  const updateSmart = (patch: Partial<SmartFormState>) => setSmartState((prev) => ({ ...prev, ...patch }));

  // ── Inline fallback chain ─────────────────────────────────────────
  const [fallbackEntries, setFallbackEntries] = useState<FallbackEntry[]>([]);
  const addFallback = () => setFallbackEntries((prev) => [...prev, { provider: '', model: '' }]);
  const removeFallback = (idx: number) => setFallbackEntries((prev) => prev.filter((_, i) => i !== idx));
  const updateFallback = (idx: number, field: keyof FallbackEntry, value: string) =>
    setFallbackEntries((prev) => prev.map((e, i) => (i === idx ? { ...e, [field]: value } : e)));

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

  // ── Mutation ──────────────────────────────────────────────────────
  const { mutate, loading } = useMutation(
    (data: RoutingRuleWritePayload) => routingApi.create(data),
    {
      onSuccess: (res) => {
        showSyncFeedback('ai-gateway');
        navigate(res?.id ? `/ai-gateway/routing/${res.id}` : '/ai-gateway/routing');
      },
      successMessage: t('pages:routing.ruleCreated'),
    },
  );

  // ── Entry helpers ─────────────────────────────────────────────────
  const updateEntry = (index: number, field: keyof ProviderModelEntry, value: string) => {
    setEntries((prev) => prev.map((e, i) => (i === index ? { ...e, [field]: value } : e)));
  };
  const addEntry = () => setEntries((prev) => [...prev, { provider: '', model: '', weight: '50' }]);
  const removeEntry = (index: number) => setEntries((prev) => prev.filter((_, i) => i !== index));

  // ── Strategy change handler ───────────────────────────────────────
  const handleStrategyChange = (next: StrategyType) => {
    if (strategyType !== 'conditional' && next === 'conditional') {
      setConditionalUi(hydrateConditionalEditorState(null, providerGroups));
    }
    setStrategyType(next);
  };

  // ── Submit handler ────────────────────────────────────────────────
  const handleSubmit = () => {
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
      ...(fallbackChainApi.length > 0 ? { fallbackChain: fallbackChainApi } : {}),
    });
  };

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
    pipelineStage, setPipelineStage,
    strategyType, handleStrategyChange,
    priority, setPriority,
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

    // Provider data
    providerGroups,

    // Derived
    showWeightColumn,
    configModelIds,

    // Actions
    handleSubmit,
    loading,
    navigate,
  };
}
