// Chat-bubble rendering for ai-* normalized payloads: one MessageBubble per
// message (role chip + finish reason), one ContentBlockRow per content block
// (text / reasoning / tool_use / tool_result / image_ref), with redaction
// spans rendered inline as badges. Shares the parent CSS module so the markup
// is identical to rendering inline in NormalizedPayloadView.

import type { useTranslation } from 'react-i18next';
import type {
  NormalizedMessage,
  NormalizedContentBlock,
  TransformSpan,
} from '@/api/types';
import { formatBytesShort } from './NormalizedHttpViews';
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
        />
      ))}
    </div>
  );
}

function ContentBlockRow({
  block,
  address,
  spans,
}: {
  block: NormalizedContentBlock;
  address: string;
  spans: TransformSpan[] | null | undefined;
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
  if (block.type === 'image_ref') {
    return (
      <div className={css.binaryCard}>
        <span>
          {block.imageRef?.contentType ?? 'image'} · {formatBytesShort(block.imageRef?.size ?? 0)}
        </span>
        {block.imageRef?.sha256 ? <code>{block.imageRef.sha256.slice(0, 16)}…</code> : null}
      </div>
    );
  }
  return null;
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
