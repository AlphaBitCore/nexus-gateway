import type { CSSProperties } from 'react';
import { useState, useEffect, useCallback } from 'react';
import { useTranslation } from 'react-i18next';
import { Link } from 'react-router-dom';
import type { TrafficEvent } from '../../../api/types';
import { Stack, AlertDialog, Button, Dialog } from '@/components/ui';
import { DRAWER_MS, DRAWER_WIDTH } from '../../governance/adminAuditLogShared';
import { formatDateTime, formatTokens } from '@/lib/format';
import { ComplianceTagChipList } from '../list/ComplianceTagChips';
import { LatencyWaterfall } from '@/components/charts/LatencyWaterfall';
import { NormalizedPayloadView } from '../list/NormalizedPayloadView';
import { useApi } from '@/hooks/useApi';
import { systemApi } from '@/api/services';
import { usePermission } from '@/hooks/usePermission';
import { useMutation } from '@/hooks/useMutation';
import { semanticFeedbackApi } from '@/api/services/cache/semanticFeedback';
import { semanticCacheConfigApi } from '@/api/services/cache/semanticCacheConfig';
import type { SemanticCacheConfig } from '@/api/types';
import { analyticsApi, type CostSummaryResponse } from '@/api/services/overview/analytics';
import { useDisableSemanticCacheFleetWide } from '../../ai-gateway/cache';
import {
  TabBar, Block, FieldGroup, TokenCard, DecisionBadge, ReasonCodeChip,
  fmtScalar, fmtCost, nameWithId, subTabStyle, type TabKey,
} from './auditDrawerPrimitives';
import { PipelineTimeline, BlockingRuleLine } from './HookTimeline';
import { JsonSection, PayloadSection } from './PayloadSections';
import { RoutingFlowCard } from './RoutingFlowCard';
import { CostBreakdown } from './CostBreakdown';
import css from './trafficAuditDrawer.module.css';

export { DRAWER_MS, DRAWER_WIDTH } from '../../governance/adminAuditLogShared';

// ── Main drawer ──────────────────────────────────────────────────────────────

interface TrafficEventDrawerProps {
  selectedEntry: TrafficEvent;
  drawerVisible: boolean;
  onClose: () => void;
  titleId?: string;
}

/* Drawer overlay and aside use inline styles because their opacity/transform
   are driven by the drawerVisible boolean (truly dynamic computed values). */

