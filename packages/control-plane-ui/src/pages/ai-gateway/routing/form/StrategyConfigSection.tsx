import { useTranslation } from 'react-i18next';
import { 
  Button,
  Card,
  Input,
  Stack,
  Tooltip
} from '@/components/ui';
import type { AdminModelsByProvider } from '@/api/types';
import { ConditionalRoutingEditor } from '../editor/ConditionalRoutingEditor';
import {
  type ConditionalEditorHydration,
  type StrategyType,
  type ProviderModelEntry,
  type SmartFormState,
} from '../_shared/routing-rule-config';
import {
  strategyConfigHelpBody,
} from '../_shared/routing-rule-field-help';
import styles from './RoutingRuleForm.module.css';
import { HelpIconButton, IconButton } from "@nexus-gateway/ui-shared";

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

export { ProviderModelSelect };

export interface StrategyConfigSectionProps {
  pipelineStage: string;
  strategyType: StrategyType;
  providerGroups: AdminModelsByProvider[];

  // Policy fields
  policyAllowM: string[];
  setPolicyAllowM: (v: string[]) => void;
  policyDenyM: string[];
  setPolicyDenyM: (v: string[]) => void;
  policyAllowP: string[];
  setPolicyAllowP: (v: string[]) => void;
  policyDenyP: string[];
  setPolicyDenyP: (v: string[]) => void;

  // Single-provider
  singleProvider: string;
  setSingleProvider: (v: string) => void;
  singleModel: string;
  setSingleModel: (v: string) => void;

  // Multi-entry
  entries: ProviderModelEntry[];
  updateEntry: (index: number, field: keyof ProviderModelEntry, value: string) => void;
  addEntry: () => void;
  removeEntry: (index: number) => void;

  // Conditional
  conditionalUi: ConditionalEditorHydration;
  setConditionalUi: (v: ConditionalEditorHydration) => void;

  // Smart
  smartState: SmartFormState;
  updateSmart: (patch: Partial<SmartFormState>) => void;

  // Derived
  showWeightColumn: boolean;
}

