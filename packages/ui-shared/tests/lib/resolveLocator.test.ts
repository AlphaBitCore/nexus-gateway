import { describe, expect, it } from 'vitest';

import { canResolveLocator, resolveLocator } from '../../src/lib/resolveLocator';

const enc = (s: string) => new TextEncoder().encode(s);

// A base64 payload may arrive RFC-2045 line-wrapped. Computing its padding
// from the wrapped length is wrong the moment atob strips the newlines
// itself — whether it still decodes comes down to engine-specific tolerance
// rather than anything guaranteed. Since a MediaRef pointing at such a
// payload is reported captured and whole, a decode failure here is a
// Download button that fails on click.
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

// The three containers the grammar promises but no test reached. Only
// `datauri:` was exercised above, so half of resolveLocator's body — the
// `body`, `json:` and `sse:` arms and every not-found path — was shipped on
// nothing but the type checker. Each of these is a Download button on the
// Agent Dashboard: the card offers the control whenever canResolveLocator
// says yes, so an arm that throws where it should resolve, or resolves where
// it should refuse, is a click that fails in front of the user.
describe('resolveLocator containers', () => {
  const b64 = (s: string) => btoa(s);

  it('serves the whole captured body without copying it', () => {
    const body = enc('raw bytes');
    const got = resolveLocator(body, 'body');
    expect(got.mime).toBe('');
    // Identity, not equality: `body` addresses the capture itself, and a
    // copy here would double the memory held by a dashboard showing several
    // large captures at once.
    expect(got.bytes).toBe(body);
  });

  it('resolves a base64 field addressed by a dotted json path', () => {
    const body = JSON.stringify({ result: { image: b64('PNGDATA') } });
    const got = resolveLocator(enc(body), 'json:result.image');
    expect(new TextDecoder().decode(got.bytes)).toBe('PNGDATA');
    // A json: locator carries no mime; the card must fall back to the ref's
    // own declared type rather than trusting an empty string.
    expect(got.mime).toBe('');
  });

  it('indexes an array segment, which is how a choices[] payload is addressed', () => {
    const body = JSON.stringify({
      choices: [{ b64_json: b64('FIRST') }, { b64_json: b64('SECOND') }],
    });
    const got = resolveLocator(enc(body), 'json:choices.1.b64_json');
    expect(new TextDecoder().decode(got.bytes)).toBe('SECOND');
  });

  it.each([
    ['a path that runs off the end of the document', 'json:result.missing'],
    ['a path that walks INTO a scalar', 'json:result.image.deeper'],
    ['a path whose array index is not a number', 'json:choices.first'],
    ['a path that dereferences a null', 'json:nothing.deeper'],
  ])('refuses %s rather than resolving to garbage', (_name, locator) => {
    const body = JSON.stringify({
      result: { image: b64('PNGDATA') },
      choices: [{ b64_json: b64('FIRST') }],
      nothing: null,
    });
    expect(() => resolveLocator(enc(body), locator)).toThrow(/resolved to nothing/);
  });

  it('resolves a field inside a numbered SSE data frame', () => {
    const stream = [
      'data: {"delta":"ignored"}',
      '',
      'data: {"image":"' + b64('FRAMEONE') + '"}',
      '',
      'data: [DONE]',
      '',
    ].join('\n');
    const got = resolveLocator(enc(stream), 'sse:1:image');
    expect(new TextDecoder().decode(got.bytes)).toBe('FRAMEONE');
  });

  it('joins a frame split across several data: lines before parsing it', () => {
    // A provider is free to wrap one JSON object over multiple data: lines.
    // Parsing each line on its own yields a syntax error on a frame that is
    // perfectly well-formed once joined.
    const stream = ['data: {"image":', 'data: "' + b64('WRAPPED') + '"}', ''].join('\n');
    const got = resolveLocator(enc(stream), 'sse:0:image');
    expect(new TextDecoder().decode(got.bytes)).toBe('WRAPPED');
  });

  it('does not count the terminator as a frame', () => {
    // [DONE] carries no payload. Counting it shifts every index after it and
    // makes the last real frame unaddressable.
    const stream = ['data: {"image":"' + b64('ONLY') + '"}', '', 'data: [DONE]', ''].join('\n');
    expect(() => resolveLocator(enc(stream), 'sse:1:image')).toThrow(/out of range/);
    expect(new TextDecoder().decode(resolveLocator(enc(stream), 'sse:0:image').bytes)).toBe('ONLY');
  });

  it.each([
    ['no frame separator at all', 'sse:image', /malformed sse locator/],
    ['a frame index past the end', 'sse:9:image', /out of range/],
    ['a negative frame index', 'sse:-1:image', /out of range/],
    ['a frame index that is not a number', 'sse:x:image', /out of range/],
    ['a path missing from the frame', 'sse:0:absent', /resolved to nothing/],
  ])('refuses an sse locator with %s', (_name, locator, want) => {
    const stream = 'data: {"image":"' + b64('ONLY') + '"}\n\n';
    expect(() => resolveLocator(enc(stream), locator)).toThrow(want);
  });

  it('refuses a datauri locator whose path resolves to something that is not a string', () => {
    // The datauri arm has its own not-found check, and it was the one arm
    // whose check no test reached — every datauri case above supplies a
    // string. A number here would otherwise reach splitDataURI as `raw` and
    // fail on `.replace`, which is a TypeError in a click handler rather than
    // the LocatorError the card knows how to report.
    const body = JSON.stringify({ output: 42, nested: { output: 'data:image/png;base64,QUJDQUJD' } });
    expect(() => resolveLocator(enc(body), 'datauri:output')).toThrow(/resolved to nothing/);
    expect(resolveLocator(enc(body), 'datauri:nested.output').mime).toBe('image/png');
  });

  it('refuses a data URI whose meta does not declare base64', () => {
    // `data:text/plain,hello` is a legal URI carrying percent-encoded text,
    // not base64. Decoding it as base64 would hand the card bytes that are
    // not the payload rather than saying it cannot serve this one.
    const body = JSON.stringify({ output: 'data:text/plain,hello' });
    expect(() => resolveLocator(enc(body), 'datauri:output')).toThrow(/not base64-encoded/);
  });

  it.each([
    ['an unknown container', 'ftp:somewhere'],
    ['a bare word', 'nonsense'],
  ])('refuses %s by name so the failure says which locator it was', (_name, locator) => {
    expect(() => resolveLocator(enc('{}'), locator)).toThrow(
      new RegExp(`unsupported locator container: ${locator}`),
    );
  });

  it('treats a missing locator as unresolvable rather than crashing the card', () => {
    // MediaRef.locator is optional on the wire. `canResolveLocator` is what
    // the card asks before rendering a Download, so undefined has to answer
    // false here rather than throw inside a render.
    expect(canResolveLocator(undefined)).toBe(false);
    expect(canResolveLocator('')).toBe(false);
    expect(canResolveLocator('body')).toBe(true);
    expect(canResolveLocator('sse:0:image')).toBe(true);
    expect(canResolveLocator('json:a.b')).toBe(true);
  });
});
