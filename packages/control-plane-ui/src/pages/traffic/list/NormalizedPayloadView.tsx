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
//   http-sse       → server-sent event frame list (event chip + data).
//   unsupported    → muted placeholder with link-to-raw hint.
//
// When `payload.redacted === true` the entire content was dropped per
// storageAction=drop-content; we render a placeholder showing the
// triggering rule IDs only.

import { useTranslation } from 'react-i18next';
import type {
  NormalizedPayload,
  TransformSpan,
} from '@/api/types';
import { MediaByteOriginContext } from './MediaByteOriginContext';
import { MessageBubble } from './NormalizedMessageBubble';
import { renderHttpJson, renderHttpText, renderHttpForm, renderHttpBinary, renderHttpSse } from './NormalizedHttpViews';
import css from './NormalizedPayloadView.module.css';
import { renderTierBadge } from './NormalizedTierBadge';

interface Props {
  payload: NormalizedPayload | null | undefined;
  spans?: TransformSpan[] | null;
  status?: 'ok' | 'partial' | 'failed' | null;
  errorReason?: string | null;
  /** "request" | "response" — addressed in TransformSpan.contentAddress prefix. */
  direction: 'request' | 'response';
  /**
   * The traffic event whose stored body backs this payload. Supplying it is
   * what lets a media card offer Preview and Download; without it the cards
   * render metadata only, which is correct for any surface that cannot
   * reach the bytes.
   */
  eventId?: string;
  /**
   * The stored body this projection was computed from is only a PREFIX. The
   * projection is faithful to the bytes it saw, but those bytes are incomplete —
   * so the view must say so rather than present a partial conversation as a
   * whole one. Mirrors the Raw tab's truncated badge; the two tabs describe the
   * same stored body and must not disagree.
   */
  truncated?: boolean;
}

export function NormalizedPayloadView(props: Props) {
  const { payload, spans, status, errorReason, truncated } = props;
  const { t } = useTranslation();

  // Truncation is orthogonal to normalize status: a prefix can normalize
  // perfectly well ("ok") and still describe an incomplete payload, so this
  // banner renders alongside the status banner rather than instead of it.
  const truncatedBanner = truncated ? (
    <div className={css.banner}>
      <strong>{t('pages:traffic.detail.normalized.banner.truncatedTitle')}</strong>
      <div>{t('pages:traffic.detail.normalized.banner.truncatedHint')}</div>
    </div>
  ) : null;

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
        {truncatedBanner}
        {banner}
        <div className={css.placeholder}>
          {t('pages:traffic.detail.normalized.banner.empty')}
        </div>
      </div>
    );
  }

  if (payload.redacted) {
    // Three distinct stories share the placeholder shape.
    // "operator-drop": dropping the content is the configured policy —
    // assert it. "redact-degraded": the operator asked for redact, but the
    // stored copy could not be redacted precisely, so it was dropped
    // instead — blame the degradation, not the operator. No reason
    // recorded (rows written before the reason was stamped): stay
    // neutral — the row cannot tell which of the two happened.
    const degraded = payload.redactedReason === 'redact-degraded';
    const operatorDrop = payload.redactedReason === 'operator-drop';
    const failedAddresses = payload.redactedDetail?.failedAddresses ?? [];
    // The degradation cause arrives as a machine token; render the
    // localized phrase when one exists, the raw token otherwise.
    const causeToken = payload.redactedDetail?.cause ?? 'unknown';
    const causeLabel = t(`pages:traffic.detail.normalized.dropped.cause.${causeToken}`, {
      defaultValue: causeToken,
    });
    return (
      <div className={css.wrap}>
        {truncatedBanner}
        {banner}
        <div className={css.banner}>
          <strong>
            {degraded
              ? t('pages:traffic.detail.normalized.dropped.degradedTitle')
              : t('pages:traffic.detail.normalized.dropped.title')}
          </strong>
          <div style={{ marginTop: 'var(--g-space-1)' }}>
            {degraded
              ? t('pages:traffic.detail.normalized.dropped.degradedHint', {
                  cause: causeLabel,
                })
              : operatorDrop
                ? t('pages:traffic.detail.normalized.dropped.hint')
                : t('pages:traffic.detail.normalized.dropped.neutralHint')}
          </div>
          {degraded && failedAddresses.length > 0 ? (
            <div style={{ marginTop: 'var(--g-space-1-5)' }}>
              {t('pages:traffic.detail.normalized.dropped.degradedAddresses')}:
              <ul style={{ margin: 'var(--g-space-0-5) 0 0', paddingLeft: 'var(--g-space-4)', fontFamily: 'var(--g-font-mono)' }}>
                {failedAddresses.map((addr) => (
                  <li key={addr}>{addr}</li>
                ))}
              </ul>
            </div>
          ) : null}
          {payload.ruleIds && payload.ruleIds.length > 0 ? (
            <div style={{ marginTop: 'var(--g-space-1-5)', fontFamily: 'var(--g-font-mono)' }}>
              {t('pages:traffic.detail.normalized.dropped.ruleIds')}: {payload.ruleIds.join(', ')}
            </div>
          ) : null}
        </div>
      </div>
    );
  }

  const body = (
    <div className={css.wrap}>
      {truncatedBanner}
        {banner}
      {renderTierBadge(payload, t)}
      {payload.kind === 'ai-embedding' ? renderAiEmbedding(payload, t) : null}
      {payload.kind !== 'ai-embedding' && payload.kind.startsWith('ai-') ? renderAi(payload, spans, props.direction, t) : null}
      {payload.kind === 'http-json' ? renderHttpJson(payload, t) : null}
      {payload.kind === 'http-text' ? renderHttpText(payload) : null}
      {payload.kind === 'http-form' ? renderHttpForm(payload, t) : null}
      {payload.kind === 'http-multipart' || payload.kind === 'http-binary' ? renderHttpBinary(payload, t) : null}
      {payload.kind === 'http-sse' ? renderHttpSse(payload, t) : null}
      {payload.kind === 'unsupported' ? (
        <div className={css.placeholder}>
          {t('pages:traffic.detail.normalized.banner.unsupported')}
        </div>
      ) : null}
    </div>
  );

  // The provider carries only what the leaf card needs to fetch bytes. When
  // eventId is absent the context stays null and every card renders
  // metadata-only — the same honest state a surface with no endpoint gets.
  if (!props.eventId) return body;
  return (
    <MediaByteOriginContext.Provider value={{ eventId: props.eventId, direction: props.direction }}>
      {body}
    </MediaByteOriginContext.Provider>
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
          {typeof usage.reasoningTokens === 'number' ? (
            <span className={css.usageItem}>
              <strong>{t('pages:traffic.detail.normalized.usage.reasoning')}:</strong>{usage.reasoningTokens}
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
 * Response side: embedding vectors are intentionally never stored —
 * we render a compact badge explaining why the body is
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
