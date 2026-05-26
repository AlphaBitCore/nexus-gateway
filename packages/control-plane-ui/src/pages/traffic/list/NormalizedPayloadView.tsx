// Operator-facing renderer for one NormalizedPayload direction
// (request or response). Branches on payload.kind:
//
//   ai-*           → chat-style bubbles per message, role chip, tool_use cards,
//                    reasoning blocks italicised, redaction spans rendered
//                    inline as badges keyed by rule_id.
//   http-json      → pretty-printed JSON tree.
//   http-text      → decoded text with monospace font and line wrapping.
//   http-form      → key=value table.
//   http-binary    → metadata card (size · content-type · sha256).
//   unsupported    → muted placeholder with link-to-raw hint.
//
// When `payload.redacted === true` the entire content was dropped per
// storageAction=drop-content; we render a placeholder showing the
// triggering rule IDs only.

import { useTranslation } from 'react-i18next';
import type {
  NormalizedPayload,
  NormalizedMessage,
  NormalizedContentBlock,
  TransformSpan,
} from '@/api/types';
import css from './NormalizedPayloadView.module.css';

interface Props {
  payload: NormalizedPayload | null | undefined;
  spans?: TransformSpan[] | null;
  status?: 'ok' | 'partial' | 'failed' | null;
  errorReason?: string | null;
  /** "request" | "response" — addressed in TransformSpan.contentAddress prefix. */
  direction: 'request' | 'response';
}

export function NormalizedPayloadView(props: Props) {
  const { payload, spans, status, errorReason } = props;
  const { t } = useTranslation();

  // Failure / partial banner.
  let banner: React.ReactNode = null;
  if (status === 'failed') {
    banner = (
      <div className={`${css.banner} ${css.bannerError}`}>
        <strong>{t('pages:traffic.detail.normalized.banner.failedTitle')}</strong>
        {errorReason ? <div>{errorReason}</div> : null}
        <div style={{ marginTop: 'var(--g-space-1)', fontSize: 'var(--g-font-size-xs)', opacity: 0.85 }}>
          {t('pages:traffic.detail.normalized.banner.failedHint')}
        </div>
      </div>
    );
  } else if (status === 'partial') {
    banner = (
      <div className={css.banner}>
        <strong>{t('pages:traffic.detail.normalized.banner.partialTitle')}</strong>
        {errorReason ? <div>{errorReason}</div> : null}
      </div>
    );
  }

  if (!payload) {
    return (
      <div className={css.wrap}>
        {banner}
        <div className={css.placeholder}>
          {t('pages:traffic.detail.normalized.banner.empty')}
        </div>
      </div>
    );
  }

  if (payload.redacted) {
    return (
      <div className={css.wrap}>
        {banner}
        <div className={css.banner}>
          <strong>{t('pages:traffic.detail.normalized.dropped.title')}</strong>
          <div style={{ marginTop: 'var(--g-space-1)' }}>{t('pages:traffic.detail.normalized.dropped.hint')}</div>
          {payload.ruleIds && payload.ruleIds.length > 0 ? (
            <div style={{ marginTop: 'var(--g-space-1-5)', fontFamily: 'var(--g-font-mono)' }}>
              {t('pages:traffic.detail.normalized.dropped.ruleIds')}: {payload.ruleIds.join(', ')}
            </div>
          ) : null}
        </div>
      </div>
    );
  }

  return (
    <div className={css.wrap}>
      {banner}
      {renderTierBadge(payload, t)}
      {payload.kind === 'ai-embedding' ? renderAiEmbedding(payload, t) : null}
      {payload.kind !== 'ai-embedding' && payload.kind.startsWith('ai-') ? renderAi(payload, spans, props.direction, t) : null}
      {payload.kind === 'http-json' ? renderHttpJson(payload) : null}
      {payload.kind === 'http-text' ? renderHttpText(payload) : null}
      {payload.kind === 'http-form' ? renderHttpForm(payload) : null}
      {payload.kind === 'http-multipart' || payload.kind === 'http-binary' ? renderHttpBinary(payload, t) : null}
      {payload.kind === 'unsupported' ? (
        <div className={css.placeholder}>
          {t('pages:traffic.detail.normalized.banner.unsupported')}
        </div>
      ) : null}
    </div>
  );
}

