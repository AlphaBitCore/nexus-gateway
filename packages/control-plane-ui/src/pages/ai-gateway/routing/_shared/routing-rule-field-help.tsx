import { useTranslation } from 'react-i18next';
import { Tooltip } from '@/components/ui';
import type { StrategyType } from './routing-rule-config';
import styles from './routing-rule-field-help.module.css';

/** Returns i18n'd routing field help texts. Must be called inside a component. */
export function useRoutingFieldHelp() {
  const { t } = useTranslation();

  return {
    primaryWinnerCallout: t('pages:routing.help.primaryWinnerCallout'),
    strategyFallbackRecoveryOnly: t('pages:routing.help.strategyFallbackRecoveryOnly'),
    strategyType: t('pages:routing.help.strategyType'),
    priority: t('pages:routing.help.priority'),
    enabled: t('pages:routing.help.enabled'),
    status: t('pages:routing.help.status'),
    configuration: t('pages:routing.help.configuration'),
    configurationSingle: t('pages:routing.help.configurationSingle'),
    configurationFallback: t('pages:routing.help.configurationFallback'),
    configurationLoadBalance: t('pages:routing.help.configurationLoadBalance'),
    configurationConditional: t('pages:routing.help.configurationConditional'),
    configurationAbSplit: t('pages:routing.help.configurationAbSplit'),
    configurationSmart: t('pages:routing.help.configurationSmart'),
    configurationPolicy: t('pages:routing.help.configurationPolicy'),
    pipelineStage: t('pages:routing.help.pipelineStage'),
    matchConditions: t('pages:routing.help.matchConditions'),
    matchModelsLabel: t('pages:routing.help.matchModelsLabel'),
  };
}

// Legacy static export for files that cannot use hooks (non-component contexts).
// Prefer useRoutingFieldHelp() in components.
export const ROUTING_RULE_FIELD_HELP = {
  primaryWinnerCallout:
    'Among all stage-1 rules whose match conditions fit the request, exactly one wins primary routing: the enabled rule with the highest numeric Priority (larger number wins). Other matching rules are not merged for the primary path. Rules whose strategy type is Fallback never win primary—they only add recovery targets after every primary upstream attempt fails.',
  strategyFallbackRecoveryOnly:
    'Strategy type Fallback does not win primary routing at stage 1. Its targets are used only as recovery after all primary upstream attempts fail. For an ordered primary chain inside one rule, use Conditional with a nested Fallback tree under default.',
  strategyType:
    'Controls how targets are chosen when the rule matches: single destination, ordered fallback, weighted load balancing, conditional tree, or A/B split. The JSON under Configuration must follow the same strategy shape the API validates.',
  priority:
    'Among stage-1 rules that match the same request, the one with the highest Priority wins primary routing (larger number wins). Rules with strategy type **Fallback** never win primary—they only contribute recovery targets after all primary upstream attempts fail (still ordered by Priority among fallback rules). Use larger numbers for more specific or business-critical routes.',
  enabled:
    'When off, the rule is skipped entirely. Matching traffic uses the next applicable rule or the model default route. Use disable instead of delete when you may re-enable later.',
  status:
    'Enabled rules participate in routing when their match conditions fit the request. Disabled rules are stored but never evaluated.',
  configuration:
    'The strategy payload the gateway loads when this rule matches: nested provider and model IDs, weights, and optional condition trees. This is the source of truth the runtime uses together with match conditions and priority.',
  configurationSingle:
    'Every matching request is sent to exactly one provider and model pair shown here. IDs refer to records configured under Providers & Models.',
  configurationFallback:
    'When this rule\'s strategy type is **Fallback** at stage 1, it does not win primary routing: its ordered targets are used only after every primary upstream attempt fails (then proxy walks the chain). Recovery targets are precomputed when the request is routed; the proxy only dials providers for them after primary targets are exhausted. For a primary route that still uses an ordered chain inside one rule, use **Conditional** with **default** set to a nested Fallback tree (see Pipeline stage help).',
  configurationLoadBalance:
    'Requests are distributed randomly by relative weight across targets. Higher weight means a larger share of traffic. Sticky affinity (when stickyOn is set) is stored in shared Redis when the gateway has Redis configured so the same key sticks to the same target across replicas; without Redis, sticky state is per process only (fine for single instance / dev).',
  configurationConditional:
    'Evaluates branches with "when" expressions against the live request context, then runs the matching "then" strategy or the mandatory "default". Use the structured editor for a default route, ordered branches (field path, operator, value, then target), and optional raw JSON for expressions the form cannot represent yet ($and / $or, nested strategies). Match conditions on the rule still decide whether this rule is considered at all.',
  configurationAbSplit:
    'Weighted random choice among flat provider/model pairs — ideal for experiments comparing models or providers at a fixed traffic mix.',
  configurationSmart:
    'Uses an AI model (the router) to analyze the user\'s request and automatically select the best model. Benefits: better model fit and cost/latency tradeoffs for mixed traffic. Costs/risks: extra router LLM call per model:auto request (latency and token spend); if the router fails or times out, the gateway uses Default Model; responses may still be cacheable under the chosen target model like any other route once routing completes.',
  configurationPolicy:
    'Stage-0 rules only. Merges allow/deny lists for gateway model IDs and provider IDs before any stage-1 route rule runs. Allow-lists from multiple matching policy rules intersect; deny-lists union. Stage-1 targets and smart-router candidates must satisfy the effective narrowing.',
  pipelineStage:
    'Choose whether this row is stage 0 (policy) or stage 1 (route). 0 — Policy: merged before any route; config must be allow/deny lists (internal gateway model and provider IDs). 1 — Route: evaluated after stage 0; among matches, the gateway picks the highest-Priority rule whose strategy type is **not** Fallback. Rules with strategy type **Fallback** supply the post-failure recovery chain only (ordered by Priority among those rules). Recovery targets are precomputed when the request is routed; the proxy only dials providers for them after primary targets are exhausted. To use an internal fallback chain as the **primary** route (e.g. Sonnet then Haiku), use strategy type **Conditional** with an empty conditions list and put the Fallback tree under **default**.',
  matchConditions:
    'Narrows which requests may use this rule. All set fields are combined with AND. Models: resolved internal gateway model IDs. Providers: internal Provider.id UUIDs — the request\'s resolved model must belong to one of these providers. Organizations: VirtualKey.projectId values (UUIDs). Virtual keys: name patterns with optional asterisk wildcards. If you leave every list empty, matching falls back to gateway defaults — use with care.',
  matchModelsLabel:
    'Gateway model IDs (not vendor API names). A request matches here when its routed model equals one of the selected IDs.',
};

