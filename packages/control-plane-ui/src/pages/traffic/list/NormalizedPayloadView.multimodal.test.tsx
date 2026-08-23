import { describe, it, expect } from 'vitest';
import { screen } from '@testing-library/react';
import { renderWithProviders } from '@/test/test-utils';
import { NormalizedPayloadView } from './NormalizedPayloadView';
import type { NormalizedPayload } from '@/api/types';

// The multimodal text codecs (e88-s8) produce ai-image / ai-tts / ai-stt
// payloads whose text must render through the shared messages view — the
// same path as chat — with no empty shell and no inlined base64. These pin
// AC-1: the drawer actually SHOWS the modality's text.

describe('NormalizedPayloadView multimodal kinds', () => {
  it('renders an ai-image request prompt as message text', () => {
    const payload: NormalizedPayload = {
      kind: 'ai-image',
      normalizeVersion: 'v2',
      model: 'dall-e-3',
      messages: [{ role: 'user', content: [{ type: 'text', text: 'a red fox in the snow' }] }],
    };
    renderWithProviders(<NormalizedPayloadView payload={payload} direction="request" />);
    expect(screen.getByText('a red fox in the snow')).toBeInTheDocument();
  });

  it('renders an ai-image response revised prompt + image_ref summary (no base64)', () => {
    const payload: NormalizedPayload = {
      kind: 'ai-image',
      normalizeVersion: 'v2',
      messages: [
        {
          role: 'assistant',
          content: [
            { type: 'text', text: 'a vivid red fox' },
            {
              type: 'media',
              mediaRef: {
                modality: 'image',
                mime: 'image/png',
                sizeBytes: 2048,
                source: 'captured',
                locator: 'json:data.0.b64_json',
              },
            },
          ],
        },
      ],
    };
    const { container } = renderWithProviders(
      <NormalizedPayloadView payload={payload} direction="response" />,
    );
    expect(screen.getByText('a vivid red fox')).toBeInTheDocument();
    // The card reports the real format, not the bare word "image" every
    // format used to collapse to, and the raw base64 never reaches the DOM.
    expect(container.textContent).toContain('image/png');
    expect(container.textContent).not.toMatch(/[A-Za-z0-9+/]{200,}={0,2}/); // no long b64 blob
  });

  it('offers no control for media whose bytes were never retained', () => {
    const payload: NormalizedPayload = {
      kind: 'ai-chat',
      normalizeVersion: 'v3',
      messages: [
        {
          role: 'user',
          content: [
            {
              type: 'media',
              mediaRef: {
                modality: 'audio',
                mime: 'audio/wav',
                sizeBytes: 9644,
                source: 'fingerprint',
                sha256: 'a'.repeat(64),
              },
            },
          ],
        },
      ],
    };
    const { container } = renderWithProviders(
      <NormalizedPayloadView payload={payload} direction="request" />,
    );
    // The no-dead-control invariant itself is exercised in the shared
    // card's own suite, with a working resolver — asserting it here would
    // pass for the wrong reason, because this surface passes no resolver at
    // all and therefore renders no control in ANY custody state.
    // What this test owns is the drawer's rendering of the metadata.
    expect(container.textContent).toContain('audio/wav');
    expect(container.textContent).toContain('audio');
    expect(container.textContent).toContain('9.4 KB');
  });

  it('renders an ai-tts input as message text', () => {
    const payload: NormalizedPayload = {
      kind: 'ai-tts',
      normalizeVersion: 'v2',
      model: 'tts-1',
      messages: [{ role: 'user', content: [{ type: 'text', text: 'read this aloud' }] }],
    };
    renderWithProviders(<NormalizedPayloadView payload={payload} direction="request" />);
    expect(screen.getByText('read this aloud')).toBeInTheDocument();
  });

  it('renders an ai-stt transcript as assistant message text', () => {
    const payload: NormalizedPayload = {
      kind: 'ai-stt',
      normalizeVersion: 'v2',
      messages: [{ role: 'assistant', content: [{ type: 'text', text: 'the transcribed words' }] }],
    };
    renderWithProviders(<NormalizedPayloadView payload={payload} direction="response" />);
    expect(screen.getByText('the transcribed words')).toBeInTheDocument();
  });
});
