/**
 * Formatting utilities — consistent date, number, and currency formatting.
 *
 * Timezone policy (see docs/developers/workflow/timezone.md):
 * Every date/time formatter renders in the *display TZ*, defaulting to
 * the user's preferred IANA TZ via [getDisplayTZ] when no explicit
 * `userTZ` argument is supplied. Render output always includes a
 * timezone designator (e.g. "GMT+8") so a viewer is never left
 * guessing whose clock the value belongs to. The backend transmits
 * absolute UTC instants; this layer is the only place where local
 * conversion happens.
 */
import { format } from 'date-fns';
import { fromZonedTime, toZonedTime } from 'date-fns-tz';

/** IANA name of the browser's current timezone, e.g. "Asia/Shanghai". */
export function browserTZ(): string {
  return Intl.DateTimeFormat().resolvedOptions().timeZone;
}

/**
 * The user's preferred display TZ. Default = browser TZ. Set via
 * [setDisplayTZ] from the user-profile bootstrap. Reading
 * is a synchronous fallback so non-React utility callers don't need
 * to thread a hook through.
 */
let displayTZ: string = browserTZ();

/** Override the display TZ (typically called once after user-profile load). */
export function setDisplayTZ(tz: string | null | undefined): void {
  displayTZ = tz && tz.length > 0 ? tz : browserTZ();
}

/** Read the active display TZ. */
export function getDisplayTZ(): string {
  return displayTZ;
}

function parseInstant(value: string | Date | null | undefined): Date | null {
  if (!value) return null;
  const d = typeof value === 'string' ? new Date(value) : value;
  if (isNaN(d.getTime())) return null;
  return d;
}

/** Short date in display TZ: "Apr 26, 2026 GMT+8". */
export function formatDate(value: string | Date | null | undefined, userTZ?: string): string {
  const d = parseInstant(value);
  if (!d) return '—';
  return new Intl.DateTimeFormat('en-US', {
    year: 'numeric',
    month: 'short',
    day: 'numeric',
    timeZone: userTZ ?? displayTZ,
    timeZoneName: 'short',
  }).format(d);
}

/** Date + time in display TZ: "Apr 26, 2026, 2:30 PM GMT+8". */
export function formatDateTime(value: string | Date | null | undefined, userTZ?: string): string {
  const d = parseInstant(value);
  if (!d) return '—';
  return new Intl.DateTimeFormat('en-US', {
    year: 'numeric',
    month: 'short',
    day: 'numeric',
    hour: '2-digit',
    minute: '2-digit',
    timeZone: userTZ ?? displayTZ,
    timeZoneName: 'short',
  }).format(d);
}

/** Time only in display TZ: "2:30:45 PM GMT+8". */
export function formatTime(value: string | Date | null | undefined, userTZ?: string): string {
  const d = parseInstant(value);
  if (!d) return '—';
  return new Intl.DateTimeFormat('en-US', {
    hour: 'numeric',
    minute: '2-digit',
    second: '2-digit',
    timeZone: userTZ ?? displayTZ,
    timeZoneName: 'short',
  }).format(d);
}

/**
 * Relative time for recent events ("3m ago", "2h ago", "yesterday"),
 * falling back to absolute formatDate for anything older. Relative
 * output is TZ-free by definition.
 */
export function formatRelativeTime(value: string | Date | null | undefined): string {
  const d = parseInstant(value);
  if (!d) return '—';
  const now = Date.now();
  const diffMs = now - d.getTime();
  const diffSec = Math.floor(diffMs / 1000);
  if (diffSec < 60) return 'just now';
  if (diffSec < 3600) return `${Math.floor(diffSec / 60)}m ago`;
  if (diffSec < 86400) return `${Math.floor(diffSec / 3600)}h ago`;
  if (diffSec < 172800) return 'yesterday';
  return formatDate(d);
}

/**
 * Convert a `<input type="datetime-local">` string ("YYYY-MM-DDTHH:mm")
 * — interpreted in `userTZ` — into a UTC RFC3339 string suitable for
 * sending to the backend. Defaults to the active display TZ.
 */
export function localInputToUTC(localStr: string, userTZ?: string): string {
  const tz = userTZ ?? displayTZ;
  return fromZonedTime(localStr, tz).toISOString();
}

/**
 * Convert a `<input type="date">` string ("YYYY-MM-DD") into the UTC
 * RFC3339 instant 1ms before the next calendar day in `userTZ` —
 * semantically "valid through this calendar day".
 */
export function endOfDayUTC(dateStr: string, userTZ?: string): string {
  const tz = userTZ ?? displayTZ;
  return fromZonedTime(`${dateStr}T23:59:59.999`, tz).toISOString();
}

