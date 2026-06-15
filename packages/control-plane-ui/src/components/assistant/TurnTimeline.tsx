import { useTranslation } from 'react-i18next';
import type { Msg, ToolChip } from './chatTypes';
import { AssistantText } from './MarkdownMessage';
import { useState } from 'react';

// TurnTimeline — the live turn rendered the way it happened: text runs
// interleaved with tool calls, in order (the TUI's grammar).

type Segment = NonNullable<Msg['segments']>[number];

function InlineTool({ tool }: { tool: ToolChip }) {
  const { t } = useTranslation();
  const [open, setOpen] = useState(false);
  const icon = tool.status === 'error' ? '✗' : tool.status === 'ok' ? '✓' : '…';
  const hasDetail =
    (tool.input != null && (typeof tool.input !== 'object' || Object.keys(tool.input as object).length > 0)) ||
    Boolean(tool.output && tool.output.trim() !== '');
  return (
    <div className="my-1 text-xs text-muted-foreground">
      {hasDetail ? (
        <button
          type="button"
          aria-label={t('common:assistant.toolDetails', { name: tool.name })}
          aria-expanded={open}
          onClick={() => setOpen((v) => !v)}
          className="rounded underline-offset-2 hover:text-foreground hover:underline"
        >
          {`▸ ${tool.name} ${icon}`}
        </button>
      ) : (
        <span>{`▸ ${tool.name} ${icon}`}</span>
      )}
      {open && (
        <div className="mt-1 flex flex-col gap-1">
          {tool.input != null && (
            <pre className="overflow-x-auto rounded-md border border-border bg-background p-2 text-xs text-foreground">
              {typeof tool.input === 'string' ? tool.input : JSON.stringify(tool.input, null, 2)}
            </pre>
          )}
          {tool.output && tool.output.trim() !== '' && (
            <div>
              <p className="font-medium">{t('common:assistant.toolResponse')}</p>
              <pre className="max-h-40 overflow-auto whitespace-pre-wrap break-words rounded-md border border-border bg-background p-2 text-xs text-muted-foreground">
                {tool.output}
              </pre>
            </div>
          )}
        </div>
      )}
    </div>
  );
}

export interface TurnTimelineProps {
  segments: Segment[];
  tools: ToolChip[];
  animate: boolean;
}

export function TurnTimeline({ segments, tools, animate }: TurnTimelineProps) {
  let lastTextIdx = -1;
  segments.forEach((s, idx) => {
    if (s.kind === 'text') lastTextIdx = idx;
  });
  return (
    <div>
      {segments.map((seg, idx) => {
        if (seg.kind === 'text') {
          return seg.text ? (
            <AssistantText key={`t-${idx}`} text={seg.text} animate={animate && idx === lastTextIdx} />
          ) : null;
        }
        const tool = seg.toolIdx != null ? tools[seg.toolIdx] : undefined;
        if (!tool) return null;
        return (
          <div key={`s-${idx}`}>
            <InlineTool tool={tool} />
          </div>
        );
      })}
    </div>
  );
}
