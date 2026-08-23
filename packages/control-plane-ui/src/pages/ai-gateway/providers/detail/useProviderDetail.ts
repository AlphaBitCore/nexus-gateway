import { useState } from 'react';
import { useParams, useNavigate } from 'react-router-dom';
import { useTranslation } from 'react-i18next';
import { useQueryClient } from '@tanstack/react-query';
import { providerApi, credentialApi, systemApi } from '@/api/services';
import type {
  CreateCredentialInput,
  UpdateCredentialInput,
  UpdateProviderInput,
} from '@/api/services';
import type { CreateModelInput, UpdateModelInput } from '@/api/types';
import { useApi } from '@/hooks/useApi';
import { useMutation } from '@/hooks/useMutation';
import { useSyncFeedback } from '@/hooks/useSyncFeedback';
import { usePermission } from '@/hooks/usePermission';
import { useZodForm } from '@/lib/forms';
import { useUnsavedChangesWarning } from '@/hooks/useUnsavedChangesWarning';
import {
  providerEditSchema,
  newCredentialSchema,
  editCredentialSchema,
  newModelSchema,
  editModelSchema,
} from './providerDetailSchemas';
import type {
  ProviderEditValues,
  NewCredentialValues,
  EditCredentialValues,
  NewModelValues,
  EditModelValues,
} from './providerDetailSchemas';
import type { Provider, Credential, Model, ProviderHealth, ModelCapabilityJson } from '@/api/types';

/* ── Helpers ─────────────────────────────────────────────────────────── */

export { formatDateTime as fmtDate } from '@/lib/format';

export type Tab = 'info' | 'credentials' | 'models' | 'health' | 'usage' | 'cache';

/* ── Analytics types ─────────────────────────────────────────────────── */

export interface ProviderAnalytics {
  summary: {
    totalRequests: number;
    errorCount: number;
    errorRate: number;
    avgLatencyMs: number;
    totalTokens: number;
    totalPromptTokens: number;
    totalCompletionTokens: number;
    totalEstimatedCostUsd: number;
    cacheHitCount: number;
    cacheHitRate: number;
  };
  byModel: Array<{
    model: string;
    requestCount: number;
    avgLatencyMs: number;
    totalTokens: number;
    promptTokens: number;
    completionTokens: number;
    estimatedCostUsd: number;
  }>;
  byProject?: Array<{
    projectId: string;
    projectName: string | null;
    projectCode: string | null;
    requestCount: number;
    avgLatencyMs: number;
    totalTokens: number;
    promptTokens: number;
    completionTokens: number;
    estimatedCostUsd: number;
  }>;
  byVirtualKey?: Array<{
    virtualKeyId: string;
    name: string | null;
    keyPrefix: string | null;
    requestCount: number;
    avgLatencyMs: number;
    totalTokens: number;
    promptTokens: number;
    completionTokens: number;
    estimatedCostUsd: number;
  }>;
  daily: Array<{
    date: string;
    requests: number;
    errors: number;
    totalTokens: number;
    estimatedCostUsd: number;
  }>;
  byStatus: Array<{
    statusCode: number;
    count: number;
  }>;
}

export {
  providerEditSchema,
  newCredentialSchema,
  editCredentialSchema,
  newModelSchema,
  editModelSchema,
} from './providerDetailSchemas';
export type {
  ProviderEditValues,
  NewCredentialValues,
  EditCredentialValues,
  NewModelValues,
  EditModelValues,
} from './providerDetailSchemas';

/* ── Hook ────────────────────────────────────────────────────────────── */