export function TrafficEventDrawer({
  selectedEntry,
  drawerVisible,
  onClose,
  titleId = 'traffic-event-drawer-title',
}: TrafficEventDrawerProps) {
  const { t } = useTranslation();
  const canDisableL2 = usePermission('semantic-cache:update');
  const canMarkBad = usePermission('semantic-cache:update');
  const [activeTab, setActiveTab] = useState<TabKey>('overview');
  // Payloads sub-tab: Normalized (default) | Raw.
  const [payloadView, setPayloadView] = useState<'normalized' | 'raw'>('normalized');
  // Emergency disable L2 confirm dialog state.
  const [confirmDisableL2Open, setConfirmDisableL2Open] = useState(false);
  // Mark-bad cache hit dialog state.
  const [markBadOpen, setMarkBadOpen] = useState(false);
  const [markBadReason, setMarkBadReason] = useState('');
  const [markBadReasonError, setMarkBadReasonError] = useState('');

  const { disable: disableL2, loading: disableL2Loading } = useDisableSemanticCacheFleetWide({
    successMessage: t('pages:traffic.detail.aiProvider.semanticHit.disabledToast'),
    errorMessage: t('pages:traffic.detail.aiProvider.semanticHit.errorToast'),
  });

  // Reflect live fleet semantic-cache state so the "Disable L2 fleet-wide"
  // button hides once L2 is already off (otherwise the button stays visible
  // forever after the first click and confuses admins about whether the
  // disable took effect).
  const { data: semanticConfig } = useApi<SemanticCacheConfig>(
    () => semanticCacheConfigApi.getConfig(),
    ['admin', 'semantic-cache', 'config', 'audit-drawer'],
  );
  const l2CurrentlyEnabled = semanticConfig?.enabled === true;

  // Fleet-wide cost policy so the drawer's "internal-ops counted/excluded"
  // hint reflects what Hub is actually rolling up. Cached by useApi key.
  const { data: costSummary } = useApi<CostSummaryResponse>(
    () => analyticsApi.costSummary(),
    ['admin', 'analytics', 'cost-summary', 'policy'],
  );
  const internalOpsExcluded = costSummary?.excludeInternalOpsFromBilledCost === true;

  const { mutate: postFeedback, loading: postFeedbackLoading } = useMutation(
    (input: Parameters<typeof semanticFeedbackApi.postFeedback>[0]) =>
      semanticFeedbackApi.postFeedback(input),
    {
      successMessage: t('pages:traffic.detail.aiProvider.semanticHit.markBadSuccessToast'),
      errorMessage: t('pages:traffic.detail.aiProvider.semanticHit.markBadErrorToast'),
      onSuccess: () => {
        setMarkBadOpen(false);
        setMarkBadReason('');
        setMarkBadReasonError('');
      },
    },
  );

  const handleDisableL2Confirm = useCallback(() => {
    setConfirmDisableL2Open(false);
    void disableL2();
  }, [disableL2]);

  // Reset to overview whenever a new event is opened.
  useEffect(() => {
    setActiveTab('overview');
    setPayloadView('normalized');
    setMarkBadOpen(false);
    setMarkBadReason('');
    setMarkBadReasonError('');
  }, [selectedEntry.id]);

  // The list endpoint deliberately omits request/response bodies for performance
  // (they live in traffic_event_payload and are LEFT-JOINed only on detail).
  // Fetch the full record here so the drawer can show payloads when present.
  const detail = useApi(
    () => systemApi.getTrafficEvent(selectedEntry.id),
    ['admin', 'traffic', 'detail', selectedEntry.id],
    { skip: !drawerVisible },
  );
  const e: TrafficEvent = detail.data ?? selectedEntry;

  // The poison list is keyed on the L2 entry's Redis HASH key
  // ("<index>:<sha256(EmbeddingInput)[:16]>"), NOT on traffic_event.id.
  // Read the stamp from the freshest record (`e` = detail.data ?? selectedEntry)
  // so a drawer opened on a stale list row still posts the correct key after
  // the detail fetch completes.
  const handleMarkBadConfirm = useCallback(() => {
    const trimmed = markBadReason.trim();
    if (trimmed.length < 5) {
      setMarkBadReasonError('Reason must be at least 5 characters.');
      return;
    }
    if (trimmed.length > 500) {
      setMarkBadReasonError('Reason must be at most 500 characters.');
      return;
    }
    // The surrounding `gatewayCacheKind === 'semantic'` button visibility
    // is the primary gate but a missing stamp on a legacy row would silently
    // no-op the poison, so surface a clear validation error instead.
    const l2EntryKey = e.gatewayCacheL2EntryKey ?? '';
    if (!l2EntryKey) {
      setMarkBadReasonError(
        'This cache hit row predates L2 entry-key tracking and cannot be poisoned. Mark the next matching hit instead.',
      );
      return;
    }
    setMarkBadReasonError('');
    void postFeedback({
      entryKey: l2EntryKey,
      vkScope: e.identity?.vk?.id ?? '',
      reason: trimmed,
    });
  }, [markBadReason, postFeedback, e.gatewayCacheL2EntryKey, e.identity?.vk?.id]);

  // Normalized sidecar fetched lazily. 404 means no traffic_event_normalized
  // row (e.g. capture disabled, protocol unsupported). The Normalized tab
  // handles null gracefully.
  const normalized = useApi(
    () => systemApi.getTrafficEventNormalized(selectedEntry.id).catch(() => null),
    ['admin', 'traffic', 'normalized', selectedEntry.id],
    { skip: !drawerVisible || activeTab !== 'payloads' },
  );

  const overlayStyle: CSSProperties = {
    position: 'fixed',
    inset: 0,
    zIndex: 'var(--g-z-overlay)',
    background: 'var(--color-overlay)',
    opacity: drawerVisible ? 1 : 0,
    transition: `opacity ${DRAWER_MS}ms cubic-bezier(0.4, 0, 0.2, 1)`,
    pointerEvents: drawerVisible ? 'auto' : 'none',
  };

  const asideStyle: CSSProperties = {
    position: 'fixed',
    top: 0,
    right: 0,
    width: DRAWER_WIDTH,
    height: '100vh',
    maxHeight: '100dvh',
    zIndex: 'var(--g-z-modal)',
    background: 'var(--color-surface)',
    borderLeft: '1px solid var(--color-border)',
    boxShadow: 'var(--shadow-xl)',
    display: 'flex',
    flexDirection: 'column',
    transform: drawerVisible ? 'translateX(0)' : 'translateX(100%)',
    transition: `transform ${DRAWER_MS}ms cubic-bezier(0.4, 0, 0.2, 1)`,
  };

  const totalHooks =
    (e.requestHooksPipeline?.length ?? 0) + (e.responseHooksPipeline?.length ?? 0);

  const hasPayloads =
    e.requestBody != null || e.responseBody != null ||
    e.requestSpillRef != null || e.responseSpillRef != null ||
    e.details != null;

  // Routing decisions only happen on ai-gateway (VK-traffic). Compliance
  // proxy and agent forward upstream traffic without picking a provider /
  // model AND with the agent's current pipeline they don't carry token /
  // cost / cache signals either — so the entire AI tab is suppressed
  // for non-ai-gateway sources. When the agent / proxy adapter pipeline
  // starts surfacing token usage we can flip this back to a labelled
  // tab; until then the tab would be a row of dashes.
  const isGatewayTraffic = e.source === 'ai-gateway';

  const tabs: Array<{ key: TabKey; label: string; badge?: number }> = [
    { key: 'overview', label: t('pages:traffic.detail.tabs.overview') },
    ...(isGatewayTraffic
      ? [{ key: 'ai' as TabKey, label: t('pages:traffic.detail.tabs.aiAndRouting') }]
      : []),
    { key: 'compliance', label: t('pages:traffic.detail.tabs.compliance'), badge: totalHooks || undefined },
    { key: 'payloads', label: t('pages:traffic.detail.tabs.payloads') },
  ];

  return (
    <>
      <div
        role="presentation"
        onClick={onClose}
        style={overlayStyle}
        aria-hidden
      />
      <aside
        role="dialog"
        aria-modal="true"
        aria-labelledby={titleId}
        data-testid="traffic-row-drawer"
        style={asideStyle}
      >
        {/* ── Header ── */}
        <Stack
          direction="horizontal"
          justify="between"
          align="center"
          className={css.drawerHeader}
        >
          <h2 id={titleId} className={css.drawerTitle}>
            {t('pages:traffic.detail.title')}
          </h2>
          <button
            type="button"
            onClick={onClose}
            aria-label={t('pages:traffic.detail.closeAriaLabel')}
            className={css.closeButton}
          >
            &times;
          </button>
        </Stack>

        {/* ── Tab bar ── */}
        <TabBar active={activeTab} onChange={setActiveTab} tabs={tabs} />

        {/* ── Tab content ── */}
        <div className={css.drawerBody}>

          {/* ────────── Overview ────────── */}
          {activeTab === 'overview' && (
            <Stack gap="lg">
              <FieldGroup
                title={t('pages:traffic.detail.basic.title')}
                fields={[
                  { label: t('pages:traffic.detail.basic.id'), value: e.id, mono: true, fullWidth: true },
                  { label: t('pages:traffic.detail.basic.source'), value: e.source },
                  { label: t('pages:traffic.detail.basic.time'), value: e.timestamp ? formatDateTime(e.timestamp) : null },
                ]}
                cols={2}
              />

              <FieldGroup
                title={t('pages:traffic.detail.request.title')}
                fields={[
                  { label: t('pages:traffic.detail.request.method'), value: e.method },
                  { label: t('pages:traffic.detail.request.status'), value: e.statusCode },
                  { label: t('pages:traffic.detail.request.path'), value: e.path, mono: true, fullWidth: true },
                  // The URL the gateway actually sent to upstream. Differs
                  // from method/path on cross-format routes (e.g. an OpenAI
                  // chat request that auto-routes to Anthropic /v1/messages,
                  // or chat→Responses upgrades). Same as method/path for
                  // transparent compliance-proxy + agent traffic — hide the
                  // duplicate row in that case to keep the panel readable.
                  ...(e.targetMethod && e.targetMethod !== e.method
                    ? [{ label: t('pages:traffic.detail.request.targetMethod', 'Target Method'), value: e.targetMethod }]
                    : []),
                  ...(e.targetPath && e.targetPath !== e.path
                    ? [{ label: t('pages:traffic.detail.request.targetPath', 'Target Path'), value: e.targetPath, mono: true, fullWidth: true }]
                    : []),
                  { label: t('pages:traffic.detail.request.targetHost'), value: e.targetHost },
                  { label: t('pages:traffic.detail.request.sourceIp'), value: e.sourceIp },
                  { label: t('pages:traffic.detail.request.latency'), value: e.latencyMs != null ? `${e.latencyMs} ms` : null },
                  { label: t('pages:traffic.detail.request.errorCode'), value: e.errorCode, mono: true },
                  { label: t('pages:traffic.detail.request.errorReason'), value: e.errorReason, fullWidth: true },
                ]}
                cols={2}
              />

              <FieldGroup
                title={t('pages:traffic.detail.identity.title')}
                fields={[
                  { label: t('pages:traffic.detail.identity.entity'), value: nameWithId(e.entityName, e.entityId) },
                  { label: t('pages:traffic.detail.identity.entityType'), value: e.entityType },
                  { label: t('pages:traffic.detail.identity.organization'), value: nameWithId(e.orgName, e.orgId) },
                  { label: t('pages:traffic.detail.identity.user'), value: nameWithId(e.identity?.user?.name, e.identity?.user?.id) },
                  { label: t('pages:traffic.detail.identity.project'), value: nameWithId(e.identity?.project?.name, e.identity?.project?.id) },
                  { label: t('pages:traffic.detail.identity.vk'), value: nameWithId(e.identity?.vk?.name, e.identity?.vk?.id) },
                  { label: t('pages:traffic.detail.identity.device'), value: nameWithId(e.identity?.device?.hostname, e.identity?.device?.id), fullWidth: true },
                ]}
                cols={2}
              />

              <FieldGroup
                title={t('pages:traffic.detail.agent.title')}
                fields={[
                  { label: t('pages:traffic.detail.agent.process'), value: e.sourceProcess },
                  { label: t('pages:traffic.detail.agent.action'), value: e.action },
                ]}
                cols={2}
              />

              {/* Reverse navigation: jump from a request to the node that
                  emitted it. Renders only when traffic_event.thing_id is set;
                  older rows have NULL and skip the link. */}
              {e.thingId ? (
                <Block label={t('pages:nodeDetail.traffic.viewEmittingNode')}>
                  <Link to={`/infrastructure/nodes/${encodeURIComponent(e.thingId)}`}>
                    {e.thingName || e.thingId} →
                  </Link>
                </Block>
              ) : null}
            </Stack>
          )}

          {/* ────────── AI & Routing ────────── (ai-gateway only) */}
          {activeTab === 'ai' && isGatewayTraffic && (
            <Stack gap="lg">
              {/* Gateway cache HIT banner — fires when gateway served (extract HIT
                  or singleflight HIT_INFLIGHT). Provider-only HITs (gateway MISS
                  + cache_read_tokens > 0) get the smaller discount line in the
                  Cache block below, not a full-screen "saved!" banner.
                  Dry-run rows never reach this branch because dry-run dispatch
                  happens BEFORE cache lookup (cache_status stays NULL).
                  When the cache kind is 'semantic', an inline "Disable L2
                  fleet-wide" action surfaces for admins with semantic-cache:update. */}
              {e.cacheStatus === 'HIT' &&
               (e.gatewayCacheStatus === 'hit' || e.gatewayCacheStatus === 'hit_inflight') && (
                <div className={css.cacheHitBanner}>
                  <span style={{ fontSize: 'var(--g-font-size-lg)', lineHeight: 1.2 }}>✓</span>
                  <div style={{ flex: 1 }}>
                    <div className={css.cacheHitBannerTitle}>
                      {e.gatewayCacheSavingsUsd != null && e.gatewayCacheSavingsUsd > 0
                        ? t('pages:traffic.detail.aiProvider.cacheSavedBanner', { value: fmtCost(e.gatewayCacheSavingsUsd) })
                        : t('pages:traffic.detail.aiProvider.cacheHitBanner')}
                    </div>
                    <div className={css.cacheHitBannerSub}>
                      {t('pages:traffic.detail.aiProvider.cacheHitBannerSub')}
                    </div>
                  </div>
                  {e.gatewayCacheKind === 'semantic' && canDisableL2 && l2CurrentlyEnabled && (
                    <Button
                      variant="danger"
                      onClick={() => setConfirmDisableL2Open(true)}
                      loading={disableL2Loading}
                      data-testid="disable-l2-btn"
                    >
                      {t('pages:traffic.detail.aiProvider.semanticHit.disableL2')}
                    </Button>
                  )}
                  {e.gatewayCacheKind === 'semantic' && canMarkBad && (
                    <Button
                      variant="secondary"
                      onClick={() => setMarkBadOpen(true)}
                      data-testid="mark-bad-hit-btn"
                    >
                      {t('pages:traffic.detail.aiProvider.semanticHit.markBadButton')}
                    </Button>
                  )}
                </div>
              )}

              {/* Routing decision: Requested → Routed To. ai-gateway-only —
                  compliance-proxy and agent forward upstream traffic
                  unchanged and never write routedProvider / routingRule on
                  traffic_event, so the card has nothing meaningful to show
                  outside the gateway path. */}
              {isGatewayTraffic && (
                <div data-testid="audit-drawer-routing-tab">
                  <RoutingFlowCard
                    e={e}
                    tRequested={t('pages:traffic.detail.routing.requested')}
                    tRoutedTo={t('pages:traffic.detail.routing.routedTo')}
                    tPassthrough={t('pages:traffic.detail.routing.passthrough')}
                    tChangedBy={t('pages:traffic.detail.routing.changedBy')}
                    tRoutingTitle={t('pages:traffic.detail.routing.title')}
                  />
                </div>
              )}

              {/* Token usage stat cards */}
              {(e.promptTokens != null || e.completionTokens != null || e.totalTokens != null) && (
                <div>
                  <h3 className={css.sectionTitle}>{t('pages:traffic.detail.aiProvider.tokenUsageTitle')}</h3>
                  <div className={css.tokenGrid}>
                    <TokenCard
                      label={t('pages:traffic.detail.aiProvider.promptTokens')}
                      value={formatTokens(e.promptTokens)}
                    />
                    <TokenCard
                      label={t('pages:traffic.detail.aiProvider.completionTokens')}
                      value={formatTokens(e.completionTokens)}
                    />
                    <TokenCard
                      label={t('pages:traffic.detail.aiProvider.totalTokens')}
                      value={formatTokens(e.totalTokens)}
                    />
                    {e.cacheCreationTokens != null && (
                      <TokenCard
                        label={t('pages:traffic.detail.aiProvider.cacheCreationTokens')}
                        value={formatTokens(e.cacheCreationTokens)}
                      />
                    )}
                    {e.cacheReadTokens != null && (
                      <TokenCard
                        label={t('pages:traffic.detail.aiProvider.cacheReadTokens')}
                        value={formatTokens(e.cacheReadTokens)}
                      />
                    )}
                    {/* reasoning_tokens already counted inside
                        completionTokens; shown here so customers can see
                        what fraction of their output cost was thinking. */}
                    {e.reasoningTokens != null && (
                      <TokenCard
                        label={t('pages:traffic.detail.aiProvider.reasoningTokens')}
                        value={formatTokens(e.reasoningTokens)}
                      />
                    )}
                  </div>
                </div>
              )}

              {/* Prompt cache normaliser metrics — only shown when normaliser touched this request */}
              {(e.normalizedStripCount != null || e.cacheMarkerInjected != null || e.cacheWriteCostUsd != null || e.cacheReadSavingsUsd != null) && (
                <FieldGroup
                  title={t('pages:traffic.detail.aiProvider.cacheNormaliserTitle')}
                  fields={[
                    { label: t('pages:traffic.detail.aiProvider.normaliserStripCount'), value: e.normalizedStripCount != null ? String(e.normalizedStripCount) : null },
                    { label: t('pages:traffic.detail.aiProvider.normaliserStripBytes'), value: e.normalizedStripBytes != null ? String(e.normalizedStripBytes) : null },
                    { label: t('pages:traffic.detail.aiProvider.cacheMarkersInjected'), value: e.cacheMarkerInjected != null ? String(e.cacheMarkerInjected) : null },
                    { label: t('pages:traffic.detail.aiProvider.cacheWriteCost'), value: e.cacheWriteCostUsd != null ? fmtCost(e.cacheWriteCostUsd) : null },
                    { label: t('pages:traffic.detail.aiProvider.cacheReadSavings'), value: e.cacheReadSavingsUsd != null ? fmtCost(e.cacheReadSavingsUsd) : null },
                    { label: t('pages:traffic.detail.aiProvider.cacheNetSavings'), value: e.cacheNetSavingsUsd != null ? fmtCost(e.cacheNetSavingsUsd) : null },
                  ]}
                  cols={2}
                />
              )}

              {/* Cache outcome block. Three layouts (gateway-served /
                  provider-discount / no-savings) per the rules in
                  cost-estimation-architecture.md § 6.4. Filter UIs bind to the
                  unified `cacheStatus` (HIT | MISS); detail labels are derived
                  from the four breakdown columns below. */}
              {e.cacheStatus != null && (() => {
                const gw = e.gatewayCacheStatus;
                const pv = e.providerCacheStatus;
                const gatewayServed = gw === 'hit' || gw === 'hit_inflight';

                let gatewayLabel: string;
                if (gw === 'hit') {
                  // Differentiate L1 extract vs L2 semantic — both are
                  // gateway-served HITs but the layer is very different
                  // (exact-hash replay vs vector lookup that ran an
                  // embedding call). The DB row carries the distinction
                  // in gatewayCacheKind; surface it in the badge.
                  const kindKey = e.gatewayCacheKind === 'semantic' ? 'hit_semantic' : 'hit_extract';
                  gatewayLabel = t(`pages:traffic.detail.cache.gateway.${kindKey}`);
                } else if (gw === 'hit_inflight') {
                  gatewayLabel = t('pages:traffic.detail.cache.gateway.hit_inflight');
                } else if (gw === 'miss') {
                  gatewayLabel = t('pages:traffic.detail.cache.gateway.miss');
                } else if (gw === 'skipped') {
                  const reason = e.gatewayCacheSkipReason ?? 'disabled';
                  // An enum value without a translation shows the raw reason,
                  // never a bare i18n key.
                  gatewayLabel = t(`pages:traffic.detail.cache.gatewaySkip.${reason}`, { defaultValue: reason });
                } else {
                  gatewayLabel = '-';
                }

                let providerLabel: string;
                if (gatewayServed) {
                  providerLabel = t('pages:traffic.detail.cache.provider.naGatewayServed');
                } else if (pv === 'hit') {
                  providerLabel = t('pages:traffic.detail.cache.provider.hit', {
                    tokens: formatTokens(e.cacheReadTokens ?? 0),
                  });
                } else if (pv === 'miss') {
                  providerLabel = t('pages:traffic.detail.cache.provider.miss');
                } else if (pv === 'na') {
                  // na with no provider call already handled by gatewayServed branch;
                  // na here = called provider but model unsupported.
                  providerLabel = t('pages:traffic.detail.cache.provider.naUnsupported');
                } else {
                  providerLabel = '-';
                }

                let savings: string | null = null;
                if (gatewayServed && e.gatewayCacheSavingsUsd != null && e.gatewayCacheSavingsUsd > 0) {
                  savings = t('pages:traffic.detail.cache.savedVsUpstream', {
                    value: fmtCost(e.gatewayCacheSavingsUsd),
                  });
                } else if (!gatewayServed && pv === 'hit' && e.cacheNetSavingsUsd != null) {
                  savings = t('pages:traffic.detail.cache.netSaved', {
                    value: fmtCost(e.cacheNetSavingsUsd),
                  });
                }

                const headline = e.cacheStatus === 'HIT'
                  ? t('pages:traffic.detail.cache.headlineHit')
                  : t('pages:traffic.detail.cache.headlineMiss');

                return (
                  <FieldGroup
                    title={t('pages:traffic.detail.cache.title')}
                    fields={[
                      { label: t('pages:traffic.detail.cache.outcome'), value: headline },
                      { label: t('pages:traffic.detail.cache.gatewayField'), value: gatewayLabel },
                      { label: t('pages:traffic.detail.cache.providerField'), value: providerLabel, fullWidth: !savings },
                      ...(savings ? [{ label: t('pages:traffic.detail.cache.savings'), value: savings, fullWidth: true }] : []),
                    ]}
                    cols={2}
                  />
                );
              })()}

              {/* Cost, usage extraction, API key — cache moved to its own block above */}
              <FieldGroup
                title={t('pages:traffic.detail.aiProvider.usageTitle')}
                fields={[
                  // Reasoning cost — subset of the upstream cost; the full
                  // upstream subtotal is shown in the Costs breakdown below.
                  { label: t('pages:traffic.detail.aiProvider.reasoningCost'), value: e.reasoningCostUsd != null ? fmtCost(e.reasoningCostUsd) : null },
                  { label: t('pages:traffic.detail.aiProvider.usageExtraction'), value: e.usageExtractionStatus },
                  { label: t('pages:traffic.detail.aiProvider.apiKeyClass'), value: e.apiKeyClass, mono: true },
                  { label: t('pages:traffic.detail.aiProvider.apiKeyFingerprint'), value: e.apiKeyFingerprint, mono: true, fullWidth: true },
                ]}
                cols={2}
              />

              {/* Costs breakdown — three-section taxonomy (upstream provider /
                  Nexus internal-ops / vs. no-gateway baseline). Hidden when
                  every line is null/zero so non-LLM rows stay clean. */}
              <CostBreakdown e={e} internalOpsExcluded={internalOpsExcluded} />

              {/* Routing trace — step-by-step routing engine decision log.
                  Only the ai-gateway routing engine writes this column;
                  hide entirely for compliance-proxy / agent so we don't
                  render an empty JsonSection header on every drawer open. */}
              {isGatewayTraffic && (
                <JsonSection label={t('pages:traffic.detail.payload.routingTrace')} value={e.routingTrace} />
              )}
            </Stack>
          )}

          {/* ────────── Compliance ────────── */}
          {activeTab === 'compliance' && (
            <Stack gap="lg">
              {/* Dual-stage decision cards */}
              <div>
                <h3 className={css.sectionTitle}>{t('pages:traffic.detail.compliance.title')}</h3>
                <Stack gap="sm">
                  <div className={css.stageGrid}>
                    <div className={css.stageCard}>
                      <div className={css.stageCardTitle}>{t('pages:traffic.detail.compliance.requestStage')}</div>
                      <Stack gap="xs">
                        <DecisionBadge decision={e.requestHookDecision} />
                        {e.requestHookReasonCode && (
                          <ReasonCodeChip code={e.requestHookReasonCode} />
                        )}
                        {e.requestHookReason && (
                          <div className={css.stageDetailText}>
                            {e.requestHookReason}
                          </div>
                        )}
                        {e.requestBlockingRule && (
                          <div style={{ marginTop: 'var(--g-space-1)' }}>
                            <BlockingRuleLine
                              label={t('pages:traffic.detail.compliance.requestBlockingRule')}
                              rule={e.requestBlockingRule}
                            />
                          </div>
                        )}
                      </Stack>
                    </div>

                    <div className={css.stageCard}>
                      <div className={css.stageCardTitle}>{t('pages:traffic.detail.compliance.responseStage')}</div>
                      <Stack gap="xs">
                        <DecisionBadge decision={e.responseHookDecision} />
                        {e.responseHookReasonCode && (
                          <ReasonCodeChip code={e.responseHookReasonCode} />
                        )}
                        {e.responseHookReason && (
                          <div className={css.stageDetailText}>
                            {e.responseHookReason}
                          </div>
                        )}
                        {e.responseBlockingRule && (
                          <div style={{ marginTop: 'var(--g-space-1)' }}>
                            <BlockingRuleLine
                              label={t('pages:traffic.detail.compliance.responseBlockingRule')}
                              rule={e.responseBlockingRule}
                            />
                          </div>
                        )}
                      </Stack>
                    </div>
                  </div>

                  {e.bumpStatus && (
                    <Block label={t('pages:traffic.detail.compliance.bumpStatus')}>
                      {fmtScalar(e.bumpStatus)}
                    </Block>
                  )}
                  {/* When CP transparently tunneled this CONNECT
                      because of a verified X-Nexus-Attestation header,
                      surface the agent's UUID so compliance officers
                      can trace the chain. Only renders for attested
                      rows; regular MITM rows leave both fields null. */}
                  {e.attestationVerified && (
                    <Block label={t('pages:traffic.detail.compliance.attestationAgent', 'Attestation Agent')}>
                      <span className={css.mono}>{e.attestationAgentId || '—'}</span>
                    </Block>
                  )}
                </Stack>
              </div>

              {e.complianceTags && e.complianceTags.length > 0 && (
                <div>
                  <h3 className={css.sectionTitle}>{t('pages:traffic.detail.complianceTags')}</h3>
                  <ComplianceTagChipList
                    tags={e.complianceTags}
                    emptyLabel={t('pages:traffic.detail.noTags')}
                  />
                </div>
              )}

              {/* Latency Phase Waterfall — Us · Upstream visual */}
              <div data-testid="audit-drawer-waterfall">
                <h3 className={css.sectionTitle}>{t('pages:traffic.detail.waterfall.title')}</h3>
                <LatencyWaterfall
                  latencyMs={e.latencyMs}
                  upstreamTtfbMs={e.upstreamTtfbMs}
                  upstreamTotalMs={e.upstreamTotalMs}
                  requestHooksMs={e.requestHooksMs}
                  responseHooksMs={e.responseHooksMs}
                />
              </div>

              <div data-testid="audit-drawer-hooks-tab">
                <h3 className={css.sectionTitle}>{t('pages:traffic.detail.hooks.title')}</h3>
                {(e.requestHooksPipeline?.length || e.responseHooksPipeline?.length) ? (
                  <Stack gap="md">
                    <PipelineTimeline
                      label={t('pages:traffic.detail.hooks.requestPipeline')}
                      rows={e.requestHooksPipeline}
                      emptyLabel={t('pages:traffic.detail.hooks.noRequestHooks')}
                    />
                    <PipelineTimeline
                      label={t('pages:traffic.detail.hooks.responsePipeline')}
                      rows={e.responseHooksPipeline}
                      emptyLabel={t('pages:traffic.detail.hooks.noResponseHooks')}
                    />
                  </Stack>
                ) : (
                  <div data-testid="audit-drawer-no-hooks" className={css.mutedText}>
                    {t('pages:traffic.detail.hooks.noRequestHooks')}
                  </div>
                )}
              </div>
            </Stack>
          )}

          {/* ────────── Payloads ────────── */}
          {activeTab === 'payloads' && (
            <Stack gap="lg">
              {/* Error envelope banner — for non-200 rows the response body is a
                  gateway/provider error envelope, not chat content. Surface it as
                  a typed error so an investigator doesn't read the raw JSON as a
                  failed normalize. The body still renders in the sections below. */}
              {e.statusCode != null && e.statusCode >= 400 ? (
                <div className={css.errorBanner} data-testid="audit-drawer-error-banner">
                  <span className={css.errorBannerBadge}>
                    {t('pages:traffic.detail.payload.errorBadge')}
                  </span>
                  <div className={css.errorBannerBody}>
                    <span className={css.errorBannerTitle}>
                      {t('pages:traffic.detail.payload.errorTitle', {
                        status: e.statusCode,
                        code: e.errorCode || '—',
                      })}
                    </span>
                    {e.errorReason ? (
                      <span className={css.errorBannerReason}>{e.errorReason}</span>
                    ) : null}
                    <span className={css.errorBannerHint}>
                      {t('pages:traffic.detail.payload.errorHint')}
                    </span>
                  </div>
                </div>
              ) : null}
              {/* Normalized | Raw sub-tabs. */}
              <Stack direction="horizontal" gap="sm">
                <button
                  type="button"
                  onClick={() => setPayloadView('normalized')}
                  style={subTabStyle(payloadView === 'normalized')}
                  data-testid="payload-subtab-normalized"
                >
                  {t('pages:traffic.detail.normalized.tabs.normalized')}
                </button>
                <button
                  type="button"
                  onClick={() => setPayloadView('raw')}
                  style={subTabStyle(payloadView === 'raw')}
                  data-testid="payload-subtab-raw"
                >
                  {t('pages:traffic.detail.normalized.tabs.raw')}
                </button>
              </Stack>

              {payloadView === 'normalized' ? (
                <Stack gap="lg">
                  <section>
                    <div className={css.detailLabel}>
                      {t('pages:traffic.detail.payload.requestBody')}
                    </div>
                    <NormalizedPayloadView
                      payload={normalized.data?.requestNormalized ?? null}
                      spans={normalized.data?.requestRedactionSpans ?? null}
                      status={normalized.data?.requestStatus ?? null}
                      errorReason={normalized.data?.requestErrorReason ?? null}
                      direction="request"
                    />
                  </section>
                  <section>
                    <div className={css.detailLabel}>
                      {t('pages:traffic.detail.payload.responseBody')}
                    </div>
                    <NormalizedPayloadView
                      payload={normalized.data?.responseNormalized ?? null}
                      spans={normalized.data?.responseRedactionSpans ?? null}
                      status={normalized.data?.responseStatus ?? null}
                      errorReason={normalized.data?.responseErrorReason ?? null}
                      direction="response"
                    />
                  </section>
                </Stack>
              ) : hasPayloads ? (
                <>
                  <PayloadSection
                    label={t('pages:traffic.detail.payload.requestBody')}
                    value={e.requestBody}
                    spillRef={e.requestSpillRef}
                  />
                  <PayloadSection
                    label={t('pages:traffic.detail.payload.responseBody')}
                    value={e.responseBody}
                    spillRef={e.responseSpillRef}
                  />
                  <JsonSection label={t('pages:traffic.detail.payload.details')} value={e.details} />
                </>
              ) : (
                <div className={css.mutedText} style={{ paddingTop: 'var(--g-space-6)' }}>
                  {detail.loading
                    ? t('pages:traffic.detail.title') + '…'
                    : 'No payload data captured for this event.'}
                </div>
              )}
            </Stack>
          )}

        </div>
      </aside>

      {/* Emergency disable L2 confirmation dialog */}
      <AlertDialog
        open={confirmDisableL2Open}
        onOpenChange={setConfirmDisableL2Open}
        title={t('pages:traffic.detail.aiProvider.semanticHit.confirmTitle')}
        description={t('pages:traffic.detail.aiProvider.semanticHit.confirmBody')}
        confirmLabel={t('pages:traffic.detail.aiProvider.semanticHit.confirmYes')}
        cancelLabel={t('pages:traffic.detail.aiProvider.semanticHit.confirmCancel')}
        onConfirm={() => { void handleDisableL2Confirm(); }}
        variant="danger"
        loading={disableL2Loading}
      />

      {/* Mark as bad cache hit dialog */}
      <Dialog
        open={markBadOpen}
        onOpenChange={(open) => {
          setMarkBadOpen(open);
          if (!open) {
            setMarkBadReason('');
            setMarkBadReasonError('');
          }
        }}
        title={t('pages:traffic.detail.aiProvider.semanticHit.markBadTitle')}
        description={t('pages:traffic.detail.aiProvider.semanticHit.markBadBody')}
        size="sm"
      >
        <Stack gap="md">
          <div>
            <label
              htmlFor="mark-bad-reason"
              style={{ display: 'block', fontWeight: 'var(--g-font-weight-medium)', marginBottom: 'var(--g-space-1)', fontSize: 'var(--g-font-size-sm)' }}
            >
              {t('pages:traffic.detail.aiProvider.semanticHit.markBadReasonLabel')}
            </label>
            <textarea
              id="mark-bad-reason"
              rows={4}
              value={markBadReason}
              onChange={(ev) => {
                setMarkBadReason(ev.target.value);
                setMarkBadReasonError('');
              }}
              placeholder={t('pages:traffic.detail.aiProvider.semanticHit.markBadReasonPlaceholder')}
              style={{
                width: '100%',
                padding: 'var(--g-space-2) var(--g-space-3)',
                border: `1px solid ${markBadReasonError ? 'var(--color-error)' : 'var(--color-border)'}`,
                borderRadius: 'var(--g-radius-md)',
                background: 'var(--color-surface)',
                color: 'var(--color-text)',
                fontSize: 'var(--g-font-size-sm)',
                resize: 'vertical',
                boxSizing: 'border-box',
              }}
              data-testid="mark-bad-reason-textarea"
            />
            {markBadReasonError && (
              <div
                style={{ color: 'var(--color-error)', fontSize: 'var(--g-font-size-xs)', marginTop: 'var(--g-space-1)' }}
                role="alert"
                data-testid="mark-bad-reason-error"
              >
                {markBadReasonError}
              </div>
            )}
          </div>
          <Stack direction="horizontal" gap="sm" justify="end">
            <Button
              variant="secondary"
              onClick={() => {
                setMarkBadOpen(false);
                setMarkBadReason('');
                setMarkBadReasonError('');
              }}
              disabled={postFeedbackLoading}
            >
              {t('pages:traffic.detail.aiProvider.semanticHit.markBadCancel')}
            </Button>
            <Button
              onClick={handleMarkBadConfirm}
              loading={postFeedbackLoading}
              data-testid="mark-bad-confirm-btn"
            >
              {t('pages:traffic.detail.aiProvider.semanticHit.markBadConfirm')}
            </Button>
          </Stack>
        </Stack>
      </Dialog>
    </>
  );
}
