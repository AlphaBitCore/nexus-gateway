import { describe, it, expect } from 'vitest';
import { readClientTags } from './CorrelationSection';

// X-Nexus-Client-Tags was persisted and then reachable only by opening the raw
// details JSON — the place an operator goes after the drawer has already failed
// them. These pin what the Overview row shows, and what it refuses to show.
describe('readClientTags', () => {
  it('renders the pairs the caller sent', () => {
    expect(readClientTags({ clientTags: { team: 'platform', env: 'prod' } })).toBe(
      'team=platform, env=prod',
    );
  });

  it('omits the row rather than showing an empty one', () => {
    // A caller that sent no tags must not get a row with an em dash in it —
    // every untagged request would carry one.
    expect(readClientTags({ clientTags: {} })).toBeNull();
    expect(readClientTags({})).toBeNull();
    expect(readClientTags(null)).toBeNull();
    expect(readClientTags(undefined)).toBeNull();
  });

  it('survives a details blob that is not the shape it expects', () => {
    // details is typed unknown and comes from a JSON column, so anything can
    // arrive. None of these may throw inside a drawer render.
    expect(readClientTags('a string')).toBeNull();
    expect(readClientTags(42)).toBeNull();
    expect(readClientTags({ clientTags: 'not-an-object' })).toBeNull();
    expect(readClientTags({ clientTags: ['a', 'b'] })).toBeNull();
    expect(readClientTags({ clientTags: null })).toBeNull();
  });

  it('drops non-string and empty values instead of rendering them', () => {
    expect(readClientTags({ clientTags: { a: 'x', b: 1, c: '', d: null } })).toBe('a=x');
  });
});

// The three ids in this section are named, not explained, and their names alone
// do not separate them — "Event ID", "Client request ID", "Trace ID" all read
// like the same thing to someone opening the drawer for the first time. Each
// carries a tooltip saying what it IS. A missing key would render the key path
// itself, which is worse than no tooltip.
describe('the three ids each explain themselves', () => {
  it('has a hint string for every id row, in every language', async () => {
    const langs = ['en', 'es', 'zh'] as const;
    const keys = ['eventIdHint', 'clientRequestIdHint', 'traceIdHint'] as const;
    for (const lang of langs) {
      const pages = (await import(`../../../i18n/locales/${lang}/pages.json`)).default as Record<
        string,
        never
      >;
      const c = (
        pages as unknown as {
          traffic: { detail: { correlation: Record<string, string> } };
        }
      ).traffic.detail.correlation;
      for (const k of keys) {
        expect(typeof c[k], `${lang}.${k}`).toBe('string');
        expect(c[k].length, `${lang}.${k}`).toBeGreaterThan(10);
      }
      // The tag row's own label too — it is the one added alongside them.
      expect(typeof c.clientTags, `${lang}.clientTags`).toBe('string');
    }
  });
});
