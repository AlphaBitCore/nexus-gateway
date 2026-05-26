import { describe, expect, it } from 'vitest';

import {
  EMPTY_LIVE_TRAFFIC_FILTERS,
  buildTrafficAuditLogQueryParams,
  describeLiveTrafficFilters,
  toRFC3339WithOffset,
} from './liveTrafficFilters';

describe('toRFC3339WithOffset', () => {
  it('produces a UTC instant matching the local datetime input', () => {
    // The semantic contract: whatever the wall-clock input is in the
    // user's display TZ, the output is the corresponding absolute UTC
    // instant. We don't pin a specific offset format here because
    // RFC3339 accepts both "+08:00" and "Z" representations of the
    // same instant — the backend parses either equivalently.
    const out = toRFC3339WithOffset('2026-04-25T23:37');
    const parsed = new Date(out);
    const expected = new Date('2026-04-25T23:37:00');
    expect(parsed.getTime()).toBe(expected.getTime());
  });

  it('returns empty string for invalid datetime input', () => {
    expect(toRFC3339WithOffset('invalid')).toBe('');
  });
});

describe('buildTrafficAuditLogQueryParams', () => {
  it('serializes start/end as parseable UTC instants', () => {
    const params = buildTrafficAuditLogQueryParams(
      {
        ...EMPTY_LIVE_TRAFFIC_FILTERS,
        startTime: '2026-04-25T22:37',
        endTime: '2026-04-25T23:37',
      },
      { limit: 20, offset: 0 },
    );

    // Emits absolute UTC RFC3339. We don't require a specific
    // offset/Z form because RFC3339 lets both represent the same
    // instant; we only require the wall-clock matches the input
    // (interpreted in the runner's display TZ).
    const start = params.get('startTime');
    const end = params.get('endTime');
    expect(start).toBeTruthy();
    expect(end).toBeTruthy();
    expect(new Date(start!).getTime()).toBe(new Date('2026-04-25T22:37:00').getTime());
    expect(new Date(end!).getTime()).toBe(new Date('2026-04-25T23:37:00').getTime());
  });

  // Confirm the unified cacheStatus (HIT | MISS) flows as a
  // `cacheStatus` query param (not the legacy `cacheHit=true|false`),
  // and that empty values stay out of the URL.
  it.each(['HIT', 'MISS'] as const)(
    'emits cacheStatus=%s when set',
    (status) => {
      const params = buildTrafficAuditLogQueryParams(
        { ...EMPTY_LIVE_TRAFFIC_FILTERS, cacheStatus: status },
        { limit: 20, offset: 0 },
      );
      expect(params.get('cacheStatus')).toBe(status);
      expect(params.get('cacheHit')).toBeNull();
    },
  );

  it('omits cacheStatus when empty', () => {
    const params = buildTrafficAuditLogQueryParams(
      EMPTY_LIVE_TRAFFIC_FILTERS,
      { limit: 20, offset: 0 },
    );
    expect(params.get('cacheStatus')).toBeNull();
  });
});

describe('describeLiveTrafficFilters cacheStatus chip', () => {
  it.each([
    ['HIT', 'Cache: HIT'],
    ['MISS', 'Cache: MISS'],
  ] as const)('produces summary line for %s', (status, expected) => {
    const lines = describeLiveTrafficFilters({
      ...EMPTY_LIVE_TRAFFIC_FILTERS,
      cacheStatus: status,
    });
    expect(lines).toContain(expected);
  });

  it('omits chip when cacheStatus empty', () => {
    const lines = describeLiveTrafficFilters(EMPTY_LIVE_TRAFFIC_FILTERS);
    expect(lines.find((l) => l.startsWith('Cache:'))).toBeUndefined();
  });
});

