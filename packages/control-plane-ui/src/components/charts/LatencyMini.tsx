/**
 * LatencyMini — compact phase-breakdown latency renderer.
 *
 * Used wherever a list cell, KPI card, or sparkline-level surface
 * needs to convey both the total latency AND the proportional split
 * between Us / Upstream / hooks without the full
 * `<LatencyWaterfall>` legend treatment. The three `size` variants
 * trade space for detail:
 *
 *   card   — KPI tile: big total + mini bar + subtitle phrase
 *            ("Us 3 · TTFB 1.2s · Upstream 1.6s"). Used by Provider
 *            Usage Avg Latency, Dashboard P95 card, Provider Health.
 *   row    — 36px table-cell form: total on one line, 3px bar below,
 *            no inline breakdown (hover tooltip carries it). Used in
 *            Traffic List, VK Access Log.
 *   inline — single 12px line: total + 24px-wide bar + hover. The
 *            smallest possible signal in cells that already have other
 *            metric chips fighting for width.
 *
 * Inputs are intentionally the same shape as the underlying DB
 * columns (latencyMs / upstreamTtfbMs / upstreamTotalMs /
 * requestHooksMs / responseHooksMs) so the component slots into
 * per-event AND per-provider aggregate sites with no field-name
 * translation: callers pass `avgUpstreamTotalMs` as
 * `upstreamTotalMs` and the renderer treats them identically.
 *
 * When `latencyMs` is null the component renders an em-dash placeholder
 * rather than collapsing to nothing — keeps list-row heights stable.
 */
import { useMemo, useState, useRef, useCallback, useLayoutEffect, type ReactElement } from 'react';
import { createPortal } from 'react-dom';
import { useTheme } from '@/theme/useTheme';
import { getPhaseColors } from '@nexus-gateway/ui-shared';

interface LatencyMiniProps {
  latencyMs?: number | null;
  upstreamTtfbMs?: number | null;
  upstreamTotalMs?: number | null;
  requestHooksMs?: number | null;
  responseHooksMs?: number | null;
  /** Layout variant — see component header for sizing notes. */
  size?: 'card' | 'row' | 'inline';
  /** Card-only title (e.g. 'AVG LATENCY' or 'P95 LATENCY'). Ignored when size != 'card'. */
  title?: string;
  /** Optional className for outer wrapper (lets callers reset padding from inside a Card / table cell). */
  className?: string;
}

// Phase colour grammar — sourced from chartColors.ts (the single sanctioned
// hex-literal source). Reads through getPhaseColors(resolvedMode) so the
// palette flips with the active theme/mode.

/** Short human description per phase — surfaced in the hover popup so
 *  operators see WHAT each segment is, not just the number. Technical
 *  jargon ("TTFB", "hooks") stays in English per the project i18n
 *  policy; the descriptions read at the same English-jargon register. */
const PHASE_DESCRIPTIONS = {
  reqHooks: 'Request-side compliance / rule-pack stages, summed.',
  our: 'Nexus internal: auth, routing, caching, transform — the only column you can directly optimise.',
  upstreamTtfb: 'Time waiting for the first byte to come back from the upstream provider.',
  upstreamBody: 'Time receiving the streaming body bytes (TTFB → close).',
  respHooks: 'Response-side compliance / signal-extraction stages, summed.',
} as const;

interface ResolvedSegments {
  reqHooks: number;
  our: number;
  upTtfb: number;
  upBody: number;
  respHooks: number;
  total: number;
  /** True when at least one upstream / hook field is non-null — drives
   *  whether to show a multi-segment bar vs a single bar. */
  hasBreakdown: boolean;
}

