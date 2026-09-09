import { describe, expect, it, vi } from 'vitest';
import { fireEvent, render, screen, waitFor } from '@testing-library/react';
import { MediaCard } from '../src/components/MediaCard';
import { canResolveLocator } from '../src/lib/resolveLocator';
import type { MediaRef } from '../src/types/media';

// The invariant this card exists to hold: a control is offered only when the
// bytes behind it can be served. Testing that without a resolver proves
// nothing — with no resolver, no state renders a control, so every assertion
// passes for the wrong reason. Each case below supplies a working resolver so
// the absence of a control is caused by custody, not by the harness.

const LABELS = {
  explain: 'no bytes here',
  download: 'Download',
  previewFailed: 'preview failed',
  sizeUnknown: 'size unknown',
};

const resolver = vi.fn(async () => ({
  blob: new Blob([new Uint8Array([1, 2, 3])], { type: 'image/png' }),
  filename: 'x.png',
}));

function card(ref: MediaRef, extra: Record<string, unknown> = {}) {
  return render(<MediaCard ref_={ref} resolve={resolver} labels={LABELS} {...extra} />);
}

describe('MediaCard offers a control only for servable bytes', () => {
  it('offers a download for captured bytes with a resolvable locator', async () => {
    const { container } = card({
      modality: 'image',
      mime: 'image/png',
      sizeBytes: 3,
      source: 'captured',
      locator: 'json:messages.0.content.1.image_url.url',
    });
    await waitFor(() => expect(container.querySelector('a[download], button')).not.toBeNull());
  });

  it.each([
    ['external', { url: 'https://example.invalid/x.png' }],
    ['provider-ref', { providerRef: 'file-abc' }],
    ['fingerprint', { sha256: 'a'.repeat(64) }],
    ['aged-out', { cause: 'blob-expired' }],
    ['absent', {}],
  ] as const)('offers no control when custody is %s, and says why', (source, extra) => {
    const { container } = card({
      modality: 'image',
      mime: 'image/png',
      sizeBytes: 3,
      source,
      ...extra,
    } as MediaRef);
    expect(container.querySelector('a[download]')).toBeNull();
    expect(container.querySelector('button')).toBeNull();
    // Every no-bytes state gets a sentence rather than silence.
    expect(screen.getByText('no bytes here')).toBeInTheDocument();
  });

  it('offers no control when the bytes arrived incomplete', () => {
    const { container } = card({
      modality: 'image',
      mime: 'image/png',
      sizeBytes: 0,
      source: 'captured',
      locator: 'json:a.b',
      truncated: true,
      cause: 'undecodable-base64',
    });
    expect(container.querySelector('a[download], button')).toBeNull();
  });

  it('offers no control when the resolver cannot read that locator container', () => {
    // A multipart envelope is not parseable client-side. Without this the
    // card would render a control that throws on click.
    const { container } = card(
      {
        modality: 'audio',
        mime: 'audio/wav',
        sizeBytes: 9644,
        source: 'captured',
        locator: 'multipart:file',
      },
      { resolvable: (r: MediaRef) => canResolveLocator(r.locator) },
    );
    expect(container.querySelector('a[download], button')).toBeNull();
    expect(screen.getByText('no bytes here')).toBeInTheDocument();
  });
});

describe('MediaCard never renders active content inline', () => {
  // THE MODALITY MUST BE ONE THAT WOULD OTHERWISE PREVIEW, or the mime rule is
  // not what is being tested. Three of these carried modality 'file', which
  // mediaIsPreviewable already excludes — so deleting the never-inline list
  // entirely left those arms green and only the svg arm reddened. The list is a
  // security rule about ACTIVE CONTENT, and the only way to exercise it is to
  // give the ref a modality that reaches the rendering branch.
  it.each([
    ['image/svg+xml', 'image'],
    ['text/html', 'image'],
    ['application/pdf', 'image'],
    ['text/xml', 'video'],
  ] as const)('refuses to render %s inline even as a previewable %s', async (mime, modality) => {
    const { container } = card({
      modality,
      mime,
      sizeBytes: 100,
      source: 'captured',
      locator: 'body',
    });
    // The control is present — these are servable bytes …
    await waitFor(() => expect(container.querySelector('a[download], button')).not.toBeNull());
    // … but they are never given a rendering context, because a type that can
    // carry script must not be handed one no matter what the wire declared.
    expect(container.querySelector('img, audio, video, iframe, object, embed')).toBeNull();
  });
});

describe('MediaCard reports what it was given', () => {
  it('shows a zero-byte element as 0 B, distinct from an unknown size', () => {
    const { container } = card({
      modality: 'image',
      mime: 'image/png',
      sizeBytes: 0,
      source: 'absent',
    });
    expect(container.textContent).toContain('0 B');
    expect(container.textContent).not.toContain('size unknown');
  });
});

