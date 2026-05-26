import type { CSSProperties, ReactNode } from 'react';
import { useState, useEffect, useCallback } from 'react';
import { useTranslation } from 'react-i18next';
import { Link } from 'react-router-dom';
import type {
  BlockingRule,
  HookExecutionRecord,
  TrafficEvent,
} from '../../../api/types';
import { Stack, AlertDialog, Button, Dialog } from '@/components/ui';
import { CopyJsonButton, DRAWER_MS } from '../../governance/adminAuditLogShared';
import { formatDateTime, formatUsdSci, formatTokens } from '@/lib/format';
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
import css from './trafficAuditDrawer.module.css';

export { DRAWER_MS, DRAWER_WIDTH } from '../../governance/adminAuditLogShared';

// Wider than the shared DRAWER_WIDTH — traffic events carry much more data
// (payloads, hooks, compliance stages) than admin audit log entries.
const TRAFFIC_DRAWER_WIDTH = 'min(860px, 95vw)';

type TabKey = 'overview' | 'ai' | 'compliance' | 'payloads';

// ── Primitive helpers ────────────────────────────────────────────────────────

function Block({ label, children }: { label: string; children: ReactNode }) {
  return (
    <div>
      <div className={css.detailLabel}>{label}</div>
      <div className={css.detailValue}>{children}</div>
    </div>
  );
}

function fmtScalar(v: unknown): string {
  if (v == null || v === '') return '—';
  return String(v);
}

function fmtCost(v: unknown): string {
  if (v == null || v === '') return '—';
  const n = typeof v === 'string' ? Number.parseFloat(v) : Number(v);
  if (Number.isNaN(n)) return String(v);
  // Audit-grade surface — use scientific-notation fallback for sub-microdollar
  // amounts so admins see "$1.5e-7" instead of the "<$0.000001" floor.
  // Other CP-UI surfaces keep formatUsd's safer floor.
  return formatUsdSci(n);
}

function nameWithId(name?: string | null, id?: string | null): string {
  if (name && id) return `${name} (${id.slice(0, 8)}…)`;
  if (name) return name;
  if (id) return `${id.slice(0, 8)}…`;
  return '—';
}

function formatBytes(n: number): string {
  if (!Number.isFinite(n) || n <= 0) return '0 B';
  const units = ['B', 'KiB', 'MiB', 'GiB', 'TiB'];
  let i = 0;
  let v = n;
  while (v >= 1024 && i < units.length - 1) {
    v /= 1024;
    i++;
  }
  return `${v.toFixed(v < 10 ? 2 : 1)} ${units[i]}`;
}

// ── Decision badge ───────────────────────────────────────────────────────────

function decisionTone(decision?: string | null): 'good' | 'bad' | 'warn' | 'muted' {
  if (!decision) return 'muted';
  const d = decision.toUpperCase();
  if (d === 'APPROVE') return 'good';
  if (d === 'REJECT_HARD' || d === 'BLOCK') return 'bad';
  if (d === 'BLOCK_SOFT' || d === 'MODIFY') return 'warn';
  if (d === 'ABSTAIN' || d === 'PASSTHROUGH' || d === 'EXEMPTED') return 'muted';
  return 'warn';
}

function decisionToneClass(decision?: string | null): string {
  switch (decisionTone(decision)) {
    case 'good': return css.decisionGood;
    case 'bad': return css.decisionBad;
    case 'warn': return css.decisionWarn;
    case 'muted':
    default: return css.decisionMuted;
  }
}

function DecisionBadge({ decision }: { decision?: string | null }) {
  const label = decision ?? '—';
  return <span className={`${css.decisionBadge} ${decisionToneClass(decision)}`}>{label}</span>;
}

// ── Hook execution helpers ───────────────────────────────────────────────────

// hookFields normalises a HookExecutionRecord across the two casings
// the data planes serialize today. Compliance-proxy sends PascalCase
// (HookName/HookID/Decision/…) because shared/hooks.HookResult has no
// json struct tags; ai-gateway sends lowerCamel (name/hookId/…).
function hookFields(r: HookExecutionRecord) {
  const id = r.hookId ?? r.HookID ?? '';
  const name = r.hookName ?? r.HookName ?? r.name ?? '';
  const impl = r.implementationId ?? r.ImplementationID ?? '';
  const decision = r.decision ?? r.Decision ?? '';
  const reason = r.reason ?? r.Reason ?? '';
  const reasonCode = r.reasonCode ?? r.ReasonCode ?? '';
  const latencyMs = r.latencyMs ?? r.LatencyMs;
  const order = r.order ?? r.Order ?? 0;
  const error = r.error ?? r.Error ?? '';
  return { id, name, impl, decision, reason, reasonCode, latencyMs, order, error };
}

