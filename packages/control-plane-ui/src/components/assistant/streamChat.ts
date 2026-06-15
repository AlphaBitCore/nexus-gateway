import { authorizedFetch } from '@/api/client';
import { parseSSEBuffer } from './sse';

export { parseSSEBuffer } from './sse';

// A navigation directive the backend emits when the agent drives the canvas; the
// widget maps view→route and navigates.
export interface NavigateDirective {
  view: string;
  status?: string;
  model?: string;
  eventId?: string;
}

// ConfirmPreview is the impact preview for a high-blast-radius confirm-tier
// tool — server-rendered from a read-only state read (current → effect), shown in the
// card before Allow. `unavailable` means the read failed (fail-open: the card still
// shows, with a caution). Absent for ordinary confirm tools.
export interface ConfirmPreview {
  action?: string;
  summary?: string;
  current?: Record<string, unknown>;
  irreversible?: boolean;
  unavailable?: boolean;
  note?: string;
}

// A confirm-tier write the agent wants to run. The widget shows an Allow/Deny card
// and POSTs the decision. `input` is the resolved structured tool input (server-
// rendered, never model free-text) so the user sees exactly what will execute.
export interface ConfirmRequest {
  callId: string;
  sessionId: string;
  tool: string;
  input: unknown;
  reason: string;
  prod: boolean;
  preview?: ConfirmPreview;
}

// Callbacks the chat widget binds to consume the assistant SSE stream. Mirrors the
// backend event protocol (text / reasoning / tool_start / tool_end
// / navigate / confirm / usage / error / turn_aborted / done).
/** A sandbox file the assistant wrote, surfaced via the structured `file` SSE event
 *  (sourced from the write_file tool's own output — not scraped from the model's prose). */
export interface FileRef {
  id: string;
  downloadPath: string;
}

export interface StreamCallbacks {
  onText?: (delta: string) => void;
  onReasoning?: (delta: string) => void;
  onToolStart?: (name: string, input?: unknown) => void;
  /** `output` is the tool's (redacted, size-capped) result text so the chip can show the
   *  response, not just the request. */
  onToolEnd?: (name: string, isError: boolean, output?: string) => void;
  onNavigate?: (directive: NavigateDirective) => void;
  /** The kernel condensed older turns to fit the context window — the persisted
   *  transcript was durably rewritten, so the UI surfaces a notice. */
  onCompact?: (messagesBefore: number, messagesAfter: number) => void;
  onConfirm?: (request: ConfirmRequest) => void;
  /** A write_file result: render a download button without waiting for the model to echo a URL. */
  onFile?: (file: FileRef) => void;
  onError?: (message: string) => void;
  /** The turn was stopped (interrupt / disconnect-grace). Not an error bubble. */
  onAborted?: () => void;
  /** The turn finished. */
  onDone?: (sessionId?: string) => void;
}

// newSessionId mints a client-side session id for a new conversation. The id is a path
// param on the split endpoints; it is only ever resolved within the caller's own userId
// namespace server-side, so a client-chosen id is safe.
export function newSessionId(): string {
  if (typeof crypto !== 'undefined' && typeof crypto.randomUUID === 'function') {
    return crypto.randomUUID();
  }
  // Fallback for environments without crypto.randomUUID (older test runners).
  return `s-${Date.now().toString(36)}-${Math.random().toString(36).slice(2, 10)}`;
}

function dispatch(event: string, raw: string, cb: StreamCallbacks): void {
  let data: Record<string, unknown>;
  try {
    data = JSON.parse(raw) as Record<string, unknown>;
  } catch {
    return; // resilient to a malformed frame
  }
  switch (event) {
    case 'text':
      cb.onText?.(String(data.delta ?? ''));
      break;
    case 'reasoning':
      cb.onReasoning?.(String(data.delta ?? ''));
      break;
    case 'tool_start':
      cb.onToolStart?.(String(data.name ?? ''), data.input);
      break;
    case 'tool_end':
      cb.onToolEnd?.(String(data.name ?? ''), Boolean(data.isError), typeof data.output === 'string' ? data.output : undefined);
      break;
    case 'confirm':
      cb.onConfirm?.({
        callId: String(data.callId ?? ''),
        sessionId: String(data.sessionId ?? ''),
        tool: String(data.tool ?? ''),
        input: data.input,
        reason: String(data.reason ?? ''),
        prod: Boolean(data.prod),
        preview: (data.preview as ConfirmPreview | undefined) ?? undefined,
      });
      break;
    case 'file':
      if (data.id) cb.onFile?.({ id: String(data.id), downloadPath: String(data.downloadPath ?? '') });
      break;
    case 'navigate':
      cb.onNavigate?.({
        view: String(data.view ?? ''),
        status: data.status ? String(data.status) : undefined,
        model: data.model ? String(data.model) : undefined,
        eventId: data.eventId ? String(data.eventId) : undefined,
      });
      break;
    case 'error':
      cb.onError?.(String(data.message ?? 'error'));
      break;
    case 'turn_aborted':
      cb.onAborted?.();
      break;
    case 'compact':
      cb.onCompact?.(Number(data.messagesBefore ?? 0), Number(data.messagesAfter ?? 0));
      break;
    case 'done':
      cb.onDone?.(data.sessionId ? String(data.sessionId) : undefined);
      break;
    // 'usage' is consumed in a later phase (token/cost surfacing); 'gap' (history
    // truncated on a long reconnect) is ignored — the live state still streams.
  }
}