// "Visible and openable" is two claims, and the suite above only proved the
// first half: that a control appears. These prove the second — that what the
// control hands over is a usable file of the right type, and that an image is
// shown rather than merely offered.
describe('MediaCard hands over a usable file, not an encoded blob', () => {
  it('previews an image inline instead of only offering a download', async () => {
    const png = vi.fn(async () => ({
      blob: new Blob([new Uint8Array([0x89, 0x50, 0x4e, 0x47])], { type: 'image/png' }),
      filename: 'shot.png',
    }));
    const { container } = render(
      <MediaCard
        ref_={{ modality: 'image', mime: 'image/png', sizeBytes: 4, source: 'captured', locator: 'body' }}
        resolve={png}
        labels={LABELS}
      />,
    );
    await waitFor(() => expect(container.querySelector('img')).not.toBeNull());
    // The preview reads from an object URL, never from a data: URI — a
    // base64 string in the DOM is the shape this card exists to remove.
    const src = container.querySelector('img')!.getAttribute('src') ?? '';
    expect(src.startsWith('data:')).toBe(false);
  });

  it('gives audio a player rather than a download-only card', async () => {
    const wav = vi.fn(async () => ({
      blob: new Blob([new Uint8Array([0x52, 0x49, 0x46, 0x46])], { type: 'audio/wav' }),
      filename: 'clip.wav',
    }));
    const { container } = render(
      <MediaCard
        ref_={{ modality: 'audio', mime: 'audio/wav', sizeBytes: 4, source: 'captured', locator: 'body' }}
        resolve={wav}
        labels={LABELS}
      />,
    );
    await waitFor(() => expect(container.querySelector('audio')).not.toBeNull());
  });

  it('saves the file on the FIRST click, with a name and type from the resolved bytes', async () => {
    // A non-previewable modality does NOT prefetch: the card renders a button
    // and resolves on click, so a 20 MB PDF is not pulled for a card nobody
    // opened. That laziness is correct and is not what this asserts.
    //
    // What this asserts is the outcome a user is after, because the mechanism
    // passed while the feature did not: the first click fetched the bytes,
    // swapped the button for a link and saved NOTHING — a 200 with the file in
    // the network panel and no file on disk — and only a second click on the
    // now-link downloaded it. A test that waits for the link to APPEAR is green
    // for that behaviour, which is how it shipped.
    //
    // The resolver reports what the bytes ARE; the wire-declared mime on the
    // ref is a display hint a caller controls and must not decide the file.
    const saved: Array<{ href: string; name: string }> = [];
    const clickSpy = vi
      .spyOn(HTMLAnchorElement.prototype, 'click')
      .mockImplementation(function (this: HTMLAnchorElement) {
        saved.push({ href: this.href, name: this.download });
      });
    const lying = vi.fn(async () => ({
      blob: new Blob([new Uint8Array([0x25, 0x50, 0x44, 0x46])], { type: 'application/pdf' }),
      filename: 'report.pdf',
    }));
    const { container } = render(
      <MediaCard
        ref_={{ modality: 'file', mime: 'text/plain', sizeBytes: 4, source: 'captured', locator: 'body' }}
        resolve={lying}
        labels={LABELS}
      />,
    );
    const button = container.querySelector('button');
    expect(button, 'a file card offers a button before any bytes are fetched').not.toBeNull();
    expect(container.querySelector('a[download]')).toBeNull();

    fireEvent.click(button!);

    await waitFor(() =>
      expect(saved, 'one click on Download must produce one saved file').toHaveLength(1),
    );
    expect(saved[0].name).toBe('report.pdf');
    // A data: URL would mean the whole file was serialised through the DOM.
    expect(saved[0].href.startsWith('data:')).toBe(false);

    // The card then settles into a link, so a second save needs no refetch.
    const link = await waitFor(() => {
      const a = container.querySelector('a[download]');
      expect(a).not.toBeNull();
      return a as HTMLAnchorElement;
    });
    // The no-refetch property, stated as something observable. A second click
    // cannot be measured here — the control is now a real anchor and jsdom's
    // fireEvent does not run its native download — so the property is asserted
    // where it actually shows: the settled link points at the object URL the
    // FIRST fetch created, so following it costs nothing.
    //
    // The previous assertion claimed "not once per click" after a single click,
    // which could only ever have caught a double-fetch within that one click.
    expect(link.getAttribute('href')).toBe(saved[0].href);
    expect(lying, 'one click, one fetch').toHaveBeenCalledTimes(1);
    expect(link.getAttribute('download')).toBe('report.pdf');
    expect(link.getAttribute('data-blob-type')).toBe('application/pdf');
    clickSpy.mockRestore();
  });

  it('says the preview failed rather than showing a broken image', async () => {
    const boom = vi.fn(async () => {
      throw new Error('unreadable');
    });
    const { container } = render(
      <MediaCard
        ref_={{ modality: 'image', mime: 'image/png', sizeBytes: 4, source: 'captured', locator: 'body' }}
        resolve={boom}
        labels={LABELS}
      />,
    );
    await waitFor(() => expect(screen.getByText('preview failed')).toBeInTheDocument());
    expect(container.querySelector('img')).toBeNull();
  });
});

