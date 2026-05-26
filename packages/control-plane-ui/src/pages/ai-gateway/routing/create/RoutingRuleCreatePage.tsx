import React, { useState } from 'react';
import clsx from 'clsx';
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
import type { AdminModelsByProvider } from '@/api/types';
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
import { HelpIconButton, IconButton } from "@nexus-gateway/ui-shared";

const WIZARD_STEP_KEYS = [
  'wizardStepBasicInfo',
  'wizardStepConfiguration',
  'wizardStepFallback',
  'wizardStepMatchConditions',
] as const;
const WIZARD_TOTAL_STEPS = WIZARD_STEP_KEYS.length;

function WizardStepBar({ current, onStepClick }: { current: number; onStepClick: (step: number) => void }) {
  const { t } = useTranslation();
  const labels = WIZARD_STEP_KEYS.map(k => t(`pages:routing.${k}`));

  return (
    <div className={styles.wizardStepBar}>
      {labels.map((label, i) => {
        const isActive = i === current;
        const isCompleted = i < current;

        return (
          <div key={label} className={styles.wizardStepItem} onClick={() => onStepClick(i)} role="button" tabIndex={0} onKeyDown={(e) => { if (e.key === 'Enter' || e.key === ' ') onStepClick(i); }}>
            <div className={styles.wizardStepContent}>
              <div
                className={clsx(
                  styles.wizardStepCircle,
                  isActive && styles.wizardStepCircleActive,
                  isCompleted && styles.wizardStepCircleCompleted,
                  !isActive && !isCompleted && styles.wizardStepCirclePending,
                )}
              >
                {isCompleted ? (
                  <svg width="14" height="14" viewBox="0 0 24 24" fill="none" stroke="currentColor" strokeWidth="2.5" strokeLinecap="round" strokeLinejoin="round" aria-hidden>
                    <path d="M20 6L9 17l-5-5" />
                  </svg>
                ) : (
                  i + 1
                )}
              </div>
              <span
                className={clsx(
                  isActive && styles.wizardStepLabelActiveColor,
                  isCompleted && styles.wizardStepLabelCompleted,
                  !isActive && !isCompleted && styles.wizardStepLabelMuted,
                )}
              >
                {label}
              </span>
            </div>
            {i < labels.length - 1 && (
              <div
                className={clsx(
                  styles.wizardStepConnector,
                  isCompleted ? styles.wizardStepConnectorCompleted : styles.wizardStepConnectorPending,
                )}
              />
            )}
          </div>
        );
      })}
    </div>
  );
}

function useStrategyOptions() {
  const { t } = useTranslation();
  return [
    { value: 'single', label: t('pages:routing.strategySingle') },
    { value: 'fallback', label: t('pages:routing.strategyFallback') },
    { value: 'loadbalance', label: t('pages:routing.strategyLoadbalance') },
    { value: 'conditional', label: t('pages:routing.strategyConditional') },
    { value: 'ab_split', label: t('pages:routing.strategyAbSplit') },
    { value: 'smart', label: t('pages:routing.strategySmart') },
  ];
}

/** Model selector for Match Conditions — excludes models already used in Configuration */
function MatchModelSelector({
  selected,
  onChange,
  providerGroups,
  excludeModels,
}: {
  selected: string[];
  onChange: (v: string[]) => void;
  providerGroups: AdminModelsByProvider[];
  excludeModels: Set<string>;
}) {
  const { t } = useTranslation();
  const handleAdd = (e: React.ChangeEvent<HTMLSelectElement>) => {
    const val = e.target.value;
    if (val && !selected.includes(val)) {
      onChange([...selected, val]);
    }
    e.target.value = '';
  };

  const handleRemove = (model: string) => {
    onChange(selected.filter(m => m !== model));
  };

  const labelMap = new Map<string, string>();
  for (const g of providerGroups) {
    for (const m of g.models) {
      labelMap.set(m.id, `${g.provider?.displayName?.trim() || g.provider?.name} / ${m.name}`);
    }
  }

  return (
    <div>
      <div className={`${styles.tagContainer} ${selected.length === 0 ? styles.tagContainerHidden : styles.tagContainerVisible}`}>
        {selected.map(modelId => (
          <span key={modelId} className={styles.tag}>
            {labelMap.get(modelId) ?? modelId}
            <IconButton size="sm" aria-label="Remove" onClick={() => handleRemove(modelId)}>×</IconButton>
          </span>
        ))}
      </div>
      <select onChange={handleAdd} value="" className={styles.selectInputFull}>
        <option value="">{t('pages:routing.addModelToMatch')}</option>
        {providerGroups.map(g => {
          const available = g?.models?.filter(m => !selected.includes(m.id) && !excludeModels.has(m.id));
          if (available.length === 0) return null;
          return (
            <optgroup key={g.provider?.id} label={g.provider?.displayName?.trim() || g.provider?.name}>
              {available.map(m => (
                <option key={m.id} value={m.id}>
                  {m.name} ({m.providerModelId})
                </option>
              ))}
            </optgroup>
          );
        })}
      </select>
    </div>
  );
}

