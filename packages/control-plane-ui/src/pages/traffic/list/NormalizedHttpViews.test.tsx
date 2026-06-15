// Renderer tests for the normalized payload views (Task 2.3, amendments
// A3/A7): http-sse frame list + expander + truncation note, the
// generic-http structural provenance badge, unescaped multi-line
// tool_use input rendering, and the reasoningTokens usage entry.
//
// Assertions target rendered business content (frame data the operator
// reads, the literal command lines an agent ran, token numbers), not
// implementation details.

import { describe, it, expect } from 'vitest';
import { screen, fireEvent } from '@testing-library/react';
import { renderWithProviders } from '@/test/test-utils';
import { NormalizedPayloadView } from './NormalizedPayloadView';
import type { NormalizedPayload, SSEFrame } from '@/api/types';

function ssePayload(frames: SSEFrame[], sseTruncated = false): NormalizedPayload {
  return {
    kind: 'http-sse',
    normalizeVersion: '2',
    detectedSpec: 'generic-http',
    confidence: 1.0,
    http: { bodyView: { sseFrames: frames, sseTruncated } },
  };
}

describe('renderHttpSse (kind=http-sse)', () => {
  it('renders one row per frame: event chip + pretty JSON for data, plain text for dataText', () => {
    renderWithProviders(
      <NormalizedPayloadView
        payload={ssePayload([
          { event: 'message_start', data: { type: 'message_start', message: { id: 'msg_01' } } },
          { dataText: '[DONE]' },
        ])}
        direction="response"
      />,
    );

    // Frame 1: named event chip + pretty-printed (indented, multi-line) JSON.
    expect(screen.getByText('message_start')).toBeInTheDocument();
    const jsonPre = screen.getByText((_, el) =>
      el?.tagName === 'PRE' && (el.textContent ?? '').includes('"id": "msg_01"'));
    expect(jsonPre.textContent).toContain('\n'); // pretty-printed, not single-line
    expect(jsonPre.textContent).toContain('"type": "message_start"');

    // Frame 2: no event name → protocol default chip; data rendered verbatim.
    expect(screen.getByText('message')).toBeInTheDocument();
    expect(screen.getByText('[DONE]')).toBeInTheDocument();
  });

  it('shows the section title and no expander/truncation chrome for small untruncated streams', () => {
    renderWithProviders(
      <NormalizedPayloadView
        payload={ssePayload([{ event: 'ping', data: { ok: true } }])}
        direction="response"
      />,
    );
    expect(screen.getByText('SSE frames')).toBeInTheDocument();
    expect(screen.queryByRole('button', { name: /Show all/ })).not.toBeInTheDocument();
    expect(screen.queryByText(/truncated/i)).not.toBeInTheDocument();
  });

  it('collapses beyond 50 frames behind a "show all N frames" expander', () => {
    const frames: SSEFrame[] = Array.from({ length: 60 }, (_, i) => ({
      event: 'delta',
      dataText: `chunk-${i}`,
    }));
    renderWithProviders(
      <NormalizedPayloadView payload={ssePayload(frames)} direction="response" />,
    );

    // First 50 visible, the rest collapsed.
    expect(screen.getByText('chunk-0')).toBeInTheDocument();
    expect(screen.getByText('chunk-49')).toBeInTheDocument();
    expect(screen.queryByText('chunk-50')).not.toBeInTheDocument();

    const expander = screen.getByRole('button', { name: 'Show all 60 frames' });
    fireEvent.click(expander);

    expect(screen.getByText('chunk-59')).toBeInTheDocument();
    expect(screen.queryByRole('button', { name: /Show all/ })).not.toBeInTheDocument();
  });

  it('renders the empty placeholder (plus truncation note) when no frames were captured', () => {
    renderWithProviders(
      <NormalizedPayloadView payload={ssePayload([], true)} direction="response" />,
    );
    expect(screen.getByText('Empty body.')).toBeInTheDocument();
    expect(
      screen.getByText('Frame view truncated — the full stream was captured and is available in the Raw tab.'),
    ).toBeInTheDocument();
  });

  it('shows the truncation note when sseTruncated is set', () => {
    renderWithProviders(
      <NormalizedPayloadView
        payload={ssePayload([{ event: 'delta', dataText: 'x' }], true)}
        direction="response"
      />,
    );
    expect(
      screen.getByText('Frame view truncated — the full stream was captured and is available in the Raw tab.'),
    ).toBeInTheDocument();
  });
});

