/**
 * TrafficEventDetail — right-side slide-in drawer that opens when a
 * Traffic table row is clicked. Structurally aligned with CP-UI's
 * trafficAuditDrawer.tsx (same Block / SectionTitle / HookCard /
 * DecisionBadge layout). Drawer width matches CP-UI (`min(860px, 95vw)`).
 *
 * Agent-specific addition: the 5-segment SVG phase waterfall (CP-UI
 * ships its own LatencyWaterfall for the same purpose).
 */
import type { ReactNode } from 'react';
import { useState } from 'react';
import { useTranslation } from 'react-i18next';
import { getPhaseColors } from '@nexus-gateway/ui-shared';
import { useTheme } from '@/theme/ThemeProvider';
import type { AgentEvent } from '@/api/agent';
import { NormalizedPayloadView } from '@/components/normalized/NormalizedPayloadView';

interface Props {
  event: AgentEvent | null;
  onClose: () => void;
}

// #75 — clamp(min, mid, max) keeps the drawer readable on every
// viewport: at least 420px so SSE / JSON payloads don't squeeze into
// unreadable wraps; mid 55vw so a 1100-1300px agent-UI window still
// shows half the page (the traffic list stays usable behind the
// drawer instead of being completely hidden); max 720px so the
// drawer never grows past where the eye can scan one column.
const DRAWER_WIDTH = 'clamp(420px, 55vw, 720px)';

function fmtMs(ms: number): string {
  if (!Number.isFinite(ms) || ms <= 0) return '0ms';
  if (ms < 1000) return `${Math.round(ms)}ms`;
  return `${(ms / 1000).toFixed(2)}s`;
}

function fmtTime(iso?: string): string {
  if (!iso) return '—';
  try {
    return new Date(iso).toLocaleString();
  } catch {
    return iso;
  }
}

// Block: label-on-top + value-below, matches CP-UI's .detailLabel / .detailValue.
function Block({ label, children }: { label: string; children: ReactNode }) {
  return (
    <div>
      <div
        style={{
          fontSize: 'var(--g-font-size-xs)',
          fontWeight: 'var(--g-font-weight-semibold)',
          color: 'var(--color-text-muted)',
          textTransform: 'uppercase',
          letterSpacing: 0,
          marginBottom: 'var(--g-space-1)',
        }}
      >
        {label}
      </div>
      <div
        style={{
          fontSize: 'var(--g-font-size-sm)',
          fontWeight: 'var(--g-font-weight-medium)',
          wordBreak: 'break-word',
          overflowWrap: 'anywhere',
          lineHeight: 1.45,
        }}
      >
        {children}
      </div>
    </div>
  );
}

// SectionTitle: uppercase divider matching CP-UI's .sectionTitle.
function SectionTitle({ children }: { children: ReactNode }) {
  return (
    <h3
      style={{
        fontSize: 'var(--g-font-size-xs)',
        fontWeight: 'var(--g-font-weight-semibold)',
        textTransform: 'uppercase',
        letterSpacing: 0,
        color: 'var(--color-text-secondary)',
        marginTop: 'var(--g-space-6)',
        marginBottom: 'var(--g-space-1)',
        paddingBottom: 'var(--g-space-1)',
        borderBottom: '1px solid var(--color-border)',
      }}
    >
      {children}
    </h3>
  );
}

// DecisionBadge tone palette aligned with CP-UI's decisionTone() switch.
function decisionTone(decision?: string | null): 'good' | 'bad' | 'warn' | 'muted' {
  if (!decision) return 'muted';
  const d = decision.toUpperCase();
  if (d === 'APPROVE' || d === 'ALLOW' || d === 'PASSTHROUGH') return 'good';
  if (d === 'REJECT_HARD' || d === 'BLOCK' || d === 'DENY') return 'bad';
  if (d === 'BLOCK_SOFT' || d === 'MODIFY' || d === 'INSPECT') return 'warn';
  if (d === 'ABSTAIN' || d === 'EXEMPTED') return 'muted';
  return 'warn';
}