function PipelineTimeline({
  label,
  rows,
  emptyLabel,
}: {
  label: string;
  rows: HookExecutionRecord[] | null | undefined;
  emptyLabel: string;
}) {
  if (!rows || rows.length === 0) {
    return (
      <div>
        <div className={css.detailLabel}>{label}</div>
        <div className={`${css.detailValue} ${css.mutedText}`}>{emptyLabel}</div>
      </div>
    );
  }
  const ordered = [...rows]
    .map((r) => ({ raw: r, ...hookFields(r) }))
    .sort((a, b) => a.order - b.order);
  return (
    <div>
      <div className={css.detailLabel}>{label} ({ordered.length})</div>
      <Stack gap="xs">
        {ordered.map((r, idx) => {
          const primary = r.name || r.id || 'hook';
          const showId = !!r.name && r.id;
          return (
            <div
              key={`${r.id || r.name || idx}`}
              className={css.hookCard}
            >
              <Stack direction="horizontal" justify="between" align="center" gap="sm">
                <Stack direction="horizontal" gap="sm" align="center">
                  <strong>{primary}</strong>
                  {r.impl && (
                    <span className={css.hookImplText}>
                      {r.impl}
                    </span>
                  )}
                </Stack>
                <DecisionBadge decision={r.decision} />
              </Stack>
              {showId && (
                <div className={css.hookIdText}>
                  id: <span className={css.mono}>{r.id}</span>
                </div>
              )}
              {(r.reason || r.reasonCode || r.latencyMs != null || r.error) && (
                <div className={css.hookDetailText}>
                  {r.reasonCode && <span style={{ marginRight: 'var(--g-space-2)' }}>[{r.reasonCode}]</span>}
                  {r.reason && <span style={{ marginRight: 'var(--g-space-2)' }}>{r.reason}</span>}
                  {r.latencyMs != null && <span style={{ marginRight: 'var(--g-space-2)' }}>{r.latencyMs} ms</span>}
                  {r.error && <span className={css.hookErrorText}>error: {r.error}</span>}
                </div>
              )}
            </div>
          );
        })}
      </Stack>
    </div>
  );
}

function BlockingRuleLine({ label, rule }: { label: string; rule: BlockingRule | null | undefined }) {
  if (!rule) return null;
  return (
    <Block label={label}>
      <span className={css.mono}>
        {rule.pack}@{rule.packVersion} · {rule.ruleId}
      </span>
    </Block>
  );
}

// ── Field group ──────────────────────────────────────────────────────────────

type FieldDef = { label: string; value: unknown; mono?: boolean; fullWidth?: boolean };

function FieldGroup({
  title,
  fields,
  cols = 1,
}: {
  title: string;
  fields: FieldDef[];
  cols?: 1 | 2;
}) {
  const nonEmpty = fields.filter((f) => f.value != null && f.value !== '' && f.value !== '—');
  if (nonEmpty.length === 0) return null;

  const renderField = (f: FieldDef) => (
    <Block key={f.label} label={f.label}>
      {f.mono ? <span className={css.mono}>{fmtScalar(f.value)}</span> : <>{fmtScalar(f.value)}</>}
    </Block>
  );

  return (
    <div>
      <h3 className={css.sectionTitle}>{title}</h3>
      {cols === 2 ? (
        <div className={css.fieldGrid}>
          {nonEmpty.map((f) => (
            <div key={f.label} style={f.fullWidth ? { gridColumn: '1 / -1' } : undefined}>
              {renderField(f)}
            </div>
          ))}
        </div>
      ) : (
        <Stack gap="sm">
          {nonEmpty.map((f) => renderField(f))}
        </Stack>
      )}
    </div>
  );
}

// ── Token stat card ──────────────────────────────────────────────────────────

