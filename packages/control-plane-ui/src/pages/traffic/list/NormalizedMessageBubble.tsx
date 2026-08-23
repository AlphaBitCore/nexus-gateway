// Chat-bubble rendering for ai-* normalized payloads: one MessageBubble per
// message (role chip + finish reason), one ContentBlockRow per content block
// (text / reasoning / tool_use / tool_result / image_ref), with redaction
// spans rendered inline as badges. Shares the parent CSS module so the markup
// is identical to rendering inline in NormalizedPayloadView.

import { useCallback } from 'react';
import type { useTranslation } from 'react-i18next';
import type {
  NormalizedMessage,
  NormalizedContentBlock,
  TransformSpan,
} from '@/api/types';
import { MediaCard, MEDIA_STATE_I18N_KEY, type MediaRef } from '@nexus-gateway/ui-shared';
import { api } from '@/api/client';
import { useMediaByteOrigin } from './MediaByteOriginContext';
import css from './NormalizedPayloadView.module.css';

export function MessageBubble({
  message,
  messageIndex,
  spans,
  t,
}: {
  message: NormalizedMessage;
  messageIndex: number;
  spans: TransformSpan[] | null | undefined;
  t: ReturnType<typeof useTranslation>['t'];
}) {
  const role = message.role;
  const bubbleClass = `${css.chatBubble} ${
    role === 'user' ? css.chatBubbleUser :
    role === 'assistant' ? css.chatBubbleAssistant :
    role === 'system' ? css.chatBubbleSystem :
    css.chatBubbleTool
  }`;
  const chipClass = `${css.roleChip} ${
    role === 'user' ? css.roleChipUser :
    role === 'assistant' ? css.roleChipAssistant :
    role === 'system' ? css.roleChipSystem :
    css.roleChipTool
  }`;
  return (
    <div className={bubbleClass}>
      <div className={css.roleRow}>
        <span className={chipClass}>{t(`pages:traffic.detail.normalized.role.${role}`)}</span>
        {message.finishReason ? (
          <span className={css.finishReason}>
            {t('pages:traffic.detail.normalized.finishReason')}: {message.finishReason}
          </span>
        ) : null}
      </div>
      {(message.content ?? []).map((b, j) => (
        <ContentBlockRow
          key={j}
          block={b}
          address={`messages.${messageIndex}.content.${j}`}
          spans={spans}
          t={t}
        />
      ))}
    </div>
  );
}

function ContentBlockRow({
  block,
  address,
  spans,
  t,
}: {
  block: NormalizedContentBlock;
  address: string;
  spans: TransformSpan[] | null | undefined;
  t: ReturnType<typeof useTranslation>['t'];
}) {
  if (block.type === 'text') {
    return <div className={css.contentText}>{renderTextWithSpans(block.text ?? '', address, spans)}</div>;
  }
  if (block.type === 'reasoning') {
    return <div className={css.contentReasoning}>{block.text ?? ''}</div>;
  }
  if (block.type === 'tool_use') {
    // Multi-line string inputs (shell commands, file contents, scripts) are
    // the compliance flagship view — "what did the agent run on the host"
    // must read line by line. JSON.stringify would re-escape every newline
    // into a literal \n, so top-level string values containing newlines are
    // lifted out of the JSON dump and rendered verbatim under their key;
    // everything else stays in the pretty-printed JSON remainder.
    const input = block.toolUse?.input ?? {};
    const multiline: Array<[string, string]> = [];
    const rest: Record<string, unknown> = {};
    for (const [k, v] of Object.entries(input)) {
      if (typeof v === 'string' && v.includes('\n')) {
        multiline.push([k, v]);
      } else {
        rest[k] = v;
      }
    }
    return (
      <div className={css.contentToolUse}>
        <strong>{block.toolUse?.name}</strong>
        {block.toolUse?.callId ? ` · ${block.toolUse.callId}` : ''}
        {multiline.map(([k, v]) => (
          <div key={k}>
            <div className={css.toolInputKey}>{k}</div>
            <pre className={css.toolInputPre}>{v}</pre>
          </div>
        ))}
        {Object.keys(rest).length > 0 || multiline.length === 0 ? (
          <pre className={css.toolInputPre}>{JSON.stringify(rest, null, 2)}</pre>
        ) : null}
      </div>
    );
  }
  if (block.type === 'tool_result') {
    return (
      <div className={css.contentToolUse}>
        <strong>tool_result</strong>
        {block.toolResult?.callId ? ` · ${block.toolResult.callId}` : ''}
        <div>{renderTextWithSpans(block.toolResult?.output ?? '', `${address}.toolResult`, spans)}</div>
      </div>
    );
  }
  if (block.type === 'media') {
    if (!block.mediaRef) return null;
    return <MediaBlockCard mediaRef={block.mediaRef} t={t} />;
  }
  return null;
}