function resolveSegments(p: LatencyMiniProps): ResolvedSegments {
  const total = Math.max(0, p.latencyMs ?? 0);
  const reqHooks = Math.max(0, p.requestHooksMs ?? 0);
  const respHooks = Math.max(0, p.responseHooksMs ?? 0);
  const upTtfb = Math.max(0, p.upstreamTtfbMs ?? 0);
  // Fallback chain: prefer upstream_total_ms (full upstream wall time).
  // When the backend column is null but ttfb is present, fall back to
  // ttfb as a lower-bound estimate of upstream — otherwise the formula
  // `our = total - upTotal` mis-attributes the entire upstream wait to
  // "Our Overhead". This fallback fires on pre-fix streaming-MISS
  // rows (the historical bug where audit defer fired before broker
  // pump closed the body) and on backends that legitimately only
  // measure TTFB. Both still leave the upstream-body segment at 0
  // because we don't have a separate signal for it.
  const upTotalRaw = p.upstreamTotalMs ?? p.upstreamTtfbMs ?? 0;
  const upTotal = Math.max(0, upTotalRaw);
  const upBody = Math.max(0, upTotal - upTtfb);
  const our = Math.max(0, total - upTotal - reqHooks - respHooks);

  const hasBreakdown =
    p.upstreamTtfbMs != null ||
    p.upstreamTotalMs != null ||
    p.requestHooksMs != null ||
    p.responseHooksMs != null;

  return { reqHooks, our, upTtfb, upBody, respHooks, total, hasBreakdown };
}

function fmtMs(ms: number): string {
  if (ms < 1000) return `${Math.round(ms)}ms`;
  return `${(ms / 1000).toFixed(2)}s`;
}

interface SegmentBarProps {
  segs: ResolvedSegments;
  height: number;
  radius: number;
  /** When true and there's no breakdown, render a single-colour grey bar
   *  the full width so the row stays visually filled. */
  fillWhenFlat?: boolean;
}

function SegmentBar({ segs, height, radius, fillWhenFlat = true }: SegmentBarProps): ReactElement {
  const { resolvedMode } = useTheme();
  const COLORS = getPhaseColors(resolvedMode);
  const sum = segs.reqHooks + segs.our + segs.upTtfb + segs.upBody + segs.respHooks;
  // sum is 0 when latencyMs is 0 (rare) — show a faint background bar so
  // the row layout doesn't collapse.
  if (sum <= 0) {
    return (
      <div style={{
        height,
        width: '100%',
        borderRadius: radius,
        background: 'var(--color-surface-2)',
        opacity: 0.4,
      }} />
    );
  }
  // When no breakdown signal is present, render one continuous bar in the
  // "our" colour. The visual is still meaningful (relative width across
  // rows is comparable) but doesn't pretend to know phase splits.
  if (!segs.hasBreakdown && fillWhenFlat) {
    return (
      <div style={{
        height,
        width: '100%',
        borderRadius: radius,
        background: COLORS.our,
      }} />
    );
  }
  const slots: Array<{ key: keyof typeof COLORS; ms: number }> = [
    { key: 'reqHooks', ms: segs.reqHooks },
    { key: 'our', ms: segs.our },
    { key: 'ttfb', ms: segs.upTtfb },
    { key: 'body', ms: segs.upBody },
    { key: 'respHooks', ms: segs.respHooks },
  ];
  return (
    <div style={{
      display: 'flex',
      height,
      width: '100%',
      borderRadius: radius,
      overflow: 'hidden',
      background: 'var(--color-surface-2)',
    }}>
      {slots.map((s) => {
        const pct = (s.ms / sum) * 100;
        if (pct <= 0) return null;
        return (
          <div
            key={s.key}
            style={{
              width: `${pct}%`,
              background: COLORS[s.key],
              transition: 'width var(--g-transition-normal)',
            }}
          />
        );
      })}
    </div>
  );
}

/** One-line phase summary like "Us 3 · TTFB 1.2s · Upstream 1.6s".
 *  Labels are hardcoded technical abbreviations per the project's i18n
 *  policy ("Us / TTFB / Upstream" are jargon — same as API, VK, mTLS —
 *  and stay English across locales). Skips fields that are null on the
 *  input so an aggregate carrying only us + upstream_total renders
 *  "Us 3 · Upstream 1.6s". */
