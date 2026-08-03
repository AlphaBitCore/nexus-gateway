/**
 * Pins the error-governance → Traffic cross-link contract: the link must
 * carry only nav params the Traffic page actually consumes (source, status,
 * errorCode, model), skip empties, and URL-encode values.
 */
import { describe, it, expect } from 'vitest';
import { trafficErrorGroupLink } from './TrafficErrorsTab';
import type { TrafficErrorGroup } from '../../../api/types';

function group(over: Partial<TrafficErrorGroup>): TrafficErrorGroup {
  return {
    errorCode: '',
    statusRange: '4xx',
    provider: '',
    model: '',
    sampleReason: '',
    count: 1,
    affectedEndUsers: 0,
    firstSeen: '2026-07-17T00:00:00Z',
    lastSeen: '2026-07-17T00:00:00Z',
    buckets: [],
    attribution: 'client',
    ...over,
  };
}

const FROM = '2026-07-16T14:00:00Z';
const TO = '2026-07-17T14:00:00Z';

describe('trafficErrorGroupLink', () => {
  it('carries the FULL group identity + window so drill-down counts match', () => {
    const url = trafficErrorGroupLink(
      group({ errorCode: 'context_overflow', statusRange: '4xx', provider: 'openai', model: 'gpt-5.6' }),
      FROM,
      TO,
    );
    const qs = new URLSearchParams(url.split('?')[1]);
    expect(url.startsWith('/traffic?')).toBe(true);
    // Groups aggregate every data-plane source; pinning a source tab would
    // empty the drill-down for proxy/agent classes, so no ?source= is set
    // (absent = the All tab, which the nav consumer matches).
    expect(qs.has('source')).toBe(false);
    expect(qs.get('status')).toBe('4xx');
    expect(qs.get('errorCode')).toBe('context_overflow');
    expect(qs.get('provider')).toBe('openai');
    // Exact-match param — substring ?model= would swallow prefix families
    // (gpt-5.6 matching gpt-5.6-terra) and break drill-down count parity.
    expect(qs.get('modelExact')).toBe('gpt-5.6');
    expect(qs.has('model')).toBe(false);
    // The aggregation window rides along — without it the list counts all
    // history and never matches the group's windowed count.
    expect(qs.get('from')).toBe(FROM);
    expect(qs.get('to')).toBe(TO);
  });

  it('links absent dimensions via sentinels so empty classes stay exact', () => {
    const qs = new URLSearchParams(
      trafficErrorGroupLink(group({ statusRange: '5xx' }), FROM, TO).split('?')[1],
    );
    expect(qs.get('status')).toBe('5xx');
    // Empty errorCode = producer did not classify; empty provider/model =
    // early rejection that never resolved them. Omitting any of these would
    // over-include rows carrying real values for that dimension.
    expect(qs.get('errorCode')).toBe('__unclassified__');
    expect(qs.get('provider')).toBe('__none__');
    expect(qs.get('modelExact')).toBe('__none__');
    expect(qs.get('from')).toBe(FROM);
  });
});
