// sse.ts — the SSE frame parser shared by the chat stream and the run
// stream: splits a raw buffer into complete id:/event:/data: frames plus the
// trailing incomplete remainder. Split from streamChat.ts along the transport
// seam (the chat protocol logic stays there).

export interface SSEFrame {
  event: string;
  data: string;
  /** The SSE `id:` — the per-session sequence number, used to reconnect with ?lastSeq=. */
  id?: string;
}

// parseSSEBuffer splits a raw buffer into complete `id:/event:/data:` frames, returning
// the parsed frames plus the trailing incomplete remainder. Exported for unit tests.
export function parseSSEBuffer(buffer: string): { frames: SSEFrame[]; rest: string } {
  const parts = buffer.split('\n\n');
  const rest = parts.pop() ?? '';
  const frames: SSEFrame[] = [];
  for (const part of parts) {
    let event = 'message';
    let data = '';
    let id: string | undefined;
    for (const line of part.split('\n')) {
      if (line.startsWith('id:')) id = line.slice(3).trim();
      else if (line.startsWith('event:')) event = line.slice(6).trim();
      else if (line.startsWith('data:')) data += line.slice(5).trim();
    }
    if (data) frames.push(id !== undefined ? { event, data, id } : { event, data });
  }
  return { frames, rest };
}

