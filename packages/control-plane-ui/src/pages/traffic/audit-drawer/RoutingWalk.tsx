import { Stack } from '@/components/ui';
import css from './trafficAuditDrawer.module.css';

// ── Routing walk ─────────────────────────────────────────────────────────────

// The dispatch walk, one row per attempt. It is the answer to "why did this
// request end up on that model", and until it was rendered the only way to ask
// was to read the raw trace JSON or write SQL.
//
// Selection stopped being positional, so the order alone explains nothing: a
// chain that jumps over three entries to reach the fourth is either a context
// overflow reaching for the largest window, a rate limit stepping off a
// provider, or a bug — and those look identical unless each row says which.
//
// Rendered beside the raw trace rather than instead of it. The JSON still
// carries the PLAN (the targets considered, the rule that produced them); this
// carries the WALK.

interface WalkAttempt {
  seq: number;
  provider: string;
  model: string;
  dispatched: boolean;
  status?: number;
  code?: string;
  selectionReason?: string;
  errorClass?: string;
  latencyMs?: number;
  coerced: string[];
  error?: string;
}

// walkAttempts narrows the untyped routingTrace column.
//
// The column is `unknown` on the wire because rows written by three data
// planes across several schema versions land in it. Anything that does not
// look like a walk yields nothing and the section disappears, which is the
// right outcome for a row that predates the field.
export function walkAttempts(trace: unknown): WalkAttempt[] {
  if (!trace || typeof trace !== 'object') return [];
  const raw = (trace as { attempts?: unknown }).attempts;
  if (!Array.isArray(raw)) return [];

  const out: WalkAttempt[] = [];
  raw.forEach((item, i) => {
    if (!item || typeof item !== 'object') return;
    const a = item as Record<string, unknown>;
    const str = (k: string) => (typeof a[k] === 'string' ? (a[k] as string) : undefined);
    const num = (k: string) => (typeof a[k] === 'number' ? (a[k] as number) : undefined);
    out.push({
      seq: num('seq') ?? i + 1,
      provider: str('provider') ?? '',
      model: str('model') ?? '',
      dispatched: a.dispatched === true,
      status: num('status'),
      code: str('code'),
      selectionReason: str('selectionReason'),
      errorClass: str('errorClass'),
      latencyMs: num('latencyMs'),
      coerced: Array.isArray(a.coerced)
        ? (a.coerced as unknown[]).filter((c): c is string => typeof c === 'string')
        : [],
      error: str('error'),
    });
  });
  return out;
}

export function RoutingWalk({
  trace,
  tTitle,
  tDispatched,
  tSkipped,
  tCoerced,
}: {
  trace: unknown;
  tTitle: string;
  tDispatched: string;
  tSkipped: string;
  tCoerced: string;
}) {
  const attempts = walkAttempts(trace);
  if (attempts.length === 0) return null;

  return (
    <div>
      <h3 className={css.sectionTitle}>{tTitle}</h3>
      <Stack gap="xs">
        {attempts.map((a) => (
          <div key={a.seq} className={a.dispatched ? css.walkRow : css.walkRowSkipped}>
            <span className={css.walkSeq}>{a.seq}</span>
            <div className={css.walkBody}>
              <div className={css.walkTarget}>
                <span className={css.walkProvider}>{a.provider || '—'}</span>
                {a.model && <span className={css.walkModel}>{a.model}</span>}
              </div>
              <div className={css.walkMeta}>
                {/* Dispatched separates a call that reached an upstream from a
                    target that was passed over. Both belong on the record, but
                    only the first kind cost anything. */}
                <span className={a.dispatched ? css.walkTagDispatched : css.walkTagSkipped}>
                  {a.dispatched ? tDispatched : tSkipped}
                </span>
                {a.status !== undefined && <span className={css.walkTag}>{a.status}</span>}
                {/* code and errorClass are one word wherever they describe one
                    failure, so showing the class only when it differs keeps the
                    row from saying the same thing twice. */}
                {a.code && <span className={css.walkTag}>{a.code}</span>}
                {a.errorClass && a.errorClass !== a.code && (
                  <span className={css.walkTag}>{a.errorClass}</span>
                )}
                {a.selectionReason && <span className={css.walkReason}>{a.selectionReason}</span>}
                {a.latencyMs !== undefined && <span className={css.walkTag}>{a.latencyMs}ms</span>}
              </div>
              {/* What we rewrote before dispatching THIS target. Per-attempt
                  because a coercion is per-target: the same request translated
                  for two wires is rewritten differently. */}
              {a.coerced.length > 0 && (
                <div className={css.walkCoerced}>
                  {tCoerced}: {a.coerced.join(', ')}
                </div>
              )}
              {a.error && <div className={css.walkError}>{a.error}</div>}
            </div>
          </div>
        ))}
      </Stack>
    </div>
  );
}
