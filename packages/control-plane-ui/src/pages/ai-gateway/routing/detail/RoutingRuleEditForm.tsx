import clsx from 'clsx';
import { useTranslation } from 'react-i18next';
import {
  Card, Input, Select as UiSelect, Switch,
  FormField, Tooltip, Stack, Grid, Button,
} from '@/components/ui';
import { FormInput } from '@/lib/forms';
import {
  buildConditionalApiConfig,
  emptyConditionalFormState,
  hydrateConditionalEditorState,
  tryParseConditionalFormFromConfig,
  validateSplitWeights,
  type StrategyType,
} from '../_shared/routing-rule-config';
import { ConditionalRoutingEditor } from '../editor/ConditionalRoutingEditor';
import { RoutingPrimaryWinnerCallout } from '../_shared/RoutingPrimaryWinnerCallout';
import { MatchConditionExtraFields } from '../editor/MatchConditionExtraFields';
import {
  ROUTING_RULE_FIELD_HELP,
  RoutingStrategyTypesHelp,
  strategyConfigHelpBody,
} from '../_shared/routing-rule-field-help';
import { ProviderModelSelect, MatchModelSelector } from './RoutingRuleHelpers';
import { useStrategyOptions } from '../_shared/useStrategyOptions';
import { RetryPolicySection } from '../form/RetryPolicySection';
import type { RoutingRuleDetailState } from './useRoutingRuleDetail';
import styles from './RoutingRuleDetail.module.css';
import { HelpIconButton } from "@nexus-gateway/ui-shared";
import { EditPolicyModelSelect, EditPolicyProviderCheckboxes } from './RoutingRuleEditForm.PolicyFields';

