import { useTranslation } from 'react-i18next';
import { Button, Input, Tooltip } from '@/components/ui';
import type { AdminModelsByProvider } from '@/api/types';
import {
  buildConditionalApiConfig,
  CONDITIONAL_FIELD_PATH_PRESETS,
  emptyConditionalFormState,
  tryParseConditionalFormFromConfig,
  type ConditionalBranchFormRow,
  type ConditionalEditorHydration,
  type ConditionalFormState,
  type ConditionalWhenOperator,
  newConditionalBranchRow,
} from '../_shared/routing-rule-config';
import { ROUTING_RULE_FIELD_HELP } from '../_shared/routing-rule-field-help';
import styles from './ConditionalRoutingEditor.module.css';
import { HelpIconButton } from '@nexus-gateway/ui-shared';

function useOperatorOptions() {
  const { t } = useTranslation();
  return [
    { value: '$eq' as ConditionalWhenOperator, label: t('pages:routing.opEquals') },
    { value: '$ne' as ConditionalWhenOperator, label: t('pages:routing.opNotEquals') },
    { value: '$gt' as ConditionalWhenOperator, label: t('pages:routing.opGreaterThan') },
    { value: '$gte' as ConditionalWhenOperator, label: t('pages:routing.opGreaterOrEqual') },
    { value: '$lt' as ConditionalWhenOperator, label: t('pages:routing.opLessThan') },
    { value: '$lte' as ConditionalWhenOperator, label: t('pages:routing.opLessOrEqual') },
    { value: '$in' as ConditionalWhenOperator, label: t('pages:routing.opInList') },
    { value: '$nin' as ConditionalWhenOperator, label: t('pages:routing.opNotInList') },
    { value: '$regex' as ConditionalWhenOperator, label: t('pages:routing.opRegex') },
  ];
}

function ProviderModelSelect({
  providerValue,
  modelValue,
  onProviderChange,
  onModelChange,
  providerGroups,
  style,
}: {
  providerValue: string;
  modelValue: string;
  onProviderChange: (v: string) => void;
  onModelChange: (v: string) => void;
  providerGroups: AdminModelsByProvider[];
  style?: React.CSSProperties;
}) {
  const { t } = useTranslation();
  const modelsForProvider = providerGroups.find((g) => g.provider?.name === providerValue)?.models ?? [];

  const noModels = Boolean(providerValue) && modelsForProvider.length === 0;

  return (
    <div className={styles.providerModelCol} style={style}>
      <div className={styles.providerModelRow}>
        <select
          value={providerValue}
          onChange={(e) => {
            onProviderChange(e.target.value);
          }}
          className={styles.selectInput}
        >
          <option value="">{t('pages:routing.selectProvider')}</option>
          {providerGroups.map((g) => (
            <option key={g.provider?.id} value={g.provider?.name}>
              {g.provider?.displayName?.trim() || g.provider?.name} ({g?.models?.length})
            </option>
          ))}
        </select>
        <select
          value={modelValue}
          onChange={(e) => onModelChange(e.target.value)}
          className={styles.selectInput}
          disabled={!providerValue || noModels}
        >
          <option value="">{providerValue ? (noModels ? t('pages:routing.noModelsYet') : t('pages:routing.selectModel')) : t('pages:routing.selectProviderFirst')}</option>
          {modelsForProvider.map((m) => (
            <option key={m.id} value={m.providerModelId}>
              {m.name} ({m.providerModelId})
            </option>
          ))}
        </select>
      </div>
      {noModels ? (
        <p className={styles.noModelsHint}>
          {t('pages:routing.noModelsHint')}
        </p>
      ) : null}
    </div>
  );
}

function updateForm(
  form: ConditionalFormState,
  patch: Partial<ConditionalFormState>,
): ConditionalFormState {
  return { ...form, ...patch };
}

function updateBranch(
  form: ConditionalFormState,
  id: string,
  patch: Partial<ConditionalBranchFormRow>,
): ConditionalFormState {
  return {
    ...form,
    branches: form.branches.map((b) => (b.id === id ? { ...b, ...patch } : b)),
  };
}

