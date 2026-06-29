import type { AlertChannel, AlertSeverity } from '@/api/services';

/**
 * Literal prefix that Hub writes when masking a sensitive config value
 * on GET. Values starting with this string were redacted server-side; the
 * UI should treat them as "do not touch unless the user explicitly edits".
 * The middle dots are Unicode bullets (U+2022), not ASCII asterisks.
 */
export const MASK_PREFIX = 'xxxx-••••-';

export const CHANNEL_TYPES: AlertChannel['type'][] = ['webhook', 'slack', 'email', 'pagerduty'];
export const SEVERITIES: AlertSeverity[] = ['critical', 'high', 'medium', 'low', 'info'];
export const SOURCE_TYPES = ['quota', 'proxy', 'thing', 'provider', 'auth', 'system'];

export interface HeaderRow {
  key: string;
  value: string;
  /** True when the value is a masked token returned by Hub. */
  masked: boolean;
}

export function isMasked(value: unknown): boolean {
  return typeof value === 'string' && value.startsWith(MASK_PREFIX);
}

/**
 * Convert Hub's `config.headers` object into an ordered editable list. We
 * track each entry's `masked` flag so the UI can render a "Change" affordance
 * for header values Hub redacted (Authorization, Token, Secret substrings).
 */
export function headersObjectToList(obj: unknown): HeaderRow[] {
  if (!obj || typeof obj !== 'object') return [];
  return Object.entries(obj as Record<string, unknown>).map(([k, v]) => ({
    key: k,
    value: String(v ?? ''),
    masked: isMasked(v),
  }));
}

export function headersListToObject(list: HeaderRow[]): Record<string, string> {
  const out: Record<string, string> = {};
  for (const row of list) {
    const k = row.key.trim();
    if (!k) continue;
    out[k] = row.value;
  }
  return out;
}
