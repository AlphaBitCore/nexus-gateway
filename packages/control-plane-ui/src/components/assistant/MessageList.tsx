import { useState } from 'react';
import { useTranslation } from 'react-i18next';
import { cn } from '@/lib/utils';
import type { Msg, ToolChip } from './ChatWithNexus';
import { AssistantText } from './MarkdownMessage';
import { TurnTimeline } from './TurnTimeline';

// safeJson pretty-prints a tool's structured input for the expanded chip, tolerating a
// non-serialisable value (it should always be plain JSON from the SSE event, but never
// let a render throw).
function safeJson(value: unknown): string {
  try {
    return typeof value === 'string' ? value : JSON.stringify(value, null, 2);
  } catch {
    return String(value);
  }
}

// ToolChips renders the per-turn tool-activity chips. A chip whose tool carried a
// structured input or produced a result is a button that toggles an inline panel
// showing the request and the response, so the user can see exactly what the
// assistant ran and what came back. Chips with neither stay plain text.
function ToolChips({ tools }: { tools: ToolChip[] }) {
  const { t } = useTranslation();
  const [open, setOpen] = useState<number | null>(null);
  const hasInput = (tool: ToolChip) =>
    tool.input != null && (typeof tool.input !== 'object' || Object.keys(tool.input as object).length > 0);
  const hasOutput = (tool: ToolChip) => Boolean(tool.output && tool.output.trim() !== '');
  const expandable = (tool: ToolChip) => hasInput(tool) || hasOutput(tool);
  return (
    <div className="mb-1 flex flex-col gap-1">
      <div className="flex flex-wrap gap-2 text-xs text-muted-foreground">
        {tools.map((tool, j) => {
          const icon = tool.status === 'error' ? '✗' : tool.status === 'ok' ? '✓' : '…';
          const label = `▸ ${tool.name} ${icon}`;
          return expandable(tool) ? (
            <button
              key={j}
              type="button"
              aria-label={t('common:assistant.toolDetails', { name: tool.name })}
              aria-expanded={open === j}
              onClick={() => setOpen((cur) => (cur === j ? null : j))}
              className="rounded underline-offset-2 hover:text-foreground hover:underline"
            >
              {label}
            </button>
          ) : (
            <span key={j}>{label}</span>
          );
        })}
      </div>
      {open != null && tools[open] && expandable(tools[open]) && (
        <div className="flex flex-col gap-1">
          {hasInput(tools[open]) && (
            <pre className="overflow-x-auto rounded-md border border-border bg-background p-2 text-xs text-foreground">
              {safeJson(tools[open].input)}
            </pre>
          )}
          {hasOutput(tools[open]) && (
            <pre className="max-h-60 overflow-auto rounded-md border border-border bg-background p-2 text-xs text-muted-foreground">
              <span className="text-foreground">{t('common:assistant.toolResponse')}</span>
              {'\n'}
              {tools[open].output}
            </pre>
          )}
        </div>
      )}
    </div>
  );
}

interface MessageListProps {
  messages: Msg[];
  streaming: boolean;
  downloadFile: (id: string) => Promise<boolean>;
  fileIdsIn: (text: string) => string[];
}

// MessageList renders the message scroll area: each turn's bubble, tool chips,
// error bubbles, the streaming placeholder, and per-file download buttons.
// Stateless — the parent owns the transcript and streaming state.
export function MessageList({ messages, streaming, downloadFile, fileIdsIn }: MessageListProps) {
  const { t } = useTranslation();
  return messages.length === 0 ? (
    <div className="space-y-2 text-sm text-muted-foreground">
      <p>{t('common:assistant.empty')}</p>
    </div>
  ) : (
    <>
      {messages.map((m, i) => (
        <div key={i} className={cn('group text-sm', m.role === 'user' && m.kind !== 'summary' ? 'text-right' : 'text-left')}>
          {/* V25: a live turn renders as a TIMELINE — text runs and tool calls
              interleaved in the order they happened (the TUI's grammar); the
              flat chips bar only serves messages without segments (reloads). */}
          {m.tools && m.tools.length > 0 && !(m.segments && m.segments.length > 0) && <ToolChips tools={m.tools} />}
          {m.kind === 'summary' ? (
            // The auto-compact condensed briefing (live notice or a reloaded
            // transcript's flagged message): system-authored context, rendered
            // as a muted left-aligned notice — never a user bubble.
            <div className="rounded-lg border border-dashed border-border px-3 py-2 text-left text-xs text-muted-foreground">
              <p className="mb-1 font-medium">{t('common:assistant.compactedTitle')}</p>
              <div className="max-h-32 overflow-y-auto whitespace-pre-wrap break-words">{m.text}</div>
            </div>
          ) : m.role === 'error' ? (
            <span className="inline-block rounded-lg bg-destructive/10 px-3 py-2 text-destructive">
              {`⚠ ${m.text}`}
            </span>
          ) : m.role === 'user' ? (
            // User turns stay a right-aligned bubble showing their text verbatim
            // (preserved whitespace, no markdown — it is the operator's literal input).
            <span className="inline-block max-w-full whitespace-pre-wrap break-words rounded-lg bg-primary px-3 py-2 text-left text-primary-foreground">
              {m.text}
            </span>
          ) : (
            // Assistant turns render as Markdown in a full-width block (so code blocks
            // and tables get room). The last in-flight turn animates via the typewriter
            // reveal; an empty in-flight turn shows the streaming placeholder.
            <div className="rounded-lg bg-muted px-3 py-2 text-left text-foreground">
              {m.reasoning ? (
                // Thinking deltas render dim + italic above the answer (the same
                // treatment as the TUI's ✱ blocks): visibly "not the answer",
                // bounded so a long think never drowns the conversation.
                <div className="mb-1 max-h-40 overflow-y-auto whitespace-pre-wrap break-words text-xs italic text-muted-foreground">
                  {`✱ ${m.reasoning}`}
                </div>
              ) : null}
              {m.segments && m.segments.length > 0 ? (
                <TurnTimeline
                  segments={m.segments}
                  tools={m.tools ?? []}
                  animate={streaming && i === messages.length - 1}
                />
              ) : m.text ? (
                <AssistantText text={m.text} animate={streaming && i === messages.length - 1} />
              ) : streaming && i === messages.length - 1 ? (
                <span className="text-muted-foreground">…</span>
              ) : null}
            </div>
          )}
          {m.role === 'assistant' &&
            // Primary source: the structured `file` SSE events (m.files), robust to the
            // model not echoing the URL. Fallback: scrape ids from the message text, the
            // only signal for transcripts loaded from history (no live SSE replay).
            // Union + de-dupe by id.
            Array.from(new Set([...(m.files ?? []).map((f) => f.id), ...fileIdsIn(m.text)])).map((fid) => (
              <button
                key={fid}
                type="button"
                onClick={() => void downloadFile(fid)}
                className="mt-1 block text-xs text-primary underline hover:opacity-80"
              >
                {`⬇ ${t('common:assistant.download')}`}
              </button>
            ))}
        </div>
      ))}
    </>
  );
}
