import { useCallback, useEffect, useRef, useState } from 'react';
import { useTranslation } from 'react-i18next';
import { useNavigate } from 'react-router-dom';
import { cn } from '@/lib/utils';
import { runChat, interruptChat, newSessionId, listSessions, getSession, deleteSession, downloadFile, fileIdsIn, listModels } from './streamChat';
import type { FileRef, ModelOption } from './streamChat';
import type { SessionMeta } from './streamChat';
import { routeSlashCommand } from './slashRouter';
import { useConfirmFlow } from './useConfirmFlow';
import { viewToRoute } from './viewRoutes';
import { ConfirmCard } from './ConfirmCard';
import { SessionHistory } from './SessionHistory';
import { MessageList } from './MessageList';
import { ModelPicker } from './ModelPicker';

export type { Msg, ToolChip } from './chatTypes';
import { transcriptToMsgs } from './chatTypes';
import type { Msg, ToolChip } from './chatTypes';

// ChatWithNexus is the globally-present floating assistant widget: a floating
// button + popup that streams agent turns. Non-modal by design (the background
// page stays interactive), so it uses a labelled dialog WITHOUT aria-modal.
// Navigation directives, confirm-gated writes, and session history layer on
// the same surface.
export function ChatWithNexus() {
  const { t } = useTranslation();
  const navigate = useNavigate();
  const [open, setOpen] = useState(false);
  const [messages, setMessages] = useState<Msg[]>([]);
  const [input, setInput] = useState('');
  const [streaming, setStreaming] = useState(false);
  const [sessionId, setSessionId] = useState<string | null>(null);
  const [showHistory, setShowHistory] = useState(false);
  const [sessions, setSessions] = useState<SessionMeta[]>([]);
  const [model, setModel] = useState('');
  const [models, setModels] = useState<ModelOption[]>([]);
  // Maximize/restore: the panel defaults to a compact 384×512 corner card; the header
  // toggle expands it to a near-fullscreen surface for longer transcripts / wide code.
  const [maximized, setMaximized] = useState(false);
  const abortRef = useRef<AbortController | null>(null);
  const mountedRef = useRef(true);
  const inputRef = useRef<HTMLInputElement | null>(null);
  // The session id of the in-flight turn, so Stop / close / unmount can interrupt it
  // server-side (a detached turn keeps running + billing until interrupted or its
  // disconnect-grace lapses). Kept in a ref so the handlers see the latest value.
  const activeSessionRef = useRef<string | null>(null);

  // Stop the in-flight turn: interrupt it server-side (the detached turn does not stop
  // just because the stream closes) and abort the local stream fetch.
  const stopTurn = useCallback(() => {
    const sid = activeSessionRef.current;
    if (sid) void interruptChat(sid);
    abortRef.current?.abort();
    activeSessionRef.current = null;
  }, []);

  // Abort + interrupt any in-flight turn on unmount: stop the agent (which bills the
  // system VK) and avoid setState-after-unmount.
  useEffect(() => {
    mountedRef.current = true;
    return () => {
      mountedRef.current = false;
      stopTurn();
    };
  }, [stopTurn]);

  // Focus the input when the popup opens (keyboard accessibility).
  useEffect(() => {
    if (open) inputRef.current?.focus();
  }, [open]);

  // Load the selectable models once the popup opens (default pre-selected).
  useEffect(() => {
    if (!open || models.length > 0) return;
    void listModels().then((opts) => {
      if (!mountedRef.current) return;
      setModels(opts.models);
      setModel((cur) => cur || opts.default);
    });
  }, [open, models.length]);

  const safeSetMessages = useCallback((fn: (m: Msg[]) => Msg[]) => {
    if (mountedRef.current) setMessages(fn);
  }, []);

  const patchLastAssistant = useCallback(
    (fn: (m: Msg) => Msg) => {
      safeSetMessages((prev) => {
        const copy = [...prev];
        for (let i = copy.length - 1; i >= 0; i--) {
          // The condensed-briefing notice is system-authored — streamed text
          // and artifact stamps belong to the model's reply, never to it.
          if (copy[i].role === 'assistant' && copy[i].kind !== 'summary') {
            copy[i] = fn(copy[i]);
            break;
          }
        }
        return copy;
      });
    },
    [safeSetMessages],
  );

  // Confirm-gated writes: card state + two-step prod resolution live in
  // the hook; failures surface as error messages in the stream.
  const onConfirmError = useCallback(
    (key: string) => safeSetMessages((m) => [...m, { role: 'error', text: t(key) }]),
    [safeSetMessages, t],
  );
  const { pendingConfirm, confirmToken, armConfirm, clearConfirm, decideConfirm } = useConfirmFlow(onConfirmError);

  const close = useCallback(() => {
    stopTurn(); // closing the popup interrupts the in-flight turn (stops VK billing)
    // Drop any parked confirm so reopening never resurrects a stale card / token
    // (the aborted turn already released the server-side entry).
    clearConfirm();
    setOpen(false);
  }, [stopTurn, clearConfirm]);

  // Session history: list the caller's own conversations, load one back into the
  // view (re-rendering its transcript), delete one, or start fresh. All scoped
  // server-side to the authenticated user.
  const openHistory = useCallback(async () => {
    setShowHistory(true);
    const list = await listSessions();
    if (mountedRef.current) setSessions(list);
  }, []);

  const loadSession = useCallback(
    async (id: string) => {
      const s = await getSession(id);
      if (!s || !mountedRef.current) return;
      // The reload mapping (summary flag) lives in chatTypes — history
      // re-renders what the live turn showed.
      const msgs = transcriptToMsgs(s);
      // The server verifies the session's trusted-audit chain on every load; a
      // failed verdict is surfaced as its own bubble — the conversation still
      // renders (never brick the surface), but the user sees the named problem.
      if (s.integrity && (s.integrity.status === 'chain_broken' || s.integrity.status === 'content_mismatch')) {
        msgs.push({ role: 'error', text: t('common:assistant.integrityWarning') });
      }
      setMessages(msgs);
      setSessionId(id);
      setShowHistory(false);
    },
    [t],
  );

  // attemptDownload surfaces a failed/expired file download as an error bubble —
  // a silent no-op on click would read as a dead button.
  const attemptDownload = useCallback(
    async (id: string): Promise<boolean> => {
      const ok = await downloadFile(id);
      if (!ok) safeSetMessages((m) => [...m, { role: 'error', text: t('common:assistant.fileExpired') }]);
      return ok;
    },
    [safeSetMessages, t],
  );

  const removeSession = useCallback(async (id: string) => {
    if (!(await deleteSession(id)) || !mountedRef.current) return;
    setSessions((prev) => prev.filter((s) => s.id !== id));
    setSessionId((cur) => (cur === id ? null : cur));
  }, []);

  const newChat = useCallback(() => {
    stopTurn();
    setMessages([]);
    setSessionId(null);
    setShowHistory(false);
  }, [stopTurn]);

  // runSlashCommand intercepts a "/" input as a LOCAL command (routing lives
  // in slashRouter.ts). Returns true when the text was a command — nothing is
  // sent to the assistant.
  const runSlashCommand = useCallback(
    (text: string): boolean =>
      routeSlashCommand(text, {
        notice: (key, vars) => safeSetMessages((m) => [...m, { role: 'assistant', text: t(key, vars) }]),
        newChat,
      }),
    [newChat, safeSetMessages, t],
  );

  const send = useCallback(async () => {
    const text = input.trim();
    if (!text || streaming) return; // no new command while a turn is in flight (also enforced server-side)
    setInput('');
    if (runSlashCommand(text)) return;
    safeSetMessages((m) => [...m, { role: 'user', text }, { role: 'assistant', text: '' }]);
    setStreaming(true);
    const ac = new AbortController();
    abortRef.current = ac;
    // A fresh chat mints a client session id; continuing reuses the existing one. The
    // id is tracked in a ref so Stop / close can interrupt this exact turn.
    const sid = sessionId ?? newSessionId();
    activeSessionRef.current = sid;
    if (!sessionId) setSessionId(sid);
    await runChat(
      sid,
      text,
      {
        onText: (d) =>
          patchLastAssistant((m) => {
            // Timeline: a text delta extends the trailing text segment,
            // or opens a new one right after a tool call — the transcript
            // grows in the order things actually happened.
            const segments = (m.segments ?? []).slice();
            const last = segments[segments.length - 1];
            if (last && last.kind === 'text') {
              segments[segments.length - 1] = { ...last, text: (last.text ?? '') + d };
            } else {
              segments.push({ kind: 'text', text: d });
            }
            return { ...m, text: m.text + d, segments };
          }),
        onReasoning: (d) => patchLastAssistant((m) => ({ ...m, reasoning: (m.reasoning ?? '') + d })),
        onToolStart: (name, input) =>
          patchLastAssistant((m) => {
            const tools = [...(m.tools ?? []), { name, status: 'running' as const, input }];
            const segments = [...(m.segments ?? []), { kind: 'tool' as const, toolIdx: tools.length - 1 }];
            return { ...m, tools, segments };
          }),
        onToolEnd: (name, isError, output) =>
          patchLastAssistant((m) => {
            const tools = (m.tools ?? []).slice();
            for (let i = tools.length - 1; i >= 0; i--) {
              if (tools[i].name === name && tools[i].status === 'running') {
                // Preserve the captured input (spread) — the status transitions and the
                // result text lands for the expanded chip's response section.
                tools[i] = { ...tools[i], status: isError ? 'error' : 'ok', output };
                break;
              }
            }
            return { ...m, tools };
          }),
        onFile: (f) =>
          patchLastAssistant((m) => {
            if ((m.files ?? []).some((x) => x.id === f.id)) return m; // de-dupe
            return { ...m, files: [...(m.files ?? []), f] };
          }),
        // The kernel condensed older turns (auto-compact): the persisted
        // transcript was durably rewritten, so say so in-stream — silence
        // would make history change invisibly.
        onCompact: () =>
          setMessages((prev) => {
            // Mid-turn compaction: the notice slots in BEFORE the in-flight
            // reply bubble, so the model's streaming text keeps its own
            // message instead of gluing onto the notice.
            const notice: Msg = { role: 'assistant', text: t('common:assistant.compacted'), kind: 'summary' };
            const last = prev[prev.length - 1];
            if (last && last.role === 'assistant' && last.kind !== 'summary') {
              return [...prev.slice(0, -1), notice, last];
            }
            return [...prev, notice];
          }),
        // The agent drove the canvas → route to the matching CP-UI page (the
        // popup stays open, floating over the new page, so the conversation is
        // retained). Unknown views are ignored (viewToRoute returns null).
        onConfirm: armConfirm,
        onNavigate: (d) => {
          const route = viewToRoute(d.view, { status: d.status, model: d.model, eventId: d.eventId });
          if (route) {
            // Route to the target page but KEEP the popup open, floating over it, so the
            // conversation stays visible and the user can keep chatting while looking at
            // the page the assistant just opened.
            navigate(route);
          }
        },
        // Errors are their own bubble so they are never confused with the
        // assistant's own words.
        onError: (msg) => safeSetMessages((m) => [...m, { role: 'error', text: msg }]),
        // The turn was stopped (Stop button / disconnect-grace). Not an error bubble —
        // the partial reply stays as-is.
        onAborted: () => {},
        // Capture the (possibly newly-created) session id so the next message
        // continues the same conversation server-side (multi-turn).
        onDone: (sid) => {
          if (sid && mountedRef.current) setSessionId(sid);
        },
      },
      ac.signal,
      model || undefined,
    );
    activeSessionRef.current = null;
    if (mountedRef.current) setStreaming(false);
  }, [input, streaming, sessionId, model, patchLastAssistant, safeSetMessages, navigate, runSlashCommand, armConfirm]);

  return (
    <>
      <button
        type="button"
        aria-label={t('common:assistant.open')}
        onClick={() => setOpen((o) => !o)}
        className={cn(
          'fixed bottom-6 right-6 z-50 flex h-14 w-14 items-center justify-center rounded-full',
          'bg-primary text-primary-foreground shadow-lg transition-transform hover:scale-105 focus:outline-none focus:ring-2 focus:ring-ring',
        )}
      >
        <span aria-hidden="true" className="text-xl">💬</span>
      </button>

      {open && (
        <div
          role="dialog"
          aria-label={t('common:assistant.title')}
          onKeyDown={(e) => {
            if (e.key === 'Escape') close();
          }}
          className={cn(
            'fixed z-50 flex flex-col',
            maximized
              ? 'bottom-6 right-6 h-[calc(100dvh-3rem)] max-h-[calc(100dvh-3rem)] w-[min(60rem,calc(100vw-3rem))] max-w-[calc(100vw-3rem)]'
              : 'bottom-24 right-6 h-[32rem] max-h-[calc(100dvh-8rem)] w-96 max-w-[calc(100vw-3rem)]',
            'max-md:inset-0 max-md:h-dvh max-md:max-h-dvh max-md:w-full max-md:max-w-full max-md:rounded-none',
            'rounded-xl border border-border bg-card text-card-foreground shadow-xl',
          )}
        >
          <div className="flex items-center justify-between border-b border-border px-4 py-3">
            <span className="font-semibold">{t('common:assistant.title')}</span>
            <div className="flex items-center gap-3">
              {models.length > 1 && <ModelPicker models={models} value={model} onChange={setModel} />}
              <button
                type="button"
                aria-label={maximized ? t('common:assistant.restore') : t('common:assistant.maximize')}
                title={maximized ? t('common:assistant.restore') : t('common:assistant.maximize')}
                onClick={() => setMaximized((v) => !v)}
                className="text-muted-foreground transition-colors hover:text-foreground"
              >
                <span aria-hidden="true">{maximized ? '⤡' : '⤢'}</span>
              </button>
              <button
                type="button"
                aria-label={t('common:assistant.newChat')}
                title={t('common:assistant.newChat')}
                onClick={newChat}
                className="text-muted-foreground transition-colors hover:text-foreground"
              >
                <span aria-hidden="true">✚</span>
              </button>
              <button
                type="button"
                aria-label={t('common:assistant.history')}
                title={t('common:assistant.history')}
                onClick={() => (showHistory ? setShowHistory(false) : void openHistory())}
                className={cn('transition-colors hover:text-foreground', showHistory ? 'text-foreground' : 'text-muted-foreground')}
              >
                <span aria-hidden="true">🕘</span>
              </button>
              <button
                type="button"
                aria-label={t('common:close')}
                onClick={close}
                className="text-muted-foreground transition-colors hover:text-foreground"
              >
                <span aria-hidden="true">✕</span>
              </button>
            </div>
          </div>

          <div className="flex-1 space-y-3 overflow-y-auto px-4 py-3">
            {showHistory ? (
              <SessionHistory
                showHistory={showHistory}
                sessions={sessions}
                loadSession={loadSession}
                removeSession={removeSession}
              />
            ) : (
              <MessageList
                messages={messages}
                streaming={streaming}
                downloadFile={attemptDownload}
                fileIdsIn={fileIdsIn}
              />
            )}
          </div>

          {pendingConfirm && (
            <ConfirmCard pendingConfirm={pendingConfirm} confirmToken={confirmToken} decideConfirm={decideConfirm} />
          )}

          <form
            className="flex gap-2 border-t border-border p-3"
            onSubmit={(e) => {
              e.preventDefault();
              void send();
            }}
          >
            <input
              ref={inputRef}
              value={input}
              onChange={(e) => setInput(e.target.value)}
              placeholder={t('common:assistant.placeholder')}
              disabled={streaming}
              className="flex-1 rounded-md border border-border bg-background px-3 py-2 text-sm outline-none focus:ring-2 focus:ring-ring disabled:opacity-50"
            />
            {streaming ? (
              // While a turn runs the input is locked; the only action is Stop, which
              // interrupts the turn server-side. No new command can be sent until this
              // one finishes or is stopped.
              <button
                type="button"
                onClick={stopTurn}
                aria-label={t('common:assistant.stop')}
                className="rounded-md border border-border px-3 py-2 text-sm text-foreground transition-colors hover:bg-muted"
              >
                {`◼ ${t('common:assistant.stop')}`}
              </button>
            ) : (
              <button
                type="submit"
                disabled={!input.trim()}
                className="rounded-md bg-primary px-3 py-2 text-sm text-primary-foreground transition-opacity hover:opacity-90 disabled:opacity-50"
              >
                {t('common:assistant.send')}
              </button>
            )}
          </form>
        </div>
      )}
    </>
  );
}