/**
 * Render an absolute UTC instant as "YYYY-MM-DDTHH:mm" suitable for
 * a `<input type="datetime-local">`'s `value` attribute. Inverse of
 * [localInputToUTC].
 */
export function utcToLocalInput(isoString: string, userTZ?: string): string {
  const tz = userTZ ?? displayTZ;
  const local = toZonedTime(new Date(isoString), tz);
  return format(local, "yyyy-MM-dd'T'HH:mm");
}

/** Compact number: "12.3K", "1.5M" */
export function formatCompact(n: number, locale?: string): string {
  return new Intl.NumberFormat(locale ?? 'en-US', {
    notation: 'compact',
    maximumFractionDigits: 1,
  }).format(n);
}

/**
 * Format a token count with automatic K/M/B/T scaling.
 * Trailing zeros are trimmed so round numbers stay clean ("128K" not "128.0K").
 *   ≥ 1 000 000 000 000 → 2-decimal T  ("1.31T")
 *   ≥ 1 000 000 000     → 2-decimal B  ("130.66B")  ← aggregate dashboard totals
 *   ≥ 1 000 000         → 2-decimal M  ("1.93M", "2M")
 *   ≥ 1 000             → 1-decimal K  ("45.6K", "128K")
 *   < 1 000             → plain number ("842")
 *   null/undefined      → "—"
 *
 * Without the B/T tiers a large aggregate (e.g. 130.66 billion tokens) rendered
 * as "130656.64M" — neither separated nor scaled.
 */
export function formatTokens(v: number | null | undefined): string {
  if (v == null) return '—';
  if (v >= 1_000_000_000_000) return `${parseFloat((v / 1_000_000_000_000).toFixed(2))}T`;
  if (v >= 1_000_000_000) return `${parseFloat((v / 1_000_000_000).toFixed(2))}B`;
  if (v >= 1_000_000) return `${parseFloat((v / 1_000_000).toFixed(2))}M`;
  if (v >= 1_000) return `${parseFloat((v / 1_000).toFixed(1))}K`;
  return String(v);
}

/**
 * USD currency with adaptive precision so per-request token costs (often
 * sub-cent — e.g. a 16-token Moonshot call is ~$0.000002) stay visible
 * instead of rounding to $0.0000:
 *
 *   n === 0           → "$0"
 *   |n| < $0.000001   → "<$0.000001" / ">-$0.000001"   (positive / negative)
 *   |n| < $0.01       → up to 6 decimals  ("$0.000006")
 *   |n| >= $0.01      → up to 4 decimals  ("$0.0123" / "$45.6789")
 *
 * The 6-decimal cap matches Model.input_price_per_million's billing
 * resolution at the Model row level (1 token of a $1/million model
 * costs $0.000001). Anything below that is reported as "<$0.000001"
 * rather than rounded to zero — so analytics cells never silently
 * imply the request was free.
 */
export function formatUsd(n: number): string {
  if (n === 0) return '$0';
  const abs = Math.abs(n);
  if (abs < 0.000001) {
    return n > 0 ? '<$0.000001' : '>-$0.000001';
  }
  const max = abs < 0.01 ? 6 : 4;
  return new Intl.NumberFormat('en-US', {
    style: 'currency',
    currency: 'USD',
    maximumFractionDigits: max,
  }).format(n);
}

/**
 * formatUsdSci is a higher-precision variant of formatUsd used ONLY on
 * audit-grade surfaces (traffic event drawer cost breakdown + traffic
 * list cost column) where admins need exact micro-dollar math, not the
 * "<$0.000001" floor.
 *
 * Behavior:
 *   - $0 → "$0"
 *   - |n| ≥ 0.000001 → same Intl.NumberFormat as formatUsd
 *   - |n| < 0.000001 → scientific notation, e.g. "$3.0e-7" or "-$1.5e-8"
 *     so admins see the exact magnitude instead of a "<$0.000001" floor.
 *
 * All other surfaces (dashboards, rollups, billing) keep using formatUsd
 * to avoid scaring users with scientific notation on aggregated totals.
 */
export function formatUsdSci(n: number): string {
  if (n === 0) return '$0';
  const abs = Math.abs(n);
  if (abs < 0.000001) {
    // toExponential(1) gives e.g. "3.0e-7"; we strip the redundant '+'
    // that Intl can leave on small positives.
    const sign = n < 0 ? '-' : '';
    return `${sign}$${abs.toExponential(1).replace('+', '')}`;
  }
  const max = abs < 0.01 ? 6 : 4;
  return new Intl.NumberFormat('en-US', {
    style: 'currency',
    currency: 'USD',
    maximumFractionDigits: max,
  }).format(n);
}