function DecisionBadge({ decision }: { decision?: string | null }) {
  const tone = decisionTone(decision);
  const palette: Record<string, { bg: string; fg: string }> = {
    good: { bg: 'var(--color-success-bg)', fg: 'var(--color-success)' },
    bad: { bg: 'var(--color-danger-bg)', fg: 'var(--color-danger)' },
    warn: { bg: 'var(--color-warning-bg)', fg: 'var(--color-warning)' },
    muted: { bg: 'var(--color-bg-subtle)', fg: 'var(--color-text-muted)' },
  };
  const { bg, fg } = palette[tone];
  return (
    <span
      style={{
        display: 'inline-block',
        padding: 'var(--g-space-0-5) var(--g-space-2)',
        borderRadius: 'var(--g-radius-sm)',
        background: bg,
        color: fg,
        fontSize: 'var(--g-font-size-xs)',
        fontWeight: 'var(--g-font-weight-semibold)',
        textTransform: 'uppercase',
        letterSpacing: 0,
      }}
    >
      {decision ?? '—'}
    </span>
  );
}

interface Segment {
  key: string;
  label: string;
  ms: number;
  color: string;
}

function Waterfall({ event, t }: { event: AgentEvent; t: ReturnType<typeof useTranslation>['t'] }) {
  const { resolvedMode } = useTheme();
  const phase = getPhaseColors(resolvedMode);
  const total = event.latencyMs ?? 0;
  const reqHooks = Math.max(0, event.requestHooksMs ?? 0);
  const respHooks = Math.max(0, event.responseHooksMs ?? 0);
  const upTtfb = Math.max(0, event.upstreamTtfbMs ?? 0);
  // Fallback chain (matches control-plane-ui LatencyMini): prefer
  // upstream_total_ms; if null but ttfb is present, use ttfb so
  // "Our Overhead" doesn't silently absorb the upstream wait.
  const upTotalRaw = event.upstreamTotalMs ?? event.upstreamTtfbMs ?? 0;
  const upTotal = Math.max(0, upTotalRaw);
  const upBody = Math.max(0, upTotal - upTtfb);
  const our = Math.max(0, total - upTotal - reqHooks - respHooks);

  const segs: Segment[] = [
    { key: 'reqHooks', label: t('traffic.waterfall.reqHooks', 'Request Hooks'), ms: reqHooks, color: phase.reqHooks },
    { key: 'our', label: t('traffic.waterfall.ourOther', 'Our Overhead'), ms: our, color: phase.our },
    { key: 'ttfb', label: t('traffic.waterfall.upstreamTtfb', 'Upstream TTFB'), ms: upTtfb, color: phase.ttfb },
    { key: 'body', label: t('traffic.waterfall.upstreamBody', 'Upstream Body'), ms: upBody, color: phase.body },
    { key: 'respHooks', label: t('traffic.waterfall.respHooks', 'Response Hooks'), ms: respHooks, color: phase.respHooks },
  ];
  const sum = segs.reduce((acc, s) => acc + s.ms, 0);
  if (sum <= 0) {
    return (
      <div style={{ color: 'var(--color-text-muted)', fontStyle: 'italic', padding: 'var(--g-space-3) var(--g-space-0)' }}>
        {t('traffic.waterfall.noData', 'No phase data available for this event.')}
      </div>
    );
  }
  return (
    <div>
      <div style={{ display: 'flex', height: 22, width: '100%', borderRadius: 'var(--g-radius-sm)', overflow: 'hidden', background: 'var(--color-surface-2)' }}>
        {segs.map((s) => (
          <div
            key={s.key}
            title={`${s.label}: ${fmtMs(s.ms)}`}
            style={{ width: `${(s.ms / sum) * 100}%`, background: s.color }}
          />
        ))}
      </div>
      <div style={{ marginTop: 'var(--g-space-2)', display: 'grid', gridTemplateColumns: 'repeat(auto-fill, minmax(160px, 1fr))', gap: 'var(--g-space-1-5)', fontSize: 'var(--g-font-size-xs)' }}>
        {segs.map((s) => (
          <div key={s.key} style={{ display: 'flex', alignItems: 'center', gap: 'var(--g-space-1-5)' }}>
            <span style={{ width: 10, height: 10, background: s.color, borderRadius: 'var(--g-radius-sm)' }} />
            <span style={{ flex: 1 }}>{s.label}</span>
            <span style={{ color: 'var(--color-text-muted)' }}>{fmtMs(s.ms)}</span>
          </div>
        ))}
      </div>
    </div>
  );
}

interface HookRow {
  id?: string;
  hookId?: string;
  hookName?: string;
  name?: string;
  implementationId?: string;
  stage?: string;
  decision?: string;
  reason?: string;
  reasonCode?: string;
  latencyMs?: number;
  order?: number;
  error?: string;
}

