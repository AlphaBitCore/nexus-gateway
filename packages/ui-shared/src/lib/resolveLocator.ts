// Client-side locator resolution.
//
// A locator addresses bytes inside a captured body, and resolving one is a
// pure function of (body bytes, locator) with no server involved. The
// Control Plane resolves it in Go against the recovered body; a surface
// that already holds the body — the Agent Dashboard, whose event objects
// carry it — resolves the identical locator here. That symmetry is a
// property of the grammar, not a coincidence: any container added later
// must stay resolvable from the bytes alone, or this surface silently
// loses media the other one can show.

/**
 * Containers this resolver can serve from bytes alone. `multipart:` is
 * absent deliberately — parsing a multipart envelope in the browser would
 * duplicate a parser the server already owns.
 *
 * Exported because the card must not offer a control for a locator the
 * resolver would refuse: "a download is offered" and "the resolver can
 * serve it" have to be one decision, not two that drift.
 */
export function canResolveLocator(locator: string | undefined): boolean {
  if (!locator) return false;
  return (
    locator === 'body' ||
    locator.startsWith('json:') ||
    locator.startsWith('datauri:') ||
    locator.startsWith('sse:')
  );
}

/** Thrown when a locator cannot be resolved against the given body. */
export class LocatorError extends Error {
  constructor(
    message: string,
    readonly kind: 'unsupported' | 'not-found' | 'undecodable',
  ) {
    super(message);
    this.name = 'LocatorError';
  }
}

/** Walks a dotted gjson-style path. Numeric segments index arrays. */
function walk(root: unknown, path: string): unknown {
  let cur: unknown = root;
  for (const seg of path.split('.')) {
    if (cur === null || cur === undefined) return undefined;
    if (Array.isArray(cur)) {
      const idx = Number(seg);
      if (!Number.isInteger(idx)) return undefined;
      cur = cur[idx];
    } else if (typeof cur === 'object') {
      cur = (cur as Record<string, unknown>)[seg];
    } else {
      return undefined;
    }
  }
  return cur;
}

function decodeBase64(b64: string): Uint8Array {
  // Tolerate the url-safe alphabet, missing padding, and RFC-2045 line
  // wrapping — a caller controls the wire, and rejecting a recoverable
  // payload helps nobody. Whitespace is stripped BEFORE padding is
  // computed: padding derived from the wrapped length is wrong once atob
  // strips the newlines itself, and whether that survives is engine
  // -specific tolerance rather than anything guaranteed.
  const normalized = b64.replace(/[\s\u0085\p{Cf}]+/gu, '').replace(/-/g, '+').replace(/_/g, '/');
  // The whitespace strip is load-bearing, and an earlier comment here
  // claimed the opposite. atob's forgiving-base64 decode removes only the
  // five ASCII whitespace characters; measured, it THROWS on a payload
  // containing VT, NBSP, U+2028 or an ideographic space, all of which
  // this strip handles. A comment asserting a line is free to delete is
  // how a load-bearing line gets deleted, so this one is pinned by test
  // rather than described.
  //
  // No padding is re-added: atob is specified as forgiving-base64 decode,
  // which already accepts a missing tail, and the one length it rejects
  // (% 4 === 1) is not valid base64 under any padding. Computing padding
  // here was a line no test could falsify — and computing it from the
  // pre-strip length was actively wrong, which is what sent a wrapped
  // payload's decode through engine-specific tolerance.
  let bin: string;
  try {
    bin = atob(normalized);
  } catch {
    throw new LocatorError('payload is not decodable base64', 'undecodable');
  }
  const out = new Uint8Array(bin.length);
  for (let i = 0; i < bin.length; i++) out[i] = bin.charCodeAt(i);
  return out;
}

