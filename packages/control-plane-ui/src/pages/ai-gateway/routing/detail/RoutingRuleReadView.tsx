import { useTranslation } from 'react-i18next';
import {
  Badge, Card, statusToVariant, Tooltip, Stack,
} from '@/components/ui';
import {
  displayStrategy,
  formatModelLabels,
} from '../_shared/routing-rule-config';
import {
  ROUTING_RULE_FIELD_HELP,
  RoutingStrategyTypesHelp,
  strategyConfigHelpBody,
} from '../_shared/routing-rule-field-help';
import { KvRow, formatProviderMatchLine } from './RoutingRuleHelpers';
import type { RoutingRuleDetailState } from './useRoutingRuleDetail';
import type { AdminModelsByProvider } from '@/api/types';
import { formatDate } from '@/lib/format';
import styles from './RoutingRuleDetail.module.css';
import { HelpIconButton } from '@nexus-gateway/ui-shared';

export function RoutingRuleReadView({ detail }: { detail: RoutingRuleDetailState }) {
  const { t } = useTranslation();
  const { rule, providerGroups, viewConfig, viewSmartParsed, viewMc } = detail;

  if (!rule) return null;

  // Only weighted strategies carry a meaningful per-target weight. latency
  // (p95-ordered) and fallback (order-only) do not, so their read view omits the
  // Weight column rather than showing a misleading placeholder.
  const showViewWeight =
    displayStrategy(rule.strategyType) === 'loadbalance' ||
    displayStrategy(rule.strategyType) === 'ab_split';

  return (
    <>
      <div className={styles.sectionBlock}>
        {/* The section heading is owned by RoutingRuleDetailPage's Card wrapper.
            Rendering it here too printed "Routing Rule Information" twice. */}
        <Card>
          <div className={styles.kvGrid}>
            <KvRow label={t('pages:routing.name')}>
              {rule.name}
            </KvRow>
            <KvRow label={t('pages:routing.description')}>
              {rule.description?.trim() ? rule.description : '—'}
            </KvRow>
            <div>
              <div className={styles.kvLabelRow}>
                <div className={styles.kvLabelInline}>{t('pages:routing.strategyType')}</div>
                <RoutingStrategyTypesHelp />
              </div>
              <div className={styles.kvValue}>{rule.strategyType}</div>
            </div>
            <KvRow label={t('pages:routing.priority')} helpTitle={t('pages:routing.priority')} helpBody={ROUTING_RULE_FIELD_HELP.priority}>
              {rule.priority}
            </KvRow>
            <KvRow label={t('pages:routing.status')} helpTitle={t('pages:routing.status')} helpBody={ROUTING_RULE_FIELD_HELP.status}>
              <div className={styles.badgeOffset}>
                <Badge variant={statusToVariant(rule.enabled ? 'enabled' : 'disabled')}>
                  {rule.enabled ? t('pages:routing.enabled') : t('pages:routing.disabled')}
                </Badge>
              </div>
            </KvRow>
            <KvRow label={t('pages:routing.created')}>
              {formatDate(rule.createdAt)}
            </KvRow>
          </div>
        </Card>
      </div>

      {/* Config (read-only) */}
      <div className={styles.sectionBlock}>
        <Stack direction="horizontal" gap="sm" align="center" className={styles.sectionHeaderRow}>
          <h3 className={styles.widgetSubtitle}>{t('pages:routing.configuration')}</h3>
          <Tooltip content={
            <>
              <p className={styles.tooltipParagraph}>{ROUTING_RULE_FIELD_HELP.configuration}</p>
              <p className={styles.tooltipParagraphLast}>
                {strategyConfigHelpBody[displayStrategy(rule.strategyType)]}
              </p>
            </>
          }>
            <HelpIconButton aria-label={t('pages:routing.ariaHelpConfiguration')} />
          </Tooltip>
        </Stack>
        <Card>
          {displayStrategy(rule.strategyType) === 'single' ? (
            <div className={styles.kvGrid}>
              <div><div className={styles.kvLabel}>{t('pages:routing.provider')}</div><div className={styles.kvValue}>{viewConfig.singleProvider || '--'}</div></div>
              <div><div className={styles.kvLabel}>{t('pages:routing.model')}</div><div className={styles.kvValue}>{viewConfig.singleModel || '--'}</div></div>
            </div>
          ) : displayStrategy(rule.strategyType) === 'conditional' ? (
            <ConditionalConfigView config={rule.config} providerGroups={providerGroups} />
          ) : viewSmartParsed ? (
            <Stack gap="md">
              <div className={styles.kvGrid}>
                <div>
                  <div className={styles.kvLabel}>{t('pages:routing.router')}</div>
                  <div className={styles.kvValue}>
                    {viewSmartParsed.routerProvider && viewSmartParsed.routerModel
                      ? `${viewSmartParsed.routerProvider} / ${viewSmartParsed.routerModel}`
                      : '—'}
                  </div>
                </div>
                <div>
                  <div className={styles.kvLabel}>{t('pages:routing.defaultFallback')}</div>
                  <div className={styles.kvValue}>
                    {viewSmartParsed.defaultProvider && viewSmartParsed.defaultModel
                      ? `${viewSmartParsed.defaultProvider} / ${viewSmartParsed.defaultModel}`
                      : '—'}
                  </div>
                </div>
                <div><div className={styles.kvLabel}>{t('pages:routing.temperature')}</div><div className={styles.kvValue}>{viewSmartParsed.temperature}</div></div>
                <div><div className={styles.kvLabel}>{t('pages:routing.maxTokensView')}</div><div className={styles.kvValue}>{viewSmartParsed.maxTokens}</div></div>
                <div><div className={styles.kvLabel}>{t('pages:routing.timeoutMs')}</div><div className={styles.kvValue}>{viewSmartParsed.timeoutMs}</div></div>
              </div>
              <div>
                <div className={styles.kvLabel}>{t('pages:routing.systemPromptView')}</div>
                <pre className={styles.systemPromptPre}>
                  {viewSmartParsed.systemPrompt}
                </pre>
              </div>
            </Stack>
          ) : (
            <div className={styles.overflowX}>
              <table className={styles.configTable}>
                <thead>
                  <tr>
                    <th className={styles.configTableHeader}>{t('pages:routing.provider')}</th>
                    <th className={styles.configTableHeader}>{t('pages:routing.model')}</th>
                    {showViewWeight && <th className={styles.configTableHeader}>{t('pages:routing.weight')}</th>}
                  </tr>
                </thead>
                <tbody>
                  {viewConfig.entries.map((e, i) => (
                    <tr key={i}>
                      <td className={styles.configTableCell}>{e.provider || '--'}</td>
                      <td className={styles.configTableCell}>{e.model || '--'}</td>
                      {showViewWeight && <td className={styles.configTableCell}>{e.weight}</td>}
                    </tr>
                  ))}
                </tbody>
              </table>
            </div>
          )}
        </Card>
      </div>

      {/* Fallback Chain (read-only) */}
      {rule.fallbackChain && Array.isArray(rule.fallbackChain) && rule.fallbackChain.length > 0 && (
        <div className={styles.sectionBlock}>
          <Stack direction="horizontal" gap="sm" align="center" className={styles.sectionHeaderRow}>
            <h3 className={styles.widgetSubtitle}>{t('pages:routing.fallbackChainTitle')}</h3>
            <Tooltip content={t('pages:routing.fallbackChainTooltipShort')}>
              <HelpIconButton aria-label={t('pages:routing.ariaHelpFallbackChain')} />
            </Tooltip>
          </Stack>
          <Card>
            <Stack gap="xs">
              {(rule.fallbackChain as Array<{ providerId: string; modelId: string }>).map((entry, idx) => (
                <Stack key={idx} direction="horizontal" gap="xs" align="center" className={styles.fallbackViewEntry}>
                  <span className={styles.fallbackIndex}>{idx + 1}.</span>
                  <span className={styles.fallbackViewLabel}>
                    {formatModelLabels(providerGroups, [entry.modelId]) || `${entry.providerId} / ${entry.modelId}`}
                  </span>
                </Stack>
              ))}
            </Stack>
          </Card>
        </div>
      )}

      {/* Retry Policy (read-only) */}
      {rule.retryPolicy && (
        <div className={styles.sectionBlock}>
          <Stack direction="horizontal" gap="sm" align="center" className={styles.sectionHeaderRow}>
            <h3 className={styles.widgetSubtitle}>{t('pages:routing.retryPolicy.title')}</h3>
          </Stack>
          <Card>
            <div className={styles.kvGrid}>
              <div>
                <div className={styles.kvLabel}>{t('pages:routing.retryPolicy.maxAttempts')}</div>
                <div className={styles.kvValue}>
                  {typeof rule.retryPolicy.maxAttemptsPerTarget === 'number'
                    ? rule.retryPolicy.maxAttemptsPerTarget
                    : '—'}
                </div>
              </div>
              <div>
                <div className={styles.kvLabel}>{t('pages:routing.retryPolicy.retryOn')}</div>
                <div className={styles.kvValue}>
                  {Array.isArray(rule.retryPolicy.retryOn) && rule.retryPolicy.retryOn.length > 0
                    ? rule.retryPolicy.retryOn.join(', ')
                    : '—'}
                </div>
              </div>
            </div>
          </Card>
        </div>
      )}

      {/* Match Conditions (read-only) */}
      <div className={styles.sectionBlock}>
        <Stack direction="horizontal" gap="sm" align="center" className={styles.sectionHeaderRow}>
          <h3 className={styles.widgetSubtitle}>{t('pages:routing.matchConditions')}</h3>
          <Tooltip content={ROUTING_RULE_FIELD_HELP.matchConditions}>
            <HelpIconButton aria-label={t('pages:routing.ariaHelpMatchConditions')} />
          </Tooltip>
        </Stack>
        <Card>
          <div className={styles.kvGrid}>
            <div>
              <KvRow
                label={t('pages:routing.model')}
                helpTitle={t('pages:routing.matchedModels')}
                helpBody={
                  <>
                    <p className={styles.tooltipParagraph}>{ROUTING_RULE_FIELD_HELP.matchModelsLabel}</p>
                    <p className={styles.tooltipParagraphLast}>{ROUTING_RULE_FIELD_HELP.matchConditions}</p>
                  </>
                }
              >
                {viewMc.models.length > 0 ? formatModelLabels(providerGroups, viewMc.models) : '—'}
              </KvRow>
            </div>
            <div>
              <KvRow label={t('pages:routing.matchProviders')}>{formatProviderMatchLine(providerGroups, viewMc.providers)}</KvRow>
            </div>
            <div>
              <KvRow label={t('pages:routing.matchProjectIds')}>
                {viewMc.projects.length > 0 ? viewMc.projects.join(', ') : '—'}
              </KvRow>
            </div>
          </div>
        </Card>
      </div>
    </>
  );
}