export function RoutingRuleEditForm({ detail }: { detail: RoutingRuleDetailState }) {
  const { t } = useTranslation();
  const strategyOptions = useStrategyOptions();

  const {
    providerGroups,
    setIsEditing,

    editForm,

    entries, updateEntry, addEntry, removeEntry,
    showWeightColumn,

    models, setModels,
    matchProviders, setMatchProviders,
    matchRequestedModelLiterals, setMatchRequestedModelLiterals,
    matchModelTypes, setMatchModelTypes,
    matchVirtualKeys, setMatchVirtualKeys,
    configModelIds,

    smartState, updateSmart,

    fallbackEntries, addFallback, removeFallback, updateFallback,

    conditionalUi, setConditionalUi,

    retryPolicyMode, setRetryPolicyMode,
    retryMaxAttempts, setRetryMaxAttempts,
    retryOn, setRetryOn,
    retryPolicyInvalid,

    handleSave, saveLoading,
  } = detail;

  const editName = editForm.watch('editName');
  const editStrategyType = editForm.watch('editStrategyType') as StrategyType;
  const editEnabled = editForm.watch('editEnabled');
  const editPipelineStage = editForm.watch('editPipelineStage');
  const policyAllowM = editForm.watch('policyAllowM');
  const policyDenyM = editForm.watch('policyDenyM');
  const policyAllowP = editForm.watch('policyAllowP');
  const policyDenyP = editForm.watch('policyDenyP');
  const singleProvider = editForm.watch('singleProvider');
  const singleModel = editForm.watch('singleModel');
  const matchProjectIds = editForm.watch('matchProjectIds');

  const weightLabel = editStrategyType === 'ab_split' ? t('pages:routing.splitPercent') : t('pages:routing.weight');

  // Weighted targets (ab_split "Split %" + loadbalance "Weight") must sum to
  // exactly 100 — see validateSplitWeights.
  const hasWeightTargets = editPipelineStage === '1' && showWeightColumn;
  const weightCheck = validateSplitWeights(entries);
  const weightSumInvalid = hasWeightTargets && !weightCheck.valid;

  const switchConditionalToJson = () => {
    if (conditionalUi.mode === 'json') return;
    const built = buildConditionalApiConfig(conditionalUi.form, providerGroups);
    const obj = built.ok
      ? built.config
      : { type: 'conditional' as const, conditions: [] as unknown[], default: { type: 'single' as const } };
    setConditionalUi({ mode: 'json', text: JSON.stringify(obj, null, 2) });
  };

  const switchConditionalToForm = () => {
    if (conditionalUi.mode === 'form') return;
    try {
      const parsed: unknown = JSON.parse(conditionalUi.text);
      const form = tryParseConditionalFormFromConfig(parsed, providerGroups);
      setConditionalUi({ mode: 'form', form: form ?? emptyConditionalFormState() });
    } catch {
      setConditionalUi({ mode: 'form', form: emptyConditionalFormState() });
    }
  };

  return (
    <Stack gap="md">
      <h2 className={`${styles.widgetTitle} ${styles.editTopTitle}`}>{t('pages:routing.routingRuleInfo')}</h2>
      <Card>
        <Stack gap="md">
          {editPipelineStage === '1' && <RoutingPrimaryWinnerCallout />}
          <div className={styles.basicInfoGrid}>
            <FormInput form={editForm} name="editName" label={t('pages:routing.name')} required />
            <FormInput form={editForm} name="editDescription" label={t('pages:routing.description')} />
            {/* pipelineStage hidden — always stage 1 (route). Stage 0 (policy) is API-only. */}
            {editPipelineStage === '1' && (
              <div>
                <Stack direction="horizontal" gap="xs" align="center" className={styles.strategyLabelRow}>
                  <label
                    htmlFor="editStrategyType"
                    className={styles.strategyLabel}
                  >
                    {t('pages:routing.strategyType')}
                    <span className={styles.requiredAsterisk} aria-hidden="true">*</span>
                  </label>
                  <RoutingStrategyTypesHelp />
                </Stack>
                <UiSelect
                  value={editStrategyType}
                  onValueChange={(v) => {
                    const next = v as StrategyType;
                    if (editStrategyType !== 'conditional' && next === 'conditional') {
                      setConditionalUi(hydrateConditionalEditorState(null, providerGroups));
                    }
                    editForm.setValue('editStrategyType', next);
                  }}
                  options={strategyOptions}
                />
                {editStrategyType === 'fallback' && (
                  <div className={styles.warningCallout} role="status">
                    {ROUTING_RULE_FIELD_HELP.strategyFallbackRecoveryOnly}
                  </div>
                )}
              </div>
            )}
            <FormInput form={editForm} name="editPriority" label={t('pages:routing.priority')} type="number" tooltip={ROUTING_RULE_FIELD_HELP.priority} />
            <div className={styles.enabledSwitchField}>
              <Stack direction="horizontal" gap="xs" align="center" className={styles.labelRow}>
                <span className={`${styles.fieldLabel} ${styles.fieldLabelNoMargin}`}>{t('pages:routing.enabled')}</span>
                <Tooltip content={ROUTING_RULE_FIELD_HELP.enabled}>
                  <HelpIconButton aria-label={t('pages:routing.ariaHelpEnabled')} />
                </Tooltip>
              </Stack>
              <Switch checked={editEnabled} onCheckedChange={(v) => editForm.setValue('editEnabled', v)} />
            </div>
          </div>
        </Stack>
      </Card>
      {/* Strategy or policy configuration */}
      <Stack
        direction="horizontal"
        gap="sm"
        align="center"
        className={clsx(styles.editSectionTitleOutside, editStrategyType === 'conditional' && styles.titleActionRow)}
      >
        <Stack direction="horizontal" gap="sm" align="center">
          <div className={styles.editSectionTitleInline}>
            {editPipelineStage === '0' && t('pages:routing.policyNarrowing')}
            {editPipelineStage === '1' && editStrategyType === 'single' && t('pages:routing.providerConfiguration')}
            {editPipelineStage === '1' && editStrategyType === 'fallback' && t('pages:routing.fallbackChainTitle')}
            {editPipelineStage === '1' && editStrategyType === 'loadbalance' && t('pages:routing.loadBalanceTargets')}
            {editPipelineStage === '1' && editStrategyType === 'conditional' && t('pages:routing.conditionalRouting')}
            {editPipelineStage === '1' && editStrategyType === 'ab_split' && t('pages:routing.abSplitTargets')}
            {editPipelineStage === '1' && editStrategyType === 'smart' && t('pages:routing.intelligentRoutingConfig')}
          </div>
          <Tooltip content={editPipelineStage === '0' ? strategyConfigHelpBody.policy : strategyConfigHelpBody[editStrategyType]}>
            <HelpIconButton aria-label={t('pages:routing.ariaHelpRoutingConfig')} />
          </Tooltip>
        </Stack>
        {editPipelineStage === '1' && editStrategyType === 'conditional' && (
          <button
            type="button"
            onClick={conditionalUi.mode === 'json' ? switchConditionalToForm : switchConditionalToJson}
            className={styles.addInlineTextButton}
          >
            {conditionalUi.mode === 'json' ? t('pages:routing.useStructuredEditor') : t('pages:routing.editAsRawJson')}
          </button>
        )}
      </Stack>
      <div className={clsx(styles.editSection, editStrategyType === 'conditional' && styles.conditionalEditSection)}>
        {editPipelineStage === '0' && (
          <Stack gap="md">
            <EditPolicyModelSelect
              selected={policyAllowM}
              onChange={(v) => editForm.setValue('policyAllowM', v)}
              providerGroups={providerGroups}
              label={t('pages:routing.allowModelIds')}
            />
            <EditPolicyModelSelect
              selected={policyDenyM}
              onChange={(v) => editForm.setValue('policyDenyM', v)}
              providerGroups={providerGroups}
              label={t('pages:routing.denyModelIds')}
            />
            <EditPolicyProviderCheckboxes
              selected={policyAllowP}
              onChange={(v) => editForm.setValue('policyAllowP', v)}
              providerGroups={providerGroups}
              label={t('pages:routing.allowProviderIds')}
            />
            <EditPolicyProviderCheckboxes
              selected={policyDenyP}
              onChange={(v) => editForm.setValue('policyDenyP', v)}
              providerGroups={providerGroups}
              label={t('pages:routing.denyProviderIds')}
            />
          </Stack>
        )}

        {editPipelineStage === '1' && editStrategyType === 'single' && (
          <ProviderModelSelect
            providerValue={singleProvider}
            modelValue={singleModel}
            onProviderChange={(v) => editForm.setValue('singleProvider', v)}
            onModelChange={(v) => editForm.setValue('singleModel', v)}
            providerGroups={providerGroups}
          />
        )}

        {editPipelineStage === '1' && editStrategyType === 'conditional' && (
          <ConditionalRoutingEditor
            value={conditionalUi}
            onChange={setConditionalUi}
            providerGroups={providerGroups}
            hideModeToggle
          />
        )}

        {editPipelineStage === '1' && editStrategyType === 'smart' && (
          <Stack gap="md">
            <div className={styles.smartFieldTitle}>{t('pages:routing.routerModel')}</div>
            <ProviderModelSelect
              providerValue={smartState.routerProvider}
              modelValue={smartState.routerModel}
              onProviderChange={(v) => updateSmart({ routerProvider: v, routerModel: '' })}
              onModelChange={(v) => updateSmart({ routerModel: v })}
              providerGroups={providerGroups}
            />
            <div>
              <Stack direction="horizontal" gap="xs" align="center" className={styles.labelRow}>
                <label className={`${styles.fieldLabel} ${styles.fieldLabelNoMargin}`}>{t('pages:routing.systemPrompt')}</label>
                <Tooltip content={t('pages:routing.systemPromptTooltip')}>
                  <HelpIconButton aria-label={t('pages:routing.ariaHelpSystemPrompt')} />
                </Tooltip>
              </Stack>
              <textarea
                value={smartState.systemPrompt}
                onChange={(e) => updateSmart({ systemPrompt: e.target.value })}
                rows={10}
                className={styles.smartPromptTextarea}
              />
            </div>
            <Grid columns={3} gap="md">
              <FormField label={t('pages:routing.temperature')}>
                <Input type="number" value={smartState.temperature} onChange={(e) => updateSmart({ temperature: e.target.value })} />
              </FormField>
              <FormField label={t('pages:routing.maxTokens')}>
                <Input type="number" value={smartState.maxTokens} onChange={(e) => updateSmart({ maxTokens: e.target.value })} />
              </FormField>
              <FormField label={t('pages:routing.timeoutMs')}>
                <Input type="number" value={smartState.timeoutMs} onChange={(e) => updateSmart({ timeoutMs: e.target.value })} />
              </FormField>
            </Grid>
            <div className={styles.smartFieldTitle}>{t('pages:routing.defaultModelFallback')}</div>
            <ProviderModelSelect
              providerValue={smartState.defaultProvider}
              modelValue={smartState.defaultModel}
              onProviderChange={(v) => updateSmart({ defaultProvider: v, defaultModel: '' })}
              onModelChange={(v) => updateSmart({ defaultModel: v })}
              providerGroups={providerGroups}
            />
          </Stack>
        )}

        {editPipelineStage === '1' && (editStrategyType === 'fallback' || editStrategyType === 'loadbalance' || editStrategyType === 'ab_split') && (
          <>
            <div className={styles.entryRowHeader}>
              <span className={styles.flexGrow2}>{t('pages:routing.providerModel')}</span>
              {showWeightColumn && <span className={styles.fixedWidth80}>{weightLabel}</span>}
              <span className={styles.fixedWidth70} />
            </div>
            {entries.map((entry, idx) => (
              <div key={idx} className={styles.row}>
                <ProviderModelSelect
                  providerValue={entry.provider}
                  modelValue={entry.model}
                  onProviderChange={v => updateEntry(idx, 'provider', v)}
                  onModelChange={v => updateEntry(idx, 'model', v)}
                  providerGroups={providerGroups}
                  className={styles.flexGrow2}
                />
                {showWeightColumn && <Input type="number" placeholder={t('pages:routing.placeholderWeight')} value={entry.weight} onChange={e => updateEntry(idx, 'weight', e.target.value)} className={clsx(styles.nativeInput, styles.fixedWidth80)} />}
                <button type="button" onClick={() => removeEntry(idx)} disabled={entries.length <= 1} className={clsx(entries.length <= 1 ? styles.removeBtnDisabled : styles.removeBtn, styles.fixedWidth70)}>{t('pages:routing.remove')}</button>
              </div>
            ))}
            <button type="button" onClick={addEntry} className={styles.smallBtn}>{t('pages:routing.addTarget')}</button>
            {weightSumInvalid && (
              <p className={styles.weightSumError} role="alert">
                {t('pages:routing.weightSumError', { total: weightCheck.total })}
              </p>
            )}
          </>
        )}
      </div>

      {/* Fallback Chain */}
      <Stack direction="horizontal" gap="sm" align="center" className={`${styles.editSectionTitleOutside} ${styles.titleActionRow}`}>
        <Stack direction="horizontal" gap="sm" align="center">
          <div className={styles.editSectionTitleInline}>{t('pages:routing.fallbackChainTitle')}</div>
          <Tooltip content={t('pages:routing.fallbackChainTooltipShort')}>
            <HelpIconButton aria-label={t('pages:routing.ariaHelpFallbackChain')} />
          </Tooltip>
        </Stack>
        <button type="button" onClick={addFallback} className={styles.addInlineTextButton}>{t('pages:routing.addFallback')}</button>
      </Stack>
      <div className={clsx(styles.editSection, styles.matchConditionsSection)}>
        {fallbackEntries.length === 0 ? (
          <div className={styles.emptyMessage}>
            {t('pages:routing.noFallbackModels')}
          </div>
        ) : (
          fallbackEntries.map((entry, idx) => (
            <div key={idx} className={styles.rowCompact}>
              <span className={styles.fallbackIndexEdit}>{idx + 1}.</span>
              <ProviderModelSelect
                providerValue={entry.provider}
                modelValue={entry.model}
                onProviderChange={(v) => updateFallback(idx, 'provider', v)}
                onModelChange={(v) => updateFallback(idx, 'model', v)}
                providerGroups={providerGroups}
                className={styles.flexGrow2}
              />
              <button type="button" onClick={() => removeFallback(idx)} className={clsx(styles.removeBtn, styles.fixedWidth70)}>{t('pages:routing.remove')}</button>
            </div>
          ))
        )}
      </div>

      {/* Retry Policy */}
      {editPipelineStage === '1' && (
        <>
          <Stack direction="horizontal" gap="sm" align="center" className={styles.editSectionTitleOutside}>
            <div className={styles.editSectionTitleInline}>{t('pages:routing.retryPolicy.title')}</div>
          </Stack>
          <RetryPolicySection
            mode={retryPolicyMode}
            onModeChange={setRetryPolicyMode}
            maxAttempts={retryMaxAttempts}
            onMaxAttemptsChange={setRetryMaxAttempts}
            retryOn={retryOn}
            onRetryOnChange={setRetryOn}
            hideTitle
          />
        </>
      )}

      {/* Match Conditions */}
      <Stack direction="horizontal" gap="sm" align="center" className={styles.editSectionTitleOutside}>
        <div className={styles.editSectionTitleInline}>{t('pages:routing.matchConditions')}</div>
        <Tooltip content={ROUTING_RULE_FIELD_HELP.matchConditions}>
          <HelpIconButton aria-label={t('pages:routing.ariaHelpMatchConditions')} />
        </Tooltip>
      </Stack>
      <div className={clsx(styles.editSection, styles.matchConditionsSection)}>
        <div className={styles.threeColumnInputGrid}>
          <div className={styles.matchFieldGroup}>
            <Stack direction="horizontal" gap="xs" align="center" className={styles.labelRow}>
              <label className={`${styles.fieldLabel} ${styles.fieldLabelNoMargin}`}>
                {t('pages:routing.matchModelsLabel')}
              </label>
              <Tooltip content={ROUTING_RULE_FIELD_HELP.matchModelsLabel}>
                <HelpIconButton aria-label={t('pages:routing.ariaHelpMatchModels')} />
              </Tooltip>
            </Stack>
            <MatchModelSelector
              selected={models}
              onChange={setModels}
              providerGroups={providerGroups}
              excludeModels={configModelIds}
              className={styles.matchControlField}
            />
          </div>
          <MatchConditionExtraFields
            providerGroups={providerGroups}
            selectedProviderIds={matchProviders}
            onChangeProviders={setMatchProviders}
            projectIds={matchProjectIds ?? []}
            onChangeProjectIds={(v) => editForm.setValue('matchProjectIds', v)}
            requestedModelLiterals={matchRequestedModelLiterals}
            onChangeRequestedModelLiterals={setMatchRequestedModelLiterals}
            modelTypes={matchModelTypes}
            onChangeModelTypes={setMatchModelTypes}
            virtualKeys={matchVirtualKeys}
            onChangeVirtualKeys={setMatchVirtualKeys}
          />
        </div>
      </div>

      <Stack direction="horizontal" gap="sm" className={styles.routingEditActions}>
        <Button
          onClick={handleSave}
          disabled={saveLoading || !editName || retryPolicyInvalid || weightSumInvalid}
        >
          {saveLoading ? t('pages:routing.saving') : t('common:save')}
        </Button>
        <Button variant="secondary" onClick={() => setIsEditing(false)}>{t('common:cancel')}</Button>
      </Stack>
    </Stack>
  );
}
