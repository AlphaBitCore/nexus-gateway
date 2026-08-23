import { describe, it, expect } from 'vitest';
import { screen } from '@testing-library/react';
import { renderWithProviders } from '@/test/test-utils';
import { PayloadSection } from './PayloadSections';

// A stored body can be a PREFIX: it reached the inline-vs-spill cutoff
// (payload_capture.maxInlineBodyBytes) with no spill backend configured, so
// spillstore.EmitBody kept the first N bytes and recorded the real size.
//
// The drawer used to render that prefix with nothing to distinguish it from a
// whole body. For a streaming response the prefix ends mid-SSE-frame, which
// reads exactly like a model that stopped part-way through its reasoning — the
// bug this section exists to prevent. These tests pin the two halves of that:
// a truncated body must SAY so and report its true size, and a whole body must
// stay unadorned.
const truncatedSse =
  'data: {"choices":[{"delta":{"reasoning_content":"用户"}}]}\n\ndata: {"object":"chat.completion.chu';

describe('PayloadSection truncation', () => {
  it('marks a truncated body and reports the TRUE captured size, not the stored prefix size', () => {
    renderWithProviders(
      <PayloadSection label="Response Body" value={truncatedSse} truncated sizeBytes={254432} />,
    );
    // 254432 B = 248.5 KiB — the size the provider actually sent, while the
    // rendered prefix is only 100 KiB. Reporting the prefix size here would
    // defeat the point: the reader could not tell how much is missing.
    expect(screen.getByText(/248\.5 KiB/)).toBeInTheDocument();
    expect(screen.getByText(/truncated/i)).toBeInTheDocument();
  });

  it('still marks truncation when the captured size was never recorded', () => {
    renderWithProviders(
      <PayloadSection label="Response Body" value={truncatedSse} truncated sizeBytes={null} />,
    );
    expect(screen.getByText(/truncated/i)).toBeInTheDocument();
  });

  it('leaves a whole body unmarked', () => {
    renderWithProviders(
      <PayloadSection label="Response Body" value='{"ok":true}' truncated={false} sizeBytes={11} />,
    );
    expect(screen.queryByText(/truncated/i)).toBeNull();
  });

  it('leaves a body unmarked when the API omits the flag entirely (older rows)', () => {
    renderWithProviders(<PayloadSection label="Response Body" value='{"ok":true}' />);
    expect(screen.queryByText(/truncated/i)).toBeNull();
  });
});
