import clsx from 'clsx';
import {
  FormField, Input, Select, Badge, statusToVariant, Button, Stack,
  MultiSelectDropdown,
} from '@/components/ui';
import { MODEL_FEATURE_OPTIONS } from '../_shared/model-feature-options';
import type { ProviderWizardHook } from './useProviderWizard';
import styles from './ProviderWizard.module.css';
import { formatTokens } from '@/lib/format';
import { LinkButton } from '@nexus-gateway/ui-shared';

export function StepModels({ wizard }: { wizard: ProviderWizardHook }) {
  const {
    t,
    selectedTemplate,
    isCustom,
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
    baseUrl,
    apiKey,
    skipCredential,
  } = wizard;

  return (
    <div className={styles.stepPanelLarge}>
      <h2 className={styles.stepTitle}>{t('pages:providers.models', 'Models')}</h2>
      <p className={styles.stepSubtitle}>
        {selectedTemplate && !isCustom
          ? t('pages:providers.modelsStepUncheckHint')
          : t('pages:providers.modelsStepCustomHint')}
      </p>

      {isCustom && (
        <div className={styles.fetchModelsSection}>
          <p className={styles.fetchModelsHint}>
            {t('pages:providers.wizardModelsFetchHint', 'Model fetch is available for OpenAI / OpenAI-compatible providers.')}
          </p>
          <Stack direction="horizontal" gap="sm" style={{ alignItems: 'center' }}>
            <Button
              variant="secondary"
              onClick={fetchModels}
              disabled={fetchingModels || !baseUrl?.trim() || (!apiKey && !skipCredential)}
              title={
                !baseUrl?.trim() || (!apiKey && !skipCredential)
                  ? t('pages:providers.wizardModelsFetchDisabledHint', 'Provide a Base URL and API key first')
                  : undefined
              }
            >
              {fetchingModels
                ? t('pages:providers.wizardModelsFetching', 'Fetching…')
                : t('pages:providers.wizardModelsFetchButton', 'Fetch from /v1/models')}
            </Button>
          </Stack>
          {fetchModelsUnsupported && (
            <p role="alert" className={styles.fetchModelsUnsupported}>
              {t(
                'pages:providers.wizardModelsFetchOpenAIOnly',
                'Model fetch from /v1/models is available for OpenAI / OpenAI-compatible providers only. Add models manually below.',
              )}
            </p>
          )}
          {fetchModelsError && !fetchModelsUnsupported && (
            <p role="alert" className={styles.fetchModelsError}>
              {fetchModelsError}
            </p>
          )}
          {fetchModelsCount !== null && fetchModelsCount !== undefined && !fetchModelsUnsupported && !fetchModelsError && (
            <p className={styles.fetchModelsHint}>
              {t('pages:providers.wizardModelsFetchCount', 'Fetched {{count}} models — select the ones to add.', { count: fetchModelsCount })}
            </p>
          )}
        </div>
      )}

      {!manualMode && models.length === 0 && isCustom && (
        <Button variant="secondary" onClick={() => setManualMode(true)} className={styles.addModelBtn}>
          {t('pages:providers.addModelManually', 'Add model manually')}
        </Button>
      )}

      {manualMode && (
        <div className={styles.manualModelForm}>
          <div className={styles.manualGrid2}>
            <FormField label={t('pages:providers.modelId')} required>
              <Input value={newModelId} onChange={e => setNewModelId(e.target.value)} placeholder={t('pages:providers.placeholderModelId')} />
            </FormField>
            <FormField label={t('pages:providers.displayName')}>
              <Input value={newModelName} onChange={e => setNewModelName(e.target.value)} placeholder={t('pages:providers.placeholderOptionalLabel')} />
            </FormField>
          </div>
          <FormField label={t('pages:providers.modelDescriptionLabel', 'Description')}>
            <Input
              value={newModelDescription}
              onChange={e => setNewModelDescription(e.target.value)}
              placeholder={t('pages:providers.placeholderOptionalDescription')}
            />
          </FormField>
          <div className={styles.manualGrid3}>
            <FormField label={t('pages:providers.type')}>
              <Select
                value={newModelType}
                onValueChange={setNewModelType}
                options={[
                  { value: 'chat', label: t('pages:providers.modelTypeChat') },
                  { value: 'completion', label: t('pages:providers.modelTypeCompletion') },
                  { value: 'embedding', label: t('pages:providers.modelTypeEmbedding') },
                  { value: 'image', label: t('pages:providers.modelTypeImage') },
                  { value: 'audio', label: t('pages:providers.modelTypeAudio', 'Audio') },
                ]}
              />
            </FormField>
            <FormField label={t('pages:providers.inputPricePerM')}>
              <Input value={newModelInputPrice} onChange={e => setNewModelInputPrice(e.target.value)} type="number" placeholder={t('pages:providers.placeholderPriceDash')} />
            </FormField>
            <FormField label={t('pages:providers.outputPricePerM')}>
              <Input value={newModelOutputPrice} onChange={e => setNewModelOutputPrice(e.target.value)} type="number" placeholder={t('pages:providers.placeholderPriceDash')} />
            </FormField>
          </div>
          <div className={styles.manualGrid2}>
            <FormField
              label={t('pages:providers.cachedInputReadPricePerM')}
              tooltip={t('pages:providers.cachedInputReadPriceHelp')}
            >
              <Input value={newModelCachedInputReadPrice} onChange={e => setNewModelCachedInputReadPrice(e.target.value)} type="number" placeholder={t('pages:providers.placeholderPriceDash')} />
            </FormField>
            <FormField
              label={t('pages:providers.cachedInputWritePricePerM')}
              tooltip={t('pages:providers.cachedInputWritePriceHelp')}
            >
              <Input value={newModelCachedInputWritePrice} onChange={e => setNewModelCachedInputWritePrice(e.target.value)} type="number" placeholder={t('pages:providers.placeholderPriceDash')} />
            </FormField>
          </div>
          <div className={styles.manualGrid2}>
            <FormField label={t('pages:providers.maxContextTokens', 'Max context tokens')}>
              <Input
                value={newModelMaxContext}
                onChange={e => setNewModelMaxContext(e.target.value)}
                type="number"
                placeholder={t('pages:providers.placeholderMaxContext', 'e.g. 128000')}
              />
            </FormField>
            <FormField label={t('pages:providers.maxOutputTokens', 'Max output tokens')}>
              <Input
                value={newModelMaxOutput}
                onChange={e => setNewModelMaxOutput(e.target.value)}
                type="number"
                placeholder={t('pages:providers.placeholderMaxOutput', 'e.g. 4096')}
              />
            </FormField>
          </div>
          <MultiSelectDropdown
            label={t('pages:providers.features', 'Features')}
            options={MODEL_FEATURE_OPTIONS}
            value={newModelFeatures}
            onChange={setNewModelFeatures}
            emptyLabel={t('pages:providers.selectCapabilities', 'Select capabilities')}
          />
          <Stack direction="horizontal" gap="sm">
            <Button onClick={addManualModel} disabled={!newModelId}>{t('pages:providers.addModel', 'Add model')}</Button>
            <Button variant="secondary" onClick={() => { setManualMode(false); resetManualModelForm(); }}>{t('pages:providers.done', 'Done')}</Button>
          </Stack>
        </div>
      )}

      {modelCodeConflicts.length > 0 && (
        <div role="alert" className={styles.conflictBanner}>
          {t('pages:providers.modelCodeConflictBanner', 'Some selected models use a code that already exists (model codes are globally unique). Rename or deselect them to continue: ')}
          {modelCodeConflicts.join(', ')}
        </div>
      )}

      {models.length > 0 && (
        <>
          {fetchModelsCount !== null && fetchModelsCount !== undefined && (
            <p className={styles.fetchModelsHint}>
              {t('pages:providers.wizardModelsTypeGuessHint', 'Model types are guessed from the id — verify before saving.')}
            </p>
          )}
          <div className={styles.modelTableWrapper}>
            <table className={styles.wizTable}>
              <thead>
                <tr className={styles.modelTableHead}>
                  <th className={clsx(styles.wizTh, styles.modelThEnable)} aria-label={t('common:enabled')} />
                  <th className={styles.wizTh}>{t('pages:providers.tableHeaderModelId')}</th>
                  <th className={styles.wizTh}>{t('pages:providers.tableHeaderName')}</th>
                  <th className={styles.wizTh}>{t('pages:providers.tableHeaderType')}</th>
                  <th className={styles.wizTh}>{t('pages:providers.features', 'Features')}</th>
                  <th className={styles.wizTh}>{t('pages:providers.tableHeaderPricing')}</th>
                  <th className={styles.wizTh}>{t('pages:providers.tableHeaderContext', 'Context / Out')}</th>
                  {isCustom && (
                    <th className={clsx(styles.wizTh, styles.modelThRemove)} aria-label={t('pages:providers.remove')} />
                  )}
                </tr>
              </thead>
              <tbody>
                {models.map((m, i) => (
                  <tr key={`${m.modelId}-${i}`} className={styles.modelRow}>
                    <td className={styles.wizTd}>
                      <input type="checkbox" checked={m.selected} onChange={() => toggleModel(i)} aria-label={`Enable ${m.name}`} className={styles.modelCheckbox} />
                    </td>
                    <td className={styles.wizTdMono}>
                      <Input
                        value={m.modelId}
                        onChange={(e) => updateModelId(i, e.target.value)}
                        aria-label={t('pages:providers.modelId')}
                      />
                      {existingModelCodes.has(m.modelId.trim().toLowerCase()) && (
                        <div className={styles.modelCodeConflictMsg}>
                          {t('pages:providers.modelCodeConflict', 'Code already exists — rename to continue')}
                        </div>
                      )}
                    </td>
                    <td className={styles.wizTd}>
                      <div style={{ fontWeight: 'var(--g-font-weight-medium)' }}>{m.name}</div>
                      {m.description && (
                        <div style={{
                          fontSize: 'var(--g-font-size-xs)',
                          color: 'var(--color-text-muted)',
                          marginTop: 'var(--g-space-0-5)',
                          maxWidth: 320,
                          overflow: 'hidden',
                          textOverflow: 'ellipsis',
                          display: '-webkit-box',
                          WebkitLineClamp: 2,
                          WebkitBoxOrient: 'vertical',
                        }} title={m.description}>
                          {m.description}
                        </div>
                      )}
                    </td>
                    <td className={styles.wizTd}><Badge variant={statusToVariant(m.type)}>{m.type}</Badge></td>
                    <td className={styles.wizTd}>
                      {m.features.length === 0 ? (
                        <span className={styles.mutedSpan}>{'—'}</span>
                      ) : (
                        <div style={{ display: 'flex', flexWrap: 'wrap', gap: 'var(--g-space-1)' }}>
                          {m.features.map((f) => (
                            <span key={f} className={styles.featurePill}>{f}</span>
                          ))}
                        </div>
                      )}
                    </td>
                    <td className={clsx(styles.wizTdMono, styles.pricingCell)}>
                      {m.inputPrice ? `$${m.inputPrice}` : '—'} / {m.outputPrice ? `$${m.outputPrice}` : '—'}
                    </td>
                    <td className={styles.wizTdMono}>
                      {formatTokens(Number(m.maxContextTokens) || undefined)} / {formatTokens(Number(m.maxOutputTokens) || undefined)}
                    </td>
                    {isCustom && (
                      <td className={styles.wizTd}>
                        <button
                          type="button"
                          onClick={() => removeModel(i)}
                          className={styles.removeModelBtn}
                          title={t('pages:providers.remove')}
                        >
                          {'×'}
                        </button>
                      </td>
                    )}
                  </tr>
                ))}
              </tbody>
            </table>
          </div>
          {isCustom && !manualMode && (
            <LinkButton onClick={() => setManualMode(true)}>
              {t('pages:providers.addAnotherModel', '+ Add another model')}
            </LinkButton>
          )}
        </>
      )}

      {models.length === 0 && !manualMode && (
        <div className={styles.emptyModels}>
          {t('pages:providers.emptyModelsHint')}
        </div>
      )}
    </div>
  );
}

