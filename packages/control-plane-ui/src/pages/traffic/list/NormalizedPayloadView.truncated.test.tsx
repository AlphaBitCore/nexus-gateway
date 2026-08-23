import { describe, it, expect } from 'vitest';
import { screen } from '@testing-library/react';
import { renderWithProviders } from '@/test/test-utils';
import { NormalizedPayloadView } from './NormalizedPayloadView';
import type { NormalizedPayload } from '@/api/types';

// The normalized view recomputes its projection from the stored body. When that
// body is only a prefix, the projection is faithful to the bytes it saw and
// still describes an incomplete payload — trailing messages, tool calls and the
// usage row are simply absent. That renders identically to a model that stopped
// early, which is the confusion this banner exists to remove.
//
// Truncation is orthogonal to normalize status: a prefix routinely normalizes
// as 'ok', so the banner must not be tied to a failure state.
const okPayload: NormalizedPayload = {
  kind: 'ai-chat',
  normalizeVersion: 'v2.1',
};

describe('NormalizedPayloadView truncation banner', () => {
  it('warns when the projection was computed from a stored prefix', () => {
    renderWithProviders(
      <NormalizedPayloadView payload={okPayload} direction="response" status="ok" truncated />,
    );
    expect(screen.getByText(/computed from a truncated body/i)).toBeInTheDocument();
  });

  it('says the client still received the whole response, so the gap reads as ours', () => {
    renderWithProviders(
      <NormalizedPayloadView payload={okPayload} direction="response" status="ok" truncated />,
    );
    expect(screen.getByText(/client received the complete response/i)).toBeInTheDocument();
  });

  it('stays silent for a whole body', () => {
    renderWithProviders(
      <NormalizedPayloadView payload={okPayload} direction="response" status="ok" truncated={false} />,
    );
    expect(screen.queryByText(/computed from a truncated body/i)).toBeNull();
  });

  it('still warns when the projection itself came out empty', () => {
    // The empty-payload arm returns early. A truncated body that normalized to
    // nothing is precisely when a reader most needs to know why.
    renderWithProviders(
      <NormalizedPayloadView payload={null} direction="response" status="ok" truncated />,
    );
    expect(screen.getByText(/computed from a truncated body/i)).toBeInTheDocument();
  });
});