function TokenCard({ label, value }: { label: string; value: string | null | undefined }) {
  if (!value) return null;
  return (
    <div className={css.tokenCard}>
      <div className={css.detailLabel}>{label}</div>
      <div className={css.tokenCardValue}>{value}</div>
    </div>
  );
}

// ── Routing flow comparison card ─────────────────────────────────────────────

// Shows "Requested → Routed To" side-by-side so it's immediately obvious
// whether the gateway rerouted the request. The right card gets a green accent
// when provider or model actually changed.
function RoutingFlowCard({
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

  const changed =
    (e.routedProviderName && e.routedProviderName !== e.providerName) ||
    (e.routedModelName && e.routedModelName !== e.modelName);

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

// ── JSON / payload sections ──────────────────────────────────────────────────

function JsonSection({ label, value }: { label: string; value: unknown }) {
  if (value == null) return null;
  if (Array.isArray(value) && value.length === 0) return null;
  if (typeof value === 'object' && !Array.isArray(value) && Object.keys(value as object).length === 0) return null;
  const text = JSON.stringify(value, null, 2);
  if (!text || text === '{}' || text === '[]') return null;
  return (
    <div className={css.jsonSectionWrap}>
      <Stack direction="horizontal" justify="between" align="center" className={css.jsonSectionHeader}>
        <strong className={css.jsonSectionLabel}>{label}</strong>
        <CopyJsonButton json={text} />
      </Stack>
      <pre className={css.preBlockLarge}>{text}</pre>
    </div>
  );
}

// PayloadSection renders request/response bodies. Bodies are stored as
// jsonb so they may arrive as JSON objects/arrays, JSON-encoded strings,
// numbers, or null. We don't assume any particular shape — strings are
// shown verbatim (without surrounding JSON quotes); structured values
// are pretty-printed; nullish / empty values are skipped silently.
//
// When `spillRef` is non-null the body was originally stored out-of-band
// (large captured payload). The CP detail handler resolves the ref and
// inlines the bytes onto `value`, but the ref metadata (backend, key,
// size, sha256) is also threaded through so the drawer can show a
// "Stored externally" badge — matters for ops who want to know whether
// a body sits in a shared bucket vs Postgres jsonb.
function PayloadSection({ label, value, spillRef }: { label: string; value: unknown; spillRef?: import('@/api/types').SpillRef | null }) {
  const hasValue = value != null && value !== '' &&
    !(typeof value === 'object' && !Array.isArray(value) && Object.keys(value as object).length === 0);
  if (!hasValue && !spillRef) return null;

  let display = '';
  if (hasValue) {
    if (typeof value === 'string') {
      display = value;
    } else {
      display = JSON.stringify(value, null, 2);
    }
  }

  return (
    <div className={css.jsonSectionWrap}>
      <Stack direction="horizontal" justify="between" align="center" className={css.jsonSectionHeader}>
        <Stack direction="horizontal" gap="sm" align="center">
          <strong className={css.jsonSectionLabel}>{label}</strong>
          {spillRef ? (
            <span title={`Backend: ${spillRef.backend}\nKey: ${spillRef.key}${spillRef.sha256 ? `\nsha256: ${spillRef.sha256}` : ''}`} className={css.mono}>
              [externally stored · {formatBytes(spillRef.size)} · {spillRef.backend}]
            </span>
          ) : null}
        </Stack>
        {display ? <CopyJsonButton json={display} /> : null}
      </Stack>
      {display ? <pre className={css.preBlockLarge}>{display}</pre> : (
        <pre className={css.preBlockLarge}>{spillRef ? '(spill body unresolved — SpillStore unreachable or not configured on Control Plane)' : ''}</pre>
      )}
    </div>
  );
}

// Renders a hook reason code with a translated tooltip for the closed set of
// known codes. Unknown codes fall back to plain mono text so future codes
// are not invisible.
function ReasonCodeChip({ code }: { code: string }) {
  const { t } = useTranslation();
  const known = new Set([
    'REDACT_INFLIGHT_UNSUPPORTED',
    'REDACT_STORAGE_ONLY_BY_POLICY',
    'STORAGE_DROPPED_BY_POLICY',
    'AIGUARD_SUGGESTED_VS_POLICY',
  ]);
  const tooltip = known.has(code)
    ? (t(`pages:traffic.detail.reasonCodes.${code}`, '') as string)
    : '';
  const isE46 = known.has(code);
  return (
    <span
      className={`${css.mono} ${css.stageReasonCode}`}
      title={tooltip || undefined}
      style={isE46 ? {
        background: 'color-mix(in srgb, var(--color-warning) 12%, transparent)',
        border: '1px solid color-mix(in srgb, var(--color-warning) 30%, transparent)',
        padding: 'var(--g-space-0-5) var(--g-space-1-5)',
        borderRadius: 'var(--g-radius-sm)',
        cursor: 'help',
      } : undefined}
    >
      [{code}]
    </span>
  );
}

// subTabStyle styles the inline Normalized / Raw toggle in the Payloads tab.
// Kept as inline style (rather than CSS module) so it inherits the active /
// inactive states without an extra CSS class.
function subTabStyle(active: boolean): CSSProperties {
  return {
    padding: '4px 12px',
    fontSize: 'var(--g-font-size-sm)',
    fontWeight: 'var(--g-font-weight-semibold)',
    border: `1px solid ${active ? 'var(--color-primary)' : 'var(--color-border)'}`,
    background: active ? 'color-mix(in srgb, var(--color-primary) 12%, transparent)' : 'transparent',
    color: active ? 'var(--color-primary)' : 'var(--color-text-muted)',
    borderRadius: 'var(--g-radius-md)',
    cursor: 'pointer',
  };
}

// ── Tab bar ──────────────────────────────────────────────────────────────────

function TabBar({
  active,
  onChange,
  tabs,
}: {
  active: TabKey;
  onChange: (t: TabKey) => void;
  tabs: Array<{ key: TabKey; label: string; badge?: number }>;
}) {
  return (
    <div className={css.tabBar}>
      {tabs.map((tab) => (
        <button
          key={tab.key}
          type="button"
          data-testid={`audit-drawer-tab-${tab.key}`}
          onClick={() => onChange(tab.key)}
          className={active === tab.key ? css.tabActive : css.tab}
        >
          {tab.label}
          {tab.badge != null && tab.badge > 0 && (
            <span className={css.tabBadge}>{tab.badge}</span>
          )}
        </button>
      ))}
    </div>
  );
}

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
    width: TRAFFIC_DRAWER_WIDTH,
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
                  gatewayLabel = t(`pages:traffic.detail.cache.gatewaySkip.${reason}`);
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

              {/* Costs breakdown: three-section taxonomy.
                  1) Upstream provider cost — what the LLM provider charged
                     (input/cache-read/cache-write/output components).
                  2) Nexus internal-ops — extra spend caused by our cache
                     machinery (L2 embedding); ai-guard cost is tagged
                     separately as a security hard-cost.
                  3) vs. no-gateway baseline — what the naive sticker cost
                     would have been, plus how much a gateway cache HIT saved.
                  Hidden when every line is null/zero so non-LLM rows stay clean. */}
              {(() => {
                const reasoning = e.reasoningCostUsd ?? 0;
                const embedding = e.embeddingCostUsd ?? 0;
                const aiGuard = e.aiGuardCostUsd ?? 0;
                const breakdown = e.internalOpsBreakdown ?? [];
                const breakdownTotal = breakdown.reduce((sum, b) => sum + (b.costUsd ?? 0), 0);
                const gatewaySavings = e.gatewayCacheSavingsUsd ?? 0;

                // Model pricing snapshot (LEFT JOIN at fetch time).
                const inputPM = e.modelInputPricePerMillion ?? null;
                const outputPM = e.modelOutputPricePerMillion ?? null;
                const cacheReadPM = e.modelCachedInputReadPricePerMillion ?? null;
                const cacheWritePM = e.modelCachedInputWritePricePerMillion ?? null;
                const promptT = e.promptTokens ?? 0;
                const completionT = e.completionTokens ?? 0;
                const cacheReadT = e.cacheReadTokens ?? 0;
                const cacheCreationT = e.cacheCreationTokens ?? 0;
                const uncachedT = Math.max(0, promptT - cacheReadT - cacheCreationT);

                // Per-component upstream math. The four catalog prices
                // displayed here are the same numbers the gateway used to
                // compute the row's cost (read from the Model row), so
                // the explicit rows sum exactly to the subtotal — no residual
                // line needed. If a Model row has NULL cache prices,
                // LookupCachePricing falls back to InputPricePM (flat rate),
                // which still keeps the decomposition closed.
                const uncachedCost = (inputPM != null && uncachedT > 0) ? (uncachedT * inputPM / 1_000_000) : null;
                const cachedReadCost = (cacheReadPM != null && cacheReadT > 0) ? (cacheReadT * cacheReadPM / 1_000_000) : null;
                const cachedWriteCost = (cacheWritePM != null && cacheCreationT > 0) ? (cacheCreationT * cacheWritePM / 1_000_000) : null;
                const outputCost = (outputPM != null && completionT > 0) ? (completionT * outputPM / 1_000_000) : null;

                // `primary` (upstream subtotal) is derived from the
                // per-component math above, not a separate
                // estimated_cost_usd column. The four components close
                // exactly (see comment block above). Reasoning sits
                // inside completion at output rate, so we exclude it
                // from this sum to avoid double-count.
                const primary = (uncachedCost ?? 0) + (cachedReadCost ?? 0) + (cachedWriteCost ?? 0) + (outputCost ?? 0);
                if (
                  primary === 0 && reasoning === 0 && embedding === 0 &&
                  aiGuard === 0 && breakdownTotal === 0 && gatewaySavings === 0
                ) {
                  return null;
                }

                // Naive baseline = "what this exact request would cost
                // without Nexus":
                //   all prompt tokens × full input price
                // + all completion tokens × full output price
                // (Reasoning tokens are a subset of completion — already
                // counted at output rate. ai-guard, L2 embedding, cache
                // savings are all Nexus-side and are explicitly NOT in
                // the baseline.)
                const naiveBaseline = (inputPM != null && outputPM != null)
                  ? (promptT * inputPM + completionT * outputPM) / 1_000_000
                  : null;

                // ai-guard is "internal-ops" too (we paid for the
                // classifier call), but it's a SECURITY hard cost so we
                // tag the row separately while still folding it into the
                // section subtotal. That keeps the math identity intact:
                //   Provider total + Internal-ops total = Net total.
                const internalOpsCost = embedding + breakdownTotal + aiGuard;
                const netSpend = primary + internalOpsCost;
                // vsBaseline = net spend with Nexus − naive without Nexus.
                // Positive = paid more (typical when caching adds write
                // surcharge or ai-guard adds security cost). Negative =
                // saved (typical on cache HIT — no upstream call at all).
                // Baseline deliberately excludes ai-guard / embedding —
                // those wouldn't exist in a no-Nexus world.
                const vsBaseline = naiveBaseline != null ? netSpend - naiveBaseline : null;
                // vsBaseline > 0 = paid more than naive (e.g. write surcharge + embedding without HIT)
                // vsBaseline < 0 = saved money (cache HIT, cache-read discount)

                const monoRight = { padding: 'var(--g-space-xs) 0', textAlign: 'right' as const, fontFamily: 'var(--g-font-mono)' };
                const monoRightStrong = { ...monoRight, fontWeight: 'var(--g-font-weight-semibold)' };
                const labelCell = (muted = false): CSSProperties => ({
                  padding: 'var(--g-space-xs) 0',
                  color: muted ? 'var(--g-color-text-muted)' : undefined,
                  fontSize: 'var(--g-font-size-sm)',
                });
                const sectionHeader = {
                  padding: 'var(--g-space-sm) 0 var(--g-space-xs)',
                  fontSize: 'var(--g-font-size-xs)',
                  textTransform: 'uppercase' as const,
                  letterSpacing: '0.04em',
                  color: 'var(--g-color-text-muted)',
                };

                return (
                  <div className={css.fieldGroup}>
                    <h3 className={css.sectionTitle}>{t('pages:traffic.detail.costs.title')}</h3>
                    <table style={{ width: '100%', borderCollapse: 'collapse' }}>
                      <tbody>
                        {/* ─── 1. Upstream provider cost ─── */}
                        {primary !== 0 && (
                          <>
                            <tr>
                              <td colSpan={2} style={sectionHeader}>
                                {t('pages:traffic.detail.costs.section.upstream')}
                              </td>
                            </tr>
                            {uncachedT > 0 && (
                              <tr>
                                <td style={labelCell(true)}>
                                  {formatTokens(uncachedT)} {inputPM != null ? <>× {fmtCost(inputPM)}/M</> : null}&nbsp;
                                  <span style={{ fontSize: 'var(--g-font-size-xs)' }}>({t('pages:traffic.detail.costs.input')})</span>
                                  {inputPM == null && <span style={{ fontSize: 'var(--g-font-size-xs)', color: 'var(--g-color-text-muted)' }}> · {t('pages:traffic.detail.costs.priceUnknown')}</span>}
                                </td>
                                <td style={monoRight}>{uncachedCost != null ? fmtCost(uncachedCost) : '—'}</td>
                              </tr>
                            )}
                            {cacheReadT > 0 && (
                              <tr>
                                <td style={labelCell(true)}>
                                  {formatTokens(cacheReadT)} {cacheReadPM != null ? <>× {fmtCost(cacheReadPM)}/M</> : null}&nbsp;
                                  <span style={{ fontSize: 'var(--g-font-size-xs)' }}>({t('pages:traffic.detail.costs.cacheRead')})</span>
                                  {cacheReadPM == null && <span style={{ fontSize: 'var(--g-font-size-xs)', color: 'var(--g-color-text-muted)' }}> · {t('pages:traffic.detail.costs.priceUnknown')}</span>}
                                </td>
                                <td style={monoRight}>{cachedReadCost != null ? fmtCost(cachedReadCost) : '—'}</td>
                              </tr>
                            )}
                            {cacheCreationT > 0 && (
                              <tr>
                                <td style={labelCell(true)}>
                                  {formatTokens(cacheCreationT)} {cacheWritePM != null ? <>× {fmtCost(cacheWritePM)}/M</> : null}&nbsp;
                                  <span style={{ fontSize: 'var(--g-font-size-xs)' }}>({t('pages:traffic.detail.costs.cacheWrite')})</span>
                                  {cacheWritePM == null && <span style={{ fontSize: 'var(--g-font-size-xs)', color: 'var(--g-color-text-muted)' }}> · {t('pages:traffic.detail.costs.priceUnknown')}</span>}
                                </td>
                                <td style={monoRight}>{cachedWriteCost != null ? fmtCost(cachedWriteCost) : '—'}</td>
                              </tr>
                            )}
                            {completionT > 0 && (
                              <tr>
                                <td style={labelCell(true)}>
                                  {formatTokens(completionT)} {outputPM != null ? <>× {fmtCost(outputPM)}/M</> : null}&nbsp;
                                  <span style={{ fontSize: 'var(--g-font-size-xs)' }}>({t('pages:traffic.detail.costs.output')})</span>
                                  {outputPM == null && <span style={{ fontSize: 'var(--g-font-size-xs)', color: 'var(--g-color-text-muted)' }}> · {t('pages:traffic.detail.costs.priceUnknown')}</span>}
                                </td>
                                <td style={monoRight}>{outputCost != null ? fmtCost(outputCost) : '—'}</td>
                              </tr>
                            )}
                            {reasoning !== 0 && (
                              <tr>
                                <td style={labelCell(true)}>{t('pages:traffic.detail.costs.reasoning')}</td>
                                <td style={monoRight}>{fmtCost(reasoning)}</td>
                              </tr>
                            )}
                            <tr style={{ borderTop: '1px solid var(--g-color-border-subtle)' }}>
                              <td style={{ ...labelCell(), fontWeight: 'var(--g-font-weight-semibold)' }}>{t('pages:traffic.detail.costs.section.upstreamSubtotal')}</td>
                              <td style={monoRightStrong}>{fmtCost(primary)}</td>
                            </tr>
                          </>
                        )}

                        {/* ─── 2. Nexus internal-ops ───
                            Combines cache-machinery costs (L2 embedding,
                            future hook-side LLMs) with security hard
                            costs (ai-guard). All are gateway-side spend
                            outside the primary upstream call. Each row
                            carries an inline tag identifying its sub-
                            category ([cache] / [security]) so admins can
                            still distinguish optimization spend from
                            security spend without breaking the math
                            identity: Provider total + Internal-ops total
                            = Net total. */}
                        {(embedding !== 0 || aiGuard !== 0 || breakdownTotal !== 0) && (
                          <>
                            <tr>
                              <td colSpan={2} style={sectionHeader}>
                                {t('pages:traffic.detail.costs.section.internalOps')}
                              </td>
                            </tr>
                            {embedding !== 0 && (
                              <tr>
                                <td style={labelCell()}>
                                  + {t('pages:traffic.detail.costs.embedding')}&nbsp;
                                  <span style={{ fontSize: 'var(--g-font-size-xs)', color: 'var(--g-color-text-muted)' }}>{t('pages:traffic.detail.costs.cacheTag')}</span>
                                </td>
                                <td style={monoRight}>+ {fmtCost(embedding)}</td>
                              </tr>
                            )}
                            {aiGuard !== 0 && (
                              <tr>
                                <td style={labelCell()}>
                                  + {t('pages:traffic.detail.costs.aiGuard')}&nbsp;
                                  <span style={{ fontSize: 'var(--g-font-size-xs)', color: 'var(--g-color-text-muted)' }}>{t('pages:traffic.detail.costs.aiGuardTag')}</span>
                                </td>
                                <td style={monoRight}>+ {fmtCost(aiGuard)}</td>
                              </tr>
                            )}
                            {breakdown.map((b, i) => (
                              <tr key={`brk-${i}`}>
                                <td style={labelCell()}>+ {b.type}{b.model ? ` (${b.model})` : ''}</td>
                                <td style={monoRight}>+ {fmtCost(b.costUsd ?? 0)}</td>
                              </tr>
                            ))}
                            <tr style={{ borderTop: '1px solid var(--g-color-border-subtle)' }}>
                              <td style={{ ...labelCell(), fontWeight: 'var(--g-font-weight-semibold)' }}>{t('pages:traffic.detail.costs.section.internalOpsSubtotal')}</td>
                              <td style={monoRightStrong}>+ {fmtCost(internalOpsCost)}</td>
                            </tr>
                          </>
                        )}

                        {/* ─── Net total ─── */}
                        <tr style={{ borderTop: '2px solid var(--g-color-border)' }}>
                          <td style={{ ...labelCell(), fontWeight: 'var(--g-font-weight-bold)', paddingTop: 'var(--g-space-sm)' }}>
                            {t('pages:traffic.detail.costs.netTotal')}
                          </td>
                          <td style={{ ...monoRightStrong, fontWeight: 'var(--g-font-weight-bold)', paddingTop: 'var(--g-space-sm)' }}>
                            {fmtCost(netSpend)}
                          </td>
                        </tr>

                        {/* ─── 4. vs. no-gateway baseline (comparison line) ─── */}
                        {naiveBaseline != null && (
                          <>
                            <tr>
                              <td colSpan={2} style={sectionHeader}>
                                {t('pages:traffic.detail.costs.section.vsBaseline')}
                              </td>
                            </tr>
                            <tr>
                              <td style={labelCell(true)}>{t('pages:traffic.detail.costs.naiveBaseline')}</td>
                              <td style={monoRight}>{fmtCost(naiveBaseline)}</td>
                            </tr>
                            {gatewaySavings !== 0 && (
                              <tr>
                                <td style={labelCell(true)}>{t('pages:traffic.detail.costs.cacheSavings')}</td>
                                <td style={monoRight}>− {fmtCost(gatewaySavings)}</td>
                              </tr>
                            )}
                            <tr>
                              <td style={{ ...labelCell(), fontWeight: 'var(--g-font-weight-semibold)' }}>
                                {vsBaseline! < 0 ? t('pages:traffic.detail.costs.savedVsBaseline') : t('pages:traffic.detail.costs.paidMoreVsBaseline')}
                              </td>
                              <td style={{ ...monoRightStrong, color: vsBaseline! < 0 ? 'var(--g-color-success)' : undefined }}>
                                {vsBaseline! < 0 ? '−' : '+'} {fmtCost(Math.abs(vsBaseline!))}
                              </td>
                            </tr>
                          </>
                        )}
                      </tbody>
                    </table>
                    {/* Quota policy note — make it clear whether ai-guard
                        / embedding count against this VK's quota. */}
                    {(embedding !== 0 || aiGuard !== 0 || breakdownTotal !== 0) && (
                      <div style={{ marginTop: 'var(--g-space-sm)', fontSize: 'var(--g-font-size-xs)', color: 'var(--g-color-text-muted)' }}>
                        {t(internalOpsExcluded
                          ? 'pages:traffic.detail.costs.internalOpsNoteExcluded'
                          : 'pages:traffic.detail.costs.internalOpsNoteCounted')}
                      </div>
                    )}
                  </div>
                );
              })()}

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
