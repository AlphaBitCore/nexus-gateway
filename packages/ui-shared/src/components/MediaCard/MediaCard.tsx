import { useCallback, useEffect, useRef, useState, type ReactNode } from 'react';
import clsx from 'clsx';
import styles from './MediaCard.module.css';
import { mediaHasBytes, mediaIsPreviewable, type MediaRef } from '../../types/media';

/**
 * Resolves a media element to its bytes. Supplied by the host app, which
 * is the only layer that knows how to reach them: the Control Plane
 * fetches an authenticated artifact response, while the Agent Dashboard
 * decodes the captured body its event object already carries.
 *
 * The returned `type` is authoritative for rendering — never `ref.mime`,
 * which is whatever the wire declared and therefore caller-controlled.
 */
export type MediaResolver = (ref: MediaRef) => Promise<{ blob: Blob; filename: string }>;

/**
 * Reports whether a resolver could serve this ref. Supplied alongside the
 * resolver so the card never offers a control the resolver would refuse —
 * a client-side resolver cannot read every locator container the server
 * can, and guessing on the card's side is how a dead button appears.
 */
export type MediaResolvable = (ref: MediaRef) => boolean;

export interface MediaCardProps {
  ref_: MediaRef;
  /**
   * Omit to render a metadata-only card with no fetch affordance. A card
   * without a resolver never offers preview or download, which is how a
   * surface that cannot reach bytes stays honest instead of shipping a
   * control that fails on click.
   */
  resolve?: MediaResolver;
  /** Defaults to "any captured ref with a locator" when omitted. */
  resolvable?: MediaResolvable;
  /**
   * Translated copy for this element's state. The host owns i18n; the
   * card owns layout. `explain` is the sentence shown when there are no
   * bytes — every no-bytes state has one, and none gets a button.
   */
  labels: {
    explain: string;
    download: string;
    previewFailed: string;
    sizeUnknown: string;
  };
  className?: string;
  /** Rendered beneath the metadata line — used for provider ids, URLs. */
  detail?: ReactNode;
}

function formatBytes(n: number | undefined, unknown: string): string {
  if (n === undefined || n === null) return unknown;
  if (n < 1024) return `${n} B`;
  if (n < 1024 * 1024) return `${(n / 1024).toFixed(1)} KB`;
  return `${(n / (1024 * 1024)).toFixed(1)} MB`;
}

/**
 * MediaCard — one media element of a captured exchange, in whichever
 * custody state it is actually in.
 *
 * The invariant this component exists to hold: **a control is offered
 * only when the bytes behind it can be served.** Every other state gets
 * a sentence saying what happened to them. A button that 404s or 422s on
 * click is the failure mode this replaces.
 *
 * Images preview inline; audio and video get native players; everything
 * else is download-only. Types that can carry active content are never
 * given a rendering context regardless of what the wire declared —
 * see `isNeverInlineMime`.
 */
export function MediaCard({ ref_, resolve, resolvable, labels, className, detail }: MediaCardProps) {
  const [objectUrl, setObjectUrl] = useState<string | null>(null);
  const [filename, setFilename] = useState<string>('');
  const [blobType, setBlobType] = useState<string>('');
  const [failed, setFailed] = useState(false);
  const revoke = useRef<string | null>(null);

  const canFetch = Boolean(resolve) && mediaHasBytes(ref_) && (resolvable ? resolvable(ref_) : true);
  const wantsPreview = canFetch && mediaIsPreviewable(ref_);

  // Returns what it resolved rather than only writing it to state: the click
  // path below has to act on the bytes within the same handler, and state has
  // not settled by the time this promise resolves.
  const load = useCallback(async (): Promise<{ url: string; name: string } | null> => {
    if (!resolve) return null;
    try {
      const { blob, filename: name } = await resolve(ref_);
      const url = URL.createObjectURL(blob);
      // Replacing the handle without revoking leaks the previous blob for the
      // page's lifetime; a card resolved more than once would hold every one.
      if (revoke.current) URL.revokeObjectURL(revoke.current);
      revoke.current = url;
      setObjectUrl(url);
      setFilename(name);
      // The rendered type comes from the resolved bytes, never from the
      // wire-declared mime.
      setBlobType(blob.type);
      return { url, name };
    } catch {
      setFailed(true);
      return null;
    }
  }, [resolve, ref_]);

  // Clicking Download saves the file. What shipped instead was half the
  // action: the click fetched the bytes and swapped the button for a link,
  // so the request completed, nothing was saved, and only a second click on
  // the now-link produced a file. A control named Download performs the
  // download.
  const save = useCallback(async () => {
    const got = objectUrl ? { url: objectUrl, name: filename } : await load();
    if (!got) return;
    const a = document.createElement('a');
    a.href = got.url;
    a.download = got.name;
    // Appended because a detached anchor is not activated in every browser.
    document.body.appendChild(a);
    a.click();
    a.remove();
  }, [objectUrl, filename, load]);

  useEffect(() => {
    if (wantsPreview) void load();
    return () => {
      if (revoke.current) {
        URL.revokeObjectURL(revoke.current);
        revoke.current = null;
      }
    };
  }, [wantsPreview, load]);

  const meta = (
    <div className={styles.meta}>
      <span className={styles.modality}>{ref_.modality}</span>
      {ref_.mime ? <span className={styles.mime}>{ref_.mime}</span> : null}
      <span className={styles.size}>{formatBytes(ref_.sizeBytes, labels.sizeUnknown)}</span>
    </div>
  );

  if (!canFetch) {
    return (
      <div className={clsx(styles.card, styles.inert, className)}>
        {meta}
        <p className={styles.explain}>{labels.explain}</p>
        {detail}
      </div>
    );
  }

  return (
    <div className={clsx(styles.card, className)}>
      {meta}
      {wantsPreview && objectUrl && !failed ? (
        <div className={styles.preview}>
          {ref_.modality === 'image' ? (
            <img className={styles.image} src={objectUrl} alt="" />
          ) : ref_.modality === 'audio' ? (
            <audio className={styles.player} controls src={objectUrl} />
          ) : (
            <video className={styles.player} controls src={objectUrl} />
          )}
        </div>
      ) : null}
      {failed ? <p className={styles.explain}>{labels.previewFailed}</p> : null}
      {objectUrl ? (
        <a className={styles.download} href={objectUrl} download={filename} data-blob-type={blobType}>
          {labels.download}
        </a>
      ) : (
        <button type="button" className={styles.download} onClick={() => void save()}>
          {labels.download}
        </button>
      )}
      {detail}
    </div>
  );
}
