import type { CSSProperties, ReactNode } from 'react';
import { useTranslation } from 'react-i18next';
import { Stack } from '@/components/ui';
import { formatUsdSci } from '@/lib/format';
import css from './trafficAuditDrawer.module.css';

export type TabKey = 'overview' | 'ai' | 'compliance' | 'payloads';

// ── Primitive helpers ────────────────────────────────────────────────────────

export function Block({ label, children }: { label: string; children: ReactNode }) {
  return (
    <div>
      <div className={css.detailLabel}>{label}</div>
      <div className={css.detailValue}>{children}</div>
    </div>
  );
}

export function fmtScalar(v: unknown): string {
  if (v == null || v === '') return '—';
  return String(v);
}

export function fmtCost(v: unknown): string {
  if (v == null || v === '') return '—';
  const n = typeof v === 'string' ? Number.parseFloat(v) : Number(v);
  if (Number.isNaN(n)) return String(v);
  // Audit-grade surface — use scientific-notation fallback for sub-microdollar
  // amounts so admins see "$1.5e-7" instead of the "<$0.000001" floor.
  // Other CP-UI surfaces keep formatUsd's safer floor.
  return formatUsdSci(n);
}

export function nameWithId(name?: string | null, id?: string | null): string {
  if (name && id) return `${name} (${id.slice(0, 8)}…)`;
  if (name) return name;
  if (id) return `${id.slice(0, 8)}…`;
  return '—';
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

export function DecisionBadge({ decision }: { decision?: string | null }) {
  const label = decision ?? '—';
  return <span className={`${css.decisionBadge} ${decisionToneClass(decision)}`}>{label}</span>;
}

// ── Field group ──────────────────────────────────────────────────────────────

export type FieldDef = { label: string; value: unknown; mono?: boolean; fullWidth?: boolean };

export function FieldGroup({
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

export function TokenCard({ label, value }: { label: string; value: string | null | undefined }) {
  if (!value) return null;
  return (
    <div className={css.tokenCard}>
      <div className={css.detailLabel}>{label}</div>
      <div className={css.tokenCardValue}>{value}</div>
    </div>
  );
}

// Renders a hook reason code with a translated tooltip for the closed set of
// known codes. Unknown codes fall back to plain mono text so future codes
// are not invisible.
export function ReasonCodeChip({ code }: { code: string }) {
  const { t } = useTranslation();
  const known = new Set([
    'REDACT_INFLIGHT_UNSUPPORTED',
    'REDACT_STORAGE_ONLY_BY_POLICY',
    'STORAGE_DROPPED_BY_POLICY',
    'AIGUARD_SUGGESTED_VS_POLICY',
    'GENERATIVE_PROMPT_BLOCKED',
  ]);
  const tooltip = known.has(code)
    ? (t(`pages:traffic.detail.reasonCodes.${code}`, '') as string)
    : '';
  const isKnown = known.has(code);
  return (
    <span
      className={`${css.mono} ${css.stageReasonCode}`}
      title={tooltip || undefined}
      style={isKnown ? {
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
export function subTabStyle(active: boolean): CSSProperties {
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

export function TabBar({
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
