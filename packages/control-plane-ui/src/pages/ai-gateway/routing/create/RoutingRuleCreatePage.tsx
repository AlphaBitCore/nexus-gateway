import { useState } from 'react';
import { useTranslation } from 'react-i18next';
import {

  Breadcrumb,
  Button,
  Card,
  Input,
  PageHeader,
  Stack,
  Switch,
  Tooltip
} from '@/components/ui';
import type { StrategyType } from '../_shared/routing-rule-config';
import { ConditionalRoutingEditor } from '../editor/ConditionalRoutingEditor';
import { RoutingPrimaryWinnerCallout } from '../_shared/RoutingPrimaryWinnerCallout';
import { MatchConditionExtraFields } from '../editor/MatchConditionExtraFields';
import {
  ROUTING_RULE_FIELD_HELP,
  RoutingStrategyTypesHelp,
  strategyConfigHelpBody,
} from '../_shared/routing-rule-field-help';
import { useRoutingRuleCreate } from './useRoutingRuleCreate';
import styles from './RoutingRuleCreate.module.css';
import { HelpIconButton } from "@nexus-gateway/ui-shared";
import { WizardStepBar, WIZARD_TOTAL_STEPS } from './WizardStepBar';
import { MatchModelSelector } from './MatchModelSelector';
import { CreatePolicyModelSelect } from './CreatePolicyModelSelect';
import { CreatePolicyProviderCheckboxes } from './CreatePolicyProviderCheckboxes';
import { ProviderModelSelect } from './ProviderModelSelect';
import { useStrategyOptions } from '../_shared/useStrategyOptions';

/* ── Component ──────────────────────────────────────────────────────────── */

