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
            { type: 'image_ref', imageRef: { size: 2048, contentType: 'image/png', sha256: '' } },
          ],
        },
      ],
    };
    const { container } = renderWithProviders(
      <NormalizedPayloadView payload={payload} direction="response" />,
    );
    expect(screen.getByText('a vivid red fox')).toBeInTheDocument();
    // The image_ref card shows mime; the raw base64 is never present.
    expect(container.textContent).toContain('image/png');
    expect(container.textContent).not.toMatch(/[A-Za-z0-9+/]{200,}={0,2}/); // no long b64 blob
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
