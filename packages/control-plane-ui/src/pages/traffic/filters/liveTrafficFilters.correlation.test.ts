/**
 * Correlation-filter contract: endUserId / sessionId must serialize onto the
 * GET /api/admin/traffic query string exactly as the backend expects
 * (`endUserId` / `sessionId`, exact-match, omitted when blank) and must show
 * up in the active-filter description lines so the chip bar renders them.
 */
import { describe, it, expect } from 'vitest';
import {
  EMPTY_LIVE_TRAFFIC_FILTERS,
  buildTrafficAuditLogQueryParams,
  describeLiveTrafficFilters,
  countLiveTrafficFilters,
} from './liveTrafficFilters';
import { correlationPivotPatch } from './correlationPivot';

const PAGE = { limit: 20, offset: 0 };

describe('correlation filters (endUserId / sessionId)', () => {
  it('serializes endUserId and sessionId as their own query params', () => {
    const params = buildTrafficAuditLogQueryParams(
      { ...EMPTY_LIVE_TRAFFIC_FILTERS, endUserId: 'cust-42', sessionId: 'conv-7' },
      PAGE,
    );
    expect(params.get('endUserId')).toBe('cust-42');
    expect(params.get('sessionId')).toBe('conv-7');
  });

  it('omits both params when blank or whitespace-only', () => {
    const params = buildTrafficAuditLogQueryParams(
      { ...EMPTY_LIVE_TRAFFIC_FILTERS, endUserId: '   ', sessionId: '' },
      PAGE,
    );
    expect(params.has('endUserId')).toBe(false);
    expect(params.has('sessionId')).toBe(false);
  });

  it('trims surrounding whitespace before sending (pasted ids)', () => {
    const params = buildTrafficAuditLogQueryParams(
      { ...EMPTY_LIVE_TRAFFIC_FILTERS, endUserId: '  cust-42  ' },
      PAGE,
    );
    expect(params.get('endUserId')).toBe('cust-42');
  });

  it('serializes errorCode (View-in-Traffic cross-link filter) and omits it when blank', () => {
    const withCode = buildTrafficAuditLogQueryParams(
      { ...EMPTY_LIVE_TRAFFIC_FILTERS, errorCode: 'context_overflow' },
      PAGE,
    );
    expect(withCode.get('errorCode')).toBe('context_overflow');
    const without = buildTrafficAuditLogQueryParams(EMPTY_LIVE_TRAFFIC_FILTERS, PAGE);
    expect(without.has('errorCode')).toBe(false);
    // Chip line so the applied filter is visible and removable.
    expect(
      describeLiveTrafficFilters({ ...EMPTY_LIVE_TRAFFIC_FILTERS, errorCode: 'context_overflow' }),
    ).toContain('Error code: context_overflow');
  });

  it('counts and describes both filters so the chip bar shows removable chips', () => {
    const filters = { ...EMPTY_LIVE_TRAFFIC_FILTERS, endUserId: 'cust-42', sessionId: 'conv-7' };
    expect(countLiveTrafficFilters(filters)).toBe(2);
    const lines = describeLiveTrafficFilters(filters);
    expect(lines).toContain('End-user ID: cust-42');
    expect(lines).toContain('Session ID: conv-7');
  });
});

describe('correlationPivotPatch (drawer click-to-pivot)', () => {
  it('resets ALL other filters and keeps only the pivoted id', () => {
    // Pivot means "jump to this id's whole slice" — a merge would intersect
    // with whatever was applied (e.g. requestId + session pivot = still one
    // row) and silently break the conversation-timeline journey.
    const patch = correlationPivotPatch('sessionId', 'conv-7');
    expect(patch).toEqual({ ...EMPTY_LIVE_TRAFFIC_FILTERS, sessionId: 'conv-7' });
    expect(countLiveTrafficFilters(patch)).toBe(1);
  });

  it.each(['requestId', 'endUserId', 'sessionId'] as const)(
    'produces exactly one active filter for %s',
    (field) => {
      const patch = correlationPivotPatch(field, 'x-1');
      expect(patch[field]).toBe('x-1');
      expect(countLiveTrafficFilters(patch)).toBe(1);
      // A stale correlation sibling must never survive a pivot.
      const siblings = (['requestId', 'endUserId', 'sessionId'] as const).filter((f) => f !== field);
      for (const s of siblings) expect(patch[s]).toBe('');
    },
  );
});
