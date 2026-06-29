import { useEffect, useRef, useState } from 'react';
import { useTranslation } from 'react-i18next';
import { Dialog, Button, Input, MultiSelectDropdown, Select, Stack } from '@/components/ui';
import { systemApi } from '@/api/services';
import { mergeModelFeatureOptions, MODEL_FEATURE_OPTIONS } from '../_shared/model-feature-options';
import { ProviderModelCapabilitiesPanel } from './ProviderModelCapabilitiesPanel';
import type { ProviderDetailState } from './useProviderDetail';
import type { ModelCapabilityJson } from '@/api/types';
import styles from './ModelFormDrawer.module.css';

export interface ModelFormDrawerProps {
  detail: ProviderDetailState;
  mode: 'create' | 'edit';
  open: boolean;
  onClose: () => void;
}

/**
 * Sectioned, type-aware right slide-out for creating and editing a model.
 * Reuses the existing `newModelForm` / `editModelForm` state and the
 * `createModel` / `handleModelUpdate` handlers from useProviderDetail — this
 * component only relocates the fields into a roomy `Dialog variant="drawer"`,
 * groups them into sections, hides type-irrelevant fields, and adds an inline
 * model-code uniqueness check so a collision surfaces before save.
 */
export function ModelFormDrawer({ detail, mode, open, onClose }: ModelFormDrawerProps) {
  const { t } = useTranslation();
  const isEdit = mode === 'edit';

  const MODEL_TYPE_OPTIONS = [
    { value: 'chat', label: t('pages:providers.modelTypeChat') },
    { value: 'embedding', label: t('pages:providers.modelTypeEmbedding') },
    { value: 'image', label: t('pages:providers.modelTypeImage') },
    { value: 'audio', label: t('pages:providers.modelTypeAudio') },
  ];
  const MODEL_STATUS_OPTIONS = [
    { value: 'active', label: t('pages:providers.modelStatusActive') },
    { value: 'deprecated', label: t('pages:providers.modelStatusDeprecated') },
    { value: 'disabled', label: t('pages:providers.modelStatusDisabled') },
    { value: 'preview', label: t('pages:providers.modelStatusPreview') },
  ];

  // The model `code` the user is editing right now — drives the uniqueness
  // check. Read from whichever form is active so the effect re-runs on change.
  const activeCode = isEdit
    ? detail.editModelForm.watch('editModelCode')
    : detail.newModelForm.watch('modelCode');
  const editingModelId = detail.editingModelId;

  const [codeError, setCodeError] = useState<string | null>(null);
  const [codeChecking, setCodeChecking] = useState(false);
  const codeSeqRef = useRef(0);

  // Create-mode capability JSON (embeddings dimensions / required extensions).
  // Edit mode uses detail.editingCapabilityJson; create has no such detail
  // field, so the drawer owns it and threads it into createModel. Reset on open.
  const [createCapability, setCreateCapability] = useState<ModelCapabilityJson | undefined>(undefined);
  useEffect(() => {
    if (open && mode === 'create') setCreateCapability(undefined);
  }, [open, mode]);

  useEffect(() => {
    if (!open) return;
    const trimmed = (activeCode ?? '').trim();
    if (!trimmed) { setCodeError(null); setCodeChecking(false); return; }
    const seq = ++codeSeqRef.current;
    setCodeChecking(true);
    const handle = setTimeout(async () => {
      try {
        const res = await systemApi.listModelsFlat({ q: trimmed, limit: '50' });
        if (seq !== codeSeqRef.current) return; // stale
        const hit = (res.data ?? []).some(
          (m) => m.code.toLowerCase() === trimmed.toLowerCase() && m.id !== editingModelId,
        );
        setCodeError(hit ? t('pages:providers.modelCodeAlreadyExists', 'A model with this code already exists — model codes are globally unique') : null);
      } catch {
        if (seq === codeSeqRef.current) setCodeError(null); // backend is the final guard
      } finally {
        if (seq === codeSeqRef.current) setCodeChecking(false);
      }
    }, 350);
    return () => clearTimeout(handle);
  }, [open, activeCode, editingModelId, t]);

  const title = isEdit
    ? t('pages:providers.editModel', 'Edit model')
    : t('pages:providers.addModel');

  const footer = (
    <>
      <Button variant="secondary" onClick={onClose}>{t('common:cancel')}</Button>
      {isEdit ? (
        <Button
          variant="primary"
          onClick={() => detail.handleModelUpdate()}
          disabled={detail.modelUpdating || codeChecking || !!codeError
            || !detail.editModelForm.watch('editModelName')
            || !detail.editModelForm.watch('editModelCode')
            || !detail.editModelForm.watch('editModelProviderModelId')}
        >
          {detail.modelUpdating ? t('pages:providers.saving') : t('common:save')}
        </Button>
      ) : (
        <Button
          variant="primary"
          onClick={() => submitCreate(detail, createCapability)}
          disabled={detail.modelCreating || codeChecking || !!codeError
            || !detail.newModelForm.watch('modelName')
            || !detail.newModelForm.watch('modelCode')
            || !detail.newModelForm.watch('modelProviderModelId')}
        >
          {detail.modelCreating ? t('pages:providers.saving') : t('common:create')}
        </Button>
      )}
    </>
  );

  return (
    <Dialog open={open} onOpenChange={(o) => { if (!o) onClose(); }} title={title} variant="drawer" size="xl" footer={footer} footerClassName={styles.footer}>
      <Stack gap="lg" className={styles.form}>
        {isEdit
          ? <EditBody detail={detail} codeError={codeError} typeOptions={MODEL_TYPE_OPTIONS} statusOptions={MODEL_STATUS_OPTIONS} />
          : <CreateBody detail={detail} codeError={codeError} typeOptions={MODEL_TYPE_OPTIONS} capability={createCapability} onCapabilityChange={setCreateCapability} />}
      </Stack>
    </Dialog>
  );
}
ModelFormDrawer.displayName = 'ModelFormDrawer';