describe('provenance tier badge', () => {
  it('renders the neutral Structural badge for detectedSpec=generic-http (not Tier 1)', () => {
    renderWithProviders(
      <NormalizedPayloadView
        payload={{
          kind: 'http-json',
          normalizeVersion: '2',
          detectedSpec: 'generic-http',
          confidence: 1.0,
          http: { bodyView: { json: { hello: 'world' } } },
        }}
        direction="response"
      />,
    );
    expect(screen.getByText('Structural')).toBeInTheDocument();
    expect(screen.getByText(/generic-http/)).toBeInTheDocument();
    expect(screen.queryByText('Tier 1')).not.toBeInTheDocument();
    expect(screen.queryByText('Tier 2')).not.toBeInTheDocument();
    // The projection confidence numeral is suppressed: "1.00" next to
    // Tier-1's "0.95" would read as more trusted than a real decode.
    expect(screen.queryByText(/1\.00/)).not.toBeInTheDocument();
  });

  it('keeps pattern:* → Tier 2 and other non-empty specs → Tier 1', () => {
    const { unmount } = renderWithProviders(
      <NormalizedPayloadView
        payload={{
          kind: 'http-json',
          normalizeVersion: '2',
          detectedSpec: 'pattern:chatgpt-web',
          confidence: 0.78,
          http: { bodyView: { json: {} } },
        }}
        direction="response"
      />,
    );
    expect(screen.getByText('Tier 2')).toBeInTheDocument();
    expect(screen.getByText(/chatgpt-web/)).toBeInTheDocument();
    unmount();

    renderWithProviders(
      <NormalizedPayloadView
        payload={{
          kind: 'ai-chat',
          normalizeVersion: '2',
          detectedSpec: 'anthropic-messages',
          confidence: 0.95,
          messages: [],
        }}
        direction="response"
      />,
    );
    expect(screen.getByText('Tier 1')).toBeInTheDocument();
    expect(screen.getByText(/anthropic-messages/)).toBeInTheDocument();
  });

  it('renders host-matched label instead of the confidence numeral for selectionEvidence=host', () => {
    renderWithProviders(
      <NormalizedPayloadView
        payload={{
          kind: 'ai-chat',
          normalizeVersion: '2',
          detectedSpec: 'claude-web',
          confidence: 0.6,
          selectionEvidence: 'host',
          messages: [],
        }}
        direction="response"
      />,
    );
    // Tier-1 badge, spec name, "host-matched" label — and the honest 0.6
    // coverage is NOT shown as a numeral (it would read as a trust score
    // comparable to a sniffed decode's 0.6, which it is not).
    expect(screen.getByText('Tier 1')).toBeInTheDocument();
    expect(screen.getByText(/claude-web · host-matched/)).toBeInTheDocument();
    expect(screen.queryByText(/0\.60/)).not.toBeInTheDocument();
  });
});

describe('tool_use input rendering (unescaped multi-line strings)', () => {
  it('renders a multi-line string field as real lines, not JSON-escaped \\n', () => {
    const command = 'cd /x\ngo test ./...\ngit add -A';
    renderWithProviders(
      <NormalizedPayloadView
        payload={{
          kind: 'ai-chat',
          normalizeVersion: '2',
          messages: [
            {
              role: 'assistant',
              content: [
                {
                  type: 'tool_use',
                  toolUse: { name: 'Bash', callId: 'call_1', input: { command, timeout: 5000 } },
                },
              ],
            },
          ],
        }}
        direction="response"
      />,
    );

    // The command renders with REAL newlines (3 visual lines)…
    const pre = screen.getByText((_, el) =>
      el?.tagName === 'PRE' && el.textContent === command);
    expect(pre.textContent).toBe('cd /x\ngo test ./...\ngit add -A');
    expect(pre.textContent).not.toContain('\\n');
    // …under its key label.
    expect(screen.getByText('command')).toBeInTheDocument();
    // Non-string fields stay in the pretty-printed JSON remainder.
    expect(screen.getByText((_, el) =>
      el?.tagName === 'PRE' && (el.textContent ?? '').includes('"timeout": 5000'))).toBeInTheDocument();
    // Tool name still shown.
    expect(screen.getByText('Bash')).toBeInTheDocument();
  });

  it('keeps single-line strings and nested values inside the JSON view', () => {
    renderWithProviders(
      <NormalizedPayloadView
        payload={{
          kind: 'ai-chat',
          normalizeVersion: '2',
          messages: [
            {
              role: 'assistant',
              content: [
                {
                  type: 'tool_use',
                  toolUse: { name: 'Read', input: { file_path: '/tmp/a.txt', opts: { limit: 5 } } },
                },
              ],
            },
          ],
        }}
        direction="response"
      />,
    );
    const json = screen.getByText((_, el) =>
      el?.tagName === 'PRE' && (el.textContent ?? '').includes('"file_path": "/tmp/a.txt"'));
    expect(json.textContent).toContain('"limit": 5');
  });
});

describe('usage row reasoningTokens', () => {
  it('renders the Reasoning entry when reasoningTokens is a number', () => {
    renderWithProviders(
      <NormalizedPayloadView
        payload={{
          kind: 'ai-chat',
          normalizeVersion: '2',
          messages: [],
          usage: { promptTokens: 10, completionTokens: 20, reasoningTokens: 128 },
        }}
        direction="response"
      />,
    );
    const reasoning = screen.getByText('Reasoning:');
    expect(reasoning.parentElement?.textContent).toBe('Reasoning:128');
  });

  it('omits the Reasoning entry when reasoningTokens is absent or null', () => {
    renderWithProviders(
      <NormalizedPayloadView
        payload={{
          kind: 'ai-chat',
          normalizeVersion: '2',
          messages: [],
          usage: { promptTokens: 10, reasoningTokens: null },
        }}
        direction="response"
      />,
    );
    expect(screen.queryByText('Reasoning:')).not.toBeInTheDocument();
  });
});