export const strategyConfigHelpBody: Record<StrategyType, string> = {
  single: ROUTING_RULE_FIELD_HELP.configurationSingle,
  fallback: ROUTING_RULE_FIELD_HELP.configurationFallback,
  loadbalance: ROUTING_RULE_FIELD_HELP.configurationLoadBalance,
  conditional: ROUTING_RULE_FIELD_HELP.configurationConditional,
  ab_split: ROUTING_RULE_FIELD_HELP.configurationAbSplit,
  smart: ROUTING_RULE_FIELD_HELP.configurationSmart,
  policy: ROUTING_RULE_FIELD_HELP.configurationPolicy,
};

/** i18n'd strategy config help — use inside components */
export function useStrategyConfigHelp(): Record<StrategyType, string> {
  const help = useRoutingFieldHelp();
  return {
    single: help.configurationSingle,
    fallback: help.configurationFallback,
    loadbalance: help.configurationLoadBalance,
    conditional: help.configurationConditional,
    ab_split: help.configurationAbSplit,
    smart: help.configurationSmart,
    policy: help.configurationPolicy,
  };
}

function useStrategyHelp() {
  const { t } = useTranslation();
  return {
    single: { title: t('pages:routing.strategy.singleTitle'), description: t('pages:routing.strategy.singleDesc') },
    fallback: { title: t('pages:routing.strategy.fallbackTitle'), description: t('pages:routing.strategy.fallbackDesc') },
    loadbalance: { title: t('pages:routing.strategy.loadbalanceTitle'), description: t('pages:routing.strategy.loadbalanceDesc') },
    conditional: { title: t('pages:routing.strategy.conditionalTitle'), description: t('pages:routing.strategy.conditionalDesc') },
    ab_split: { title: t('pages:routing.strategy.abSplitTitle'), description: t('pages:routing.strategy.abSplitDesc') },
    smart: { title: t('pages:routing.strategy.smartTitle'), description: t('pages:routing.strategy.smartDesc') },
    policy: { title: t('pages:routing.strategy.policyTitle'), description: t('pages:routing.strategy.policyDesc') },
  };
}

/** Shared "?" control: what strategy type means, plus a catalog of all strategies. */
export function RoutingStrategyTypesHelp() {
  const { t } = useTranslation();
  const help = useRoutingFieldHelp();
  const strategyHelp = useStrategyHelp();

  return (
    <Tooltip
      content={
        <div className={styles.tooltipContent}>
          <p className={styles.tooltipIntro}>
            {help.strategyType}
          </p>
          <div className={styles.strategyOptionsLabel}>
            {t('pages:routing.strategyOptions')}
          </div>
          <div>
            {(Object.keys(strategyHelp) as StrategyType[]).map((key) => (
              <div key={key} className={styles.strategyItem}>
                <div className={styles.strategyTitle}>{strategyHelp[key].title}</div>
                <div className={styles.strategyDescription}>
                  {strategyHelp[key].description}
                </div>
              </div>
            ))}
          </div>
        </div>
      }
    >
      <button
        type="button"
        aria-label={t('pages:routing.helpStrategyType')}
        className={styles.helpButton}
      >
        ?
      </button>
    </Tooltip>
  );
}
