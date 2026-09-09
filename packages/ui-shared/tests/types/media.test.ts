import { describe, expect, it } from 'vitest';

import { isNeverInlineMime, mediaIsPreviewable } from '../../src/types/media';
import type { MediaRef } from '../../src/types/media';

// Both predicates decide what a captured media element does on screen, and
// neither had a test of its own — they were reached only incidentally through
// MediaCard, which meant the never-inline list could be emptied without a
// single failure. That list is the reason an SVG or an HTML fragment captured
// from a provider response is offered as a download instead of being rendered
// into the operator's own page.

describe('isNeverInlineMime', () => {
  it.each([
    ['svg', 'image/svg+xml'],
    ['html', 'text/html'],
    ['xhtml', 'application/xhtml+xml'],
    ['xml', 'text/xml'],
    ['pdf', 'application/pdf'],
  ])('refuses to inline %s, which can carry script or navigate the page', (_name, mime) => {
    expect(isNeverInlineMime(mime)).toBe(true);
  });

  it('reads the type through parameters and case, the way it arrives on the wire', () => {
    // A captured Content-Type is `image/svg+xml; charset=utf-8` as often as
    // the bare type, and providers spell it in either case. Comparing the raw
    // header would inline exactly the payload this list exists to hold back.
    expect(isNeverInlineMime('image/svg+xml; charset=utf-8')).toBe(true);
    expect(isNeverInlineMime('IMAGE/SVG+XML')).toBe(true);
    expect(isNeverInlineMime('  text/html ; boundary=x')).toBe(true);
  });

  it('does not hold back an ordinary raster type', () => {
    expect(isNeverInlineMime('image/png')).toBe(false);
    expect(isNeverInlineMime('audio/mpeg')).toBe(false);
  });

  it('treats an absent mime as not-never-inline, leaving the decision to modality', () => {
    // Absent is not the same as dangerous. Answering true here would suppress
    // the preview for every ref whose provider omitted the type, which is the
    // common case for an inline base64 image.
    expect(isNeverInlineMime(undefined)).toBe(false);
    expect(isNeverInlineMime('')).toBe(false);
  });
});

describe('mediaIsPreviewable', () => {
  const captured = (over: Partial<MediaRef>): MediaRef =>
    ({
      modality: 'image',
      mime: 'image/png',
      sizeBytes: 3,
      source: 'captured',
      // Without a locator there is nothing to resolve, so mediaHasBytes says
      // no and every assertion below would pass for the wrong reason.
      locator: 'body',
      ...over,
    }) as MediaRef;

  it.each([['image'], ['audio'], ['video']])('previews captured %s bytes', (modality) => {
    expect(mediaIsPreviewable(captured({ modality } as Partial<MediaRef>))).toBe(true);
  });

  it('does not preview a modality with no player, even when the bytes are held', () => {
    expect(mediaIsPreviewable(captured({ modality: 'document' } as Partial<MediaRef>))).toBe(false);
  });

  it('does not preview a never-inline type even when the modality has a player', () => {
    // An SVG is an image by modality and a script host by format. This is the
    // one combination where the two answers disagree, and the format wins.
    expect(mediaIsPreviewable(captured({ mime: 'image/svg+xml' }))).toBe(false);
  });

  it('does not preview a capture the gateway had to truncate', () => {
    // Truncated bytes are not the payload. Rendering them shows a broken
    // image and tells the operator the capture is intact when it is not.
    expect(mediaIsPreviewable(captured({ truncated: true }))).toBe(false);
  });

  it('does not preview a captured ref with no locator to resolve', () => {
    expect(mediaIsPreviewable(captured({ locator: undefined }))).toBe(false);
  });

  it('does not preview a ref whose bytes were never captured', () => {
    expect(mediaIsPreviewable({ modality: 'image', mime: 'image/png', source: 'external', url: 'https://example.invalid/x.png' } as MediaRef)).toBe(false);
  });
});