// HookCard matches CP-UI's hookCard pattern: top row primary+impl + decision
// badge; secondary row id (mono); tertiary row reason/code/latency/error.
function HookCard({ row }: { row: HookRow }) {
  const primary = row.hookName ?? row.name ?? row.hookId ?? row.id ?? 'hook';
  const showId = (row.hookName || row.name) && (row.hookId || row.id);
  return (
    <div
      style={{
        padding: 'var(--g-space-2-5) var(--g-space-3)',
        background: 'var(--color-bg-subtle)',
        border: '1px solid var(--color-border)',
        borderRadius: 'var(--g-radius-md)',
        display: 'flex',
        flexDirection: 'column',
        gap: 'var(--g-space-1)',
      }}
    >
      <div style={{ display: 'flex', justifyContent: 'space-between', alignItems: 'center', gap: 'var(--g-space-2)' }}>
        <div style={{ display: 'flex', alignItems: 'center', gap: 'var(--g-space-2)' }}>
          <strong style={{ fontSize: 'var(--g-font-size-sm)' }}>{primary}</strong>
          {row.implementationId && (
            <span style={{ fontSize: 'var(--g-font-size-xs)', color: 'var(--color-text-muted)' }}>
              {row.implementationId}
            </span>
          )}
          {row.stage && (
            <span style={{ fontSize: 'var(--g-font-size-xs)', color: 'var(--color-text-muted)' }}>
              · {row.stage}
            </span>
          )}
        </div>
        <DecisionBadge decision={row.decision} />
      </div>
      {showId && (
        <div style={{ fontSize: 'var(--g-font-size-xs)', color: 'var(--color-text-muted)' }}>
          id: <span style={{ fontFamily: 'var(--g-font-mono)' }}>{row.hookId ?? row.id}</span>
        </div>
      )}
      {(row.reason || row.reasonCode || row.latencyMs != null || row.error) && (
        <div style={{ fontSize: 'var(--g-font-size-xs)', color: 'var(--color-text-secondary)' }}>
          {row.reasonCode && <span style={{ marginRight: 'var(--g-space-2)' }}>[{row.reasonCode}]</span>}
          {row.reason && <span style={{ marginRight: 'var(--g-space-2)' }}>{row.reason}</span>}
          {row.latencyMs != null && <span style={{ marginRight: 'var(--g-space-2)' }}>{fmtMs(row.latencyMs)}</span>}
          {row.error && <span style={{ color: 'var(--color-danger)' }}>error: {row.error}</span>}
        </div>
      )}
    </div>
  );
}

function HookList({ raw, t }: { raw: unknown; t: ReturnType<typeof useTranslation>['t'] }) {
  // Backend sends event.hooksPipeline as a json.RawMessage on the Go
  // side, which the JSON encoder writes as the RAW value (an array),
  // not as a quoted JSON string. The previous code assumed `raw` was
  // always a string and called JSON.parse on the array, which throws
  // ("[object Array]" is not valid JSON) and rendered "unparseable"
  // even when the pipeline executed normally. Now we accept either
  // shape: string → parse; array → use directly; anything else → empty.
  let rows: HookRow[] = [];
  if (Array.isArray(raw)) {
    rows = raw as HookRow[];
  } else if (typeof raw === 'string' && raw.length > 0) {
    try {
      const parsed = JSON.parse(raw);
      if (Array.isArray(parsed)) rows = parsed as HookRow[];
    } catch {
      return (
        <div style={{ color: 'var(--color-text-muted)', fontStyle: 'italic' }}>
          {t('traffic.detail.hooksParseError', 'Hook pipeline payload is unparseable.')}
        </div>
      );
    }
  }
  if (rows.length === 0) {
    return (
      <div style={{ color: 'var(--color-text-muted)', fontStyle: 'italic' }}>
        {t('traffic.detail.hooksEmpty', 'No hooks executed for this event.')}
      </div>
    );
  }
  // Sort by order if present, falling back to original index.
  const ordered = rows
    .map((r, i) => ({ row: r, order: r.order ?? i }))
    .sort((a, b) => a.order - b.order)
    .map((x) => x.row);
  return (
    <div style={{ display: 'flex', flexDirection: 'column', gap: 'var(--g-space-2)' }}>
      {ordered.map((r, i) => (
        <HookCard key={`${r.hookId ?? r.id ?? i}`} row={r} />
      ))}
    </div>
  );
}

