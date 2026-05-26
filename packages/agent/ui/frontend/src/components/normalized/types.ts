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

export interface HTTPBodyView {
  text?: string;
  json?: unknown;
  form?: Record<string, string>;
  binaryRef?: BinaryRef;
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
  ruleIds?: string[];
  confidence?: number;
  detectedSpec?: string;
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