function submitCreate(detail: ProviderDetailState, capability?: ModelCapabilityJson) {
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
    ...(v.modelMaxContext && { maxContextTokens: Number(v.modelMaxContext) }),
    ...(v.modelMaxOutput && { maxOutputTokens: Number(v.modelMaxOutput) }),
    features: v.modelSelectedFeatures,
    aliases: v.modelAliases ? v.modelAliases.split(',').map((s) => s.trim()).filter(Boolean) : [],
    ...(capability && { capabilityJson: capability }),
  });
}

function FieldError({ message }: { message: string | null }) {
  if (!message) return null;
  return <span className={styles.fieldError}>{message}</span>;
}

interface CreateBodyProps {
  detail: ProviderDetailState;
  codeError: string | null;
  typeOptions: { value: string; label: string }[];
  capability?: ModelCapabilityJson;
  onCapabilityChange: (next: ModelCapabilityJson) => void;
}

function CreateBody({ detail, codeError, typeOptions, capability, onCapabilityChange }: CreateBodyProps) {
  const { t } = useTranslation();
  const f = detail.newModelForm;
  const v = {
    name: f.watch('modelName'), code: f.watch('modelCode'), pmid: f.watch('modelProviderModelId'),
    type: f.watch('modelType'), description: f.watch('modelDescription'),
    inputPrice: f.watch('modelInputPrice'), outputPrice: f.watch('modelOutputPrice'),
    cachedRead: f.watch('modelCachedInputReadPrice'), cachedWrite: f.watch('modelCachedInputWritePrice'),
    maxContext: f.watch('modelMaxContext'), maxOutput: f.watch('modelMaxOutput'),
    features: f.watch('modelSelectedFeatures'), aliases: f.watch('modelAliases'),
  };
  const isEmbedding = v.type === 'embedding';
  return (
    <>
      <section className={styles.section}>
        <h3 className={styles.sectionHeader}>{t('pages:providers.sectionIdentity', 'Identity')}</h3>
        <div className={styles.grid}>
          <div className={styles.field}><label className={styles.label}>{t('pages:providers.newModelNameLabel')}</label>
            <Input value={v.name} onChange={(e) => f.setValue('modelName', e.target.value)} placeholder={t('pages:providers.placeholderModelName')} /></div>
          <div className={styles.field}><label className={styles.label}>{t('pages:providers.modelCodeLabel')}</label>
            <Input value={v.code} onChange={(e) => f.setValue('modelCode', e.target.value)} placeholder={t('pages:providers.placeholderModelCode')} />
            <FieldError message={codeError} /></div>
          <div className={styles.field}><label className={styles.label}>{t('pages:providers.providerModelIdLabel')}</label>
            <Input value={v.pmid} onChange={(e) => f.setValue('modelProviderModelId', e.target.value)} placeholder={t('pages:providers.placeholderProviderModelId')} /></div>
          <div className={styles.field}><label className={styles.label}>{t('pages:providers.type')}</label>
            <Select value={v.type} onValueChange={(value) => f.setValue('modelType', value)} options={typeOptions} /></div>
          <div className={styles.field + ' ' + styles.fieldFull}><label className={styles.label}>{t('pages:providers.modelDescriptionLabel')}</label>
            <Input value={v.description} onChange={(e) => f.setValue('modelDescription', e.target.value)} placeholder={t('pages:providers.placeholderOptionalDescription')} /></div>
        </div>
      </section>

      <section className={styles.section}>
        <h3 className={styles.sectionHeader}>{t('pages:providers.sectionPricing', 'Pricing (per 1M tokens)')}</h3>
        <div className={styles.grid}>
          <div className={styles.field}><label className={styles.label}>{t('pages:providers.inputPricePerM')}</label>
            <Input value={v.inputPrice} onChange={(e) => f.setValue('modelInputPrice', e.target.value)} type="number" step="0.01" /></div>
          <div className={styles.field}><label className={styles.label}>{t('pages:providers.outputPricePerM')}</label>
            <Input value={v.outputPrice} onChange={(e) => f.setValue('modelOutputPrice', e.target.value)} type="number" step="0.01" /></div>
          <div className={styles.field}><label className={styles.label}>{t('pages:providers.cachedInputReadPricePerM')}</label>
            <Input value={v.cachedRead} onChange={(e) => f.setValue('modelCachedInputReadPrice', e.target.value)} type="number" step="0.01" /></div>
          <div className={styles.field}><label className={styles.label}>{t('pages:providers.cachedInputWritePricePerM')}</label>
            <Input value={v.cachedWrite} onChange={(e) => f.setValue('modelCachedInputWritePrice', e.target.value)} type="number" step="0.01" /></div>
        </div>
      </section>

      <section className={styles.section}>
        <h3 className={styles.sectionHeader}>{t('pages:providers.sectionLimits', 'Limits')}</h3>
        <div className={styles.grid}>
          <div className={styles.field}><label className={styles.label}>{t('pages:providers.maxContextTokens')}</label>
            <Input value={v.maxContext} onChange={(e) => f.setValue('modelMaxContext', e.target.value)} type="number" /></div>
          {!isEmbedding && (
            <div className={styles.field}><label className={styles.label}>{t('pages:providers.maxOutputTokens')}</label>
              <Input value={v.maxOutput} onChange={(e) => f.setValue('modelMaxOutput', e.target.value)} type="number" /></div>
          )}
          <div className={`${styles.field} ${styles.fieldFull}`}><label className={styles.label}>{t('pages:providers.modelAliasesLabel')}</label>
            <Input value={v.aliases} onChange={(e) => f.setValue('modelAliases', e.target.value)} placeholder={t('pages:providers.placeholderModelAliases')} /></div>
        </div>
      </section>

      <section className={styles.section}>
        <h3 className={styles.sectionHeader}>{t('pages:providers.sectionFeatures', 'Features')}</h3>
        <MultiSelectDropdown
          label={t('pages:providers.features')}
          className={styles.featureSelect}
          options={MODEL_FEATURE_OPTIONS}
          value={v.features}
          onChange={(val) => f.setValue('modelSelectedFeatures', val)}
          emptyLabel={t('pages:providers.selectCapabilities')}
        />
      </section>

      {(v.type === 'embedding' || v.type === 'chat') && (
        <section className={styles.section}>
          <h3 className={styles.sectionHeader}>{t('pages:providers.capabilities.sectionTitle', '能力配置')}</h3>
          <ProviderModelCapabilitiesPanel
            modelType={v.type}
            value={capability}
            onChange={onCapabilityChange}
            editable={detail.canCreateModel}
          />
        </section>
      )}
    </>
  );
}