function CreatePolicyModelSelect({
  selected,
  onChange,
  providerGroups,
  label,
}: {
  selected: string[];
  onChange: (v: string[]) => void;
  providerGroups: AdminModelsByProvider[];
  label: string;
}) {
  const { t } = useTranslation();
  const handleAdd = (e: React.ChangeEvent<HTMLSelectElement>) => {
    const val = e.target.value;
    if (val && !selected.includes(val)) {
      onChange([...selected, val]);
    }
    e.target.value = '';
  };

  const handleRemove = (id: string) => {
    onChange(selected.filter(m => m !== id));
  };

  const labelMap = new Map<string, string>();
  for (const g of providerGroups) {
    for (const m of g.models) {
      labelMap.set(m.id, `${g.provider?.displayName?.trim() || g.provider?.name} / ${m.name}`);
    }
  }

  return (
    <div className={styles.fieldGroup}>
      <label className={styles.fieldLabel}>{label}</label>
      {selected.length > 0 && (
        <div className={`${styles.tagContainer} ${styles.tagContainerVisible}`}>
          {selected.map(id => (
            <span key={id} className={styles.tag}>
              {labelMap.get(id) ?? id}
              <IconButton size="sm" aria-label="Remove" onClick={() => handleRemove(id)}>×</IconButton>
            </span>
          ))}
        </div>
      )}
      <select onChange={handleAdd} value="" className={styles.selectInputFull}>
        <option value="">{t('pages:routing.addModelToPolicy')}</option>
        {providerGroups.map(g => {
          const available = g?.models?.filter(m => !selected.includes(m.id));
          if (!available || available.length === 0) return null;
          return (
            <optgroup key={g.provider?.id} label={g.provider?.displayName?.trim() || g.provider?.name}>
              {available.map(m => (
                <option key={m.id} value={m.id}>
                  {m.name} ({m.providerModelId})
                </option>
              ))}
            </optgroup>
          );
        })}
      </select>
    </div>
  );
}

function CreatePolicyProviderCheckboxes({
  selected,
  onChange,
  providerGroups,
  label,
}: {
  selected: string[];
  onChange: (v: string[]) => void;
  providerGroups: AdminModelsByProvider[];
  label: string;
}) {
  const toggle = (id: string) => {
    if (selected.includes(id)) {
      onChange(selected.filter(x => x !== id));
    } else {
      onChange([...selected, id]);
    }
  };

  return (
    <div className={styles.fieldGroup}>
      <label className={styles.fieldLabel}>{label}</label>
      <div style={{ display: 'flex', flexWrap: 'wrap', gap: 'var(--g-space-2) var(--g-space-4)', marginTop: 'var(--g-space-1)' }}>
        {providerGroups
          .filter(g => g.provider?.enabled)
          .map(g => (
            <label key={g.provider?.id} style={{ display: 'flex', alignItems: 'center', gap: 'var(--g-space-1)', fontSize: 'var(--g-font-size-sm)' }}>
              <input
                type="checkbox"
                checked={selected.includes(g.provider?.id)}
                onChange={() => toggle(g.provider?.id)}
              />
              {g.provider?.displayName?.trim() || g.provider?.name}
            </label>
          ))}
      </div>
    </div>
  );
}

/** Provider+Model cascading selector */
function ProviderModelSelect({
  providerValue,
  modelValue,
  onProviderChange,
  onModelChange,
  providerGroups,
  className,
}: {
  providerValue: string;
  modelValue: string;
  onProviderChange: (v: string) => void;
  onModelChange: (v: string) => void;
  providerGroups: AdminModelsByProvider[];
  className?: string;
}) {
  const { t } = useTranslation();
  const modelsForProvider = providerGroups.find(g => g.provider?.name === providerValue)?.models ?? [];

  return (
    <div className={`${styles.providerModelRow} ${className ?? ''}`}>
      <select
        value={providerValue}
        onChange={e => {
          onProviderChange(e.target.value);
          onModelChange('');
        }}
        className={styles.selectInput}
      >
        <option value="">{t('pages:routing.selectProvider')}</option>
        {providerGroups.map(g => (
          <option key={g.provider?.id} value={g.provider?.name}>
            {g.provider?.displayName?.trim() || g.provider?.name} ({g?.models?.length})
          </option>
        ))}
      </select>
      <select
        value={modelValue}
        onChange={e => onModelChange(e.target.value)}
        className={styles.selectInput}
        disabled={!providerValue}
      >
        <option value="">{providerValue ? t('pages:routing.selectModel') : t('pages:routing.selectProviderFirst')}</option>
        {modelsForProvider.map(m => (
          <option key={m.id} value={m.providerModelId}>
            {m.name} ({m.providerModelId})
          </option>
        ))}
      </select>
    </div>
  );
}

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