function PolicyModelSelect({
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

function PolicyProviderCheckboxes({
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

export function StrategyConfigSection({
  pipelineStage,
  strategyType,
  providerGroups,
  policyAllowM, setPolicyAllowM,
  policyDenyM, setPolicyDenyM,
  policyAllowP, setPolicyAllowP,
  policyDenyP, setPolicyDenyP,
  singleProvider, setSingleProvider,
  singleModel, setSingleModel,
  entries, updateEntry, addEntry, removeEntry,
  conditionalUi, setConditionalUi,
  smartState, updateSmart,
  showWeightColumn,
}: StrategyConfigSectionProps) {
  const { t } = useTranslation();
  const weightLabel = strategyType === 'ab_split' ? t('pages:routing.splitPercent') : t('pages:routing.weight');

  return (
    <Card padding="lg">
      <div className={`${styles.labelRow} ${styles.sectionTitleSpacing}`}>
        <div className={styles.sectionTitle}>
          {pipelineStage === '0' && t('pages:routing.policyNarrowing')}
          {pipelineStage === '1' && strategyType === 'single' && t('pages:routing.providerConfiguration')}
          {pipelineStage === '1' && strategyType === 'fallback' && t('pages:routing.fallbackChainTitle')}
          {pipelineStage === '1' && strategyType === 'loadbalance' && t('pages:routing.loadBalanceTargets')}
          {pipelineStage === '1' && strategyType === 'conditional' && t('pages:routing.conditionalRouting')}
          {pipelineStage === '1' && strategyType === 'ab_split' && t('pages:routing.abSplitTargets')}
          {pipelineStage === '1' && strategyType === 'smart' && t('pages:routing.intelligentRoutingConfig')}
        </div>
        <Tooltip
          content={pipelineStage === '0' ? strategyConfigHelpBody.policy : strategyConfigHelpBody[strategyType]}
        >
          <HelpIconButton aria-label={t('pages:routing.ariaHelpRoutingConfig')} />
        </Tooltip>
      </div>

      {pipelineStage === '0' && (
        <Stack gap="md">
          <PolicyModelSelect
            selected={policyAllowM}
            onChange={setPolicyAllowM}
            providerGroups={providerGroups}
            label={t('pages:routing.allowModelIds')}
          />
          <PolicyModelSelect
            selected={policyDenyM}
            onChange={setPolicyDenyM}
            providerGroups={providerGroups}
            label={t('pages:routing.denyModelIds')}
          />
          <PolicyProviderCheckboxes
            selected={policyAllowP}
            onChange={setPolicyAllowP}
            providerGroups={providerGroups}
            label={t('pages:routing.allowProviderIds')}
          />
          <PolicyProviderCheckboxes
            selected={policyDenyP}
            onChange={setPolicyDenyP}
            providerGroups={providerGroups}
            label={t('pages:routing.denyProviderIds')}
          />
        </Stack>
      )}

      {pipelineStage === '1' && strategyType === 'single' && (
        <ProviderModelSelect
          providerValue={singleProvider}
          modelValue={singleModel}
          onProviderChange={setSingleProvider}
          onModelChange={setSingleModel}
          providerGroups={providerGroups}
        />
      )}

      {pipelineStage === '1' && strategyType === 'conditional' && (
        <ConditionalRoutingEditor
          value={conditionalUi}
          onChange={setConditionalUi}
          providerGroups={providerGroups}
        />
      )}

      {pipelineStage === '1' && strategyType === 'smart' && (
        <Stack gap="md">
          <div className={styles.miniLabel}>{t('pages:routing.routerModel')}</div>
          <ProviderModelSelect
            providerValue={smartState.routerProvider}
            modelValue={smartState.routerModel}
            onProviderChange={(v) => updateSmart({ routerProvider: v, routerModel: '' })}
            onModelChange={(v) => updateSmart({ routerModel: v })}
            providerGroups={providerGroups}
          />
          <div className={styles.fieldGroup}>
            <div className={styles.labelRow}>
              <label className={styles.fieldLabel}>{t('pages:routing.systemPrompt')}</label>
              <Tooltip
                content={t('pages:routing.systemPromptTooltip')}
              >
                <HelpIconButton aria-label={t('pages:routing.ariaHelpSystemPrompt')} />
              </Tooltip>
            </div>
            <textarea
              value={smartState.systemPrompt}
              onChange={(e) => updateSmart({ systemPrompt: e.target.value })}
              rows={10}
              className={`${styles.monoTextarea} ${styles.resizeVertical}`}
            />
          </div>
          <div className={styles.threeColGrid}>
            <div className={styles.fieldGroup}>
              <label className={styles.fieldLabel}>{t('pages:routing.temperature')}</label>
              <Input type="number" className={styles.textInput} value={smartState.temperature} onChange={(e) => updateSmart({ temperature: e.target.value })} />
            </div>
            <div className={styles.fieldGroup}>
              <label className={styles.fieldLabel}>{t('pages:routing.maxTokens')}</label>
              <Input type="number" className={styles.textInput} value={smartState.maxTokens} onChange={(e) => updateSmart({ maxTokens: e.target.value })} />
            </div>
            <div className={styles.fieldGroup}>
              <label className={styles.fieldLabel}>{t('pages:routing.timeoutMs')}</label>
              <Input type="number" className={styles.textInput} value={smartState.timeoutMs} onChange={(e) => updateSmart({ timeoutMs: e.target.value })} />
            </div>
          </div>
          <div className={styles.miniLabel}>{t('pages:routing.defaultModelFallback')}</div>
          <ProviderModelSelect
            providerValue={smartState.defaultProvider}
            modelValue={smartState.defaultModel}
            onProviderChange={(v) => updateSmart({ defaultProvider: v, defaultModel: '' })}
            onModelChange={(v) => updateSmart({ defaultModel: v })}
            providerGroups={providerGroups}
          />
        </Stack>
      )}

      {pipelineStage === '1' &&
        (strategyType === 'fallback' || strategyType === 'loadbalance' || strategyType === 'ab_split') && (
        <>
          {/* Column headers */}
          <div className={`${styles.entryRow} ${styles.entryHeaderRow}`}>
            <span className={styles.flexGrow2}>{t('pages:routing.providerModel')}</span>
            {showWeightColumn && <span className={styles.colWidthWeight} title={t('pages:routing.weightTooltip', 'Higher weight = more traffic')}>{weightLabel}</span>}
            <span className={styles.colWidthActions} />
          </div>
          {entries.map((entry, idx) => (
            <div key={idx} className={styles.entryRow}>
              <ProviderModelSelect
                providerValue={entry.provider}
                modelValue={entry.model}
                onProviderChange={v => updateEntry(idx, 'provider', v)}
                onModelChange={v => updateEntry(idx, 'model', v)}
                providerGroups={providerGroups}
                className={styles.flexGrow2}
              />
              {showWeightColumn && (
                <Input
                  type="number"
                  placeholder={t('pages:routing.placeholderWeight')}
                  value={entry.weight}
                  onChange={(e) => updateEntry(idx, 'weight', e.target.value)}
                  className={styles.weightInput}
                />
              )}
              <Button
                variant="danger"
                size="sm"
                onClick={() => removeEntry(idx)}
                disabled={entries.length <= 1}
              >
                {t('pages:routing.remove')}
              </Button>
            </div>
          ))}
          {showWeightColumn && (
            <p className={styles.weightHint}>
              {t('pages:routing.weightHint')}
            </p>
          )}
          <Button variant="secondary" size="sm" onClick={addEntry}>
            {t('pages:routing.addTarget')}
          </Button>
        </>
      )}
    </Card>
  );
}