const maxStreamReconnects = 5;

// runChat drives one turn under the command/data-stream split: it POSTs the chat
// to start the turn (which runs detached server-side), then opens the long-lived SSE
// stream and consumes it, auto-reconnecting with ?lastSeq= across a transient drop
// (the turn keeps running) until a terminal event (done / turn_aborted) or the caller
// aborts. The session id is caller-supplied (newSessionId() for a fresh chat); the same
// id continues an owned conversation.
export async function runChat(
  sessionId: string,
  message: string,
  cb: StreamCallbacks,
  signal?: AbortSignal,
  model?: string,
): Promise<void> {
  let firedDone = false;
  const fireDone = (sid?: string) => {
    if (firedDone) return;
    firedDone = true;
    cb.onDone?.(sid);
  };
  const local: StreamCallbacks = { ...cb, onDone: fireDone };
  const base = `/api/admin/assistant/sessions/${encodeURIComponent(sessionId)}`;

  // 1. Start the turn. A 409 (turn_in_progress), 422 (auth), or 503 surfaces as an error.
  try {
    const startRes = await authorizedFetch(`${base}/chat`, {
      method: 'POST',
      headers: { 'Content-Type': 'application/json' },
      body: JSON.stringify({ message, ...(model ? { model } : {}) }),
      signal,
    });
    if (!startRes.ok) {
      let msg = `request failed (${startRes.status})`;
      try {
        const j = (await startRes.json()) as { error?: { message?: string } };
        msg = j?.error?.message ?? msg;
      } catch {
        /* keep the status message */
      }
      cb.onError?.(msg);
      fireDone();
      return;
    }
  } catch (e) {
    if (signal?.aborted) {
      fireDone();
      return;
    }
    cb.onError?.(e instanceof Error ? e.message : 'network error');
    fireDone();
    return;
  }

  // 2. Stream the turn, reconnecting across transient drops with the last seq seen.
  let lastSeq = 0;
  let attempts = 0;
  while (!signal?.aborted) {
    let terminal = false;
    try {
      const res = await authorizedFetch(`${base}/stream?lastSeq=${lastSeq}`, { signal });
      if (res.status === 404) break; // turn finished + entry reclaimed, or never started
      if (!res.ok || !res.body) {
        cb.onError?.(`stream failed (${res.status})`);
        break;
      }
      const reader = res.body.getReader();
      const decoder = new TextDecoder();
      let buffer = '';
      for (;;) {
        const { done, value } = await reader.read();
        if (done) break;
        buffer += decoder.decode(value, { stream: true });
        const { frames, rest } = parseSSEBuffer(buffer);
        buffer = rest;
        for (const f of frames) {
          attempts = 0; // a frame is real progress — only NO-progress reconnects count toward the cap
          if (f.id !== undefined) {
            const n = parseInt(f.id, 10);
            if (!isNaN(n) && n > lastSeq) lastSeq = n;
          }
          dispatch(f.event, f.data, local);
          if (f.event === 'done' || f.event === 'turn_aborted') terminal = true;
        }
      }
      if (terminal) break;
      // The stream ended without a terminal event: the server dropped us mid-turn (the
      // turn keeps running). Reconnect with lastSeq to resume.
    } catch {
      if (signal?.aborted) break;
      // Transient network error → reconnect with backoff below.
    }
    if (++attempts > maxStreamReconnects) {
      cb.onError?.('lost the connection to the assistant');
      break;
    }
    await new Promise((r) => setTimeout(r, Math.min(250 * attempts, 1500)));
  }
  fireDone(sessionId);
}

// interruptChat stops an in-flight turn for the session (the Stop button / a deliberate
// popup close). Best-effort: a 409 (nothing running) is ignored.
export async function interruptChat(sessionId: string): Promise<void> {
  try {
    await authorizedFetch(`/api/admin/assistant/sessions/${encodeURIComponent(sessionId)}/interrupt`, {
      method: 'POST',
    });
  } catch {
    /* best-effort: the turn's disconnect-grace will cancel it if this never lands */
  }
}

// Session metadata returned by GET /assistant/sessions.
export interface SessionMeta {
  id: string;
  title: string;
  updatedAt: string;
}

// listSessions fetches the caller's own conversation history (metadata only).
export async function listSessions(): Promise<SessionMeta[]> {
  const res = await authorizedFetch('/api/admin/assistant/sessions');
  if (!res.ok) return [];
  const j = (await res.json()) as { sessions?: SessionMeta[] };
  return j.sessions ?? [];
}