// The metadata line and the remaining preview arm. None of it was reached:
// every fixture above declares a mime and a 3–4 byte size, so the size
// formatter only ever took its smallest branch and the mime-absent arm was
// never rendered at all.
describe('MediaCard metadata line', () => {
  const withSize = (sizeBytes: number | undefined, mime?: string) =>
    render(
      <MediaCard
        ref_={{ modality: 'file', mime, sizeBytes, source: 'captured', locator: 'body' } as MediaRef}
        resolve={resolver}
        labels={LABELS}
      />,
    );

  it.each([
    ['bytes below a kilobyte', 900, '900 B'],
    ['kilobytes', 2048, '2.0 KB'],
    ['megabytes', 3 * 1024 * 1024, '3.0 MB'],
  ])('reports %s in a unit an operator can read', (_name, n, want) => {
    withSize(n as number);
    expect(screen.getByText(want as string)).toBeInTheDocument();
  });

  it('says the size is unknown rather than showing a confident zero', () => {
    // A ref whose provider declared no length is not a zero-byte payload.
    // "0 B" beside a working Download is a contradiction the operator has to
    // resolve by clicking.
    withSize(undefined);
    expect(screen.getByText('size unknown')).toBeInTheDocument();
    expect(screen.queryByText('0 B')).toBeNull();
  });

  it('omits the type entirely when the wire declared none', () => {
    // An empty <span> where the mime goes reads as a type that is blank
    // rather than a type nobody sent.
    const { container } = withSize(900);
    const spans = Array.from(container.querySelectorAll('span')).map((s) => s.textContent);
    expect(spans).toEqual(['file', '900 B']);
  });

  it('shows the declared type when there is one', () => {
    withSize(900, 'application/pdf');
    expect(screen.getByText('application/pdf')).toBeInTheDocument();
  });
});

describe('MediaCard preview arms and failure paths', () => {
  it('previews captured video with a player, not a still frame', async () => {
    // The third preview arm. image and audio were covered; video fell through
    // to the same else-branch and nothing distinguished it, so routing video
    // to <audio> would have gone unnoticed.
    const mp4 = vi.fn(async () => ({
      blob: new Blob([new Uint8Array([0, 0, 0, 1])], { type: 'video/mp4' }),
      filename: 'clip.mp4',
    }));
    const { container } = render(
      <MediaCard
        ref_={{ modality: 'video', mime: 'video/mp4', sizeBytes: 4, source: 'captured', locator: 'body' }}
        resolve={mp4}
        labels={LABELS}
      />,
    );
    await waitFor(() => expect(container.querySelector('video')).not.toBeNull());
    expect(container.querySelector('audio')).toBeNull();
    expect(container.querySelector('img')).toBeNull();
  });

  it('releases the previous object URL when the same card resolves a second ref', async () => {
    // Without the revoke, a card that re-resolves holds every blob it has ever
    // fetched for the lifetime of the page. The Agent Dashboard re-renders one
    // card per selected event, so this is a leak per click, not per session.
    const revokeSpy = vi.spyOn(URL, 'revokeObjectURL');
    const first = vi.fn(async () => ({
      blob: new Blob([new Uint8Array([1])], { type: 'image/png' }),
      filename: 'a.png',
    }));
    const { container, rerender } = render(
      <MediaCard
        ref_={{ modality: 'image', mime: 'image/png', sizeBytes: 1, source: 'captured', locator: 'body' }}
        resolve={first}
        labels={LABELS}
      />,
    );
    const firstUrl = await waitFor(() => {
      const img = container.querySelector('img');
      expect(img).not.toBeNull();
      return (img as HTMLImageElement).getAttribute('src');
    });

    revokeSpy.mockClear();
    rerender(
      <MediaCard
        ref_={{ modality: 'image', mime: 'image/png', sizeBytes: 2, source: 'captured', locator: 'json:other' }}
        resolve={first}
        labels={LABELS}
      />,
    );
    await waitFor(() => expect(revokeSpy).toHaveBeenCalledWith(firstUrl));
    revokeSpy.mockRestore();
  });

  it('saves nothing and says so when the click itself cannot fetch the bytes', async () => {
    // A non-previewable card resolves on click, so a resolver that throws
    // reaches save() rather than the prefetch effect. Without the null check
    // the handler would build an anchor pointing at undefined and "download"
    // a file named undefined.
    const clickSpy = vi
      .spyOn(HTMLAnchorElement.prototype, 'click')
      .mockImplementation(() => undefined);
    const boom = vi.fn(async () => {
      throw new Error('gone');
    });
    const { container } = render(
      <MediaCard
        ref_={{ modality: 'file', mime: 'application/pdf', sizeBytes: 4, source: 'captured', locator: 'body' }}
        resolve={boom}
        labels={LABELS}
      />,
    );
    fireEvent.click(container.querySelector('button')!);
    await waitFor(() => expect(screen.getByText('preview failed')).toBeInTheDocument());
    expect(clickSpy).not.toHaveBeenCalled();
    expect(container.querySelector('a[download]')).toBeNull();
    clickSpy.mockRestore();
  });
});
