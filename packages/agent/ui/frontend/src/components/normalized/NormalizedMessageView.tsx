// Chat-bubble rendering for ai-* normalized payloads: one bubble per message,
// one row per content block, redaction spans inline as badges.
//
// Split from NormalizedPayloadView because that file had become the place
// every renderer went — the file-size ratchet is how the repo notices that
// before it is a problem rather than after.

import { useTranslation } from 'react-i18next';
import type { NormalizedMessage, NormalizedContentBlock, TransformSpan } from './types';
import {
  MediaCard,
  MEDIA_STATE_I18N_KEY,
  resolveLocator,
  canResolveLocator,
  sniffMime,
  extensionForMime,
} from '@nexus-gateway/ui-shared';
import css from './NormalizedPayloadView.module.css';


const MEDIA_FALLBACK: Record<string, string> = {
  captured: 'Stored with this request — bytes available.',
  external: 'Remote content — never fetched or stored by Nexus.',
  providerRef: 'Content held by the provider.',
  fingerprint: 'Bytes not retained — fingerprint only.',
  agedOut: 'Captured, then expired by retention.',
  absent: 'Not captured.',
  truncated: 'Capture incomplete or unreadable — media unrecoverable.',
};

export function MessageBubble({
  message,
  messageIndex,
  spans,
  t,
  body,
}: {
  message: NormalizedMessage;
  messageIndex: number;
  spans: TransformSpan[] | null | undefined;
  t: ReturnType<typeof useTranslation>['t'];
  body?: Uint8Array;
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
        <span className={chipClass}>{t(`normalized.role.${role}`)}</span>
        {message.finishReason ? (
          <span className={css.finishReason}>
            {t('normalized.finishReason')}: {message.finishReason}
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
          body={body}
        />
      ))}
    </div>
  );
}

export function ContentBlockRow({
  block,
  address,
  spans,
  t,
  body,
}: {
  block: NormalizedContentBlock;
  address: string;
  spans: TransformSpan[] | null | undefined;
  t: ReturnType<typeof useTranslation>['t'];
  body?: Uint8Array;
}) {
  if (block.type === 'text') {
    return <div className={css.contentText}>{renderTextWithSpans(block.text ?? '', address, spans)}</div>;
  }
  if (block.type === 'reasoning') {
    return <div className={css.contentReasoning}>{block.text ?? ''}</div>;
  }
  if (block.type === 'tool_use') {
    return (
      <div className={css.contentToolUse}>
        <strong>{block.toolUse?.name}</strong>
        {block.toolUse?.callId ? ` · ${block.toolUse.callId}` : ''}
        <div>{JSON.stringify(block.toolUse?.input ?? {}, null, 2)}</div>
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
    return <AgentMediaCard mediaRef={block.mediaRef} body={body} t={t} />;
  }
  return null;
}

/**
 * Agent-side media card.
 *
 * Unlike the Control Plane, the agent needs no artifact endpoint: the
 * captured body is already in the browser, shipped inside the event
 * object because the agent is self-contained. So the locator resolves
 * locally and preview plus download work in full.
 *
 * The one state that cannot reach bytes is a body spilled to a remote
 * backend — the agent deliberately holds no credential for those, and the
 * card says so rather than offering a control that would fail.
 */
export function AgentMediaCard({
  mediaRef,
  body,
  t,
}: {
  mediaRef: NonNullable<NormalizedContentBlock['mediaRef']>;
  body?: Uint8Array;
  t: ReturnType<typeof useTranslation>['t'];
}) {
  const explainKey = mediaRef.truncated === true ? 'truncated' : MEDIA_STATE_I18N_KEY[mediaRef.source];
  const resolve = body
    ? async (ref: typeof mediaRef) => {
        const { bytes } = resolveLocator(body, ref.locator ?? '');
        // The rendered type comes from the bytes, never from the
        // wire-declared mime, which a caller controls.
        const mime = sniffMime(bytes);
        return {
          blob: new Blob([bytes as BlobPart], { type: mime }),
          filename: `nexus-media-${ref.modality}${extensionForMime(mime)}`,
        };
      }
    : undefined;
  return (
    <MediaCard
      ref_={mediaRef}
      resolve={resolve}
      // The browser resolver cannot read a multipart envelope; without this
      // the card would offer a download that throws when clicked.
      resolvable={(r) => canResolveLocator(r.locator)}
      labels={{
        explain: t(`normalized.media.${explainKey}`, MEDIA_FALLBACK[explainKey] ?? ''),
        download: t('normalized.media.download', 'Download'),
        previewFailed: t('normalized.media.previewFailed', 'Preview unavailable.'),
        sizeUnknown: t('normalized.media.sizeUnknown', 'size unknown'),
      }}
      detail={mediaRef.sha256 ? <code>{mediaRef.sha256}</code> : null}
    />
  );
}

// renderTextWithSpans inserts redaction badges for spans that address
// this content block. Spans not addressing this block are ignored.
export function renderTextWithSpans(
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
