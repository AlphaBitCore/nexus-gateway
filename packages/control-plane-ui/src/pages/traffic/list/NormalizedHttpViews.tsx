// HTTP-kind body renderers for NormalizedPayloadView (http-json, http-text,
// http-form, http-binary/multipart, http-sse). Shares the parent CSS module
// so the markup is identical to rendering inline in the dispatcher.

import { useState } from 'react';
import type { useTranslation } from 'react-i18next';
import type { NormalizedPayload, SSEFrame } from '@/api/types';
import css from './NormalizedPayloadView.module.css';

export function renderHttpJson(
  payload: NormalizedPayload,
  t: ReturnType<typeof useTranslation>['t'],
) {
  const json = payload.http?.bodyView?.json;
  if (json != null) {
    return <pre className={css.jsonTree}>{JSON.stringify(json, null, 2)}</pre>;
  }
  // Defence-in-depth for legacy rows that left Kind=http-json populated but
  // only carried BodyView.text (the normalizer wrote raw bytes into text and
  // returned an error). Show the raw text so the operator still sees content
  // instead of the empty placeholder. Current rows route correctly to
  // http-text and never reach this branch.
  const text = payload.http?.bodyView?.text;
  if (text) {
    return <pre className={css.jsonTree}>{text}</pre>;
  }
  return <div className={css.placeholder}>{t('pages:traffic.detail.normalized.emptyBody')}</div>;
}

export function renderHttpText(payload: NormalizedPayload) {
  const text = payload.http?.bodyView?.text ?? '';
  return <pre className={css.jsonTree}>{text}</pre>;
}

export function renderHttpForm(
  payload: NormalizedPayload,
  t: ReturnType<typeof useTranslation>['t'],
) {
  const form = payload.http?.bodyView?.form ?? {};
  const rows = Object.entries(form);
  if (rows.length === 0) {
    return <div className={css.placeholder}>{t('pages:traffic.detail.normalized.emptyBody')}</div>;
  }
  return (
    <pre className={css.jsonTree}>
      {rows.map(([k, v]) => `${k}=${v}`).join('\n')}
    </pre>
  );
}

export function renderHttpBinary(
  payload: NormalizedPayload,
  t: ReturnType<typeof useTranslation>['t'],
) {
  const ref = payload.http?.bodyView?.binaryRef;
  return (
    <div className={css.binaryCard}>
      <strong>{t('pages:traffic.detail.normalized.binary.title')}</strong>
      {ref ? (
        <>
          <span>
            {t('pages:traffic.detail.normalized.binary.size')}: <code>{formatBytesShort(ref.size)}</code>
          </span>
          <span>
            {t('pages:traffic.detail.normalized.binary.contentType')}: <code>{ref.contentType || '(unknown)'}</code>
          </span>
          {ref.sha256 ? (
            <span>
              sha256: <code>{ref.sha256}</code>
            </span>
          ) : null}
        </>
      ) : (
        <span>{t('pages:traffic.detail.normalized.binary.metadataOnly')}</span>
      )}
    </div>
  );
}

// Frames rendered before the "show all N frames" expander takes over.
// Keeps a 2000-frame stream from flooding the DOM on first paint.
const SSE_COLLAPSE_LIMIT = 50;

export function renderHttpSse(
  payload: NormalizedPayload,
  t: ReturnType<typeof useTranslation>['t'],
) {
  return (
    <HttpSseView
      frames={payload.http?.bodyView?.sseFrames ?? []}
      truncated={payload.http?.bodyView?.sseTruncated === true}
      t={t}
    />
  );
}

// HttpSseView lists decoded SSE frames in stream order: one row per frame
// with an event-name chip (SSE default event type "message" when the frame
// carried no event field) plus the frame data — pretty-printed JSON when
// the normalizer parsed it, verbatim text otherwise.
function HttpSseView({
  frames,
  truncated,
  t,
}: {
  frames: SSEFrame[];
  truncated: boolean;
  t: ReturnType<typeof useTranslation>['t'];
}) {
  const [showAll, setShowAll] = useState(false);

  if (frames.length === 0) {
    return (
      <div className={css.wrap}>
        <div className={css.placeholder}>{t('pages:traffic.detail.normalized.emptyBody')}</div>
        {truncated ? (
          <div className={css.sseTruncatedNote}>
            {t('pages:traffic.detail.normalized.sse.truncatedNote')}
          </div>
        ) : null}
      </div>
    );
  }

  const visible = showAll ? frames : frames.slice(0, SSE_COLLAPSE_LIMIT);
  return (
    <div className={css.sseWrap}>
      <div className={css.sectionLabel}>
        {t('pages:traffic.detail.normalized.sse.framesTitle')}
      </div>
      <ol className={css.sseFrameList}>
        {visible.map((f, i) => (
          <li key={i} className={css.sseFrameRow}>
            <span className={css.sseEventChip}>
              {f.event || t('pages:traffic.detail.normalized.sse.eventFallbackLabel')}
            </span>
            <pre className={css.sseFrameData}>
              {f.dataText != null ? f.dataText : JSON.stringify(f.data, null, 2)}
            </pre>
          </li>
        ))}
      </ol>
      {!showAll && frames.length > SSE_COLLAPSE_LIMIT ? (
        <button
          type="button"
          className={css.sseExpandBtn}
          onClick={() => setShowAll(true)}
        >
          {t('pages:traffic.detail.normalized.sse.showAllFrames', { count: frames.length })}
        </button>
      ) : null}
      {truncated ? (
        <div className={css.sseTruncatedNote}>
          {t('pages:traffic.detail.normalized.sse.truncatedNote')}
        </div>
      ) : null}
    </div>
  );
}

export function formatBytesShort(n: number): string {
  if (n < 1024) return `${n} B`;
  if (n < 1024 * 1024) return `${(n / 1024).toFixed(1)} KiB`;
  return `${(n / 1024 / 1024).toFixed(1)} MiB`;
}
