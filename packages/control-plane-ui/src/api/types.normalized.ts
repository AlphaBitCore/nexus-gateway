// NormalizedPayload + sidecar (traffic_event_normalized)

/** kind discriminator on NormalizedPayload — must mirror Go normalize.Kind. */
export type NormalizedKind =
  | 'ai-chat'
  | 'ai-completion'
  | 'ai-embedding'
  | 'ai-image'
  | 'ai-tts'
  | 'ai-stt'
  | 'http-json'
  | 'http-text'
  | 'http-form'
  | 'http-multipart'
  | 'http-binary'
  | 'http-sse'
  | 'unsupported';

// MediaRef and its custody vocabulary are defined once in
// @nexus-gateway/ui-shared because the Agent Dashboard renders the same
// normalized payload. Re-exported here so existing imports from this
// module keep working.
export type { MediaRef, MediaModality, MediaSource } from '@nexus-gateway/ui-shared';

export interface ToolUse {
  callId?: string;
  name: string;
  input?: Record<string, unknown>;
}

export interface ToolResult {
  callId?: string;
  output?: string;
}

// 'media' replaces the former 'image_ref': modality is a field on the ref,
// not a block type, so audio, video and documents stop masquerading as
// images.
export type ContentBlockType = 'text' | 'media' | 'tool_use' | 'tool_result' | 'reasoning';

export interface NormalizedContentBlock {
  type: ContentBlockType;
  text?: string;
  mediaRef?: import('@nexus-gateway/ui-shared').MediaRef;
  toolUse?: ToolUse;
  toolResult?: ToolResult;
}

export interface NormalizedMessage {
  role: 'system' | 'user' | 'assistant' | 'tool';
  content: NormalizedContentBlock[];
  finishReason?: string;
}

/** One server-sent event captured by the generic-http SSE projection.
 *  At most one of `data` (frame data parsed as JSON) or `dataText`
 *  (verbatim non-JSON data) is set; a frame whose data line was empty
 *  carries neither. */
export interface SSEFrame {
  event?: string;
  data?: unknown;
  dataText?: string;
}

export interface HTTPBodyView {
  text?: string;
  json?: unknown;
  form?: Record<string, string>;
  /** Whole-body binary. Its locator is `body`. */
  mediaRef?: import('@nexus-gateway/ui-shared').MediaRef;
  /** kind=http-sse: decoded event frames in stream order. */
  sseFrames?: SSEFrame[];
  /** kind=http-sse: true when the capture limit cut the frame list short. */
  sseTruncated?: boolean;
}

export interface HTTPPayload {
  method?: string;
  url?: string;
  headersFiltered?: Record<string, string>;
  bodyView?: HTTPBodyView;
}

export interface NormalizedUsage {
  promptTokens?: number;
  completionTokens?: number;
  totalTokens?: number;
  cacheReadTokens?: number;
  cacheCreationTokens?: number;
  reasoningTokens?: number | null;
}

export interface NormalizedPayload {
  kind: NormalizedKind;
  normalizeVersion: string;
  protocol?: string;
  model?: string;
  stream?: boolean;
  messages?: NormalizedMessage[];
  tools?: unknown[];
  params?: Record<string, unknown>;
  usage?: NormalizedUsage;
  finishReason?: string;
  http?: HTTPPayload;
  /** drop-content placeholder marker — when true the payload is metadata-only. */
  redacted?: boolean;
  /** Why the content was dropped: the operator chose drop-content, or a
   *  redact storage policy could not be applied precisely and degraded.
   *  Absent on rows written before the reason was stamped — the UI then
   *  renders a neutral notice asserting neither story. */
  redactedReason?: 'operator-drop' | 'redact-degraded';
  /** Degradation diagnosis when redactedReason === 'redact-degraded'.
   *  failedAddresses lists content addresses only — never content.
   *  cause is an open vocabulary: the listed tokens render as localized
   *  phrases, unknown future tokens render verbatim. */
  redactedDetail?: {
    cause: 'no-spans' | 'payload-unmarshal' | 'spans-unresolved' | 'marshal-failed' | (string & {});
    failedAddresses?: string[];
  };
  /** rule IDs that triggered the drop-content storage policy. */
  ruleIds?: string[];
  /** Normalizer-reported confidence in [0,1]. Absent/0 on
   *  older rows is interpreted as fully confident (1.0). */
  confidence?: number;
  /** Which spec the normalizer matched. Examples:
   *  "openai-chat" (Tier 1 AI builtin), "chatgpt-web" (Tier 1
   *  per-host adapter), "pattern:chatgpt-web" (Tier 2 multi-spec
   *  pattern probe), "generic-http" (structural fallback projection
   *  of the raw HTTP body, confidence 1.0 = confidence in the
   *  projection only, no AI-semantics claim). Empty on legacy
   *  verbatim rows. */
  detectedSpec?: string;
  /** "host" when the normalizer was selected by interception-domain
   *  host match (a per-host adapter) rather than by decode coverage —
   *  the confidence is then the honest coverage of a known-adapter
   *  body (a single-prompt spec caps near 0.6 by design), NOT a trust
   *  score comparable to a Tier-1 sniffed decode. The badge renders a
   *  "host-matched" label in place of the numeral. Absent for keyed /
   *  sniffed / pattern / fallback rows. */
  selectionEvidence?: 'host';
  /** Text input strings for kind=ai-embedding requests.
   *  Nil/absent when the input was a binary token array (not stored)
   *  or when this is a response payload (embedding vectors are never
   *  stored). */
  inputs?: string[] | null;
}

