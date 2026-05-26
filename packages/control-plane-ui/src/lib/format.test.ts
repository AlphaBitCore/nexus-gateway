import { describe, it, expect } from 'vitest';
import { formatUsd, formatUsdSci } from './format';

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
