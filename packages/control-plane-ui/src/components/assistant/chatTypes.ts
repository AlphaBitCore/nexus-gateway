import type { FileRef, SessionTranscript } from './streamChat';

// chatTypes.ts — the chat transcript's message shapes, split from
// ChatWithNexus.tsx along the type seam (the widget keeps the behavior).

export interface ToolChip {
  name: string;
  status: 'running' | 'ok' | 'error';
  /** The resolved structured tool input (from the tool_start SSE event), shown when the
   *  chip is expanded so the user can see exactly what the assistant ran. */
  input?: unknown;
  /** The tool's (redacted, size-capped) result text from the tool_end SSE event, shown
   *  in the expanded chip below the request. */
  output?: string;
}

/** One chronological slice of a live turn: a text run OR a tool call —
 *  the index points into Msg.tools so the segment shares the chip's
 *  status/output as they update (the TUI-style timeline). */
export interface MsgSegment {
  kind: 'text' | 'tool';
  text?: string;
  toolIdx?: number;
}

export interface Msg {
  role: 'user' | 'assistant' | 'error';
  text: string;
  /** Streamed reasoning/thinking deltas — display-only (dim block above the answer,
   *  matching the TUI), never persisted: a reloaded conversation has no reasoning. */
  reasoning?: string;
  tools?: ToolChip[];
  /** The live turn's chronological feed (text runs interleaved with tool
   *  calls, in the order they happened). Built only while streaming; a
   *  reloaded message has none and falls back to the flat layout. */
  segments?: MsgSegment[];
  /** Sandbox files the assistant wrote this turn, from the structured `file` SSE event. */
  files?: FileRef[];
  /** "summary" marks the auto-compact condensed briefing (live notice or a
   *  reloaded transcript's flagged message) — rendered as a system notice. */
  kind?: 'summary';
}


/** transcriptToMsgs maps a reloaded transcript onto the chat's message shape:
 *  the condensed-briefing flag rides along so history re-renders exactly what
 *  the live turn showed. */
export function transcriptToMsgs(s: SessionTranscript): Msg[] {
  return s.messages.map((m): Msg => ({
    role: m.role === 'user' ? 'user' : 'assistant',
    text: m.text,
    ...(m.kind === 'summary' ? { kind: 'summary' as const } : {}),
  }));
}
