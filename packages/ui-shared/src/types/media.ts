// Media shapes shared by the Control Plane admin UI and the Agent
// Dashboard. Both render the same normalized payload, so both must agree
// on what a media element is — two parallel copies of this shape is the
// drift that made audio and PDFs render as zero-byte images.

/** Which kind of thing the bytes are. Never inferred from the block type. */
export type MediaModality = 'image' | 'audio' | 'video' | 'file';

/**
 * Custody: whether the bytes exist, and if not, why. Only `captured` has
 * reachable bytes; every other state must render an explanation rather
 * than a control that would fail when clicked.
 */
export type MediaSource =
  /** Held in the governed body, addressable via `locator`. */
  | 'captured'
  /** Never held by design — a remote URL the gateway never fetched. */
  | 'external'
  /** Never held by design — the provider holds it under its own id. */
  | 'provider-ref'
  /** Bytes were not retained; a sha256/size/mime stamp survives. */
  | 'fingerprint'
  /**
   * Captured once, then removed by retention. Produced by the Control
   * Plane when it serves a stored sidecar after establishing that no body
   * is recoverable — the digest survives so the file is still identifiable,
   * the locator does not, so no control is offered for bytes that are gone.
   */
  | 'aged-out'
  /** Never captured at all. */
  | 'absent';

/**
 * One media element of a captured exchange. It never carries content
 * bytes in any encoding: `locator` addresses them inside the captured
 * body, and the app's byte resolver dereferences it.
 */
export interface MediaRef {
  modality: MediaModality;
  /**
   * Wire-declared type. A display hint ONLY — a caller controls the wire,
   * so this must never be used to construct a Blob type. The bytes' real
   * type comes from the resolver.
   */
  mime?: string;
  /**
   * Decoded size in bytes. Always present — a zero-byte element and an
   * element of unknown size are different facts and must stay
   * distinguishable.
   */
  sizeBytes: number;
  source: MediaSource;
  /** Set only when source is `captured`. */
  locator?: string;
  /** Set only when source is `external`. Inert — never dereferenced. */
  url?: string;
  /** Set only when source is `provider-ref`. */
  providerRef?: string;
  /** A real hex digest, or absent. */
  sha256?: string;
  /** Bytes are unrecoverable even though the element was seen. */
  truncated?: boolean;
  /** Machine-readable reason accompanying `aged-out` or `truncated`. */
  cause?: string;
}

/** True when the element has bytes a resolver could serve. */
export function mediaHasBytes(ref: MediaRef): boolean {
  return ref.source === 'captured' && !ref.truncated && Boolean(ref.locator);
}

/**
 * Media types that must never be rendered inline regardless of what the
 * wire declared or a sniffer concluded. SVG and HTML carry script; XML
 * and PDF have their own active-content histories. A captured body is
 * attacker-influenced content, so it is offered for download and never
 * given a rendering context.
 */
export const NEVER_INLINE_MIMES: readonly string[] = [
  'image/svg+xml',
  'text/html',
  'application/xhtml+xml',
  'text/xml',
  'application/xml',
  'application/pdf',
];

/** True when `mime` must be downloaded rather than previewed. */
export function isNeverInlineMime(mime: string | undefined): boolean {
  if (!mime) return false;
  const bare = mime.split(';')[0].trim().toLowerCase();
  return NEVER_INLINE_MIMES.includes(bare);
}

/** True when the card should attempt an inline preview. */
export function mediaIsPreviewable(ref: MediaRef): boolean {
  if (!mediaHasBytes(ref)) return false;
  if (isNeverInlineMime(ref.mime)) return false;
  return ref.modality === 'image' || ref.modality === 'audio' || ref.modality === 'video';
}

/**
 * Custody state → i18n key suffix. Both UIs render the same six states and
 * both need words rather than slugs, so both were carrying their own copy of
 * this table — and the copies had already diverged: the Agent Dashboard's
 * still listed `redacted`, a state deleted for having no producer, while the
 * Control Plane's did not. One concept, two shapes, drifting.
 *
 * Exhaustive over MediaSource by construction: the Record key type makes a
 * new custody state a compile error here rather than a missing string at
 * runtime on whichever surface was forgotten.
 */
export const MEDIA_STATE_I18N_KEY: Record<MediaSource, string> = {
  captured: 'captured',
  external: 'external',
  'provider-ref': 'providerRef',
  fingerprint: 'fingerprint',
  'aged-out': 'agedOut',
  absent: 'absent',
};