// renderTierBadge surfaces the normalizer-reported DetectedSpec + Confidence
// so operators can tell whether a row was parsed by a precise Tier-1
// per-host normalizer ("chatgpt-web" confidence 0.95), the Tier-2 multi-spec
// pattern probe ("pattern:chatgpt-web" confidence 0.78), or fell through to
// Tier 3 verbatim (no badge). The tier is inferred from the detectedSpec
// prefix: "pattern:" = Tier 2, anything else = Tier 1.
function renderTierBadge(
  payload: NormalizedPayload,
  t: ReturnType<typeof useTranslation>['t'],
): React.ReactNode {
  if (!payload.detectedSpec) return null;
  const isTier2 = payload.detectedSpec.startsWith('pattern:');
  const specLabel = isTier2
    ? payload.detectedSpec.slice('pattern:'.length)
    : payload.detectedSpec;
  const tierKey = isTier2 ? 'tier2' : 'tier1';
  const tierLabel = t(`pages:traffic.detail.normalized.tier.${tierKey}`);
  // Confidence is absent on older rows — omit when not present. Two decimals.
  const confLabel =
    typeof payload.confidence === 'number' && payload.confidence > 0
      ? ` · ${payload.confidence.toFixed(2)}`
      : '';
  const styles: React.CSSProperties = {
    display: 'inline-flex',
    alignItems: 'center',
    gap: 'var(--g-space-1)',
    padding: 'var(--g-space-0-5) var(--g-space-1-5)',
    marginBottom: 'var(--g-space-2)',
    borderRadius: 'var(--radius-sm)',
    fontSize: 'var(--g-font-size-xs)',
    fontFamily: 'var(--g-font-mono)',
    background: isTier2 ? 'var(--color-warning-soft)' : 'var(--color-success-soft)',
    color: isTier2 ? 'var(--color-warning-text)' : 'var(--color-text-primary)',
    border: '1px solid',
    borderColor: isTier2 ? 'var(--color-warning-border)' : 'var(--color-border-subtle)',
  };
  return (
    <div style={styles} title={t('pages:traffic.detail.normalized.tier.hint')}>
      <span>{tierLabel}</span>
      <span>·</span>
      <span>{specLabel}{confLabel}</span>
    </div>
  );
}

// AI rendering

function renderAi(
  payload: NormalizedPayload,
  allSpans: TransformSpan[] | null | undefined,
  _direction: 'request' | 'response',
  t: ReturnType<typeof useTranslation>['t'],
) {
  const usage = payload.usage;
  const modelLine = payload.model ?? payload.protocol ?? '';

  return (
    <>
      {modelLine ? (
        <div className={css.sectionLabel}>
          {t('pages:traffic.detail.normalized.modelLabel')}: <span style={{ fontFamily: 'var(--g-font-mono)' }}>{modelLine}</span>
        </div>
      ) : null}

      {(payload.messages ?? []).map((m, i) => (
        <MessageBubble
          key={i}
          message={m}
          messageIndex={i}
          spans={allSpans}
          t={t}
        />
      ))}

      {usage ? (
        <div className={css.usageRow}>
          {usage.promptTokens != null ? (
            <span className={css.usageItem}>
              <strong>{t('pages:traffic.detail.normalized.usage.prompt')}:</strong>{usage.promptTokens}
            </span>
          ) : null}
          {usage.completionTokens != null ? (
            <span className={css.usageItem}>
              <strong>{t('pages:traffic.detail.normalized.usage.completion')}:</strong>{usage.completionTokens}
            </span>
          ) : null}
          {usage.totalTokens != null ? (
            <span className={css.usageItem}>
              <strong>{t('pages:traffic.detail.normalized.usage.total')}:</strong>{usage.totalTokens}
            </span>
          ) : null}
          {usage.cacheReadTokens != null ? (
            <span className={css.usageItem}>
              <strong>{t('pages:traffic.detail.normalized.usage.cacheRead')}:</strong>{usage.cacheReadTokens}
            </span>
          ) : null}
          {usage.cacheCreationTokens != null ? (
            <span className={css.usageItem}>
              <strong>{t('pages:traffic.detail.normalized.usage.cacheCreate')}:</strong>{usage.cacheCreationTokens}
            </span>
          ) : null}
        </div>
      ) : null}

      {payload.finishReason ? (
        <div className={css.sectionLabel}>
          {t('pages:traffic.detail.normalized.finishReason')}: <code>{payload.finishReason}</code>
        </div>
      ) : null}
    </>
  );
}

// Embedding rendering

