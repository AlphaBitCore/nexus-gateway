import { describe, it, expect, vi } from 'vitest';
import { render, screen } from '@testing-library/react';
import { MessageList } from './MessageList';
import type { Msg } from './ChatWithNexus';

vi.mock('react-i18next', () => ({ useTranslation: () => ({ t: (k: string) => k }) }));
// AssistantText pulls the markdown renderer; stub it to its text so this test stays
// focused on the message-list mount logic.
vi.mock('./MarkdownMessage', () => ({ AssistantText: ({ text }: { text: string }) => <span>{text}</span> }));

const noop = async () => false;
const noIds = () => [];

describe('MessageList — condensed-briefing notice (auto-compact visibility)', () => {
  it('renders a kind=summary message as a system notice, never a user bubble', () => {
    const messages: Msg[] = [
      { role: 'user', text: '[earlier conversation summary]\nfindings…', kind: 'summary' },
      { role: 'user', text: 'a real question' },
    ];
    render(<MessageList messages={messages} streaming={false} downloadFile={noop} fileIdsIn={noIds} />);
    expect(screen.getByText('common:assistant.compactedTitle')).toBeInTheDocument();
    // The real user turn still renders as the primary-colored bubble; the
    // summary must not (it uses the dashed-border notice container).
    const bubble = screen.getByText('a real question');
    expect(bubble.className).toContain('bg-primary');
    const notice = screen.getByText(/findings…/);
    expect(notice.className).not.toContain('bg-primary');
  });
});

describe('compact notice placement', () => {
  it('keeps the streamed reply OUT of the condensed-briefing notice', () => {
    // The transcript shape ChatWithNexus produces after a mid-turn compact:
    // notice BEFORE the in-flight reply; both render as separate blocks.
    const messages: Msg[] = [
      { role: 'assistant', text: 'condensed notice', kind: 'summary' },
      { role: 'assistant', text: 'the actual reply' },
    ];
    render(<MessageList messages={messages} streaming={false} downloadFile={noop} fileIdsIn={noIds} />);
    const notice = screen.getByText('condensed notice');
    const reply = screen.getByText('the actual reply');
    expect(notice).toBeInTheDocument();
    expect(reply).toBeInTheDocument();
    // The reply renders as a normal assistant block, not inside the dashed notice.
    expect(notice.closest('div.border-dashed')).not.toBeNull();
    expect(reply.closest('div.border-dashed')).toBeNull();
  });
});

describe('turn timeline', () => {
  it('renders text and tool calls interleaved in event order, hiding the flat chips bar', () => {
    const messages: Msg[] = [
      {
        role: 'assistant',
        text: 'first thoughtsecond thought',
        tools: [
          { name: 'resource_search', status: 'ok', output: 'three hits' },
          { name: 'resource_read', status: 'running' },
        ],
        segments: [
          { kind: 'text', text: 'first thought' },
          { kind: 'tool', toolIdx: 0 },
          { kind: 'text', text: 'second thought' },
          { kind: 'tool', toolIdx: 1 },
        ],
      },
    ];
    const { container } = render(
      <MessageList messages={messages} streaming={false} downloadFile={noop} fileIdsIn={noIds} />,
    );
    const html = container.innerHTML;
    // Order: first thought → search chip → second thought → read chip.
    const a = html.indexOf('first thought');
    const b = html.indexOf('resource_search');
    const c = html.indexOf('second thought');
    const d = html.indexOf('resource_read');
    expect(a).toBeGreaterThan(-1);
    expect(b).toBeGreaterThan(a);
    expect(c).toBeGreaterThan(b);
    expect(d).toBeGreaterThan(c);
    // The flat top chips bar is hidden for a timeline message: each tool name
    // appears exactly once (the inline segment).
    expect(html.split('resource_search').length - 1).toBe(1);
  });
});

describe('per-file download buttons', () => {
  it('renders one de-duped download button per file (structured event + text scan union)', () => {
    const messages: Msg[] = [
      {
        role: 'assistant',
        text: 'Saved file f-1',
        files: [{ id: 'f-1', downloadPath: '/api/admin/assistant/files/f-1' }],
      },
    ];
    render(
      <MessageList
        messages={messages}
        streaming={false}
        downloadFile={noop}
        fileIdsIn={(text) => (text.includes('f-1') ? ['f-1'] : [])}
      />,
    );
    expect(screen.getAllByRole('button', { name: /download/ })).toHaveLength(1);
  });
});
