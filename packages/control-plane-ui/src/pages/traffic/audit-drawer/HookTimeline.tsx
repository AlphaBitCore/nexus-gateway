import type { BlockingRule, HookExecutionRecord } from '../../../api/types';
import { Stack } from '@/components/ui';
import { Block, DecisionBadge } from './auditDrawerPrimitives';
import css from './trafficAuditDrawer.module.css';

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
  const latencyUs = r.latencyUs ?? r.LatencyUs;
  const order = r.order ?? r.Order ?? 0;
  const error = r.error ?? r.Error ?? '';
  return { id, name, impl, decision, reason, reasonCode, latencyMs, latencyUs, order, error };
}

type HookFields = ReturnType<typeof hookFields>;
type CollapsedHook = HookFields & { count: number; latencyMsSum?: number; latencyUsSum?: number };

// formatLatency renders the most precise available value: microseconds when
// present (hooks run at microsecond scale, so the millisecond value floors a
// sub-millisecond hook to 0), otherwise the legacy millisecond value. Sub-ms
// totals show as "210 µs"; larger ones as fractional milliseconds.
function formatLatency(latencyMs?: number, latencyUs?: number): string | null {
  if (latencyUs != null && latencyUs > 0) {
    if (latencyUs < 1000) return `${latencyUs} µs`;
    return `${(latencyUs / 1000).toFixed(latencyUs < 10000 ? 2 : 1)} ms`;
  }
  if (latencyMs != null) return `${latencyMs} ms`;
  return null;
}

// collapseDuplicates folds repeated executions of the same hook (same id + impl)
// into ONE entry carrying the execution count and the summed latency. The data
// plane already emits one row per hook going forward, but historical rows
// captured before that fix can list the same hook many times — a streamed
// response was scanned at every checkpoint. Collapsing keeps the drawer readable
// while still showing how many times the hook ran (×N) and the total time; the
// latest scan's decision/reason win.
function collapseDuplicates(items: HookFields[]): CollapsedHook[] {
  const byKey = new Map<string, CollapsedHook>();
  const order: string[] = [];
  for (const it of items) {
    const key = `${it.id}|${it.impl}|${it.name}`;
    const cur = byKey.get(key);
    if (cur) {
      cur.count += 1;
      if (it.latencyMs != null) cur.latencyMsSum = (cur.latencyMsSum ?? 0) + it.latencyMs;
      if (it.latencyUs != null) cur.latencyUsSum = (cur.latencyUsSum ?? 0) + it.latencyUs;
      cur.decision = it.decision;
      cur.reason = it.reason;
      cur.reasonCode = it.reasonCode;
      if (it.error) cur.error = it.error;
    } else {
      byKey.set(key, { ...it, count: 1, latencyMsSum: it.latencyMs, latencyUsSum: it.latencyUs });
      order.push(key);
    }
  }
  return order.map((k) => byKey.get(k)!);
}

export function PipelineTimeline({
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
  const ordered = collapseDuplicates(
    [...rows].map((r) => hookFields(r)).sort((a, b) => a.order - b.order),
  );
  return (
    <div>
      <div className={css.detailLabel}>{label} ({ordered.length})</div>
      <Stack gap="xs">
        {ordered.map((r, idx) => {
          const primary = r.name || r.id || 'hook';
          const showId = !!r.name && r.id;
          const latency = formatLatency(r.latencyMsSum, r.latencyUsSum);
          return (
            <div
              key={`${r.id || r.name || idx}`}
              className={css.hookCard}
            >
              <Stack direction="horizontal" justify="between" align="center" gap="sm">
                <Stack direction="horizontal" gap="sm" align="center">
                  <strong>{primary}</strong>
                  {r.count > 1 && (
                    <span className={css.hookImplText}>×{r.count}</span>
                  )}
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
              {(r.reason || r.reasonCode || latency || r.error) && (
                <div className={css.hookDetailText}>
                  {r.reasonCode && <span style={{ marginRight: 'var(--g-space-2)' }}>[{r.reasonCode}]</span>}
                  {r.reason && <span style={{ marginRight: 'var(--g-space-2)' }}>{r.reason}</span>}
                  {latency && <span style={{ marginRight: 'var(--g-space-2)' }}>{latency}</span>}
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

export function BlockingRuleLine({ label, rule }: { label: string; rule: BlockingRule | null | undefined }) {
  if (!rule) return null;
  return (
    <Block label={label}>
      <span className={css.mono}>
        {rule.pack}@{rule.packVersion} · {rule.ruleId}
      </span>
    </Block>
  );
}
