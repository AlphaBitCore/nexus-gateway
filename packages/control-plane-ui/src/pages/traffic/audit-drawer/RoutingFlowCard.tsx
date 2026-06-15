import type { TrafficEvent } from '../../../api/types';
import { nameWithId } from './auditDrawerPrimitives';
import css from './trafficAuditDrawer.module.css';

// ── Routing flow comparison card ─────────────────────────────────────────────

// Shows "Requested → Routed To" side-by-side so it's immediately obvious
// whether the gateway rerouted the request. The right card gets a green accent
// when provider or model actually changed.
export function RoutingFlowCard({
  e,
  tRequested,
  tRoutedTo,
  tPassthrough,
  tChangedBy,
  tRoutingTitle,
}: {
  e: TrafficEvent;
  tRequested: string;
  tRoutedTo: string;
  tPassthrough: string;
  tChangedBy: string;
  tRoutingTitle: string;
}) {
  const hasRequested = e.providerName || e.modelName;
  const hasRouted = e.routedProviderName || e.routedModelName;
  if (!hasRequested && !hasRouted) return null;

  // Only flag "changed/rerouted" when the client actually pinned a requested
  // provider/model that differs from what served. For `model="auto"` (and
  // OpenAI-style requests that don't pin a provider) the requested side is
  // empty — the gateway selecting a target is not a "reroute", so the card
  // stays neutral rather than always lighting up.
  const changed =
    (!!e.providerName && !!e.routedProviderName && e.routedProviderName !== e.providerName) ||
    (!!e.modelName && !!e.routedModelName && e.routedModelName !== e.modelName);

  const rightCardClass = changed ? css.routingFlowCardChanged : css.routingFlowCard;

  return (
    <div>
      <h3 className={css.sectionTitle}>{tRoutingTitle}</h3>
      <div className={css.routingFlow}>
        <div className={css.routingFlowCard}>
          <div className={css.routingFlowLabel}>{tRequested}</div>
          <div className={css.routingFlowProvider}>{e.providerName ?? '—'}</div>
          <div className={css.routingFlowModel}>{e.modelName ?? '—'}</div>
        </div>

        <div className={css.routingFlowArrow}>→</div>

        <div className={rightCardClass}>
          <div className={css.routingFlowLabel}>{tRoutedTo}</div>
          {hasRouted ? (
            <>
              <div className={css.routingFlowProvider}>{e.routedProviderName ?? '—'}</div>
              <div className={css.routingFlowModel}>{e.routedModelName ?? '—'}</div>
            </>
          ) : (
            <div className={`${css.routingFlowProvider} ${css.routingPassthroughText}`}>
              {tPassthrough}
            </div>
          )}
          {(e.routingRuleName || e.routingRuleId) && (
            <div
              className={css.routingFlowRule}
              title={[e.routingRuleName, e.routingRuleId ? `ID: ${e.routingRuleId}` : null].filter(Boolean).join('\n')}
            >
              {tChangedBy}: {nameWithId(e.routingRuleName, e.routingRuleId)}
            </div>
          )}
        </div>
      </div>
    </div>
  );
}
