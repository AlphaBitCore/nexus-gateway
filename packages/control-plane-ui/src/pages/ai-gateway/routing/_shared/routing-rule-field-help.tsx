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
    configurationLatency: t('pages:routing.help.configurationLatency'),
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
    'Fallback lists provider+model entries tried in order within THIS rule. It is an ordinary strategy and can win primary routing like any other. To back up one rule with another, author the backup as a separate rule at a lower Priority — there is no special rule type for that. Entries name a provider and a model directly; a strategy nested inside another is rejected.',
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
    'Fallback\'s ordered entries are tried within this rule, after its own first choice fails. Each entry names a provider and a model directly — a strategy nested inside another is rejected, because the gateway resolves an entry as a leaf and does not evaluate one strategy inside another. To back up this rule with a different one, author that one at a lower Priority.',
  configurationLoadBalance:
    'Requests are distributed randomly by relative weight across targets. Higher weight means a larger share of traffic. Each request is weighed independently, so a client is not pinned to the target it reached last time.',
  configurationConditional:
    'Evaluates branches with "when" expressions against the live request context, then runs the matching "then" strategy or the mandatory "default". Use the structured editor for a default route, ordered branches (field path, operator, value, then target), and optional raw JSON for expressions the form cannot represent yet ($and / $or). Match conditions on the rule still decide whether this rule is considered at all.',
  configurationAbSplit:
    'Weighted random choice among flat provider/model pairs — ideal for experiments comparing models or providers at a fixed traffic mix.',
  configurationLatency:
    'Routes to the measured-fastest provider among the listed provider/model pairs. Unlike load balancing (fixed weights), traffic goes to the fastest tier by recently observed p95 latency; slower healthy providers serve only as failover. Ties within ~100ms are spread across the fastest targets to avoid overloading one, and new or idle providers get a small bounded share of exploration traffic so their speed can be learned. Health always dominates: a failing provider is still tried last regardless of latency.',
  configurationSmart:
    'Uses an AI model (the router) to analyze the user\'s request and automatically select the best model. Benefits: better model fit and cost/latency tradeoffs for mixed traffic. Costs/risks: extra router LLM call per model:auto request (latency and token spend); if the router fails or times out, the gateway uses Default Model; responses may still be cacheable under the chosen target model like any other route once routing completes. After the router picks, two other models from the same filtered pool ride along behind it as failover targets (the cheapest of that pool first), so a transient failure is retried inside this rule instead of leaving it — which of them is tried next depends on the failure, since a rate limit or an outage reaches for a different provider — which raises the upstream calls one auto-routed request may make; retryPolicy.maxUpstreamCalls bounds the spend.',
  matchConditions:
    'Narrows which requests may use this rule. All set fields are combined with AND. Models: resolved internal gateway model IDs. Providers: internal Provider.id UUIDs — compared against every provider that serves the model code the caller named, and INAPPLICABLE rather than failed when the caller named no model, so a provider-scoped rule still applies to model:auto. Request model keywords: matched against the raw model string before it is resolved, with optional asterisk wildcards (gpt-4-*). Model types: compared against the ENDPOINT the request arrived on, not the named model\'s category. Organizations: VirtualKey.projectId values (UUIDs). Virtual keys: name patterns with optional asterisk wildcards. If you leave every list empty, matching falls back to gateway defaults — use with care.',
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
  latency: ROUTING_RULE_FIELD_HELP.configurationLatency,
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
    latency: help.configurationLatency,
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
    latency: { title: t('pages:routing.strategy.latencyTitle'), description: t('pages:routing.strategy.latencyDesc') },
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