// deleteSession removes one of the caller's sessions (row + spilled transcript).
export async function deleteSession(id: string): Promise<boolean> {
  const res = await authorizedFetch(`/api/admin/assistant/sessions/${encodeURIComponent(id)}`, {
    method: 'DELETE',
  });
  return res.ok;
}

// A past conversation re-rendered as a flat role+text transcript, plus the
// trusted-audit verdict for its revision chain (verified on every load).
export interface SessionTranscript {
  id: string;
  messages: {
    role: string;
    text: string;
    /** "summary" marks the auto-compact condensed briefing — system-authored
     *  context the UI renders as a notice, not a user bubble. */
    kind?: string;
  }[];
  integrity?: { status: string; detail?: string };
}

// getSession fetches one of the caller's conversations for re-rendering.
export async function getSession(id: string): Promise<SessionTranscript | null> {
  const res = await authorizedFetch(`/api/admin/assistant/sessions/${encodeURIComponent(id)}`);
  if (!res.ok) return null;
  return (await res.json()) as SessionTranscript;
}

// downloadFile fetches a sandbox file WITH the bearer (the endpoint is auth-gated, so
// a plain link would 401) and triggers a browser save. Returns false on failure.
export async function downloadFile(id: string): Promise<boolean> {
  const res = await authorizedFetch(`/api/admin/assistant/files/${encodeURIComponent(id)}`);
  if (!res.ok) return false;
  const blob = await res.blob();
  const cd = res.headers.get('Content-Disposition') ?? '';
  const name = /filename="?([^"]+)"?/.exec(cd)?.[1] ?? id;
  const url = URL.createObjectURL(blob);
  const a = document.createElement('a');
  a.href = url;
  a.download = name;
  document.body.appendChild(a);
  a.click();
  a.remove();
  URL.revokeObjectURL(url);
  return true;
}

// A single selectable model: the bare code sent in the chat request, plus a display
// label and the owning provider's name (both catalog-sourced; label falls back to the
// code and provider to "" when the catalog has no row) for the grouped picker.
export interface ModelOption {
  code: string;
  label: string;
  provider: string;
}

// The default + client-selectable inference models (GET /assistant/models).
export interface ModelOptions {
  default: string;
  models: ModelOption[];
}

// listModels fetches the backend's default + selectable models for the picker.
export async function listModels(): Promise<ModelOptions> {
  const res = await authorizedFetch('/api/admin/assistant/models');
  if (!res.ok) return { default: '', models: [] };
  return (await res.json()) as ModelOptions;
}

// fileIdsIn extracts assistant-sandbox file ids from a message (the write_file tool
// reports a `/api/admin/assistant/files/<id>` download path in its reply).
export function fileIdsIn(text: string): string[] {
  const ids: string[] = [];
  const re = /\/api\/admin\/assistant\/files\/([a-f0-9]+)/g;
  let m: RegExpExecArray | null;
  while ((m = re.exec(text)) !== null) ids.push(m[1]);
  return ids;
}



// ConfirmResult is the JSON returned by POST /assistant/confirm. In production a
// first Allow comes back with secondConfirmRequired + a one-time challengeToken: the
// write is NOT executed yet, and the client must POST a second Allow echoing that
// token (backend-enforced second confirm). Non-prod / deny / the second Allow
// return { ok: true } with no second step.
export interface ConfirmResult {
  ok: boolean;
  secondConfirmRequired?: boolean;
  challengeToken?: string;
  /** True on a 421 — the session is owned by another CP instance (multi-replica
   * affinity miss); the action did not run and the user should retry. */
  misrouted?: boolean;
  /** True on a `restart_reissue` 409 — the pod that parked the confirm restarted
   * ; the action did not run and the user should re-issue it. */
  reissue?: boolean;
}

// confirmDecision POSTs an Allow/Deny for a parked confirm-tier tool. It is a
// separate request from the SSE stream (the turn is blocked server-side waiting
// for it); the stream resumes once the decision is delivered. challengeToken is sent
// only on the second step of a production confirm.
export async function confirmDecision(
  sessionId: string,
  callId: string,
  decision: boolean,
  challengeToken?: string,
): Promise<ConfirmResult> {
  const res = await authorizedFetch('/api/admin/assistant/confirm', {
    method: 'POST',
    headers: { 'Content-Type': 'application/json' },
    body: JSON.stringify({ sessionId, callId, decision, ...(challengeToken ? { challengeToken } : {}) }),
  });
  // 421 = the parked confirm lives on another CP instance (affinity miss). The
  // action did not run; tell the caller to retry rather than showing "expired".
  if (res.status === 421) {
    return { ok: false, misrouted: true };
  }
  try {
    const body = (await res.json()) as ConfirmResult & { error?: { type?: string } };
    if (res.ok) return body;
    // A non-ok body distinguishes restart_reissue (the pod restarted, re-issue)
    // from a plain expiry, so the UI can guide the user instead of a confusing "expired".
    return { ok: false, reissue: body?.error?.type === 'restart_reissue' };
  } catch {
    return { ok: res.ok };
  }
}