/** One TransformSpan emitted by hook / aiguard / cache normaliser. */
export interface TransformSpan {
  source: 'hook' | 'aiguard' | 'cache-normaliser' | 'cache-control-inject' | 'cache-key-strip';
  sourceId?: string;
  action: 'redact' | 'strip' | 'inject' | 'replace';
  contentAddress: string;
  start: number;
  end: number;
  replacement?: string;
  reason?: string;
}

export type NormalizeStatus = 'ok' | 'partial' | 'failed';

/** One transform span emitted by a hook / aiguard / cache normaliser. */
export interface TransformSpan {
  source: 'hook' | 'aiguard' | 'cache-normaliser' | 'cache-control-inject' | 'cache-key-strip';
  sourceId?: string;
  action: 'redact' | 'strip' | 'inject' | 'replace';
  contentAddress: string;
  start: number;
  end: number;
  replacement?: string;
  reason?: string;
}

/**
 * Sidecar row for traffic_event_normalized. The Admin API
 * endpoint GET /api/admin/traffic/:id/normalized returns this shape;
 * 404 when no normalize row exists (e.g. capture was disabled).
 */
/** One sparkline bin of a TrafficErrorGroup. */
export interface TrafficErrorBucket {
  ts: string;
  count: number;
}

/**
 * One aggregated error class from GET /api/admin/traffic/errors/groups:
 * failed traffic_event rows grouped by (errorCode, statusRange, provider,
 * model) with a computed first-suspect attribution — "ours" (gateway
 * routing / translation / provider config), "client" (caller-side), or
 * "upstream" (provider-side). Attribution is a triage priority and filter
 * dimension, never an exclusion gate.
 */
export interface TrafficErrorGroup {
  /** Raw stored failure classification; empty = producer did not classify. */
  errorCode: string;
  statusRange: '4xx' | '5xx';
  /** Serving provider/model (routed, falling back to requested); empty on early rejections. */
  provider: string;
  model: string;
  sampleReason: string;
  count: number;
  /**
   * DISTINCT caller-declared end-user tags in the group; 0 when callers send
   * none. Null when the backend skipped the distinct scan (rollup path,
   * class above its affordability threshold) — render as "—".
   */
  affectedEndUsers: number | null;
  firstSeen: string;
  lastSeen: string;
  buckets: TrafficErrorBucket[];
  /** "mixed" marks the overflow pseudo-class (classes shed by the rollup cap). */
  attribution: 'ours' | 'client' | 'upstream' | 'mixed';
}

export interface TrafficErrorGroupsResponse {
  data: TrafficErrorGroup[];
  /** Sparkline bin width the backend picked for the window. */
  bucketSeconds: number;
  /** Which read path served the aggregation: pre-aggregated rollup or raw scan. */
  dataSource: 'rollup' | 'direct';
}

export interface TrafficEventNormalized {
  trafficEventId: string;
  normalizeVersion: string;
  requestNormalized?: NormalizedPayload | null;
  responseNormalized?: NormalizedPayload | null;
  requestStatus?: NormalizeStatus | null;
  responseStatus?: NormalizeStatus | null;
  requestErrorReason?: string | null;
  responseErrorReason?: string | null;
  requestRedactionSpans?: TransformSpan[] | null;
  responseRedactionSpans?: TransformSpan[] | null;
  createdAt: string;
  /**
   * True when the body this projection was computed FROM is only a stored
   * prefix (it reached payload_capture.maxInlineBodyBytes with no spill backend
   * configured). The projection is faithful to the bytes it saw — but those
   * bytes are incomplete, and a partial conversation rendered as a whole one is
   * indistinguishable from a model that stopped early.
   */
  requestTruncated?: boolean;
  /** Response-side truncation — same semantics as requestTruncated. */
  responseTruncated?: boolean;
}