export function ConditionalRoutingEditor({
  value,
  onChange,
  providerGroups,
}: {
  value: ConditionalEditorHydration;
  onChange: (next: ConditionalEditorHydration) => void;
  providerGroups: AdminModelsByProvider[];
}) {
  const { t } = useTranslation();
  const OPERATOR_OPTIONS = useOperatorOptions();
  const switchToJson = () => {
    if (value.mode === 'json') return;
    const built = buildConditionalApiConfig(value.form, providerGroups);
    const obj = built.ok
      ? built.config
      : { type: 'conditional' as const, conditions: [] as unknown[], default: { type: 'single' as const } };
    onChange({ mode: 'json', text: JSON.stringify(obj, null, 2) });
  };

  const switchToForm = () => {
    if (value.mode === 'form') return;
    try {
      const parsed: unknown = JSON.parse(value.text);
      const form = tryParseConditionalFormFromConfig(parsed, providerGroups);
      onChange({ mode: 'form', form: form ?? emptyConditionalFormState() });
    } catch {
      onChange({ mode: 'form', form: emptyConditionalFormState() });
    }
  };

  if (value.mode === 'json') {
    return (
      <div className={styles.colGapMd}>
        <div className={styles.warningBanner}>
          {t('pages:routing.conditionalJsonWarning')}
        </div>
        <div className={styles.fieldGroup}>
          <div className={styles.labelRow}>
            <label className={styles.fieldLabel}>{t('pages:routing.conditionalStrategyJson')}</label>
            <Tooltip
              content={ROUTING_RULE_FIELD_HELP.configurationConditional}
            >
              <HelpIconButton aria-label={t('pages:routing.helpConditionalRouting')} />
            </Tooltip>
          </div>
          <textarea
            value={value.text}
            onChange={(e) => onChange({ mode: 'json', text: e.target.value })}
            rows={18}
            spellCheck={false}
            className={styles.jsonTextarea}
          />
        </div>
        <Button variant="secondary" size="sm" onClick={switchToForm}>
          {t('pages:routing.useStructuredEditor')}
        </Button>
      </div>
    );
  }

  const form = value.form;
  const setForm = (next: ConditionalFormState) => onChange({ mode: 'form', form: next });

  return (
    <div className={styles.colGapLg}>
      <div className={styles.justifyEnd}>
        <Button variant="secondary" size="sm" onClick={switchToJson}>
          {t('pages:routing.editAsRawJson')}
        </Button>
      </div>

      <div>
        <div className={styles.sectionLabel}>{t('pages:routing.defaultRoute')}</div>
        <p className={styles.sectionHint}>
          {t('pages:routing.defaultRouteHint')}
        </p>
        <ProviderModelSelect
          providerValue={form.defaultProvider}
          modelValue={form.defaultModel}
          onProviderChange={(v) => setForm(updateForm(form, { defaultProvider: v, defaultModel: '' }))}
          onModelChange={(v) => setForm(updateForm(form, { defaultModel: v }))}
          providerGroups={providerGroups}
        />
      </div>

      <div>
        <div className={styles.sectionLabel}>{t('pages:routing.conditions')}</div>
        <p className={styles.conditionsHint}>
          {t('pages:routing.conditionsHint')}
        </p>

        {form.branches.map((branch, index) => (
          <div key={branch.id} className={styles.branchCard}>
            <div className={styles.branchTitle}>{t('pages:routing.branch', { index: index + 1 })}</div>
            <div className={styles.branchGrid}>
              <div className={styles.fieldGroup}>
                <label className={styles.fieldLabel}>{t('pages:routing.whenFieldPath')}</label>
                <div className={styles.fieldPathRow}>
                  <select
                    value={CONDITIONAL_FIELD_PATH_PRESETS.includes(branch.fieldPath) ? branch.fieldPath : '__custom__'}
                    onChange={(e) => {
                      const v = e.target.value;
                      if (v === '__custom__') return;
                      setForm(updateBranch(form, branch.id, { fieldPath: v }));
                    }}
                    className={styles.fieldPathSelect}
                  >
                    <option value="__custom__">{t('pages:routing.customPath')}</option>
                    {CONDITIONAL_FIELD_PATH_PRESETS.map((p) => (
                      <option key={p} value={p}>
                        {p}
                      </option>
                    ))}
                  </select>
                  <Input
                    type="text"
                    placeholder={t('pages:routing.placeholderFieldPath')}
                    value={branch.fieldPath}
                    onChange={(e) => setForm(updateBranch(form, branch.id, { fieldPath: e.target.value }))}
                    className={styles.fieldPathInput}
                  />
                </div>
              </div>
              <div className={styles.operatorValueGrid}>
                <div className={styles.fieldGroup}>
                  <label className={styles.fieldLabel}>{t('pages:routing.operator')}</label>
                  <select
                    value={branch.operator}
                    onChange={(e) =>
                      setForm(
                        updateBranch(form, branch.id, { operator: e.target.value as ConditionalWhenOperator }),
                      )
                    }
                    className={styles.selectInputFull}
                  >
                    {OPERATOR_OPTIONS.map((o) => (
                      <option key={o.value} value={o.value}>
                        {o.label}
                      </option>
                    ))}
                  </select>
                </div>
                <div className={styles.fieldGroup}>
                  <div className={styles.labelRow}>
                    <label className={styles.fieldLabel}>{t('pages:routing.value')}</label>
                    <span className={styles.valueHint}>
                      {t('pages:routing.valueHint')}
                    </span>
                  </div>
                  <Input
                    type="text"
                    value={branch.value}
                    onChange={(e) => setForm(updateBranch(form, branch.id, { value: e.target.value }))}
                    className={styles.textInputFull}
                  />
                </div>
              </div>
            </div>
            <div className={styles.thenLabel}>{t('pages:routing.thenRouteTo')}</div>
            <ProviderModelSelect
              providerValue={branch.thenProvider}
              modelValue={branch.thenModel}
              onProviderChange={(v) => setForm(updateBranch(form, branch.id, { thenProvider: v, thenModel: '' }))}
              onModelChange={(v) => setForm(updateBranch(form, branch.id, { thenModel: v }))}
              providerGroups={providerGroups}
            />
            <div className={styles.removeSection}>
              <Button
                variant="danger"
                size="sm"
                onClick={() =>
                  setForm({
                    ...form,
                    branches: form.branches.filter((b) => b.id !== branch.id),
                  })
                }
                disabled={form.branches.length <= 1}
              >
                {t('pages:routing.removeBranch')}
              </Button>
            </div>
          </div>
        ))}

        <Button
          variant="secondary"
          size="sm"
          onClick={() => setForm({ ...form, branches: [...form.branches, newConditionalBranchRow()] })}
        >
          {t('pages:routing.addConditionBranch')}
        </Button>
      </div>
    </div>
  );
}