interface EditBodyProps {
  detail: ProviderDetailState;
  codeError: string | null;
  typeOptions: { value: string; label: string }[];
  statusOptions: { value: string; label: string }[];
}

function EditBody({ detail, codeError, typeOptions, statusOptions }: EditBodyProps) {
  const { t } = useTranslation();
  const [lifecycleOpen, setLifecycleOpen] = useState(false);
  const f = detail.editModelForm;
  const v = {
    name: f.watch('editModelName'), code: f.watch('editModelCode'), pmid: f.watch('editModelProviderModelId'),
    type: f.watch('editModelType'), status: f.watch('editModelStatus'), description: f.watch('editModelDescription'),
    inputPrice: f.watch('editModelInputPrice'), outputPrice: f.watch('editModelOutputPrice'),
    cachedRead: f.watch('editModelCachedInputReadPrice'), cachedWrite: f.watch('editModelCachedInputWritePrice'),
    maxContext: f.watch('editModelMaxContext'), maxOutput: f.watch('editModelMaxOutput'),
    features: f.watch('editModelFeatures'), aliases: f.watch('editModelAliases'),
    enabled: f.watch('editModelEnabled'), deprecationDate: f.watch('editModelDeprecationDate'),
    replacedBy: f.watch('editModelReplacedBy'),
  };
  const isEmbedding = v.type === 'embedding';
  return (
    <>
      <section className={styles.section}>
        <h3 className={styles.sectionHeader}>{t('pages:providers.sectionIdentity', 'Identity')}</h3>
        <div className={styles.grid}>
          <div className={styles.field}><label className={styles.label}>{t('pages:providers.newModelNameLabel')}</label>
            <Input value={v.name} onChange={(e) => f.setValue('editModelName', e.target.value)} /></div>
          <div className={styles.field}><label className={styles.label}>{t('pages:providers.modelCodeLabel')}</label>
            <Input value={v.code} onChange={(e) => f.setValue('editModelCode', e.target.value)} />
            <FieldError message={codeError} /></div>
          <div className={styles.field}><label className={styles.label}>{t('pages:providers.providerModelIdLabel')}</label>
            <Input value={v.pmid} onChange={(e) => f.setValue('editModelProviderModelId', e.target.value)} /></div>
          <div className={styles.field}><label className={styles.label}>{t('pages:providers.type')}</label>
            <Select value={v.type} onValueChange={(value) => f.setValue('editModelType', value)} options={typeOptions} /></div>
          <div className={styles.field}><label className={styles.label}>{t('pages:providers.status')}</label>
            <Select value={v.status} onValueChange={(value) => f.setValue('editModelStatus', value)} options={statusOptions} /></div>
          <div className={styles.field + ' ' + styles.fieldFull}><label className={styles.label}>{t('pages:providers.description')}</label>
            <Input value={v.description} onChange={(e) => f.setValue('editModelDescription', e.target.value)} /></div>
        </div>
      </section>

      <section className={styles.section}>
        <h3 className={styles.sectionHeader}>{t('pages:providers.sectionPricing', 'Pricing (per 1M tokens)')}</h3>
        <div className={styles.grid}>
          <div className={styles.field}><label className={styles.label}>{t('pages:providers.inputPricePerM')}</label>
            <Input value={v.inputPrice} onChange={(e) => f.setValue('editModelInputPrice', e.target.value)} type="number" step="0.01" /></div>
          <div className={styles.field}><label className={styles.label}>{t('pages:providers.outputPricePerM')}</label>
            <Input value={v.outputPrice} onChange={(e) => f.setValue('editModelOutputPrice', e.target.value)} type="number" step="0.01" /></div>
          <div className={styles.field}><label className={styles.label}>{t('pages:providers.cachedInputReadPricePerM')}</label>
            <Input value={v.cachedRead} onChange={(e) => f.setValue('editModelCachedInputReadPrice', e.target.value)} type="number" step="0.01" /></div>
          <div className={styles.field}><label className={styles.label}>{t('pages:providers.cachedInputWritePricePerM')}</label>
            <Input value={v.cachedWrite} onChange={(e) => f.setValue('editModelCachedInputWritePrice', e.target.value)} type="number" step="0.01" /></div>
        </div>
      </section>

      <section className={styles.section}>
        <h3 className={styles.sectionHeader}>{t('pages:providers.sectionLimits', 'Limits')}</h3>
        <div className={styles.grid}>
          <div className={styles.field}><label className={styles.label}>{t('pages:providers.maxContextTokens')}</label>
            <Input value={v.maxContext} onChange={(e) => f.setValue('editModelMaxContext', e.target.value)} type="number" /></div>
          {!isEmbedding && (
            <div className={styles.field}><label className={styles.label}>{t('pages:providers.maxOutputTokens')}</label>
              <Input value={v.maxOutput} onChange={(e) => f.setValue('editModelMaxOutput', e.target.value)} type="number" /></div>
          )}
          <div className={`${styles.field} ${styles.fieldFull}`}><label className={styles.label}>{t('pages:providers.modelAliasesLabel')}</label>
            <Input value={v.aliases} onChange={(e) => f.setValue('editModelAliases', e.target.value)} placeholder={t('pages:providers.placeholderModelAliases')} /></div>
        </div>
      </section>

      <section className={styles.section}>
        <h3 className={styles.sectionHeader}>{t('pages:providers.sectionFeatures', 'Features')}</h3>
        <MultiSelectDropdown
          label={t('pages:providers.features')}
          className={styles.featureSelect}
          options={mergeModelFeatureOptions(v.features)}
          value={v.features}
          onChange={(val) => f.setValue('editModelFeatures', val)}
          emptyLabel={t('pages:providers.selectCapabilities')}
        />
      </section>

      {(v.type === 'embedding' || v.type === 'chat') && (
        <section className={styles.section}>
          <h3 className={styles.sectionHeader}>{t('pages:providers.capabilities.sectionTitle', '能力配置')}</h3>
          <ProviderModelCapabilitiesPanel
            modelType={v.type}
            value={detail.editingCapabilityJson}
            onChange={(next) => detail.setEditingCapabilityJson(next)}
            editable={detail.canUpdate}
          />
        </section>
      )}

      <section className={styles.section}>
        <button
          data-design-system-escape="collapsible section header toggle"
          type="button"
          className={styles.collapseSummary}
          aria-expanded={lifecycleOpen}
          onClick={() => setLifecycleOpen((o) => !o)}
        >
          <span className={styles.collapseCaret}>{lifecycleOpen ? '▾' : '▸'}</span>
          {t('pages:providers.sectionLifecycle', 'Lifecycle')}
        </button>
        {lifecycleOpen && (
          <div className={styles.grid} style={{ marginTop: 'var(--g-space-2)' }}>
            <div className={styles.field}>
              <label className={styles.label}>{t('common:enabled')}</label>
              <Button variant="ghost" size="sm" onClick={() => f.setValue('editModelEnabled', !v.enabled)}>
                {v.enabled ? t('common:enabled') : t('common:disabled')}
              </Button>
            </div>
            {v.status === 'deprecated' && (
              <>
                <div className={styles.field}><label className={styles.label}>{t('pages:providers.deprecationDate')}</label>
                  <Input value={v.deprecationDate} onChange={(e) => f.setValue('editModelDeprecationDate', e.target.value)} type="date" /></div>
                <div className={styles.field}><label className={styles.label}>{t('pages:providers.replacedByModel')}</label>
                  <Input value={v.replacedBy} onChange={(e) => f.setValue('editModelReplacedBy', e.target.value)} placeholder={t('pages:providers.placeholderReplacedBy')} /></div>
              </>
            )}
          </div>
        )}
      </section>
    </>
  );
}
