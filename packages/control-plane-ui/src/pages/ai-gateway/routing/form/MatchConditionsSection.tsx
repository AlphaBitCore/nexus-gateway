import { useMemo } from 'react';
import { useTranslation } from 'react-i18next';
import { Tooltip, Card } from '@/components/ui';
import { MultiSelectDropdown } from '@/components/ui/MultiSelectDropdown';
import type { AdminModelsByProvider } from '@/api/types';
import { MatchConditionExtraFields } from '../editor/MatchConditionExtraFields';
import { ROUTING_RULE_FIELD_HELP } from '../_shared/routing-rule-field-help';
import styles from './RoutingRuleForm.module.css';
import { HelpIconButton } from '@nexus-gateway/ui-shared';

/** Searchable multi-select for Match Conditions models. Options are grouped
 *  by provider; models already used in configuration (primary/fallback) are
 *  filtered out via `excludeModels`. */
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
  const options = useMemo(() => {
    const out: { value: string; label: string; group: string }[] = [];
    for (const g of providerGroups) {
      const providerLabel = g.provider?.displayName?.trim() || g.provider?.name || '';
      for (const m of g.models ?? []) {
        if (excludeModels.has(m.id) && !selected.includes(m.id)) continue;
        out.push({
          value: m.id,
          label: `${m.name} (${m.providerModelId})`,
          group: providerLabel,
        });
      }
    }
    return out;
  }, [providerGroups, excludeModels, selected]);

  return (
    <MultiSelectDropdown
      label=""
      options={options}
      value={selected}
      onChange={onChange}
      emptyLabel={t('pages:routing.addModelToMatch')}
      searchable
      searchPlaceholder={t('common:searchModels')}
    />
  );
}

export interface MatchConditionsSectionProps {
  models: string[];
  setModels: (v: string[]) => void;
  matchProviders: string[];
  setMatchProviders: (v: string[]) => void;
  matchProjectIds: string[];
  setMatchProjectIds: (v: string[]) => void;
  matchRequestedModelLiterals: string[];
  setMatchRequestedModelLiterals: (v: string[]) => void;
  matchModelTypes: string[];
  setMatchModelTypes: (v: string[]) => void;
  matchVirtualKeys: string[];
  setMatchVirtualKeys: (v: string[]) => void;
  providerGroups: AdminModelsByProvider[];
  configModelIds: Set<string>;
}

export function MatchConditionsSection({
  models,
  setModels,
  matchProviders,
  setMatchProviders,
  matchProjectIds,
  setMatchProjectIds,
  matchRequestedModelLiterals,
  setMatchRequestedModelLiterals,
  matchModelTypes,
  setMatchModelTypes,
  matchVirtualKeys,
  setMatchVirtualKeys,
  providerGroups,
  configModelIds,
}: MatchConditionsSectionProps) {
  const { t } = useTranslation();

  return (
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
          selected={models}
          onChange={setModels}
          providerGroups={providerGroups}
          excludeModels={configModelIds}
        />
      </div>
      <MatchConditionExtraFields
        providerGroups={providerGroups}
        selectedProviderIds={matchProviders}
        onChangeProviders={setMatchProviders}
        projectIds={matchProjectIds}
        onChangeProjectIds={setMatchProjectIds}
        requestedModelLiterals={matchRequestedModelLiterals}
        onChangeRequestedModelLiterals={setMatchRequestedModelLiterals}
        modelTypes={matchModelTypes}
        onChangeModelTypes={setMatchModelTypes}
        virtualKeys={matchVirtualKeys}
        onChangeVirtualKeys={setMatchVirtualKeys}
      />
    </Card>
  );
}
