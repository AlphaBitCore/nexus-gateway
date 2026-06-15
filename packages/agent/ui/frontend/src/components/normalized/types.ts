// V2 (#58) — types mirror packages/control-plane-ui/src/api/types.ts
// for NormalizedPayload + nested shapes. Copied here so agent UI can
// render the Normalized tab without importing across packages. Keep
// in sync with CP-UI types; a future refactor should move both into
// packages/ui-shared.

export type NormalizedKind =
  | 'ai-chat'
  | 'ai-completion'
  | 'ai-embedding'
  | 'ai-image'
  | 'http-json'
  | 'http-text'
  | 'http-form'
  | 'http-multipart'
  | 'http-sse'
  | 'http-binary'
  | 'unsupported';

export interface BinaryRef {
  size: number;
  contentType: string;
  sha256: string;
  spillKey?: string;
}

export interface ToolUse {
  callId?: string;
  name: string;
  input?: Record<string, unknown>;
}

export interface ToolResult {
  callId?: string;
  output?: string;
}

export type ContentBlockType = 'text' | 'image_ref' | 'tool_use' | 'tool_result' | 'reasoning';

export interface NormalizedContentBlock {
  type: ContentBlockType;
  text?: string;
  imageRef?: BinaryRef;
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
  binaryRef?: BinaryRef;
  sseFrames?: SSEFrame[];
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
  ruleIds?: string[];
  confidence?: number;
  detectedSpec?: string;
  /** "host" when the adapter was chosen by interception-domain host
   *  match rather than decode coverage — the badge shows a host-matched
   *  label in place of the (honest but not-comparable) confidence
   *  numeral. Keep in sync with CP-UI types. */
  selectionEvidence?: 'host';
  inputs?: string[] | null;
}

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