// Renders a target leaf ({modelId, providerId}) as "Provider / Model", the
// same label the other strategy views show, resolved from the provider groups.
function targetLabel(providerGroups: AdminModelsByProvider[], leaf: unknown): string {
  const modelId = String((leaf as Record<string, unknown> | null)?.modelId ?? '');
  if (!modelId) return '—';
  const label = formatModelLabels(providerGroups, [modelId]);
  return label || modelId;
}

// Renders one `when` clause — a map of routing-context field → operator/value —
// as readable lines rather than raw JSON. Handles the common $eq/$in/$ne shapes
// and falls back to a compact JSON string for anything else.
function formatWhenClause(when: unknown): string[] {
  if (!when || typeof when !== 'object') return [];
  return Object.entries(when as Record<string, unknown>).map(([field, cond]) => {
    if (cond && typeof cond === 'object') {
      const entries = Object.entries(cond as Record<string, unknown>);
      if (entries.length === 1) {
        const [op, val] = entries[0];
        const opLabel = op === '$eq' ? '=' : op === '$ne' ? '≠' : op === '$in' ? 'in' : op;
        const valLabel = Array.isArray(val) ? val.join(', ') : String(val);
        return `${field} ${opLabel} ${valLabel}`;
      }
    }
    return `${field}: ${JSON.stringify(cond)}`;
  });
}

