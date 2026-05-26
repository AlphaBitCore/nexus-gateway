// Unit tests for NormalizedPayloadView.
import { describe, it, expect } from 'vitest';
import '@testing-library/jest-dom/vitest';
import { render, screen } from '@testing-library/react';
import { I18nextProvider } from 'react-i18next';
import i18n from 'i18next';
import { initReactI18next } from 'react-i18next';
import type { NormalizedPayload, TransformSpan } from '@/api/types';
import { NormalizedPayloadView } from './NormalizedPayloadView';

// Minimal i18n setup so t() returns key fragments verbatim — enough for
// snapshot-style checks without depending on the live locale files.
i18n.use(initReactI18next).init({
  lng: 'en',
  fallbackLng: 'en',
  resources: { en: { pages: { traffic: { detail: { normalized: {
    tabs: { normalized: 'Normalized', raw: 'Raw' },
    modelLabel: 'Model',
    finishReason: 'Finish reason',
    role: { system: 'System', user: 'User', assistant: 'Assistant', tool: 'Tool' },
    usage: { prompt: 'Prompt', completion: 'Completion', total: 'Total', cacheRead: 'Cache read', cacheCreate: 'Cache create' },
    banner: {
      failedTitle: 'Normalize failed.',
      failedHint: 'Switch to Raw.',
      partialTitle: 'Partial normalize.',
      empty: 'No normalized payload.',
      unsupported: 'Unsupported kind.',
    },
    dropped: { title: 'Content dropped.', hint: 'storageAction=drop-content.', ruleIds: 'Triggered by rules' },
    binary: { title: 'Binary', size: 'Size', contentType: 'Content type', metadataOnly: 'Metadata-only.' },
    embedding: {
      inputsLabel: 'Embedding inputs',
      binaryInputNotStored: 'Binary token-array input — not stored.',
      vectorsNotStored: 'Embedding vectors not stored.',
      noInputs: 'No inputs captured.',
    },
  } } } } } },
  interpolation: { escapeValue: false },
});

const wrap = (node: React.ReactNode) => <I18nextProvider i18n={i18n}>{node}</I18nextProvider>;