export function useProviderDetail() {
  const { id } = useParams<{ id: string }>();
  const navigate = useNavigate();
  const { t } = useTranslation('pages');
  const queryClient = useQueryClient();
  const [activeTab, setActiveTab] = useState<Tab>('info');
  const [isEditing, setIsEditing] = useState(false);
  const [deleting, setDeleting] = useState(false);
  const showSyncFeedback = useSyncFeedback();

  // ── Form instances ──
  const providerForm = useZodForm<ProviderEditValues>({
    schema: providerEditSchema,
    defaultValues: { name: '', displayName: '', description: '', baseUrl: '', adapterType: '', region: '', apiVersion: '', enabled: true },
  });

  const newCredForm = useZodForm<NewCredentialValues>({
    schema: newCredentialSchema,
    defaultValues: { credName: '', credApiKey: '', newCredEnabled: true, credExpiresAt: '' },
  });

  const editCredForm = useZodForm<EditCredentialValues>({
    schema: editCredentialSchema,
    defaultValues: { editCredName: '', editCredApiKey: '', editCredEnabled: true, editCredExpiresAt: '' },
  });

  const newModelForm = useZodForm<NewModelValues>({
    schema: newModelSchema,
    defaultValues: {
      modelName: '', modelProviderModelId: '', modelCode: '', modelType: 'chat',
      modelDescription: '', modelInputPrice: '', modelOutputPrice: '',
      modelCachedInputReadPrice: '', modelCachedInputWritePrice: '',
      modelAudioInputPrice: '', modelAudioOutputPrice: '', modelCachedAudioReadPrice: '',
      modelMaxContext: '', modelMaxOutput: '', modelSelectedFeatures: [],
      // Empty on a new model: the server derives them from type and features
      // rather than the form guessing, so an admin who does not open the
      // editor still gets a coherent row.
      modelInputModalities: [], modelOutputModalities: [],
      modelRequiredModalities: [], modelAliases: '',
    },
  });

  const editModelForm = useZodForm<EditModelValues>({
    schema: editModelSchema,
    defaultValues: {
      editModelCode: '', editModelProviderModelId: '',
      editModelName: '', editModelDescription: '',
      editModelInputPrice: '', editModelOutputPrice: '',
      editModelCachedInputReadPrice: '', editModelCachedInputWritePrice: '',
      editModelAudioInputPrice: '', editModelAudioOutputPrice: '', editModelCachedAudioReadPrice: '',
      editModelMaxContext: '', editModelMaxOutput: '',
      editModelFeatures: [], editModelInputModalities: [], editModelOutputModalities: [],
      editModelRequiredModalities: [],
      editModelType: 'chat',
      editModelStatus: 'active', editModelAliases: '', editModelEnabled: true,
      editModelDeprecationDate: '', editModelReplacedBy: '',
    },
  });

  useUnsavedChangesWarning(
    providerForm.formState.isDirty ||
    newCredForm.formState.isDirty ||
    editCredForm.formState.isDirty ||
    newModelForm.formState.isDirty ||
    editModelForm.formState.isDirty,
  );

  // ── UI state ──
  const [showCredForm, setShowCredForm] = useState(false);
  const [editingCredId, setEditingCredId] = useState<string | null>(null);
  const [deletingCred, setDeletingCred] = useState<Credential | null>(null);
  const [showModelForm, setShowModelForm] = useState(false);
  const [editingModelId, setEditingModelId] = useState<string | null>(null);
  const [deletingModel, setDeletingModel] = useState<Model | null>(null);
  /** Capability JSON for the model currently being edited. null = clear; undefined = unchanged. */
  const [editingCapabilityJson, setEditingCapabilityJson] = useState<ModelCapabilityJson | null | undefined>(undefined);

  const canUpdate = usePermission('provider:update');
  const canDelete = usePermission('provider:delete');
  const canCreateCredential = usePermission('credential:create');
  const canCreateModel = usePermission('model:create');
  // Model rows carry their own IAM resource: the write endpoints behind the
  // model affordances are PUT/DELETE /models/:id, guarded on model.update /
  // model.delete — not on the provider action that guards this page. Gating a
  // model write on the provider permission offers the affordance to a
  // principal the backend then rejects, mid-write.
  const canUpdateModel = usePermission('model:update');
  const canDeleteModel = usePermission('model:delete');
  // Credential rows carry their own IAM resource for the same reason, and the
  // dedicated credential detail page already gates on it — only this page's
  // embedded tab did not.
  const canUpdateCredential = usePermission('credential:update');
  const canDeleteCredential = usePermission('credential:delete');

  // ── Data fetching ──
  const { data: provider, loading, error, refetch } = useApi<Provider>(
    () => providerApi.get(id!),
    ['providers', 'detail', id],
  );

  const { data: credData, refetch: refetchCreds } = useApi<{ data: Credential[] }>(
    () => credentialApi.list(),
    ['credentials', 'list', id],
  );

  const { data: modelsData, refetch: refetchModels } = useApi<{ data: Model[] }>(
    () => providerApi.getModels(id!),
    ['providers', 'models', id],
  );

  const { data: healthData } = useApi<ProviderHealth>(
    () => providerApi.getHealth(id!),
    ['providers', 'health', id],
  );

  const { data: analyticsData } = useApi<ProviderAnalytics>(
    () => providerApi.getAnalytics(id!) as Promise<ProviderAnalytics>,
    ['providers', 'analytics', id],
  );

  // ── Mutations ──
  const syncProviderDetail = (updated: Provider) => {
    queryClient.setQueryData(['api', 'providers', 'detail', id], updated);
    refetch();
  };

  const { mutate: toggleEnabled, loading: toggleLoading } = useMutation(
    (enabled: boolean) => providerApi.update(id!, { enabled }),
    { onSuccess: syncProviderDetail, successMessage: t('providers.providerUpdated') },
  );

  const { mutate: saveProvider, loading: saveLoading } = useMutation(
    (data: unknown) => providerApi.update(id!, data as UpdateProviderInput),
    {
      onSuccess: (updated) => {
        showSyncFeedback('ai-gateway');
        setIsEditing(false);
        syncProviderDetail(updated);
      },
      successMessage: t('providers.providerUpdated'),
    },
  );

  const { mutate: deleteProvider, loading: deleteLoading } = useMutation(
    () => providerApi.delete(id!),
    { onSuccess: () => navigate('/ai-gateway/providers'), successMessage: t('providers.providerDeleted') },
  );

  // Credential mutations
  const { mutate: createCredential, loading: credCreating } = useMutation(
    (data: CreateCredentialInput) => credentialApi.create(data),
    { onSuccess: () => { setShowCredForm(false); newCredForm.reset(); refetchCreds(); }, successMessage: t('credentials.credentialCreated') },
  );

  const { mutate: updateCredential, loading: credUpdating } = useMutation(
    (data: { id: string; payload: UpdateCredentialInput }) => credentialApi.update(data.id, data.payload),
    { onSuccess: () => { setEditingCredId(null); refetchCreds(); }, successMessage: t('credentials.credentialUpdated') },
  );

  const { mutate: deleteCredential, loading: credDeleting } = useMutation(
    (credId: string) => credentialApi.delete(credId),
    { onSuccess: () => { setDeletingCred(null); refetchCreds(); }, successMessage: t('credentials.credentialDeleted') },
  );

  const { mutate: toggleCredEnabled } = useMutation(
    (data: { id: string; enabled: boolean }) => credentialApi.update(data.id, { enabled: data.enabled }),
    { onSuccess: () => refetchCreds(), successMessage: t('credentials.credentialUpdated') },
  );

  // Model mutations
  const { mutate: createModel, loading: modelCreating } = useMutation(
    (data: CreateModelInput) => providerApi.addModel(id!, data),
    {
      onSuccess: () => {
        setShowModelForm(false);
        newModelForm.reset();
        refetchModels();
      },
      successMessage: t('models.modelCreated'),
    },
  );

  const { mutate: updateModel, loading: modelUpdating } = useMutation(
    (data: { id: string; payload: UpdateModelInput }) => systemApi.updateModel(data.id, data.payload),
    {
      onSuccess: () => {
        setEditingModelId(null);
        setEditingCapabilityJson(undefined);
        refetchModels();
      },
      successMessage: t('models.modelUpdated'),
    },
  );

  const { mutate: deleteModel, loading: modelDeleting } = useMutation(
    (modelId: string) => systemApi.deleteModel(modelId),
    { onSuccess: () => { setDeletingModel(null); refetchModels(); }, successMessage: t('models.modelDeleted') },
  );

  const { mutate: toggleModelEnabled } = useMutation(
    (data: { id: string; enabled: boolean }) => systemApi.updateModel(data.id, { enabled: data.enabled }),
    { onSuccess: () => refetchModels(), successMessage: t('models.modelUpdated') },
  );

  // ── Handlers ──
  const startEditing = () => {
    if (!provider) return;
    providerForm.reset({
      name: provider.name,
      displayName: provider.displayName ?? '',
      description: provider.description ?? '',
      baseUrl: provider.baseUrl,
      adapterType: provider.adapterType,
      region: provider.region ?? '',
      apiVersion: provider.apiVersion ?? '',
      enabled: provider.enabled,
    });
    setIsEditing(true);
  };

  const handleSave = () => {
    const v = providerForm.getValues();
    saveProvider({
      name: v.name, displayName: v.displayName, description: v.description,
      baseUrl: v.baseUrl, adapterType: v.adapterType,
      region: v.region || undefined,
      apiVersion: v.apiVersion || undefined,
      enabled: v.enabled,
    });
  };

  const resetModelForm = () => {
    newModelForm.reset();
  };

  const startEditingModel = (m: Model) => {
    setEditingModelId(m.id);
    // Initialise capability JSON from the model's existing document.
    setEditingCapabilityJson(m.capabilityJson ?? null);
    editModelForm.reset({
      editModelCode: m.code,
      editModelProviderModelId: m.providerModelId,
      editModelName: m.name,
      editModelDescription: m.description ?? '',
      editModelInputPrice: m.inputPricePerMillion != null ? String(m.inputPricePerMillion) : '',
      editModelOutputPrice: m.outputPricePerMillion != null ? String(m.outputPricePerMillion) : '',
      editModelCachedInputReadPrice: m.cachedInputReadPricePerMillion != null ? String(m.cachedInputReadPricePerMillion) : '',
      editModelCachedInputWritePrice: m.cachedInputWritePricePerMillion != null ? String(m.cachedInputWritePricePerMillion) : '',
      editModelAudioInputPrice: m.audioInputPricePerMillion != null ? String(m.audioInputPricePerMillion) : '',
      editModelAudioOutputPrice: m.audioOutputPricePerMillion != null ? String(m.audioOutputPricePerMillion) : '',
      editModelCachedAudioReadPrice: m.cachedAudioInputReadPricePerMillion != null ? String(m.cachedAudioInputReadPricePerMillion) : '',
      editModelMaxContext: m.maxContextTokens != null ? String(m.maxContextTokens) : '',
      editModelMaxOutput: m.maxOutputTokens != null ? String(m.maxOutputTokens) : '',
      editModelFeatures: Array.isArray(m.features) ? [...m.features] : [],
      // Rendered as-is. The backend derives these from type+features when a
      // caller sends none, so what the API returned is already the resolved
      // value; deriving again here would be a second source of truth.
      editModelInputModalities: Array.isArray(m.inputModalities) ? [...m.inputModalities] : [],
      editModelOutputModalities: Array.isArray(m.outputModalities) ? [...m.outputModalities] : [],
      editModelRequiredModalities: Array.isArray(m.requiredModalities)
        ? [...m.requiredModalities]
        : [],
      // 'audio' stays in this list deliberately, even though it is no longer
      // offered when CREATING a model. This maps an existing row onto the
      // edit form, and dropping it would silently rewrite a deprecated-but-
      // working audio row to chat the first time an admin opened it — an edit
      // nobody asked for, on a row the routing map still honours.
      editModelType: ['chat', 'embedding', 'image', 'audio', 'tts', 'stt', 'rerank', 'video', 'realtime'].includes(m.type) ? m.type : 'chat',
      editModelStatus: m.status ?? 'active',
      editModelAliases: Array.isArray(m.aliases) ? m.aliases.join(', ') : '',
      editModelEnabled: m.enabled,
      editModelDeprecationDate: m.deprecationDate ? m.deprecationDate.split('T')[0] : '',
      editModelReplacedBy: m.replacedBy ?? '',
    });
  };

  const handleModelUpdate = () => {
    if (!editingModelId) return;
    const v = editModelForm.getValues();
    const aliases = v.editModelAliases
      ? v.editModelAliases.split(',').map(s => s.trim()).filter(Boolean)
      : [];
    updateModel({
      id: editingModelId,
      payload: {
        code: v.editModelCode,
        providerModelId: v.editModelProviderModelId,
        name: v.editModelName,
        description: v.editModelDescription || undefined,
        inputPricePerMillion: v.editModelInputPrice ? Number(v.editModelInputPrice) : undefined,
        outputPricePerMillion: v.editModelOutputPrice ? Number(v.editModelOutputPrice) : undefined,
        cachedInputReadPricePerMillion: v.editModelCachedInputReadPrice ? Number(v.editModelCachedInputReadPrice) : undefined,
        cachedInputWritePricePerMillion: v.editModelCachedInputWritePrice ? Number(v.editModelCachedInputWritePrice) : undefined,
        audioInputPricePerMillion: v.editModelAudioInputPrice ? Number(v.editModelAudioInputPrice) : undefined,
        audioOutputPricePerMillion: v.editModelAudioOutputPrice ? Number(v.editModelAudioOutputPrice) : undefined,
        cachedAudioInputReadPricePerMillion: v.editModelCachedAudioReadPrice ? Number(v.editModelCachedAudioReadPrice) : undefined,
        maxContextTokens: v.editModelMaxContext ? Number(v.editModelMaxContext) : undefined,
        maxOutputTokens: v.editModelMaxOutput ? Number(v.editModelMaxOutput) : undefined,
        features: v.editModelFeatures,
        inputModalities: v.editModelInputModalities,
        outputModalities: v.editModelOutputModalities,
        // Sent even when empty: an explicit [] is how an admin CLEARS a
        // floor, and the API distinguishes that from an absent field.
        requiredModalities: v.editModelRequiredModalities,
        type: v.editModelType,
        status: v.editModelStatus,
        deprecationDate: v.editModelDeprecationDate || undefined,
        replacedBy: v.editModelReplacedBy || undefined,
        aliases,
        enabled: v.editModelEnabled,
        // Include capabilityJson only when the admin has opened the editor
        // and set a value; undefined = no change; null = clear.
        ...(editingCapabilityJson !== undefined && { capabilityJson: editingCapabilityJson }),
      },
    });
  };

  const startEditingCred = (c: Credential) => {
    setEditingCredId(c.id);
    editCredForm.reset({
      editCredName: c.name,
      editCredApiKey: '',
      editCredEnabled: c.enabled,
      editCredExpiresAt: c.expiresAt ? c.expiresAt.split('T')[0] : '',
    });
  };

  const handleCredUpdate = () => {
    if (!editingCredId) return;
    const v = editCredForm.getValues();
    const payload: Record<string, unknown> = { name: v.editCredName, enabled: v.editCredEnabled };
    if (v.editCredApiKey) payload.apiKey = v.editCredApiKey;
    // expiresAt: empty string = clear (null); non-empty = set; absent key = keep (but we always send it)
    payload.expiresAt = v.editCredExpiresAt ? `${v.editCredExpiresAt}T00:00:00Z` : null;
    updateCredential({ id: editingCredId, payload });
  };

  const credentials = (credData?.data ?? []).filter(c => c.providerId === id);
  const models = modelsData?.data ?? [];

  return {
    // Route
    id,
    navigate,

    // Tab state
    activeTab, setActiveTab,

    // Provider data
    provider, loading, error, refetch,
    healthData,
    analyticsData,

    // Permissions
    canUpdate, canDelete, canCreateCredential, canCreateModel,
    canUpdateModel, canDeleteModel,
    canUpdateCredential, canDeleteCredential,

    // Provider toggle / delete
    toggleEnabled, toggleLoading,
    deleting, setDeleting,
    deleteProvider, deleteLoading,

    // Provider edit
    isEditing, setIsEditing,
    providerForm,
    startEditing, handleSave, saveLoading,

    // Credentials
    credentials,
    showCredForm, setShowCredForm,
    newCredForm,
    createCredential, credCreating,
    editingCredId, setEditingCredId,
    editCredForm,
    startEditingCred, handleCredUpdate, credUpdating,
    toggleCredEnabled,
    deletingCred, setDeletingCred,
    deleteCredential, credDeleting,

    // Models
    models,
    refetchModels,
    showModelForm, setShowModelForm,
    newModelForm,
    createModel, modelCreating,
    resetModelForm,
    editingModelId, setEditingModelId,
    editModelForm,
    startEditingModel, handleModelUpdate, modelUpdating,
    editingCapabilityJson, setEditingCapabilityJson,
    toggleModelEnabled,
    deletingModel, setDeletingModel,
    deleteModel, modelDeleting,
  };
}

export type ProviderDetailState = ReturnType<typeof useProviderDetail>;