function splitDataURI(raw: string): { mime: string; payload: string } {
  // Strip exactly what Go strips: `unicode.IsSpace | unicode.Cf`. Expressed
  // as a property class, not an enumeration — the hand-written version
  // covered 32 code points against Go's 195, and each of the 163 it missed
  // produced a captured ref with a locator that this resolver then refused:
  // a Download that throws on click. U+0085 is spelled out because `\s`
  // omits it; `\p{Cf}` supplies the rest, U+FEFF included.
  //
  // Leading only, and MEASURED this time rather than argued: the mime comes
  // from the meta, which sits before the comma, so a trailing character
  // never reaches it, and the payload's own strip in decodeBase64 removes
  // the rest. A both-ends form here is a line no test can fail. The earlier
  // note claiming the opposite reasoning was right about the characters and
  // wrong about which half of the function needed them.
  const s = raw.replace(/^[\s\u0085\p{Cf}]+/u, '');
  const comma = s.indexOf(',');
  // Case-insensitive scheme — RFC 3986 §3.1, and Go folds it too. `DATA:`
  // is a legal spelling of the same URI, so rejecting it here would refuse
  // a reference the codec correctly captured.
  if (!/^data:/i.test(s) || comma < 0) {
    throw new LocatorError('not a data URI', 'undecodable');
  }
  const meta = s.slice('data:'.length, comma);
  if (!meta.includes('base64')) {
    throw new LocatorError('data URI is not base64-encoded', 'undecodable');
  }
  return { mime: meta.replace(/;base64$/, '').replace(/;$/, '').trim(), payload: s.slice(comma + 1) };
}

/** Splits an SSE stream into its data-frame payloads, in arrival order. */
function sseDataFrames(text: string): string[] {
  const frames: string[] = [];
  for (const block of text.split(/\r?\n\r?\n/)) {
    const data = block
      .split(/\r?\n/)
      .filter((l) => l.startsWith('data:'))
      .map((l) => l.slice(5).trimStart())
      .join('\n');
    if (data && data !== '[DONE]') frames.push(data);
  }
  return frames;
}

export interface ResolvedMedia {
  bytes: Uint8Array;
  /** Mime learned while resolving (a data URI declares one); may be empty. */
  mime: string;
}

/**
 * Resolves `locator` against `body`, the captured bytes of the same
 * direction as the payload that carried the reference.
 *
 * Supported containers: `body`, `json:<path>`, `datauri:<path>`,
 * `sse:<frame>:<path>`. `multipart:<part>` is intentionally not handled
 * here — parsing a multipart envelope in the browser would duplicate a
 * parser the server already owns, and no surface needs it client-side.
 */
export function resolveLocator(body: Uint8Array, locator: string): ResolvedMedia {
  if (locator === 'body') return { bytes: body, mime: '' };

  const text = () => new TextDecoder('utf-8').decode(body);

  if (locator.startsWith('json:')) {
    const v = walk(JSON.parse(text()), locator.slice(5));
    if (typeof v !== 'string') throw new LocatorError('locator resolved to nothing', 'not-found');
    return { bytes: decodeBase64(v), mime: '' };
  }

  if (locator.startsWith('datauri:')) {
    const v = walk(JSON.parse(text()), locator.slice(8));
    if (typeof v !== 'string') throw new LocatorError('locator resolved to nothing', 'not-found');
    const { mime, payload } = splitDataURI(v);
    return { bytes: decodeBase64(payload), mime };
  }

  if (locator.startsWith('sse:')) {
    const rest = locator.slice(4);
    const colon = rest.indexOf(':');
    if (colon < 0) throw new LocatorError('malformed sse locator', 'unsupported');
    const frameIdx = Number(rest.slice(0, colon));
    const frames = sseDataFrames(text());
    if (!Number.isInteger(frameIdx) || frameIdx < 0 || frameIdx >= frames.length) {
      throw new LocatorError('sse frame out of range', 'not-found');
    }
    const v = walk(JSON.parse(frames[frameIdx]), rest.slice(colon + 1));
    if (typeof v !== 'string') throw new LocatorError('locator resolved to nothing', 'not-found');
    return { bytes: decodeBase64(v), mime: '' };
  }

  throw new LocatorError(`unsupported locator container: ${locator}`, 'unsupported');
}
