import { useMemo } from 'react';
import { useTranslation } from 'react-i18next';

import { STRATEGY_TYPES, type StrategyType } from './routing-rule-config';

/**
 * i18n key for each user-selectable strategy. Typed as a total Record over the
 * non-policy StrategyType union, so adding a new strategy to the union forces a
 * label key here at compile time — the picker and the list filter can never drift
 * from the strategy set.
 */
const STRATEGY_LABEL_KEY: Record<Exclude<StrategyType, 'policy'>, string> = {
  single: 'pages:routing.strategySingle',
  fallback: 'pages:routing.strategyFallback',
  loadbalance: 'pages:routing.strategyLoadbalance',
  conditional: 'pages:routing.strategyConditional',
  ab_split: 'pages:routing.strategyAbSplit',
  smart: 'pages:routing.strategySmart',
};

export interface StrategyOption {
  value: Exclude<StrategyType, 'policy'>;
  label: string;
}

/**
 * The single source of truth for the strategy {value,label} list rendered by the
 * Strategy Type picker (create/edit) and the strategy filter (list page). Both
 * derive from STRATEGY_TYPES so they always show the same six strategies with the
 * same labels — fixing the prior drift where the list filter was built from
 * whatever strategy values happened to appear in the current page of rules.
 */
export function useStrategyOptions(): StrategyOption[] {
  const { t } = useTranslation();
  return useMemo(
    () => STRATEGY_TYPES.map(value => ({ value, label: t(STRATEGY_LABEL_KEY[value]) })),
    [t],
  );
}
