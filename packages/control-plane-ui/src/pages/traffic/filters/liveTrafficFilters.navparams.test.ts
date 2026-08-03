import { describe, it, expect } from 'vitest';
import { parseTrafficNavParams } from './liveTrafficNavParams';

// Unit tests for the web-assistant navigation param parser (#17 C1). These pin
// the contract the reactive TrafficTab consumer relies on: which params produce
// which filter patch, the status-vocabulary mapping (incl. error→5xx), and the
// "consume OR drop" key set the consumer strips from the URL.

const sp = (q: string) => new URLSearchParams(q);

describe('parseTrafficNavParams', () => {
  it('returns hasNav=false and an empty patch when no nav params are present', () => {
    const nav = parseTrafficNavParams(sp('source=vk&thingId=abc'));
    expect(nav.hasNav).toBe(false);
    expect(nav.eventId).toBeNull();
    expect(nav.filterPatch).toEqual({});
    expect(nav.consumedKeys).toEqual([]);
  });

  it('maps ?model into modelUsed + _modelLabel and marks it consumed', () => {
    const nav = parseTrafficNavParams(sp('model=gpt-4o'));
    expect(nav.hasNav).toBe(true);
    expect(nav.filterPatch).toEqual({ modelUsed: 'gpt-4o', _modelLabel: 'gpt-4o' });
    expect(nav.consumedKeys).toEqual(['model']);
    expect(nav.eventId).toBeNull();
  });

  it('passes 4xx / 5xx / 2xx status ranges through verbatim', () => {
    expect(parseTrafficNavParams(sp('status=4xx')).filterPatch).toEqual({ statusRange: '4xx' });
    expect(parseTrafficNavParams(sp('status=5xx')).filterPatch).toEqual({ statusRange: '5xx' });
    expect(parseTrafficNavParams(sp('status=2xx')).filterPatch).toEqual({ statusRange: '2xx' });
  });

  it('folds the kernel "error" status into the 5xx range (single-select cannot express 4xx+5xx)', () => {
    const nav = parseTrafficNavParams(sp('status=error'));
    expect(nav.filterPatch).toEqual({ statusRange: '5xx' });
    expect(nav.consumedKeys).toEqual(['status']);
  });

  it('strips an unrecognized status without applying a filter', () => {
    const nav = parseTrafficNavParams(sp('status=teapot'));
    expect(nav.hasNav).toBe(true);
    expect(nav.filterPatch).toEqual({});
    expect(nav.consumedKeys).toEqual(['status']);
  });

  it('extracts ?eventId for the drawer and marks it consumed', () => {
    const nav = parseTrafficNavParams(sp('eventId=evt-123'));
    expect(nav.eventId).toBe('evt-123');
    expect(nav.consumedKeys).toEqual(['eventId']);
    expect(nav.filterPatch).toEqual({});
  });

  it('treats an empty ?eventId= as no event but still strips it', () => {
    const nav = parseTrafficNavParams(sp('eventId='));
    expect(nav.eventId).toBeNull();
    expect(nav.hasNav).toBe(true);
    expect(nav.consumedKeys).toEqual(['eventId']);
  });

  it('treats an empty ?model= as no filter but still strips it', () => {
    const nav = parseTrafficNavParams(sp('model='));
    expect(nav.filterPatch).toEqual({});
    expect(nav.consumedKeys).toEqual(['model']);
  });

  it('combines status + model into one patch and reports both consumed keys', () => {
    const nav = parseTrafficNavParams(sp('status=4xx&model=claude-sonnet-4-6'));
    expect(nav.filterPatch).toEqual({
      statusRange: '4xx',
      modelUsed: 'claude-sonnet-4-6',
      _modelLabel: 'claude-sonnet-4-6',
    });
    expect(nav.consumedKeys).toEqual(['status', 'model']);
  });

  it('does not include unrelated params (thingId/source) in consumedKeys', () => {
    const nav = parseTrafficNavParams(sp('eventId=evt-1&thingId=node-9&source=agent'));
    expect(nav.consumedKeys).toEqual(['eventId']);
  });

  // ?errorCode is the error-governance "View in Traffic" cross-link: it must
  // land in the errorCode filter and be stripped like the other nav params.
  it('maps ?errorCode into the errorCode filter and marks it consumed', () => {
    const nav = parseTrafficNavParams(sp('errorCode=context_overflow&status=4xx'));
    expect(nav.filterPatch).toEqual({ statusRange: '4xx', errorCode: 'context_overflow' });
    expect(nav.consumedKeys).toEqual(['status', 'errorCode']);
  });

  it('treats an empty ?errorCode= as no filter but still strips it', () => {
    const nav = parseTrafficNavParams(sp('errorCode='));
    expect(nav.filterPatch).toEqual({});
    expect(nav.consumedKeys).toEqual(['errorCode']);
  });

  // provider + from/to complete the error-governance cross-link: without the
  // window the drill-down counts all history and never matches the group.
  it('maps ?provider and converts ?from/?to (RFC3339) into datetime-local start/end', () => {
    const nav = parseTrafficNavParams(
      sp('provider=openai&from=2026-07-16T14:00:00Z&to=2026-07-17T14:00:00Z'),
    );
    expect(nav.consumedKeys).toEqual(['provider', 'from', 'to']);
    expect(nav.filterPatch.provider).toBe('openai');
    // Second-precise datetime-local in the display tz: YYYY-MM-DDTHH:mm:ss.
    // The literal depends on the configured display timezone, so pin the
    // shape; the round-trip exactness is covered by utcToLocalInputSeconds
    // being the inverse of localInputToUTC (same displayTZ on both sides).
    const dtLocal = /^\d{4}-\d{2}-\d{2}T\d{2}:\d{2}:\d{2}$/;
    expect(nav.filterPatch.startTime).toMatch(dtLocal);
    expect(nav.filterPatch.endTime).toMatch(dtLocal);
  });

  it('strips unparseable ?from/?to without applying a time filter', () => {
    const nav = parseTrafficNavParams(sp('from=garbage&to='));
    expect(nav.filterPatch).toEqual({});
    expect(nav.consumedKeys).toEqual(['from', 'to']);
  });
});