export function TrafficEventDetail({ event, onClose }: Props) {
  const { t } = useTranslation();
  if (!event) return null;
  return (
    <div
      role="dialog"
      aria-modal="true"
      style={{
        position: 'fixed',
        top: 0, right: 0, bottom: 0,
        width: DRAWER_WIDTH,
        background: 'var(--color-surface)',
        borderLeft: '1px solid var(--color-border)',
        boxShadow: 'var(--shadow-xl)',
        zIndex: 'var(--g-z-modal)',
        display: 'flex',
        flexDirection: 'column',
      }}
    >
      {/* drawerHeader — fixed top, separator below */}
      <div
        style={{
          flexShrink: 0,
          padding: 'var(--g-space-4) var(--g-space-5)',
          borderBottom: '1px solid var(--color-border)',
          display: 'flex',
          justifyContent: 'space-between',
          alignItems: 'center',
        }}
      >
        <h2
          style={{
            margin: 'var(--g-space-0)',
            fontFamily: 'var(--g-font-display)',
            fontWeight: 'var(--g-font-weight-semibold)',
            fontSize: 'var(--g-font-size-md)',
          }}
        >
          {t('traffic.detail.title', 'Event Details')}
        </h2>
        <button
          type="button"
          onClick={onClose}
          aria-label={t('traffic.detail.close', 'Close')}
          style={{
            display: 'inline-flex',
            alignItems: 'center',
            justifyContent: 'center',
            width: 36,
            height: 36,
            border: 0,
            borderRadius: 'var(--g-radius-md)',
            background: 'transparent',
            color: 'var(--color-text-muted)',
            fontSize: 'var(--g-font-size-xl)',
            lineHeight: 1,
            cursor: 'pointer',
          }}
        >
          ×
        </button>
      </div>

      {/* drawerBody — scrollable */}
      <div
        style={{
          flex: 1,
          overflowY: 'auto',
          padding: 'var(--g-space-5)',
        }}
      >
        {/* Overview block */}
        <div style={{ display: 'grid', gridTemplateColumns: 'repeat(2, 1fr)', gap: 'var(--g-space-3) var(--g-space-4)' }}>
          <Block label={t('traffic.detail.fieldTime', 'Time')}>
            <span style={{ fontFamily: 'var(--g-font-mono)' }}>{fmtTime(event.timestamp)}</span>
          </Block>
          <Block label={t('traffic.detail.fieldId', 'Event ID')}>
            <span style={{ fontFamily: 'var(--g-font-mono)' }}>{event.id ?? '—'}</span>
          </Block>
          <Block label={t('traffic.detail.fieldProcess', 'Process')}>{event.sourceProcess ?? '—'}</Block>
          <Block label={t('traffic.detail.fieldTarget', 'Target')}>
            <span style={{ fontFamily: 'var(--g-font-mono)' }}>{event.targetHost ?? event.destHost ?? '—'}</span>
          </Block>
          <Block label={t('traffic.detail.fieldAction', 'Action')}>
            <DecisionBadge decision={event.action} />
          </Block>
          <Block label={t('traffic.detail.fieldHookDecision', 'Hook Decision')}>
            {event.hookDecision ? <DecisionBadge decision={event.hookDecision} /> : '—'}
          </Block>
          <Block label={t('traffic.detail.fieldLatency', 'Latency')}>
            {event.latencyMs != null ? fmtMs(event.latencyMs) : '—'}
          </Block>
          <Block label={t('traffic.detail.fieldStatus', 'Status')}>
            {/* Use truthy check so 0 (passthrough flows have no visible
                HTTP layer; inspect flows where upstream RTT never landed
                a status) renders as "—" instead of literal "0".
                Consistent with Method/Path fields below which use `|| '—'`. */}
            {(event as { statusCode?: number }).statusCode || '—'}
          </Block>
          <Block label={t('traffic.detail.fieldMethod', 'Method')}>
            <span style={{ fontFamily: 'var(--g-font-mono)' }}>
              {(event as { method?: string }).method || '—'}
            </span>
          </Block>
          <Block label={t('traffic.detail.fieldPath', 'Path / URL')}>
            <span style={{ fontFamily: 'var(--g-font-mono)', wordBreak: 'break-all' }}>
              {(event as { path?: string }).path || '—'}
            </span>
          </Block>
          <Block label={t('traffic.detail.fieldDestIp', 'Destination IP:Port')}>
            <span style={{ fontFamily: 'var(--g-font-mono)' }}>
              {(() => {
                const e = event as { destIp?: string; destPort?: number };
                if (!e.destIp) return '—';
                return e.destPort ? `${e.destIp}:${e.destPort}` : e.destIp;
              })()}
            </span>
          </Block>
          <Block label={t('traffic.detail.fieldBytes', 'Bytes (in / out)')}>
            <span style={{ fontFamily: 'var(--g-font-mono)' }}>
              {(() => {
                const e = event as { bytesIn?: number; bytesOut?: number };
                const inB = e.bytesIn ?? 0;
                const outB = e.bytesOut ?? 0;
                if (inB === 0 && outB === 0) return '—';
                const fmt = (n: number) =>
                  n >= 1_000_000 ? `${(n / 1_000_000).toFixed(2)} MB`
                  : n >= 1_000 ? `${(n / 1_000).toFixed(1)} KB`
                  : `${n} B`;
                return `${fmt(inB)} ↓ / ${fmt(outB)} ↑`;
              })()}
            </span>
          </Block>
          <Block label={t('traffic.detail.fieldProvider', 'AI Provider / Model')}>
            <span>
              {(() => {
                const e = event as { providerName?: string; modelName?: string };
                if (!e.providerName && !e.modelName) return '—';
                return [e.providerName, e.modelName].filter(Boolean).join(' / ');
              })()}
            </span>
          </Block>
        </div>

        {/* Phase waterfall — agent specialty (CP-UI ships its own LatencyWaterfall) */}
        <SectionTitle>{t('traffic.detail.waterfall.title', 'Phase Breakdown')}</SectionTitle>
        <Waterfall event={event} t={t} />

        {/* Hook pipeline */}
        <SectionTitle>{t('traffic.detail.hooksTitle', 'Hook Pipeline')}</SectionTitle>
        {event.hooksPipeline ? <HookList raw={event.hooksPipeline} t={t} /> : (
          <div style={{ color: 'var(--color-text-muted)', fontStyle: 'italic' }}>
            {t('traffic.detail.hooksEmpty', 'No hooks executed for this event.')}
          </div>
        )}

        {/* Payloads (Request / Response bodies) — parity with CP UI
            traffic-event drawer. Currently shows Raw (utf-8 decoded
            base64 from the wire). Normalized tab (per-adapter codec
            translation) is a follow-up; for now the Raw view answers
            "what did the LLM see" / "what did it return". */}
        <SectionTitle>{t('traffic.detail.payloadsTitle', 'Payloads')}</SectionTitle>
        <PayloadsSection event={event} t={t} />
      </div>
    </div>
  );
}

