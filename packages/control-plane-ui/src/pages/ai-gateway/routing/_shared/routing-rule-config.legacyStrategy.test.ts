import { describe, expect, it } from 'vitest';
import { displayStrategy, mapLegacyStrategy } from './routing-rule-config';

describe('mapLegacyStrategy', () => {
  // A strategy the gateway no longer dispatches has to be distinguishable from
  // one it does. This used to fall back to 'single', so opening a stored
  // `policy` rule rendered the picker as "Single" and saving ANY field — the
  // name, the priority — persisted a single-shaped rule over the admin's
  // configuration. The detail page meanwhile printed the stored value, so the
  // two pages disagreed about what the rule was.
  it('says it does not recognise a strategy the gateway dropped', () => {
    expect(mapLegacyStrategy('policy')).toBeNull();
    expect(mapLegacyStrategy('made-up')).toBeNull();
    expect(mapLegacyStrategy('')).toBeNull();
  });

  // Renaming an old vocabulary onto the current one is the function's actual
  // job and must keep working: these are stored values from before the
  // strategy names settled, and they DO describe something dispatchable.
  it('still maps the historical names onto what they became', () => {
    expect(mapLegacyStrategy('priority')).toBe('single');
    expect(mapLegacyStrategy('round-robin')).toBe('loadbalance');
    expect(mapLegacyStrategy('weighted')).toBe('loadbalance');
    expect(mapLegacyStrategy('cost')).toBe('single');
  });

  it('passes the current names through', () => {
    for (const s of ['single', 'fallback', 'loadbalance', 'conditional', 'ab_split', 'latency', 'smart']) {
      expect(mapLegacyStrategy(s)).toBe(s);
    }
  });
});

describe('displayStrategy', () => {
  // The read-only view still has to render something, so it takes a
  // placeholder. The separation is the point: a caller that WRITES must use
  // mapLegacyStrategy and refuse on null, and the two are now different
  // functions rather than one function with a silent default.
  it('falls back to a placeholder so a page can render an unknown strategy', () => {
    expect(displayStrategy('policy')).toBe('single');
    expect(displayStrategy('smart')).toBe('smart');
  });
});
