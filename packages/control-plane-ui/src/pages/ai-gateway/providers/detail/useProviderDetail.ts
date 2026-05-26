import { useState } from 'react';
import { useParams, useNavigate } from 'react-router-dom';
import { useTranslation } from 'react-i18next';
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
import { z } from 'zod';
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

/* ── Zod schemas ──────────────────────────────────────────────────────── */

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
  modelMaxContext: z.string().optional().default(''),
  modelMaxOutput: z.string().optional().default(''),
  modelSelectedFeatures: z.array(z.string()),
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
  editModelMaxContext: z.string().optional().default(''),
  editModelMaxOutput: z.string().optional().default(''),
  editModelFeatures: z.array(z.string()),
  editModelType: z.string().min(1),
  editModelStatus: z.string().min(1),
  editModelAliases: z.string().optional().default(''),
  editModelEnabled: z.boolean(),
  editModelDeprecationDate: z.string().optional().default(''),
  editModelReplacedBy: z.string().optional().default(''),
});
export type EditModelValues = z.infer<typeof editModelSchema>;

/* ── Hook ────────────────────────────────────────────────────────────── */

export function useProviderDetail() {
  const { id } = useParams<{ id: string }>();
  const navigate = useNavigate();
  const { t } = useTranslation('pages');
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
      modelMaxContext: '', modelMaxOutput: '', modelSelectedFeatures: [], modelAliases: '',
    },
  });

  const editModelForm = useZodForm<EditModelValues>({
    schema: editModelSchema,
    defaultValues: {
      editModelCode: '', editModelProviderModelId: '',
      editModelName: '', editModelDescription: '',
      editModelInputPrice: '', editModelOutputPrice: '',
      editModelCachedInputReadPrice: '', editModelCachedInputWritePrice: '',
      editModelMaxContext: '', editModelMaxOutput: '',
      editModelFeatures: [], editModelType: 'chat',
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
  const { mutate: toggleEnabled, loading: toggleLoading } = useMutation(
    (enabled: boolean) => providerApi.update(id!, { enabled }),
    { onSuccess: () => refetch(), successMessage: t('providers.providerUpdated') },
  );

  const { mutate: saveProvider, loading: saveLoading } = useMutation(
    (data: unknown) => providerApi.update(id!, data as UpdateProviderInput),
    { onSuccess: () => { showSyncFeedback('ai-gateway'); setIsEditing(false); refetch(); }, successMessage: t('providers.providerUpdated') },
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
      editModelMaxContext: m.maxContextTokens != null ? String(m.maxContextTokens) : '',
      editModelMaxOutput: m.maxOutputTokens != null ? String(m.maxOutputTokens) : '',
      editModelFeatures: Array.isArray(m.features) ? [...m.features] : [],
      editModelType: ['chat', 'embedding', 'image', 'audio'].includes(m.type) ? m.type : 'chat',
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
        maxContextTokens: v.editModelMaxContext ? Number(v.editModelMaxContext) : undefined,
        maxOutputTokens: v.editModelMaxOutput ? Number(v.editModelMaxOutput) : undefined,
        features: v.editModelFeatures,
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