/**
 * PayloadsSection — V2 (#58) — Request + Response payload viewer with
 * Raw / Normalized tab switch. Mirrors CP-UI trafficAuditDrawer's
 * PayloadSection layout: a tab strip per direction, Normalized renders
 * via the shared NormalizedPayloadView (canonical OpenAI-shape chat
 * bubbles / tool-use cards / etc.), Raw renders the utf-8 decoded
 * base64 bytes. Normalized tab is disabled when the event carries no
 * pre-normalized JSON (non-AI traffic / non-bumped flow).
 */
function PayloadsSection({
  event,
  t,
}: {
  event: AgentEvent;
  t: ReturnType<typeof useTranslation>['t'];
}) {
  const hasNormalized = !!(event.normalizedRequest || event.normalizedResponse);
  // Single Tab controls BOTH directions — matches CP UI trafficAuditDrawer.
  // (Earlier per-direction tabs felt redundant — switching once should flip
  // both Request and Response views consistently.)
  const [tab, setTab] = useState<'normalized' | 'raw'>(hasNormalized ? 'normalized' : 'raw');
  return (
    <>
      <div style={{ display: 'flex', gap: 'var(--g-space-1)', marginBottom: 'var(--g-space-3)' }}>
        <button
          type="button"
          onClick={() => setTab('normalized')}
          disabled={!hasNormalized}
          style={{
            padding: 'var(--g-space-1) var(--g-space-3)',
            border: '1px solid var(--color-border)',
            borderRadius: 'var(--g-radius-sm)',
            background: tab === 'normalized' ? 'var(--color-bg-pressed, rgba(0,0,0,0.08))' : 'transparent',
            fontWeight: tab === 'normalized' ? 600 : 400,
            opacity: hasNormalized ? 1 : 0.5,
            fontSize: 'var(--g-font-size-sm)',
          }}
          title={hasNormalized ? undefined : t('trafficDetail.tabNormalizedDisabled', 'No normalized payload for this event (non-AI traffic or non-bumped flow).')}
        >
          {t('trafficDetail.tabNormalized', 'Normalized')}
        </button>
        <button
          type="button"
          onClick={() => setTab('raw')}
          style={{
            padding: 'var(--g-space-1) var(--g-space-3)',
            border: '1px solid var(--color-border)',
            borderRadius: 'var(--g-radius-sm)',
            background: tab === 'raw' ? 'var(--color-bg-pressed, rgba(0,0,0,0.08))' : 'transparent',
            fontWeight: tab === 'raw' ? 600 : 400,
            fontSize: 'var(--g-font-size-sm)',
          }}
        >
          {t('trafficDetail.tabRaw', 'Raw')}
        </button>
      </div>
      <PayloadDirection
        label={t('traffic.detail.payloadRequest', 'Request body')}
        base64={(event as { payloadRequest?: string }).payloadRequest}
        normalized={event.normalizedRequest}
        direction="request"
        tab={tab}
        t={t}
      />
      <PayloadDirection
        label={t('traffic.detail.payloadResponse', 'Response body')}
        base64={(event as { payloadResponse?: string }).payloadResponse}
        normalized={event.normalizedResponse}
        direction="response"
        tab={tab}
        t={t}
      />
    </>
  );
}