export function RoutingRuleCreate() {
  const { t } = useTranslation();
  const strategyOptions = useStrategyOptions();
  const h = useRoutingRuleCreate();
  const [currentStep, setCurrentStep] = useState(0);

  const weightLabel = h.strategyType === 'ab_split' ? t('pages:routing.splitPercent') : t('pages:routing.weight');
  const isLastStep = currentStep === WIZARD_TOTAL_STEPS - 1;

  const goNext = () => setCurrentStep(s => Math.min(s + 1, WIZARD_TOTAL_STEPS - 1));
  const goBack = () => {
    if (currentStep === 0) { h.navigate('/ai-gateway/routing'); return; }
    setCurrentStep(s => s - 1);
  };

  return (
    <Stack gap="lg">
      <Breadcrumb items={[
        { label: t('pages:routing.title', 'Routing Rules'), to: '/ai-gateway/routing' },
        { label: t('pages:routing.createRule') },
      ]} />

      <PageHeader title={t('pages:routing.createRule')} subtitle={t('pages:routing.createSubtitle', 'Define a new model routing strategy')} />

      <WizardStepBar current={currentStep} onStepClick={setCurrentStep} />

      <Card padding="lg">
        <Stack gap="md">

          {/* ── Step 0: Basic Info ── */}
          <div className={currentStep !== 0 ? styles.wizardStepHidden : undefined}>
            {h.pipelineStage === '1' && <RoutingPrimaryWinnerCallout />}

            <div className={styles.fieldGroup}>
              <label className={styles.fieldLabel}>
                {t('pages:routing.name')} <span className={styles.required}>*</span>
              </label>
              <Input data-testid="routing-rule-name" className={styles.textInput} value={h.name} onChange={(e) => h.setName(e.target.value)} required />
            </div>

            <div className={styles.fieldGroup}>
              <label className={styles.fieldLabel}>{t('pages:routing.description')}</label>
              <Input className={styles.textInput} value={h.description} onChange={(e) => h.setDescription(e.target.value)} />
            </div>

            {h.pipelineStage === '1' && (
              <div>
                <div className={styles.labelRow}>
                  <label htmlFor="strategyType" className={styles.fieldLabel}>
                    {t('pages:routing.strategyType')} <span className={styles.required}>*</span>
                  </label>
                  <RoutingStrategyTypesHelp />
                </div>
                <select
                  id="strategyType"
                  value={h.strategyType}
                  onChange={(e) => h.handleStrategyChange(e.target.value as StrategyType)}
                  className={styles.selectInputFull}
                >
                  {strategyOptions.map(opt => (
                    <option key={opt.value} value={opt.value}>{opt.label}</option>
                  ))}
                </select>
                {h.strategyType === 'fallback' && (
                  <div className={styles.warningBanner} role="status">
                    {ROUTING_RULE_FIELD_HELP.strategyFallbackRecoveryOnly}
                  </div>
                )}
              </div>
            )}

            <div className={styles.fieldGroup}>
              <div className={styles.labelRow}>
                <label className={styles.fieldLabel}>{t('pages:routing.priority')}</label>
                <Tooltip content={ROUTING_RULE_FIELD_HELP.priority}>
                  <HelpIconButton aria-label={t('pages:routing.ariaHelpPriority')} />
                </Tooltip>
              </div>
              <Input className={styles.textInput} type="number" value={h.priority} onChange={(e) => h.setPriority(e.target.value)} />
            </div>

            <div className={styles.switchRow}>
              <label className={styles.fieldLabel}>{t('pages:routing.enabled')}</label>
              <Tooltip content={ROUTING_RULE_FIELD_HELP.enabled}>
                <HelpIconButton aria-label={t('pages:routing.ariaHelpEnabled')} />
              </Tooltip>
              <Switch checked={h.enabled} onCheckedChange={h.setEnabled} />
            </div>
          </div>

          {/* ── Step 1: Strategy / Provider Configuration ── */}
          <div className={currentStep !== 1 ? styles.wizardStepHidden : undefined}>
            <Card padding="lg">
              <div className={`${styles.labelRow} ${styles.sectionTitleSpacing}`}>
                <div className={styles.sectionTitle}>
                  {h.pipelineStage === '0' && t('pages:routing.policyNarrowing')}
                  {h.pipelineStage === '1' && h.strategyType === 'single' && t('pages:routing.providerConfiguration')}
                  {h.pipelineStage === '1' && h.strategyType === 'fallback' && t('pages:routing.fallbackChainTitle')}
                  {h.pipelineStage === '1' && h.strategyType === 'loadbalance' && t('pages:routing.loadBalanceTargets')}
                  {h.pipelineStage === '1' && h.strategyType === 'conditional' && t('pages:routing.conditionalRouting')}
                  {h.pipelineStage === '1' && h.strategyType === 'ab_split' && t('pages:routing.abSplitTargets')}
                  {h.pipelineStage === '1' && h.strategyType === 'smart' && t('pages:routing.intelligentRoutingConfig')}
                </div>
                <Tooltip content={h.pipelineStage === '0' ? strategyConfigHelpBody.policy : strategyConfigHelpBody[h.strategyType]}>
                  <HelpIconButton aria-label={t('pages:routing.ariaHelpRoutingConfig')} />
                </Tooltip>
              </div>

              {h.pipelineStage === '0' && (
                <Stack gap="md">
                  <CreatePolicyModelSelect
                    selected={h.policyAllowM}
                    onChange={h.setPolicyAllowM}
                    providerGroups={h.providerGroups}
                    label={t('pages:routing.allowModelIds')}
                  />
                  <CreatePolicyModelSelect
                    selected={h.policyDenyM}
                    onChange={h.setPolicyDenyM}
                    providerGroups={h.providerGroups}
                    label={t('pages:routing.denyModelIds')}
                  />
                  <CreatePolicyProviderCheckboxes
                    selected={h.policyAllowP}
                    onChange={h.setPolicyAllowP}
                    providerGroups={h.providerGroups}
                    label={t('pages:routing.allowProviderIds')}
                  />
                  <CreatePolicyProviderCheckboxes
                    selected={h.policyDenyP}
                    onChange={h.setPolicyDenyP}
                    providerGroups={h.providerGroups}
                    label={t('pages:routing.denyProviderIds')}
                  />
                </Stack>
              )}

              {h.pipelineStage === '1' && h.strategyType === 'single' && (
                <div data-testid="routing-single-config">
                  <ProviderModelSelect
                    providerValue={h.singleProvider}
                    modelValue={h.singleModel}
                    onProviderChange={h.setSingleProvider}
                    onModelChange={h.setSingleModel}
                    providerGroups={h.providerGroups}
                  />
                </div>
              )}

              {h.pipelineStage === '1' && h.strategyType === 'conditional' && (
                <ConditionalRoutingEditor
                  value={h.conditionalUi}
                  onChange={h.setConditionalUi}
                  providerGroups={h.providerGroups}
                />
              )}

              {h.pipelineStage === '1' && h.strategyType === 'smart' && (
                <Stack gap="md">
                  <div className={styles.miniLabel}>{t('pages:routing.routerModel')}</div>
                  <ProviderModelSelect
                    providerValue={h.smartState.routerProvider}
                    modelValue={h.smartState.routerModel}
                    onProviderChange={(v) => h.updateSmart({ routerProvider: v, routerModel: '' })}
                    onModelChange={(v) => h.updateSmart({ routerModel: v })}
                    providerGroups={h.providerGroups}
                  />
                  <div className={styles.fieldGroup}>
                    <div className={styles.labelRow}>
                      <label className={styles.fieldLabel}>{t('pages:routing.systemPrompt')}</label>
                      <Tooltip content={t('pages:routing.systemPromptTooltip')}>
                        <HelpIconButton aria-label={t('pages:routing.ariaHelpSystemPrompt')} />
                      </Tooltip>
                    </div>
                    <textarea
                      value={h.smartState.systemPrompt}
                      onChange={(e) => h.updateSmart({ systemPrompt: e.target.value })}
                      rows={10}
                      className={`${styles.monoTextarea} ${styles.resizeVertical}`}
                    />
                  </div>
                  <div className={styles.threeColGrid}>
                    <div className={styles.fieldGroup}>
                      <label className={styles.fieldLabel}>{t('pages:routing.temperature')}</label>
                      <Input type="number" className={styles.textInput} value={h.smartState.temperature} onChange={(e) => h.updateSmart({ temperature: e.target.value })} />
                    </div>
                    <div className={styles.fieldGroup}>
                      <label className={styles.fieldLabel}>{t('pages:routing.maxTokens')}</label>
                      <Input type="number" className={styles.textInput} value={h.smartState.maxTokens} onChange={(e) => h.updateSmart({ maxTokens: e.target.value })} />
                    </div>
                    <div className={styles.fieldGroup}>
                      <label className={styles.fieldLabel}>{t('pages:routing.timeoutMs')}</label>
                      <Input type="number" className={styles.textInput} value={h.smartState.timeoutMs} onChange={(e) => h.updateSmart({ timeoutMs: e.target.value })} />
                    </div>
                  </div>
                  <div className={styles.miniLabel}>{t('pages:routing.defaultModelFallback')}</div>
                  <ProviderModelSelect
                    providerValue={h.smartState.defaultProvider}
                    modelValue={h.smartState.defaultModel}
                    onProviderChange={(v) => h.updateSmart({ defaultProvider: v, defaultModel: '' })}
                    onModelChange={(v) => h.updateSmart({ defaultModel: v })}
                    providerGroups={h.providerGroups}
                  />
                </Stack>
              )}

              {h.pipelineStage === '1' &&
                (h.strategyType === 'fallback' || h.strategyType === 'loadbalance' || h.strategyType === 'ab_split') && (
                <>
                  <div className={`${styles.entryRow} ${styles.entryHeaderRow}`}>
                    <span className={styles.flexGrow2}>{t('pages:routing.providerModel')}</span>
                    {h.showWeightColumn && <span className={styles.colWidthWeight}>{weightLabel}</span>}
                    <span className={styles.colWidthActions} />
                  </div>
                  {h.entries.map((entry, idx) => (
                    <div key={idx} className={styles.entryRow}>
                      <ProviderModelSelect
                        providerValue={entry.provider}
                        modelValue={entry.model}
                        onProviderChange={v => h.updateEntry(idx, 'provider', v)}
                        onModelChange={v => h.updateEntry(idx, 'model', v)}
                        providerGroups={h.providerGroups}
                        className={styles.flexGrow2}
                      />
                      {h.showWeightColumn && <Input type="number" placeholder={t('pages:routing.placeholderWeight')} value={entry.weight} onChange={e => h.updateEntry(idx, 'weight', e.target.value)} className={styles.weightInput} />}
                      <Button variant="danger" size="sm" onClick={() => h.removeEntry(idx)} disabled={h.entries.length <= 1}>{t('pages:routing.remove')}</Button>
                    </div>
                  ))}
                  <Button variant="secondary" size="sm" onClick={h.addEntry}>{t('pages:routing.addTarget')}</Button>
                </>
              )}
            </Card>
          </div>

          {/* ── Step 2: Fallback Chain ── */}
          <div className={currentStep !== 2 ? styles.wizardStepHidden : undefined}>
            {h.pipelineStage !== '0' && (
              <Card padding="lg">
                <div className={`${styles.labelRow} ${styles.sectionTitleSpacing}`}>
                  <div className={styles.sectionTitle}>{t('pages:routing.fallbackChainTitle')}</div>
                  <Tooltip content={t('pages:routing.fallbackChainTooltip')}>
                    <HelpIconButton aria-label={t('pages:routing.ariaHelpFallbackChain')} />
                  </Tooltip>
                </div>
                {h.fallbackEntries.length === 0 ? (
                  <div className={styles.emptyHint}>
                    {t('pages:routing.noFallbackModelsLong')}
                  </div>
                ) : (
                  h.fallbackEntries.map((entry, idx) => (
                    <div key={idx} className={`${styles.entryRow} ${styles.fallbackEntrySpacing}`}>
                      <span className={styles.indexLabel}>{idx + 1}.</span>
                      <ProviderModelSelect
                        providerValue={entry.provider}
                        modelValue={entry.model}
                        onProviderChange={(v) => h.updateFallback(idx, 'provider', v)}
                        onModelChange={(v) => h.updateFallback(idx, 'model', v)}
                        providerGroups={h.providerGroups}
                        className={styles.flexGrow2}
                      />
                      <Button variant="danger" size="sm" onClick={() => h.removeFallback(idx)}>{t('pages:routing.remove')}</Button>
                    </div>
                  ))
                )}
                <Button variant="secondary" size="sm" onClick={h.addFallback}>{t('pages:routing.addFallback')}</Button>
              </Card>
            )}
          </div>

          {/* ── Step 3: Match Conditions ── */}
          <div className={currentStep !== 3 ? styles.wizardStepHidden : undefined}>
            <Card padding="lg">
              <div className={`${styles.labelRow} ${styles.sectionTitleSpacing}`}>
                <div className={styles.sectionTitle}>{t('pages:routing.matchConditions')}</div>
                <Tooltip content={ROUTING_RULE_FIELD_HELP.matchConditions}>
                  <HelpIconButton aria-label={t('pages:routing.ariaHelpMatchConditions')} />
                </Tooltip>
              </div>
              <div className={styles.fieldGroup}>
                <div className={styles.labelRow}>
                  <label className={styles.fieldLabel}>{t('pages:routing.matchModelsLabel')}</label>
                  <Tooltip content={ROUTING_RULE_FIELD_HELP.matchModelsLabel}>
                    <HelpIconButton aria-label={t('pages:routing.ariaHelpMatchModels')} />
                  </Tooltip>
                </div>
                <MatchModelSelector
                  selected={h.models}
                  onChange={h.setModels}
                  providerGroups={h.providerGroups}
                  excludeModels={h.configModelIds}
                />
              </div>
              <MatchConditionExtraFields
                providerGroups={h.providerGroups}
                selectedProviderIds={h.matchProviders}
                onChangeProviders={h.setMatchProviders}
                projectIds={h.matchProjectIds}
                onChangeProjectIds={h.setMatchProjectIds}
                requestedModelLiterals={h.matchRequestedModelLiterals}
                onChangeRequestedModelLiterals={h.setMatchRequestedModelLiterals}
                modelTypes={h.matchModelTypes}
                onChangeModelTypes={h.setMatchModelTypes}
                virtualKeys={h.matchVirtualKeys}
                onChangeVirtualKeys={h.setMatchVirtualKeys}
              />
            </Card>
          </div>

          {/* ── Wizard navigation footer ── */}
          <div className={styles.wizardFooter}>
            <Button variant="secondary" onClick={goBack}>
              {currentStep === 0 ? t('common:cancel') : t('common:back')}
            </Button>
            {isLastStep ? (
              <Button variant="primary" onClick={h.handleSubmit} disabled={h.loading || !h.name}>
                {h.loading ? t('pages:routing.creating', 'Creating...') : t('pages:routing.createRule')}
              </Button>
            ) : (
              <Button variant="primary" onClick={goNext}>
                {t('pages:routing.wizardContinue', 'Continue')}
              </Button>
            )}
          </div>
        </Stack>
      </Card>
    </Stack>
  );
}