function summaryLine(p: LatencyMiniProps, segs: ResolvedSegments): string {
  const parts: string[] = [];
  // Always show Us — even when it's 0, the "Us 0" hint reassures the
  // reader that the gateway is not the slow link.
  parts.push(`Us ${fmtMs(segs.our)}`);
  if (p.upstreamTtfbMs != null) {
    parts.push(`TTFB ${fmtMs(segs.upTtfb)}`);
  }
  if (p.upstreamTotalMs != null) {
    parts.push(`Upstream ${fmtMs(segs.upTtfb + segs.upBody)}`);
  }
  return parts.join(' · ');
}

interface PopupPosition {
  top: number;
  left: number;
}

/** Hover popup body — renders the same phase rows as LatencyWaterfall's
 *  legend, plus a one-line description per row. Rendered through a
 *  React portal so it escapes any `overflow: hidden` / `overflow: auto`
 *  parent (DataTable rows, Card containers, etc. used to clip the
 *  previous absolute-positioned version when the trigger was near the
 *  table top edge). Position is computed from the trigger's
 *  getBoundingClientRect with viewport-edge clamping + a flip to below
 *  when there's not enough space above. */
function HoverPopup({ segs, p, position }: { segs: ResolvedSegments; p: LatencyMiniProps; position: PopupPosition }): ReactElement | null {
  // Popup uses semantic tokens so light/dark + any skin flip the surface and
  // text colours automatically. Chart phase colours come from
  // theme/chartColors.ts so the swatch matches the bar.
  const { resolvedMode } = useTheme();
  const phase = getPhaseColors(resolvedMode);

  const rows: Array<{ key: keyof typeof phase; ms: number; label: string; desc: string; present: boolean }> = [
    { key: 'reqHooks', ms: segs.reqHooks, label: 'Request Hooks', desc: PHASE_DESCRIPTIONS.reqHooks, present: p.requestHooksMs != null },
    { key: 'our', ms: segs.our, label: 'Our Overhead', desc: PHASE_DESCRIPTIONS.our, present: true },
    { key: 'ttfb', ms: segs.upTtfb, label: 'Upstream TTFB', desc: PHASE_DESCRIPTIONS.upstreamTtfb, present: p.upstreamTtfbMs != null },
    { key: 'body', ms: segs.upBody, label: 'Upstream Body', desc: PHASE_DESCRIPTIONS.upstreamBody, present: p.upstreamTotalMs != null },
    { key: 'respHooks', ms: segs.respHooks, label: 'Response Hooks', desc: PHASE_DESCRIPTIONS.respHooks, present: p.responseHooksMs != null },
  ];
  // Total summary at the top of the popup so the user can read it as
  // "total breakdown" rather than 5 disconnected numbers.
  return createPortal(
    <div
      role="tooltip"
      style={{
        position: 'fixed',
        top: position.top,
        left: position.left,
        minWidth: 320,
        maxWidth: 400,
        zIndex: 'var(--g-z-tooltip)',
        background: 'var(--color-surface-raised)',
        color: 'var(--color-text)',
        border: '1px solid var(--color-border-strong)',
        borderRadius: 'var(--g-radius-md)',
        padding: 'var(--g-space-2-5) var(--g-space-3)',
        boxShadow: 'var(--shadow-xl)',
        fontSize: 'var(--g-font-size-xs)',
        lineHeight: 1.45,
        whiteSpace: 'normal',
        pointerEvents: 'none',
      }}
    >
      <div style={{
        display: 'flex',
        justifyContent: 'space-between',
        marginBottom: 'var(--g-space-2)',
        paddingBottom: 'var(--g-space-1-5)',
        borderBottom: '1px solid var(--color-border-light)',
      }}>
        <span style={{ color: 'var(--color-text-muted)' }}>Total latency</span>
        <span style={{ fontWeight: 'var(--g-font-weight-semibold)', fontVariantNumeric: 'tabular-nums' }}>{fmtMs(segs.total)}</span>
      </div>
      {rows.map(r => (
        <div key={r.key} style={{ marginBottom: 'var(--g-space-1-5)', opacity: r.present ? 1 : 0.55 }}>
          <div style={{ display: 'flex', justifyContent: 'space-between', alignItems: 'center', gap: 'var(--g-space-3)' }}>
            <span style={{ display: 'inline-flex', alignItems: 'center', gap: 'var(--g-space-1-5)' }}>
              <span style={{ width: 8, height: 8, background: phase[r.key], borderRadius: 'var(--g-radius-sm)', flexShrink: 0 }} />
              <span>{r.label}</span>
              {!r.present && (
                <span style={{
                  fontSize: 'var(--g-font-size-xs)',
                  padding: 'var(--g-space-0) var(--g-space-1-5)',
                  borderRadius: 'var(--g-radius-md)',
                  border: '1px solid var(--color-border-light)',
                  color: 'var(--color-text-muted)',
                }}>
                  no data
                </span>
              )}
            </span>
            <span style={{ fontVariantNumeric: 'tabular-nums' }}>{r.present ? fmtMs(r.ms) : '—'}</span>
          </div>
          <div style={{ color: 'var(--color-text-muted)', fontSize: 'var(--g-font-size-xs)', marginTop: 'var(--g-space-0-5)', marginLeft: 'var(--g-space-3)' }}>
            {r.desc}
          </div>
        </div>
      ))}
    </div>,
    document.body,
  );
}