describe('NormalizedPayloadView', () => {
  it('renders ai-chat messages with role chips', () => {
    const payload: NormalizedPayload = {
      kind: 'ai-chat',
      normalizeVersion: '1',
      protocol: 'openai-chat',
      model: 'gpt-4o-mini',
      messages: [
        { role: 'system', content: [{ type: 'text', text: 'You are helpful.' }] },
        { role: 'user', content: [{ type: 'text', text: 'Hi' }] },
        { role: 'assistant', content: [{ type: 'text', text: 'Hello there.' }], finishReason: 'stop' },
      ],
      usage: { promptTokens: 5, completionTokens: 3, totalTokens: 8 },
    };
    render(wrap(<NormalizedPayloadView payload={payload} direction="request" />));
    expect(screen.getByText('System')).toBeInTheDocument();
    expect(screen.getByText('User')).toBeInTheDocument();
    expect(screen.getByText('Assistant')).toBeInTheDocument();
    expect(screen.getByText('You are helpful.')).toBeInTheDocument();
    expect(screen.getByText('Hi')).toBeInTheDocument();
    expect(screen.getByText('Hello there.')).toBeInTheDocument();
  });

  it('renders redaction badge for matching span', () => {
    const payload: NormalizedPayload = {
      kind: 'ai-chat',
      normalizeVersion: '1',
      messages: [
        { role: 'user', content: [{ type: 'text', text: 'my email is [REDACTED_EMAIL] please' }] },
      ],
    };
    const spans: TransformSpan[] = [{
      source: 'hook',
      sourceId: 'email',
      action: 'redact',
      contentAddress: 'messages.0.content.0',
      start: 12,
      end: 28,
      replacement: '[REDACTED_EMAIL]',
    }];
    const { container } = render(
      wrap(<NormalizedPayloadView payload={payload} spans={spans} direction="request" />),
    );
    // The badge wraps the replacement substring.
    const badges = container.querySelectorAll('span[title]');
    expect(badges.length).toBeGreaterThan(0);
    // At least one badge contains the replacement marker.
    const matches = Array.from(badges).filter((el) => el.textContent === '[REDACTED_EMAIL]');
    expect(matches.length).toBe(1);
  });

  it('shows failure banner when status=failed', () => {
    render(wrap(
      <NormalizedPayloadView
        payload={null}
        status="failed"
        errorReason="parse error: unexpected token"
        direction="request"
      />,
    ));
    expect(screen.getByText('Normalize failed.')).toBeInTheDocument();
    expect(screen.getByText('parse error: unexpected token')).toBeInTheDocument();
  });

  it('shows drop-content placeholder with rule IDs', () => {
    const payload: NormalizedPayload = {
      kind: 'ai-chat',
      normalizeVersion: '1',
      redacted: true,
      ruleIds: ['email', 'phone'],
    };
    render(wrap(<NormalizedPayloadView payload={payload} direction="request" />));
    expect(screen.getByText('Content dropped.')).toBeInTheDocument();
    expect(screen.getByText(/Triggered by rules.*email, phone/)).toBeInTheDocument();
  });

  it('renders http-json as pretty-printed JSON tree', () => {
    const payload: NormalizedPayload = {
      kind: 'http-json',
      normalizeVersion: '1',
      http: { bodyView: { json: { key: 'value', n: 42 } } },
    };
    const { container } = render(wrap(<NormalizedPayloadView payload={payload} direction="request" />));
    const pre = container.querySelector('pre');
    expect(pre?.textContent).toContain('"key"');
    expect(pre?.textContent).toContain('"value"');
    expect(pre?.textContent).toContain('"n": 42');
  });

  it('renders http-binary as metadata card', () => {
    const payload: NormalizedPayload = {
      kind: 'http-binary',
      normalizeVersion: '1',
      http: { bodyView: { binaryRef: { size: 4096, contentType: 'application/pdf', sha256: 'abc123def' } } },
    };
    render(wrap(<NormalizedPayloadView payload={payload} direction="request" />));
    expect(screen.getByText('Binary')).toBeInTheDocument();
    expect(screen.getByText('application/pdf')).toBeInTheDocument();
    expect(screen.getByText('abc123def')).toBeInTheDocument();
  });

  it('shows empty placeholder when payload is null and no status', () => {
    render(wrap(<NormalizedPayloadView payload={null} direction="request" />));
    expect(screen.getByText('No normalized payload.')).toBeInTheDocument();
  });

  // ai-embedding renderer
  it('renders ai-embedding payload with text inputs as numbered list', () => {
    const payload: NormalizedPayload = {
      kind: 'ai-embedding',
      normalizeVersion: '1',
      model: 'text-embedding-3-small',
      inputs: ['hello world', 'foo bar'],
    };
    render(wrap(<NormalizedPayloadView payload={payload} direction="request" />));
    expect(screen.getByText('Embedding inputs')).toBeInTheDocument();
    expect(screen.getByText('hello world')).toBeInTheDocument();
    expect(screen.getByText('foo bar')).toBeInTheDocument();
    // Model label should be shown.
    expect(screen.getByText('text-embedding-3-small')).toBeInTheDocument();
  });

  it('renders ai-embedding payload with binary input notice when inputs is empty array', () => {
    const payload: NormalizedPayload = {
      kind: 'ai-embedding',
      normalizeVersion: '1',
      inputs: [],
    };
    render(wrap(<NormalizedPayloadView payload={payload} direction="request" />));
    expect(screen.getByText('Binary token-array input — not stored.')).toBeInTheDocument();
  });

  it('renders ai-embedding response payload with vectors-not-stored notice', () => {
    const payload: NormalizedPayload = {
      kind: 'ai-embedding',
      normalizeVersion: '1',
      // inputs absent (null/undefined) on response side
    };
    render(wrap(<NormalizedPayloadView payload={payload} direction="response" />));
    expect(screen.getByText('Embedding vectors not stored.')).toBeInTheDocument();
  });
});