// Visual read view for a conditional strategy: a default target plus an
// ordered list of when→then branches. Previously this fell back to raw JSON,
// the only strategy type without a form-style view.
function ConditionalConfigView({
  config, providerGroups,
}: { config: unknown; providerGroups: AdminModelsByProvider[] }) {
  const { t } = useTranslation();
  const cfg = (config ?? {}) as Record<string, unknown>;
  const conditions = Array.isArray(cfg.conditions) ? cfg.conditions : [];

  return (
    <Stack gap="md">
      <div className={styles.kvGrid}>
        <div>
          <div className={styles.kvLabel}>{t('pages:routing.defaultFallback')}</div>
          <div className={styles.kvValue}>{targetLabel(providerGroups, cfg.default)}</div>
        </div>
      </div>
      <div className={styles.overflowX}>
        <table className={styles.configTable}>
          <thead>
            <tr>
              <th className={styles.configTableHeader}>{t('pages:routing.conditionalWhen')}</th>
              <th className={styles.configTableHeader}>{t('pages:routing.conditionalThen')}</th>
            </tr>
          </thead>
          <tbody>
            {conditions.length === 0 ? (
              <tr><td className={styles.configTableCell} colSpan={2}>—</td></tr>
            ) : conditions.map((c, i) => {
              const cond = (c ?? {}) as Record<string, unknown>;
              const lines = formatWhenClause(cond.when);
              return (
                <tr key={i}>
                  <td className={styles.configTableCell}>
                    {lines.length > 0 ? lines.map((l, j) => <div key={j}>{l}</div>) : '—'}
                  </td>
                  <td className={styles.configTableCell}>{targetLabel(providerGroups, cond.then)}</td>
                </tr>
              );
            })}
          </tbody>
        </table>
      </div>
    </Stack>
  );
}
