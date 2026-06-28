import { describe, it, expect, afterEach } from 'vitest';
import {
  formatUsd,
  formatUsdSci,
  browserTZ,
  setDisplayTZ,
  getDisplayTZ,
  formatDate,
  formatDateTime,
  formatTime,
  formatRelativeTime,
  localInputToUTC,
  endOfDayUTC,
  utcToLocalInput,
  formatCompact,
  formatTokens,
} from '../../src/lib/format';

// Regression guard for Cost-shows-$0 in the VK traffic list. The previous
// formatUsd was Intl.NumberFormat with maximumFractionDigits: 4, which
// rounded sub-cent costs to "$0.0000" — since most per-request token
// costs in dev (a 16-token Moonshot call is ~$0.000002) sit well below
// that, every Cost cell looked like a free request.
describe('formatUsd', () => {
  it('renders zero compactly', () => {
    expect(formatUsd(0)).toBe('$0');
  });

  it('keeps sub-cent precision visible (6 decimals)', () => {
    expect(formatUsd(0.000006)).toBe('$0.000006');
    expect(formatUsd(0.000002)).toBe('$0.000002');
    expect(formatUsd(0.000945)).toBe('$0.000945');
  });

  it('uses 4 decimals when >= 1 cent', () => {
    expect(formatUsd(0.01)).toBe('$0.01');
    expect(formatUsd(0.0123)).toBe('$0.0123');
    expect(formatUsd(45.6789)).toBe('$45.6789');
    expect(formatUsd(45)).toBe('$45.00');
  });

  it('caps below microcent with a less-than indicator', () => {
    expect(formatUsd(0.0000001)).toBe('<$0.000001');
    expect(formatUsd(-0.0000001)).toBe('>-$0.000001');
  });

  it('handles negatives in normal range', () => {
    expect(formatUsd(-0.0006)).toBe('-$0.0006');
    expect(formatUsd(-12.34)).toBe('-$12.34');
  });
});

// formatUsdSci is the audit-grade variant used ONLY on the traffic event
// drawer + traffic list. For sub-microdollar amounts it falls back to
// scientific notation instead of the "<$0.000001" floor so admins see
// the exact magnitude. All other behaviour matches formatUsd.
describe('formatUsdSci', () => {
  it('matches formatUsd for the normal range', () => {
    expect(formatUsdSci(0)).toBe('$0');
    expect(formatUsdSci(0.000006)).toBe('$0.000006');
    expect(formatUsdSci(0.0123)).toBe('$0.0123');
    expect(formatUsdSci(45)).toBe('$45.00');
    expect(formatUsdSci(-0.0006)).toBe('-$0.0006');
  });

  it('uses scientific notation below microdollar (no floor)', () => {
    expect(formatUsdSci(0.0000003)).toBe('$3.0e-7');
    expect(formatUsdSci(0.00000015)).toBe('$1.5e-7');
    expect(formatUsdSci(0.0000000005)).toBe('$5.0e-10');
  });

  it('handles negative sub-microdollar', () => {
    expect(formatUsdSci(-0.0000003)).toBe('-$3.0e-7');
  });
});

describe('display TZ', () => {
  afterEach(() => setDisplayTZ(null)); // reset to browser TZ
  it('browserTZ returns a non-empty IANA name', () => {
    expect(browserTZ().length).toBeGreaterThan(0);
  });
  it('setDisplayTZ sets explicit TZ, falls back to browser on empty/undefined', () => {
    setDisplayTZ('Asia/Shanghai');
    expect(getDisplayTZ()).toBe('Asia/Shanghai');
    setDisplayTZ('');
    expect(getDisplayTZ()).toBe(browserTZ());
    setDisplayTZ(undefined);
    expect(getDisplayTZ()).toBe(browserTZ());
  });
});

describe('date formatters (explicit UTC for determinism)', () => {
  const iso = '2026-04-26T14:30:45Z';
  it('formatDate renders the day with a TZ designator', () => {
    const out = formatDate(iso, 'UTC');
    expect(out).toContain('2026');
    expect(out).toContain('Apr');
    expect(out).toContain('UTC');
  });
  it('formatDateTime includes the date + TZ', () => {
    expect(formatDateTime(iso, 'UTC')).toMatch(/2026.*UTC/);
  });
  it('formatTime renders with a TZ designator', () => {
    expect(formatTime(iso, 'UTC')).toContain('UTC');
  });
  it('accepts a Date object', () => {
    expect(formatDate(new Date(iso), 'UTC')).toContain('Apr');
  });
  it('null / invalid input → em dash', () => {
    expect(formatDate(null)).toBe('—');
    expect(formatDate('not-a-date')).toBe('—');
    expect(formatDateTime(undefined)).toBe('—');
    expect(formatTime('')).toBe('—');
    expect(formatRelativeTime(null)).toBe('—');
  });
});

describe('formatRelativeTime buckets', () => {
  const ago = (sec: number) => new Date(Date.now() - sec * 1000);
  it('< 60s → just now', () => expect(formatRelativeTime(ago(30))).toBe('just now'));
  it('minutes', () => expect(formatRelativeTime(ago(5 * 60))).toBe('5m ago'));
  it('hours', () => expect(formatRelativeTime(ago(2 * 3600))).toBe('2h ago'));
  it('yesterday', () => expect(formatRelativeTime(ago(36 * 3600))).toBe('yesterday'));
  it('older → absolute date fallback', () => {
    expect(formatRelativeTime(ago(5 * 86400))).toMatch(/\d{4}/); // contains a year
  });
});

describe('TZ <input> round-trips (UTC)', () => {
  it('localInputToUTC', () => {
    expect(localInputToUTC('2026-05-27T12:00', 'UTC')).toBe('2026-05-27T12:00:00.000Z');
  });
  it('endOfDayUTC = last millisecond of the calendar day', () => {
    expect(endOfDayUTC('2026-05-27', 'UTC')).toBe('2026-05-27T23:59:59.999Z');
  });
  it('utcToLocalInput inverts localInputToUTC', () => {
    expect(utcToLocalInput('2026-05-27T12:00:00.000Z', 'UTC')).toBe('2026-05-27T12:00');
  });
});

describe('count formatters', () => {
  it('formatCompact', () => {
    expect(formatCompact(12300)).toBe('12.3K');
    expect(formatCompact(1_500_000)).toBe('1.5M');
  });
  it('formatTokens scales + trims trailing zeros', () => {
    expect(formatTokens(null)).toBe('—');
    expect(formatTokens(undefined)).toBe('—');
    expect(formatTokens(842)).toBe('842');
    expect(formatTokens(128_000)).toBe('128K');
    expect(formatTokens(45_600)).toBe('45.6K');
    expect(formatTokens(2_000_000)).toBe('2M');
    expect(formatTokens(1_930_000)).toBe('1.93M');
    // B/T tiers: a 130.66-billion aggregate previously rendered as "130656.64M"
    // (neither scaled nor separated) — the dashboard "Total Tokens" bug.
    expect(formatTokens(5_000_000_000)).toBe('5B');
    expect(formatTokens(130_656_640_000)).toBe('130.66B');
    expect(formatTokens(1_310_000_000_000)).toBe('1.31T');
  });
});