/** Approximate popup dimensions used for placement math. Slightly over
 *  the real rendered size so flip logic kicks in early enough to avoid
 *  any one-frame clipping when the popup mounts. */
const POPUP_W = 360;
const POPUP_H = 240;
const POPUP_GAP = 6;

export function LatencyMini(props: LatencyMiniProps): ReactElement {
  const { size = 'card', title, className } = props;
  const segs = useMemo(() => resolveSegments(props), [props]);
  const [hover, setHover] = useState(false);
  const [popupPos, setPopupPos] = useState<PopupPosition | null>(null);
  const triggerRef = useRef<HTMLElement | null>(null);

  /** Compute popup placement from the trigger's screen rect. Prefers
   *  ABOVE the trigger so list rows reading top-to-bottom don't get
   *  the popup landing on the next row; flips BELOW when there isn't
   *  enough room. Horizontally aligns to the trigger's left edge but
   *  clamps to the viewport with an 8px margin so the popup never
   *  bleeds off-screen on narrow viewports. */
  const computePosition = useCallback((): PopupPosition | null => {
    const el = triggerRef.current;
    if (!el) return null;
    const rect = el.getBoundingClientRect();
    const spaceAbove = rect.top;
    const spaceBelow = window.innerHeight - rect.bottom;
    const placeAbove = spaceAbove >= POPUP_H + POPUP_GAP || spaceAbove >= spaceBelow;
    const top = placeAbove
      ? Math.max(8, rect.top - POPUP_H - POPUP_GAP)
      : Math.min(window.innerHeight - POPUP_H - 8, rect.bottom + POPUP_GAP);
    const left = Math.min(window.innerWidth - POPUP_W - 8, Math.max(8, rect.left));
    return { top, left };
  }, []);

  /** Recompute on hover-true so the popup follows the trigger if the
   *  user scrolled between hovers. useLayoutEffect avoids a paint flash
   *  by positioning before the popup renders. */
  useLayoutEffect(() => {
    if (hover) {
      setPopupPos(computePosition());
    } else {
      setPopupPos(null);
    }
  }, [hover, computePosition]);

  // Null total → em-dash. Keeps every row visually anchored even when a
  // backend returns null because the column is empty for that aggregate.
  if (props.latencyMs == null) {
    return <span style={{ color: 'var(--color-text-muted)' }}>—</span>;
  }

  // Hover popup is gated on having any breakdown data — a flat-row total
  // would just say "total Xms" five times. The bar itself stays
  // interactive (cursor: help) so users learn it's hoverable.
  const popupVisible = hover && segs.hasBreakdown && popupPos != null;

  // Callback ref — refs the trigger element so the portal's
  // `position: fixed` placement math has a stable getBoundingClientRect
  // source. Works across the div / span variants without TS gymnastics.
  const setTriggerRef = (el: HTMLElement | null) => { triggerRef.current = el; };

  if (size === 'card') {
    return (
      <div
        ref={setTriggerRef}
        className={className}
        onMouseEnter={() => setHover(true)}
        onMouseLeave={() => setHover(false)}
        style={{
          display: 'flex',
          flexDirection: 'column',
          gap: 'var(--g-space-1-5)',
          minWidth: 0,
          cursor: segs.hasBreakdown ? 'help' : 'default',
        }}
      >
        {popupVisible && popupPos && <HoverPopup segs={segs} p={props} position={popupPos} />}
        {title && (
          <div style={{
            fontSize: 'var(--g-font-size-xs)',
            letterSpacing: '0.04em',
            textTransform: 'uppercase',
            color: 'var(--color-text-muted)',
          }}>
            {title}
          </div>
        )}
        <div style={{
          fontSize: 'var(--g-font-size-2xl)',
          fontWeight: 'var(--g-font-weight-semibold)',
          fontVariantNumeric: 'tabular-nums',
          lineHeight: 1.1,
        }}>
          {fmtMs(segs.total)}
        </div>
        <SegmentBar segs={segs} height={6} radius={3} />
        {segs.hasBreakdown && (
          <div style={{
            fontSize: 'var(--g-font-size-xs)',
            color: 'var(--color-text-muted)',
            fontVariantNumeric: 'tabular-nums',
            whiteSpace: 'nowrap',
            overflow: 'hidden',
            textOverflow: 'ellipsis',
          }}>
            {summaryLine(props, segs)}
          </div>
        )}
      </div>
    );
  }

  if (size === 'row') {
    return (
      <div
        ref={setTriggerRef}
        className={className}
        onMouseEnter={() => setHover(true)}
        onMouseLeave={() => setHover(false)}
        style={{
          display: 'flex',
          flexDirection: 'column',
          gap: 'var(--g-space-1)',
          minWidth: 84,
          cursor: segs.hasBreakdown ? 'help' : 'default',
        }}
      >
        {popupVisible && popupPos && <HoverPopup segs={segs} p={props} position={popupPos} />}
        <div style={{
          fontSize: 'var(--g-font-size-xs)',
          fontWeight: 'var(--g-font-weight-medium)',
          fontVariantNumeric: 'tabular-nums',
          lineHeight: 1,
        }}>
          {fmtMs(segs.total)}
        </div>
        <SegmentBar segs={segs} height={3} radius={2} />
      </div>
    );
  }

  return (
    <span
      ref={setTriggerRef}
      className={className}
      onMouseEnter={() => setHover(true)}
      onMouseLeave={() => setHover(false)}
      style={{
        display: 'inline-flex',
        alignItems: 'center',
        gap: 'var(--g-space-1-5)',
        fontVariantNumeric: 'tabular-nums',
        cursor: segs.hasBreakdown ? 'help' : 'default',
      }}
    >
      {popupVisible && popupPos && <HoverPopup segs={segs} p={props} position={popupPos} />}
      <span>{fmtMs(segs.total)}</span>
      <span style={{ display: 'inline-block', width: 32, opacity: hover ? 1 : 0.7 }}>
        <SegmentBar segs={segs} height={4} radius={2} />
      </span>
    </span>
  );
}