/**
 * Renders one media element through the shared MediaCard.
 *
 * The resolver goes through the artifact endpoint rather than decoding the
 * stored body in the browser. Two reasons, and the second is the one that
 * settles it: a 20 MB video would otherwise have to reach the page before
 * anything could be shown, and the server can read every locator container
 * while a browser cannot read multipart at all. Resolving where all five
 * containers are readable is what keeps the card from having to guess which
 * of its own references it is allowed to offer.
 *
 * With no MediaSource in context — any surface that cannot reach an
 * endpoint — the card falls back to metadata-only rather than offering a
 * control that would fail.
 */
export function MediaBlockCard({
  mediaRef,
  t,
}: {
  mediaRef: NonNullable<NormalizedContentBlock['mediaRef']>;
  t: ReturnType<typeof useTranslation>['t'];
}) {
  // Truncation outranks custody: bytes that were captured but arrived
  // incomplete are unrecoverable, and saying "captured" would imply they
  // are not.
  const explainKey = mediaRef.truncated === true ? 'truncated' : MEDIA_STATE_I18N_KEY[mediaRef.source];
  const source = useMediaByteOrigin();

  const resolve = useCallback(
    async (ref: MediaRef) => {
      if (!source) throw new Error('no media source in context');
      // The admin API prefix is part of the path here: getBlob only applies
      // the deployment sub-path prefix, exactly like api.get, so every caller
      // writes the full route. Without /api/admin this resolves against the
      // SPA route table instead and returns index.html with a 200 — the
      // download saved an HTML file and the image preview failed to decode,
      // both from one missing segment.
      const { blob, filename } = await api.getBlob(`/api/admin/traffic/${source.eventId}/artifact`, {
        locator: ref.locator ?? '',
        direction: source.direction,
      });
      // The endpoint names the download from the type it sniffed; the
      // fallback only covers a response that carried no disposition.
      return { blob, filename: filename ?? `nexus-${source.eventId}` };
    },
    [source],
  );

  return (
    <MediaCard
      ref_={mediaRef}
      resolve={source ? resolve : undefined}
      labels={{
        explain: t(`pages:traffic.detail.normalized.media.${explainKey}`),
        download: t('pages:traffic.detail.normalized.media.download'),
        previewFailed: t('pages:traffic.detail.normalized.media.previewFailed'),
        sizeUnknown: t('pages:traffic.detail.normalized.media.sizeUnknown'),
      }}
      detail={
        mediaRef.sha256 ? <code className={css.binaryCard}>{mediaRef.sha256.slice(0, 16)}…</code> : null
      }
    />
  );
}

// renderTextWithSpans inserts redaction badges for spans that address
// this content block. Spans not addressing this block are ignored.
function renderTextWithSpans(
  text: string,
  address: string,
  spans: TransformSpan[] | null | undefined,
): React.ReactNode {
  if (!spans || spans.length === 0) return text;
  const relevant = spans
    .filter((s) => s.contentAddress === address)
    .sort((a, b) => a.start - b.start);
  if (relevant.length === 0) return text;

  // Walk text left-to-right, alternating verbatim slices and badges.
  const out: React.ReactNode[] = [];
  let cursor = 0;
  relevant.forEach((s, i) => {
    if (s.start > cursor) {
      out.push(text.slice(cursor, s.start));
    }
    // The redacted substring has already been replaced by ApplySpans in
    // the stored payload — what we read in `text` is the post-redact
    // version. We render a badge with the rule id + tooltip.
    const tooltip = [
      s.sourceId ? `rule: ${s.sourceId}` : null,
      s.source ? `source: ${s.source}` : null,
      s.action ? `action: ${s.action}` : null,
      s.reason ? `reason: ${s.reason}` : null,
    ].filter(Boolean).join('\n');
    // The replacement string is already substituted in `text`; we
    // overlay a badge styling on it by rendering the same text inside
    // a span with the badge class for the replacement-length.
    // For simplicity we render the badge AROUND the replacement text.
    const replacementLen = s.replacement ? s.replacement.length : (s.end - s.start);
    const segEnd = s.start + replacementLen;
    out.push(
      <span key={i} className={css.redactBadge} title={tooltip}>
        {text.slice(s.start, segEnd)}
      </span>,
    );
    cursor = segEnd;
  });
  if (cursor < text.length) {
    out.push(text.slice(cursor));
  }
  return out;
}