/**
 * renderAiEmbedding renders a kind=ai-embedding payload.
 *
 * Request side: shows the text inputs as numbered rows in a compact
 * monospace list. When inputs is absent (binary token array or the
 * payload is a response), renders a "vectors not stored" notice instead
 * so operators can distinguish "captured text" from "binary input" from
 * "response vectors".
 *
 * Response side: embedding vectors are intentionally never stored per
 * SDD §T2.3 — we render a compact badge explaining why the body is
 * empty rather than the generic placeholder.
 */
function renderAiEmbedding(
  payload: NormalizedPayload,
  t: ReturnType<typeof useTranslation>['t'],
): React.ReactNode {
  const inputs = payload.inputs;
  const modelLine = payload.model ?? payload.protocol ?? '';
  const hasInputs = Array.isArray(inputs) && inputs.length > 0;
  const isBinaryOnly = !hasInputs && Array.isArray(inputs);
  // inputs === null/undefined on a response payload — show "vectors not stored"
  const isResponseVectors = inputs == null && !Array.isArray(inputs);

  return (
    <>
      {modelLine ? (
        <div className={css.sectionLabel}>
          {t('pages:traffic.detail.normalized.modelLabel')}: <span style={{ fontFamily: 'var(--g-font-mono)' }}>{modelLine}</span>
        </div>
      ) : null}

      <div className={css.sectionLabel}>
        {t('pages:traffic.detail.normalized.embedding.inputsLabel')}
      </div>

      {hasInputs ? (
        <ol className={css.embeddingInputList}>
          {inputs!.map((input, i) => (
            <li key={i} className={css.embeddingInputItem}>
              <span className={css.embeddingInputIndex}>{i + 1}</span>
              <span className={css.embeddingInputText}>{input}</span>
            </li>
          ))}
        </ol>
      ) : isBinaryOnly ? (
        <div className={css.placeholder}>
          {t('pages:traffic.detail.normalized.embedding.binaryInputNotStored')}
        </div>
      ) : isResponseVectors ? (
        <div className={css.placeholder}>
          {t('pages:traffic.detail.normalized.embedding.vectorsNotStored')}
        </div>
      ) : (
        <div className={css.placeholder}>
          {t('pages:traffic.detail.normalized.embedding.noInputs')}
        </div>
      )}

      {payload.usage ? (
        <div className={css.usageRow}>
          {payload.usage.promptTokens != null ? (
            <span className={css.usageItem}>
              <strong>{t('pages:traffic.detail.normalized.usage.prompt')}:</strong>{payload.usage.promptTokens}
            </span>
          ) : null}
          {payload.usage.totalTokens != null ? (
            <span className={css.usageItem}>
              <strong>{t('pages:traffic.detail.normalized.usage.total')}:</strong>{payload.usage.totalTokens}
            </span>
          ) : null}
        </div>
      ) : null}
    </>
  );
}

function MessageBubble({
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

// HTTP rendering

function renderHttpJson(payload: NormalizedPayload) {
  const json = payload.http?.bodyView?.json;
  if (json != null) {
    return <pre className={css.jsonTree}>{JSON.stringify(json, null, 2)}</pre>;
  }
  // Defence-in-depth for legacy rows that left Kind=http-json populated but
  // only carried BodyView.text (the normalizer wrote raw bytes into text and
  // returned an error). Show the raw text so the operator still sees content
  // instead of "(empty)". Current rows route correctly to http-text and
  // never reach this branch.
  const text = payload.http?.bodyView?.text;
  if (text) {
    return <pre className={css.jsonTree}>{text}</pre>;
  }
  return <div className={css.placeholder}>(empty)</div>;
}

function renderHttpText(payload: NormalizedPayload) {
  const text = payload.http?.bodyView?.text ?? '';
  return <pre className={css.jsonTree}>{text}</pre>;
}

function renderHttpForm(payload: NormalizedPayload) {
  const form = payload.http?.bodyView?.form ?? {};
  const rows = Object.entries(form);
  if (rows.length === 0) {
    return <div className={css.placeholder}>(empty)</div>;
  }
  return (
    <pre className={css.jsonTree}>
      {rows.map(([k, v]) => `${k}=${v}`).join('\n')}
    </pre>
  );
}

function renderHttpBinary(
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

function formatBytesShort(n: number): string {
  if (n < 1024) return `${n} B`;
  if (n < 1024 * 1024) return `${(n / 1024).toFixed(1)} KiB`;
  return `${(n / 1024 / 1024).toFixed(1)} MiB`;
}
