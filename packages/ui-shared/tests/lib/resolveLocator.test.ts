import { describe, expect, it } from 'vitest';

import { canResolveLocator, resolveLocator } from '../../src/lib/resolveLocator';

const enc = (s: string) => new TextEncoder().encode(s);

// A base64 payload may arrive RFC-2045 line-wrapped. The decoder used to
// compute its padding from the wrapped length, which is wrong the moment
// atob strips the newlines itself — whether that still decoded came down to
// engine-specific tolerance rather than anything guaranteed. Since a
// MediaRef pointing at such a payload is reported captured and whole, a
// decode failure here is a Download button that fails on click.
describe('resolveLocator base64 handling', () => {
  const payload = 'QUJD'.repeat(5000); // 20000 chars → 15000 bytes
  const wrapped = (payload.match(/.{1,76}/g) ?? []).join('\r\n');

  it('decodes a line-wrapped data URI to the same bytes as the unwrapped one', () => {
    const body = JSON.stringify({ output: `data:image/png;base64,${wrapped}` });
    const got = resolveLocator(enc(body), 'datauri:output');
    expect(got.bytes.byteLength).toBe(15000);
    expect(got.mime).toBe('image/png');

    const flat = resolveLocator(
      enc(JSON.stringify({ output: `data:image/png;base64,${payload}` })),
      'datauri:output',
    );
    expect(Array.from(got.bytes.slice(0, 32))).toEqual(Array.from(flat.bytes.slice(0, 32)));
    expect(got.bytes.byteLength).toBe(flat.bytes.byteLength);
  });

  // decodeBase64 advertises three tolerances. Line wrapping was pinned by
  // the test above; these are the other two. The wrapping fixture is
  // 20 000 chars — already a multiple of 4 — so it can never exercise the
  // padding branch, and deleting the padding computation or the url-safe
  // mapping went undetected against the whole suite.
  it('decodes an unpadded payload', () => {
    // 5 bytes → 8 base64 chars with no padding needed, 4 bytes → one '='.
    const bytes = Uint8Array.from([1, 2, 3, 4]);
    const std = btoa(String.fromCharCode(...bytes)); // ends with '='
    expect(std.endsWith('=')).toBe(true);
    const unpadded = std.replace(/=+$/, '');

    const body = JSON.stringify({ output: `data:application/octet-stream;base64,${unpadded}` });
    const got = resolveLocator(enc(body), 'datauri:output');
    expect(Array.from(got.bytes)).toEqual(Array.from(bytes));
  });

  it('decodes the url-safe alphabet', () => {
    // 0xfb 0xff 0xbf encodes to '+/+/' in the standard alphabet.
    const bytes = Uint8Array.from([0xfb, 0xff, 0xbf]);
    const std = btoa(String.fromCharCode(...bytes));
    expect(std).toMatch(/[+/]/);
    const urlSafe = std.replace(/\+/g, '-').replace(/\//g, '_').replace(/=+$/, '');

    const body = JSON.stringify({ output: `data:application/octet-stream;base64,${urlSafe}` });
    const got = resolveLocator(enc(body), 'datauri:output');
    expect(Array.from(got.bytes)).toEqual(Array.from(bytes));
  });

  // atob removes ASCII whitespace only. Each of these makes it throw, so
  // each one is a Download button that fails on click if the strip goes.
  it.each([
    ['VT', '\u000b'],
    ['NBSP', '\u00a0'],
    ['line separator', '\u2028'],
    ['ideographic space', '\u3000'],
    ['BOM', '\ufeff'],
  ])('decodes a payload containing %s', (_name, ch) => {
    const body = JSON.stringify({ output: `data:image/png;base64,QUJD${ch}QUJD` });
    const got = resolveLocator(enc(body), 'datauri:output');
    expect(got.bytes.byteLength).toBe(6);
  });

  // Go trims before deciding a value is a whole-value data URI, so a stored
  // value with leading whitespace still carries a captured ref and a
  // locator. If this side does not trim, the card offers a Download that
  // throws on click.
  it('resolves a data URI that the stored value pads with whitespace', () => {
    const body = JSON.stringify({ output: '  data:image/png;base64,QUJDQUJD\n' });
    const got = resolveLocator(enc(body), 'datauri:output');
    expect(got.bytes.byteLength).toBe(6);
    expect(got.mime).toBe('image/png');
  });

  // A `datauri:` locator asserts the value IS a data URI. Without the
  // scheme check any comma-and-"base64" string decodes, so a locator whose
  // path drifted onto the wrong field would serve bytes from it instead of
  // reporting that it resolved to the wrong thing.
  it('refuses a value that is not a data URI even if it looks decodable', () => {
    const body = JSON.stringify({ output: 'blob:image/png;base64,QUJDQUJD' });
    expect(() => resolveLocator(enc(body), 'datauri:output')).toThrow(/not a data URI/);
  });

  // Go trims unicode.White_Space, which includes U+0085; ECMAScript's \s
  // does not. That one character was the whole remaining disagreement
  // between the two sides, and it produced a captured ref this resolver
  // refused — a Download that throws on click.
  it.each([
    ['leading NEL', '\u0085data:image/png;base64,QUJDQUJD'],
    ['trailing NEL', 'data:image/png;base64,QUJDQUJD\u0085'],
    ['leading ideographic space', '\u3000data:image/png;base64,QUJDQUJD'],
  ])('resolves a value padded with %s', (_name, value) => {
    const got = resolveLocator(enc(JSON.stringify({ output: value })), 'datauri:output');
    expect(got.bytes.byteLength).toBe(6);
  });

  // Go trims unicode.White_Space AND the Unicode format category before
  // deciding a value is a whole-value data URI, and folds the scheme case.
  // Every one of these therefore arrives with a captured ref and a locator
  // pointing here; a narrower test on this side is a Download that throws.
  it.each([
    ['BOM', '\ufeffdata:image/png;base64,QUJDQUJD'],
    ['ZWSP', '\u200bdata:image/png;base64,QUJDQUJD'],
    ['LRM', '\u200edata:image/png;base64,QUJDQUJD'],
    ['word joiner', '\u2060data:image/png;base64,QUJDQUJD'],
    ['uppercase scheme', 'DATA:image/png;base64,QUJDQUJD'],
    ['mixed-case scheme', 'Data:image/png;base64,QUJDQUJD'],
  ])('resolves a value spelled with %s', (_name, value) => {
    const got = resolveLocator(enc(JSON.stringify({ output: value })), 'datauri:output');
    expect(got.bytes.byteLength).toBe(6);
    expect(got.mime).toBe('image/png');
  });

  // Go strips `unicode.IsSpace | unicode.Cf` — 195 code points — at BOTH
  // ends. A hand-written class here covered 32; each of the 163 it missed
  // yielded a captured ref with a locator that this resolver then refused,
  // which is a Download that throws on click. These are the ones most
  // likely to ride along in copy-pasted text.
  const IGNORABLE: Array<[string, string]> = [
    ['soft hyphen U+00AD', '\u00ad'],
    ['arabic letter mark U+061C', '\u061c'],
    ['mongolian vowel separator U+180E', '\u180e'],
    ['left-to-right embedding U+202A', '\u202a'],
    ['right-to-left override U+202E', '\u202e'],
    ['right-to-left isolate U+2067', '\u2067'],
    ['nominal digit shapes U+206F', '\u206f'],
    ['interlinear annotation anchor U+FFF9', '\ufff9'],
  ];

  it.each(IGNORABLE)('resolves a value led by %s', (_name, ch) => {
    const got = resolveLocator(enc(JSON.stringify({ output: `${ch}data:image/png;base64,QUJDQUJD` })), 'datauri:output');
    expect(got.bytes.byteLength).toBe(6);
    expect(got.mime).toBe('image/png');
  });

  // Trailing too: Go's TrimFunc trims both ends, and the earlier
  // leading-only form was justified by a claim that held for 32 characters
  // and not for the other 163.
  it.each(IGNORABLE)('resolves a value trailed by %s', (_name, ch) => {
    const got = resolveLocator(enc(JSON.stringify({ output: `data:image/png;base64,QUJDQUJD${ch}` })), 'datauri:output');
    expect(got.bytes.byteLength).toBe(6);
  });

  // INSIDE the payload, where splitDataURI's leading strip cannot reach it.
  // Every case above puts the character at an end, so none of them exercise
  // decodeBase64's own strip — deleting it left them all green. Go's
  // validBase64 rejects such a payload, so the ref arrives truncated and no
  // control is offered; this keeps the resolver from being the stricter of
  // the two if that ever changes.
  it.each(IGNORABLE)('decodes a payload containing %s', (_name, ch) => {
    const got = resolveLocator(
      enc(JSON.stringify({ output: `data:image/png;base64,QUJD${ch}QUJD` })),
      'datauri:output',
    );
    expect(got.bytes.byteLength).toBe(6);
  });

  it('still rejects a payload that is not recoverable base64', () => {
    const body = JSON.stringify({ output: 'data:image/png;base64,!!!!not base64!!!!' });
    expect(() => resolveLocator(enc(body), 'datauri:output')).toThrow();
  });

  it('reports multipart as unresolvable client-side rather than failing at click time', () => {
    expect(canResolveLocator('multipart:file')).toBe(false);
    expect(canResolveLocator('datauri:output')).toBe(true);
  });
});
