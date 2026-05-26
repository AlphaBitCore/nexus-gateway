import { useTranslation } from 'react-i18next';
import {
  Badge,
  Button,
  Card,
  Input,
  MultiSelectDropdown,
  Stack,
  statusToVariant
} from '@/components/ui';
import { mergeModelFeatureOptions, MODEL_FEATURE_OPTIONS } from '../_shared/model-feature-options';
import type { ProviderDetailState } from './useProviderDetail';
import { ProviderModelCapabilitiesPanel } from './ProviderModelCapabilitiesPanel';
import styles from './ProviderDetail.module.css';
import { formatTokens } from '@/lib/format';

interface ProviderModelsTabProps {
  detail: ProviderDetailState;
}

export function ProviderModelsTab({ detail }: ProviderModelsTabProps) {
  const { t } = useTranslation();
  const {
    models,
    canUpdate, canDelete, canCreateModel,
    showModelForm, setShowModelForm,
    newModelForm,
    createModel, modelCreating,
    resetModelForm,
    editingModelId, setEditingModelId,
    editModelForm,
    startEditingModel, handleModelUpdate, modelUpdating,
    editingCapabilityJson, setEditingCapabilityJson,
    toggleModelEnabled,
    setDeletingModel,
  } = detail;

  const modelName = newModelForm.watch('modelName');
  const modelProviderModelId = newModelForm.watch('modelProviderModelId');
  const modelCode = newModelForm.watch('modelCode');
  const modelType = newModelForm.watch('modelType');
  const modelDescription = newModelForm.watch('modelDescription');
  const modelInputPrice = newModelForm.watch('modelInputPrice');
  const modelOutputPrice = newModelForm.watch('modelOutputPrice');
  const modelCachedInputReadPrice = newModelForm.watch('modelCachedInputReadPrice');
  const modelCachedInputWritePrice = newModelForm.watch('modelCachedInputWritePrice');
  const modelMaxContext = newModelForm.watch('modelMaxContext');
  const modelMaxOutput = newModelForm.watch('modelMaxOutput');
  const modelSelectedFeatures = newModelForm.watch('modelSelectedFeatures');
  const modelAliases = newModelForm.watch('modelAliases');

  const editModelCode = editModelForm.watch('editModelCode');
  const editModelProviderModelId = editModelForm.watch('editModelProviderModelId');
  const editModelName = editModelForm.watch('editModelName');
  const editModelDescription = editModelForm.watch('editModelDescription');
  const editModelInputPrice = editModelForm.watch('editModelInputPrice');
  const editModelOutputPrice = editModelForm.watch('editModelOutputPrice');
  const editModelCachedInputReadPrice = editModelForm.watch('editModelCachedInputReadPrice');
  const editModelCachedInputWritePrice = editModelForm.watch('editModelCachedInputWritePrice');
  const editModelMaxContext = editModelForm.watch('editModelMaxContext');
  const editModelMaxOutput = editModelForm.watch('editModelMaxOutput');
  const editModelFeatures = editModelForm.watch('editModelFeatures');
  const editModelType = editModelForm.watch('editModelType');
  const editModelStatus = editModelForm.watch('editModelStatus');
  const editModelAliases = editModelForm.watch('editModelAliases');
  const editModelEnabled = editModelForm.watch('editModelEnabled');
  const editModelDeprecationDate = editModelForm.watch('editModelDeprecationDate');
  const editModelReplacedBy = editModelForm.watch('editModelReplacedBy');

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

  return (
    <Card>
      <div className={styles.toolbarEnd}>
        {canCreateModel && (
          <Button onClick={() => setShowModelForm(!showModelForm)}>{t('pages:providers.addModel')}</Button>
        )}
      </div>

      {showModelForm && (
        <div className={styles.inlineFormRow}>
          <div className={styles.flexWrap}>
            <div className={styles.flexFieldMd}>
              <label className={styles.inlineLabel}>{t('pages:providers.newModelNameLabel')}</label>
              <Input value={modelName} onChange={e => newModelForm.setValue('modelName', e.target.value)} placeholder={t('pages:providers.placeholderModelName')} className={styles.inlineInput} />
            </div>
            <div className={styles.flexFieldMd}>
              <label className={styles.inlineLabel}>{t('pages:providers.modelCodeLabel')}</label>
              <Input value={modelCode} onChange={e => newModelForm.setValue('modelCode', e.target.value)} placeholder={t('pages:providers.placeholderModelCode')} className={styles.inlineInput} />
            </div>
            <div className={styles.flexFieldMd}>
              <label className={styles.inlineLabel}>{t('pages:providers.providerModelIdLabel')}</label>
              <Input value={modelProviderModelId} onChange={e => newModelForm.setValue('modelProviderModelId', e.target.value)} placeholder={t('pages:providers.placeholderProviderModelId')} className={styles.inlineInput} />
            </div>
            <div className={styles.flexFieldXs}>
              <label className={styles.inlineLabel}>{t('pages:providers.type')}</label>
              <select value={modelType} onChange={e => newModelForm.setValue('modelType', e.target.value)} className={styles.inlineSelect}>
                {MODEL_TYPE_OPTIONS.map(o => <option key={o.value} value={o.value}>{o.label}</option>)}
              </select>
            </div>
          </div>
          <div className={styles.flexWrap}>
            <div className={styles.flexFieldMd}>
              <label className={styles.inlineLabel}>{t('pages:providers.modelDescriptionLabel')}</label>
              <Input value={modelDescription} onChange={e => newModelForm.setValue('modelDescription', e.target.value)} placeholder={t('pages:providers.placeholderOptionalDescription')} className={styles.inlineInput} />
            </div>
            <div className={styles.flexFieldXs}>
              <label className={styles.inlineLabel}>{t('pages:providers.inputPricePerM')}</label>
              <Input value={modelInputPrice} onChange={e => newModelForm.setValue('modelInputPrice', e.target.value)} placeholder={t('pages:providers.placeholderInputPrice')} type="number" step="0.01" className={styles.inlineInput} />
            </div>
            <div className={styles.flexFieldXs}>
              <label className={styles.inlineLabel}>{t('pages:providers.outputPricePerM')}</label>
              <Input value={modelOutputPrice} onChange={e => newModelForm.setValue('modelOutputPrice', e.target.value)} placeholder={t('pages:providers.placeholderOutputPrice')} type="number" step="0.01" className={styles.inlineInput} />
            </div>
            <div
              className={styles.flexFieldXs}
              title={t('pages:providers.cachedInputReadPriceHelp')}
            >
              <label className={styles.inlineLabel}>{t('pages:providers.cachedInputReadPricePerM')}</label>
              <Input value={modelCachedInputReadPrice} onChange={e => newModelForm.setValue('modelCachedInputReadPrice', e.target.value)} placeholder={t('pages:providers.placeholderCachedInputReadPrice')} type="number" step="0.01" className={styles.inlineInput} />
            </div>
            <div
              className={styles.flexFieldXs}
              title={t('pages:providers.cachedInputWritePriceHelp')}
            >
              <label className={styles.inlineLabel}>{t('pages:providers.cachedInputWritePricePerM')}</label>
              <Input value={modelCachedInputWritePrice} onChange={e => newModelForm.setValue('modelCachedInputWritePrice', e.target.value)} placeholder={t('pages:providers.placeholderCachedInputWritePrice')} type="number" step="0.01" className={styles.inlineInput} />
            </div>
          </div>
          <div className={styles.flexWrap}>
            <div className={styles.flexFieldSm}>
              <label className={styles.inlineLabel}>{t('pages:providers.maxContextTokens')}</label>
              <Input value={modelMaxContext} onChange={e => newModelForm.setValue('modelMaxContext', e.target.value)} placeholder={t('pages:providers.placeholderMaxContext')} type="number" className={styles.inlineInput} />
            </div>
            <div className={styles.flexFieldSm}>
              <label className={styles.inlineLabel}>{t('pages:providers.maxOutputTokens')}</label>
              <Input value={modelMaxOutput} onChange={e => newModelForm.setValue('modelMaxOutput', e.target.value)} placeholder={t('pages:providers.placeholderMaxOutput')} type="number" className={styles.inlineInput} />
            </div>
            <div className={styles.flexField}>
              <label className={styles.inlineLabel}>{t('pages:providers.modelAliasesLabel')}</label>
              <Input value={modelAliases} onChange={e => newModelForm.setValue('modelAliases', e.target.value)} placeholder={t('pages:providers.placeholderModelAliases')} className={styles.inlineInput} />
            </div>
          </div>
          <div className={styles.fullWidth}>
            <MultiSelectDropdown
              label={t('pages:providers.features')}
              options={MODEL_FEATURE_OPTIONS}
              value={modelSelectedFeatures}
              onChange={(v) => newModelForm.setValue('modelSelectedFeatures', v)}
              emptyLabel={t('pages:providers.selectCapabilities')}
            />
          </div>
          <Stack direction="horizontal" gap="xs" className={styles.justifyEnd}>
            <Button variant="secondary" size="sm" onClick={() => { setShowModelForm(false); resetModelForm(); }}>{t('common:cancel')}</Button>
            <Button size="sm"
              onClick={() => {
                if (modelName && modelCode && modelProviderModelId) {
                  createModel({
                    name: modelName, providerModelId: modelProviderModelId, type: modelType,
                    code: modelCode,
                    ...(modelDescription && { description: modelDescription }),
                    ...(modelInputPrice && { inputPricePerMillion: Number(modelInputPrice) }),
                    ...(modelOutputPrice && { outputPricePerMillion: Number(modelOutputPrice) }),
                    ...(modelCachedInputReadPrice && { cachedInputReadPricePerMillion: Number(modelCachedInputReadPrice) }),
                    ...(modelCachedInputWritePrice && { cachedInputWritePricePerMillion: Number(modelCachedInputWritePrice) }),
                    ...(modelMaxContext && { maxContextTokens: Number(modelMaxContext) }),
                    ...(modelMaxOutput && { maxOutputTokens: Number(modelMaxOutput) }),
                    features: modelSelectedFeatures,
                    aliases: modelAliases ? modelAliases.split(',').map(s => s.trim()).filter(Boolean) : [],
                  });
                }
              }}
              disabled={modelCreating || !modelName || !modelCode || !modelProviderModelId}
            >{modelCreating ? t('pages:providers.saving') : t('common:create')}</Button>
          </Stack>
        </div>
      )}

      {/* Editing model inline */}
      {editingModelId && (() => {
        const m = models.find(mo => mo.id === editingModelId);
        if (!m) return null;
        return (
          <div className={styles.inlineFormRowHighlight}>
            <div className={styles.editingTitle}>
              {t('pages:providers.editing', { name: m.name })}
            </div>
            {/* Row 1: Name → Model ID → Upstream Model ID → Type → Status (matches create order) */}
            <div className={styles.flexWrap}>
              <div className={styles.flexFieldMd}>
                <label className={styles.inlineLabel}>{t('pages:providers.newModelNameLabel')}</label>
                <Input value={editModelName} onChange={e => editModelForm.setValue('editModelName', e.target.value)} className={styles.inlineInput} />
              </div>
              <div className={styles.flexFieldMd}>
                <label className={styles.inlineLabel}>{t('pages:providers.modelCodeLabel')}</label>
                <Input value={editModelCode} onChange={e => editModelForm.setValue('editModelCode', e.target.value)} className={styles.inlineInput} />
              </div>
              <div className={styles.flexFieldMd}>
                <label className={styles.inlineLabel}>{t('pages:providers.providerModelIdLabel')}</label>
                <Input value={editModelProviderModelId} onChange={e => editModelForm.setValue('editModelProviderModelId', e.target.value)} className={styles.inlineInput} />
              </div>
              <div className={styles.flexFieldXs}>
                <label className={styles.inlineLabel}>{t('pages:providers.type')}</label>
                <select value={editModelType} onChange={e => editModelForm.setValue('editModelType', e.target.value)} className={styles.inlineSelect}>
                  {MODEL_TYPE_OPTIONS.map(o => <option key={o.value} value={o.value}>{o.label}</option>)}
                </select>
              </div>
              <div className={styles.flexFieldXs}>
                <label className={styles.inlineLabel}>{t('pages:providers.status')}</label>
                <select value={editModelStatus} onChange={e => editModelForm.setValue('editModelStatus', e.target.value)} className={styles.inlineSelect}>
                  {MODEL_STATUS_OPTIONS.map(o => <option key={o.value} value={o.value}>{o.label}</option>)}
                </select>
              </div>
            </div>
            {/* Row 2: Description → Input → Output */}
            <div className={styles.flexWrap}>
              <div className={styles.flexField}>
                <label className={styles.inlineLabel}>{t('pages:providers.description')}</label>
                <Input value={editModelDescription} onChange={e => editModelForm.setValue('editModelDescription', e.target.value)} className={styles.inlineInput} />
              </div>
              <div className={styles.flexFieldXs}>
                <label className={styles.inlineLabel}>{t('pages:providers.inputPricePerM')}</label>
                <Input value={editModelInputPrice} onChange={e => editModelForm.setValue('editModelInputPrice', e.target.value)} type="number" step="0.01" className={styles.inlineInput} />
              </div>
              <div className={styles.flexFieldXs}>
                <label className={styles.inlineLabel}>{t('pages:providers.outputPricePerM')}</label>
                <Input value={editModelOutputPrice} onChange={e => editModelForm.setValue('editModelOutputPrice', e.target.value)} type="number" step="0.01" className={styles.inlineInput} />
              </div>
              <div
                className={styles.flexFieldXs}
                title={t('pages:providers.cachedInputReadPriceHelp')}
              >
                <label className={styles.inlineLabel}>{t('pages:providers.cachedInputReadPricePerM')}</label>
                <Input value={editModelCachedInputReadPrice} onChange={e => editModelForm.setValue('editModelCachedInputReadPrice', e.target.value)} placeholder={t('pages:providers.placeholderCachedInputReadPrice')} type="number" step="0.01" className={styles.inlineInput} />
              </div>
              <div
                className={styles.flexFieldXs}
                title={t('pages:providers.cachedInputWritePriceHelp')}
              >
                <label className={styles.inlineLabel}>{t('pages:providers.cachedInputWritePricePerM')}</label>
                <Input value={editModelCachedInputWritePrice} onChange={e => editModelForm.setValue('editModelCachedInputWritePrice', e.target.value)} placeholder={t('pages:providers.placeholderCachedInputWritePrice')} type="number" step="0.01" className={styles.inlineInput} />
              </div>
            </div>
            {/* Row 3: Max Context → Max Output → Aliases → Enabled */}
            <div className={styles.flexWrap}>
              <div className={styles.flexFieldSm}>
                <label className={styles.inlineLabel}>{t('pages:providers.maxContextTokens')}</label>
                <Input value={editModelMaxContext} onChange={e => editModelForm.setValue('editModelMaxContext', e.target.value)} type="number" className={styles.inlineInput} />
              </div>
              <div className={styles.flexFieldSm}>
                <label className={styles.inlineLabel}>{t('pages:providers.maxOutputTokens')}</label>
                <Input value={editModelMaxOutput} onChange={e => editModelForm.setValue('editModelMaxOutput', e.target.value)} type="number" className={styles.inlineInput} />
              </div>
              <div className={styles.flexField}>
                <label className={styles.inlineLabel}>{t('pages:providers.modelAliasesLabel')}</label>
                <Input value={editModelAliases} onChange={e => editModelForm.setValue('editModelAliases', e.target.value)} placeholder={t('pages:providers.placeholderModelAliases')} className={styles.inlineInput} />
              </div>
              <div className={styles.flexFieldAuto}>
                <Button variant="ghost" size="sm" onClick={() => editModelForm.setValue('editModelEnabled', !editModelEnabled)}
                  className={editModelEnabled ? styles.enableToggleEnabled : styles.enableToggle}
                >
                  {editModelEnabled ? t('common:enabled') : t('common:disabled')}
                </Button>
              </div>
            </div>
            {editModelStatus === 'deprecated' && (
              <div className={styles.flexWrap}>
                <div className={styles.flexFieldSm}>
                  <label className={styles.inlineLabel}>{t('pages:providers.deprecationDate')}</label>
                  <Input value={editModelDeprecationDate} onChange={e => editModelForm.setValue('editModelDeprecationDate', e.target.value)} type="date" className={styles.inlineInput} />
                </div>
                <div className={styles.flexFieldMd}>
                  <label className={styles.inlineLabel}>{t('pages:providers.replacedByModel')}</label>
                  <Input value={editModelReplacedBy} onChange={e => editModelForm.setValue('editModelReplacedBy', e.target.value)} placeholder={t('pages:providers.placeholderReplacedBy')} className={styles.inlineInput} />
                </div>
              </div>
            )}
            <div className={styles.fullWidth}>
              <MultiSelectDropdown
                label={t('pages:providers.features')}
                options={mergeModelFeatureOptions(editModelFeatures)}
                value={editModelFeatures}
                onChange={(v) => editModelForm.setValue('editModelFeatures', v)}
                emptyLabel={t('pages:providers.selectCapabilities')}
              />
            </div>
            {/* Capabilities panel — visible for embedding and chat models */}
            {canUpdate && (editModelType === 'embedding' || editModelType === 'chat') && (
              <div className={styles.fullWidth}>
                <ProviderModelCapabilitiesPanel
                  modelType={editModelType}
                  value={editingCapabilityJson}
                  onChange={(next) => setEditingCapabilityJson(next)}
                  editable={true}
                />
              </div>
            )}
            <Stack direction="horizontal" gap="xs" className={styles.justifyEnd}>
              <Button variant="secondary" size="sm" onClick={() => { setEditingModelId(null); setEditingCapabilityJson(undefined); }}>{t('common:cancel')}</Button>
              <Button size="sm" onClick={handleModelUpdate} disabled={modelUpdating || !editModelName || !editModelCode || !editModelProviderModelId}>
                {modelUpdating ? t('pages:providers.saving') : t('common:save')}
              </Button>
            </Stack>
          </div>
        );
      })()}

      {models.length === 0 ? (
        <div className={styles.emptyState}>{t('pages:providers.noModels')}</div>
      ) : (
        <div className={styles.modelCardGrid}>
          {models.map((m) => (
            <div key={m.id} className={styles.modelCard}>
              <div className={styles.modelCardHeader}>
                <div>
                  <div className={styles.modelCardName}>{m.name}</div>
                  <div className={styles.modelCardModelId}>{m.code}</div>
                  {m.providerModelId !== m.code && (
                    <div className={styles.modelCardModelId}>{t('pages:providers.providerModelId')}: {m.providerModelId}</div>
                  )}
                </div>
                <Stack direction="horizontal" gap="xs" align="center">
                  <Badge variant={statusToVariant(m.type)}>
                    {t(`pages:providers.modelType${m.type.charAt(0).toUpperCase() + m.type.slice(1)}`, m.type)}
                  </Badge>
                  <Badge variant={statusToVariant(m.enabled ? (m.status ?? 'active') : 'disabled')}>
                    {m.enabled
                      ? t(`pages:providers.modelStatus${(m.status ?? 'active').charAt(0).toUpperCase() + (m.status ?? 'active').slice(1)}`, m.status ?? 'active')
                      : t('common:disabled')}
                  </Badge>
                </Stack>
              </div>
              {m.description && <div className={styles.modelCardDesc}>{m.description}</div>}
              <div className={styles.modelCardStats}>
                {m.inputPricePerMillion != null && (
                  <div className={styles.modelCardStat}>
                    <span className={styles.modelCardStatLabel}>{t('pages:providers.modelTableInputPrice')}</span>
                    <span className={styles.modelCardStatValue}>${m.inputPricePerMillion}</span>
                  </div>
                )}
                {m.outputPricePerMillion != null && (
                  <div className={styles.modelCardStat}>
                    <span className={styles.modelCardStatLabel}>{t('pages:providers.modelTableOutputPrice')}</span>
                    <span className={styles.modelCardStatValue}>${m.outputPricePerMillion}</span>
                  </div>
                )}
                {m.cachedInputReadPricePerMillion != null && (
                  <div className={styles.modelCardStat}>
                    <span className={styles.modelCardStatLabel}>{t('pages:providers.modelTableCachedInputReadPrice')}</span>
                    <span className={styles.modelCardStatValue}>${m.cachedInputReadPricePerMillion}</span>
                  </div>
                )}
                {m.cachedInputWritePricePerMillion != null && (
                  <div className={styles.modelCardStat}>
                    <span className={styles.modelCardStatLabel}>{t('pages:providers.modelTableCachedInputWritePrice')}</span>
                    <span className={styles.modelCardStatValue}>${m.cachedInputWritePricePerMillion}</span>
                  </div>
                )}
                {m.maxContextTokens != null && (
                  <div className={styles.modelCardStat}>
                    <span className={styles.modelCardStatLabel}>{t('pages:providers.modelTableContext')}</span>
                    <span className={styles.modelCardStatValue}>{formatTokens(m.maxContextTokens)}</span>
                  </div>
                )}
                {m.maxOutputTokens != null && (
                  <div className={styles.modelCardStat}>
                    <span className={styles.modelCardStatLabel}>{t('pages:providers.modelTableOutput')}</span>
                    <span className={styles.modelCardStatValue}>{formatTokens(m.maxOutputTokens)}</span>
                  </div>
                )}
              </div>
              {m.features?.length > 0 && (
                <div className={styles.modelCardFeatures}>
                  {m.features.map((f) => (
                    <span key={f} className={styles.modelCardFeatureTag}>{f}</span>
                  ))}
                </div>
              )}
              {m.aliases && m.aliases.length > 0 && (
                <div className={styles.modelCardFeatures}>
                  <span className={styles.modelCardStatLabel}>{t('pages:providers.modelAliasesLabel')}:&nbsp;</span>
                  {m.aliases.map((a) => (
                    <span key={a} className={styles.modelCardFeatureTag}>{a}</span>
                  ))}
                </div>
              )}
              {(m.deprecationDate || m.replacedBy) && (
                <div className={styles.modelCardStats}>
                  {m.deprecationDate && (
                    <div className={styles.modelCardStat}>
                      <span className={styles.modelCardStatLabel}>{t('pages:providers.deprecationDate')}</span>
                      <span className={styles.modelCardStatValue}>{m.deprecationDate.split('T')[0]}</span>
                    </div>
                  )}
                  {m.replacedBy && (
                    <div className={styles.modelCardStat}>
                      <span className={styles.modelCardStatLabel}>{t('pages:providers.replacedByModel')}</span>
                      <span className={styles.modelCardStatValue}>{m.replacedBy}</span>
                    </div>
                  )}
                </div>
              )}
              <div className={styles.modelCardActions}>
                {canUpdate && (
                  <Button variant="secondary" size="sm" onClick={() => startEditingModel(m)}>{t('common:edit')}</Button>
                )}
                {canUpdate && (
                  <Button variant="ghost" size="sm" onClick={() => toggleModelEnabled({ id: m.id, enabled: !m.enabled })}>
                    {m.enabled ? t('pages:providers.disable') : t('pages:providers.enable')}
                  </Button>
                )}
                {canDelete && (
                  <Button variant="danger" size="sm" onClick={() => setDeletingModel(m)}>{t('common:delete')}</Button>
                )}
              </div>
            </div>
          ))}
        </div>
      )}
    </Card>
  );
}
