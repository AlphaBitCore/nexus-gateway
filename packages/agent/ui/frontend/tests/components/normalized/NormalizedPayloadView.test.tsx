import { describe, it, expect } from 'vitest';
import { render, screen } from '@testing-library/react';
import { I18nextProvider } from 'react-i18next';
import i18n from '@/i18n';
import { NormalizedPayloadView } from '@/components/normalized/NormalizedPayloadView';

const wrap = (payload: unknown, extra: Record<string, unknown> = {}) =>
  render(<I18nextProvider i18n={i18n}><NormalizedPayloadView payload={payload as never} spans={[]} direction="request" {...extra} /></I18nextProvider>);

describe('NormalizedPayloadView', () => {
  it('renders ai-chat messages as role bubbles incl. tool_use', () => {
    wrap({
      kind: 'ai-chat', normalizeVersion: 'v2',
      messages: [
        { role: 'user', content: [{ type: 'text', text: 'hi there' }] },
        { role: 'assistant', content: [{ type: 'text', text: 'hello back' }, { type: 'tool_use', toolUse: { name: 'web_search', input: { q: 'x' } } }] },
      ],
    });
    expect(screen.getByText('hi there')).toBeInTheDocument();
    expect(screen.getByText('hello back')).toBeInTheDocument();
    expect(screen.getByText(/web_search/)).toBeInTheDocument();
  });

  it('renders an ai-embedding payload', () => {
    const { container } = wrap({ kind: 'ai-embedding', normalizeVersion: 'v2', model: 'text-embedding-3', inputs: ['alpha', 'beta'] });
    expect((container.textContent || '').length).toBeGreaterThan(0);
  });

  it('renders an http-json payload', () => {
    const { container } = wrap({ kind: 'http-json', normalizeVersion: 'v2', http: { bodyView: { json: { hello: 'world' } } } });
    expect(container.textContent).toMatch(/hello/);
  });

  it('renders an http-binary metadata card', () => {
    wrap({ kind: 'http-binary', normalizeVersion: 'v2', http: { bodyView: { binaryRef: { size: 2048, contentType: 'application/octet-stream', sha256: 'abc123' } } } });
    expect(screen.getByText(/abc123/)).toBeInTheDocument();
  });

  it('renders the unsupported placeholder', () => {
    const { container } = wrap({ kind: 'unsupported', normalizeVersion: 'v2' });
    expect((container.textContent || '').length).toBeGreaterThan(0);
  });

  it('renders the redacted placeholder when content was dropped', () => {
    const { container } = wrap({ kind: 'ai-chat', normalizeVersion: 'v2', redacted: true, messages: [] });
    expect((container.textContent || '').length).toBeGreaterThan(0);
  });

  it('renders the operator-drop story when the operator chose drop-content', () => {
    wrap({ kind: 'ai-chat', normalizeVersion: 'v2', redacted: true, redactedReason: 'operator-drop', ruleIds: ['email'] });
    expect(screen.getByText('Content dropped per storage policy.')).toBeInTheDocument();
    expect(screen.getByText(/Operator set storageAction=drop-content/)).toBeInTheDocument();
    expect(screen.queryByText(/could not be safely applied/)).not.toBeInTheDocument();
  });

  it('stays neutral for rows without a recorded reason', () => {
    wrap({ kind: 'ai-chat', normalizeVersion: 'v2', redacted: true, ruleIds: ['email'] });
    expect(screen.getByText('Content dropped per storage policy.')).toBeInTheDocument();
    expect(screen.getByText(/Content not stored per the storage policy/)).toBeInTheDocument();
    // Neither operator intent nor a degradation may be asserted.
    expect(screen.queryByText(/Operator set storageAction=drop-content/)).not.toBeInTheDocument();
    expect(screen.queryByText(/could not be safely applied/)).not.toBeInTheDocument();
  });

  it('renders the degradation story with a localized cause and failed addresses', () => {
    wrap({
      kind: 'ai-chat', normalizeVersion: 'v2', redacted: true,
      redactedReason: 'redact-degraded',
      redactedDetail: { cause: 'spans-unresolved', failedAddresses: ['messages.2.content.0'] },
      ruleIds: ['email'],
    });
    expect(screen.getByText(/could not be safely applied to the stored copy/)).toBeInTheDocument();
    // The cause renders as a readable phrase carrying the machine token,
    // and the hint does not claim the redaction was applied to the live
    // request.
    expect(screen.getByText(/Cause: the redaction positions could not be resolved on the stored copy \(spans-unresolved\)/)).toBeInTheDocument();
    expect(screen.queryByText(/applied to the live request/)).not.toBeInTheDocument();
    expect(screen.getByText('messages.2.content.0')).toBeInTheDocument();
    // The operator is not blamed for a degradation.
    expect(screen.queryByText(/Operator set storageAction=drop-content/)).not.toBeInTheDocument();
  });

  it('falls back to the raw cause token when no localization exists', () => {
    wrap({
      kind: 'ai-chat', normalizeVersion: 'v2', redacted: true,
      redactedReason: 'redact-degraded',
      redactedDetail: { cause: 'some-future-cause' },
      ruleIds: ['email'],
    });
    expect(screen.getByText(/Cause: some-future-cause/)).toBeInTheDocument();
  });

  it('renders http-sse frames with event chips and per-frame data', () => {
    wrap({
      kind: 'http-sse', normalizeVersion: '2', detectedSpec: 'generic-http', confidence: 1.0,
      http: { bodyView: { sseFrames: [
        { event: 'message_start', data: { type: 'message_start', message: { id: 'msg_01' } } },
        { dataText: '[DONE]' },
      ] } },
    });
    expect(screen.getByText('message_start')).toBeInTheDocument();
    expect(screen.getByText(/msg_01/)).toBeInTheDocument();
    // No-event frame falls back to the SSE protocol default event chip.
    expect(screen.getByText('message')).toBeInTheDocument();
    expect(screen.getByText('[DONE]')).toBeInTheDocument();
  });

  it('shows the truncation note when sseTruncated is set', () => {
    const { container } = wrap({
      kind: 'http-sse', normalizeVersion: '2', detectedSpec: 'generic-http', confidence: 1.0,
      http: { bodyView: { sseFrames: [{ event: 'delta', dataText: 'x' }], sseTruncated: true } },
    });
    expect(container.textContent).toMatch(/Raw/);
  });

  it('renders the neutral Structural badge (not green Tier 1, no confidence numeral) for fallback rows', () => {
    wrap({
      kind: 'http-json', normalizeVersion: '2', detectedSpec: 'generic-http', confidence: 1.0,
      http: { bodyView: { json: { hello: 'world' } } },
    });
    expect(screen.getByText('Structural')).toBeInTheDocument();
    expect(screen.queryByText('Tier 1')).not.toBeInTheDocument();
    // The projection confidence numeral is suppressed: "1.00" next to a
    // real decode's 0.95 would read as more trusted than the decode.
    expect(screen.queryByText(/1\.00/)).not.toBeInTheDocument();
  });

  it('renders the reasoning tokens usage entry when present', () => {
    wrap({
      kind: 'ai-chat', normalizeVersion: '2',
      messages: [{ role: 'assistant', content: [{ type: 'text', text: 'ok' }] }],
      usage: { promptTokens: 10, completionTokens: 20, reasoningTokens: 7 },
    });
    expect(screen.getByText(/Reasoning/)).toBeInTheDocument();
    expect(screen.getByText('7')).toBeInTheDocument();
  });
});