function PayloadDirection({
  label,
  base64,
  normalized,
  direction,
  tab,
  t,
}: {
  label: string;
  base64?: string;
  normalized?: import('@/components/normalized/types').NormalizedPayload;
  direction: 'request' | 'response';
  tab: 'normalized' | 'raw';
  t: ReturnType<typeof useTranslation>['t'];
}) {
  return (
    <div style={{ marginBottom: 'var(--g-space-4)' }}>
      <div style={{ fontSize: 'var(--g-font-size-sm)', color: 'var(--color-text-muted)', marginBottom: 'var(--g-space-1)' }}>
        {label}
      </div>
      {tab === 'normalized' && normalized ? (
        <NormalizedPayloadView payload={normalized} direction={direction} />
      ) : (
        <PayloadView base64={base64} t={t} />
      )}
    </div>
  );
}

/**
 * PayloadView renders a single captured body. Wire shape is base64
 * (Go json.Marshal of []byte). We try utf-8 decode for human-readable
 * text; if that yields replacement chars we fall back to a hex byte
 * preview. Empty / missing payload renders an explanatory placeholder
 * (capture disabled / spill / passthrough).
 */
function PayloadView({
  base64,
  t,
}: {
  base64?: string;
  t: ReturnType<typeof useTranslation>['t'];
}) {
  // 2026-05-24: removed internal label render — caller (PayloadDirection)
  // owns the label so we don't show "Response body / Response body" twice
  // when PayloadDirection has its own header. PayloadView is now pure body.
  if (!base64) {
    return (
      <span style={{ color: 'var(--color-text-muted)', fontStyle: 'italic' }}>
        {t('traffic.detail.payloadEmpty', 'Not captured (payload capture disabled or body spilled to remote store).')}
      </span>
    );
  }
  try {
    const bytes = Uint8Array.from(atob(base64), (c) => c.charCodeAt(0));
    const decoded = new TextDecoder('utf-8', { fatal: false }).decode(bytes);
    return (
      <pre
        style={{
          background: 'var(--color-bg-subtle)',
          padding: 'var(--g-space-3)',
          borderRadius: 'var(--g-radius-sm)',
          fontSize: 'var(--g-font-size-xs)',
          fontFamily: 'var(--g-font-mono)',
          maxHeight: 360,
          overflow: 'auto',
          whiteSpace: 'pre-wrap',
          wordBreak: 'break-all',
          margin: 'var(--g-space-0)',
        }}
      >
        {decoded}
      </pre>
    );
  } catch {
    return (
      <span style={{ color: 'var(--color-status-warning)' }}>
        {t('traffic.detail.payloadDecodeError', 'Failed to decode base64 payload.')}
      </span>
    );
  }
}
