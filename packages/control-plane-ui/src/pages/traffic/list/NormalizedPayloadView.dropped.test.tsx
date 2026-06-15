import { describe, it, expect } from 'vitest';
import { screen } from '@testing-library/react';
import { renderWithProviders } from '@/test/test-utils';
import { NormalizedPayloadView } from './NormalizedPayloadView';
import type { NormalizedPayload } from '@/api/types';

// The dropped-content placeholder tells three different stories and the
// banner must not conflate them: "operator-drop" (dropping is the
// configured policy), "redact-degraded" (the operator asked for redact;
// the stored copy could not be redacted precisely and was dropped
// instead), and rows with no recorded reason (written before the reason
// was stamped — neither story can be asserted). A degradation must never
// be presented as an operator decision, and a reason-less row must not
// claim operator intent.

const base: NormalizedPayload = {
  kind: 'ai-chat',
  normalizeVersion: 'v2.1',
  redacted: true,
  ruleIds: ['email', 'phone'],
};

describe('NormalizedPayloadView dropped banner', () => {
  it('renders the operator-drop story when the operator chose drop-content', () => {
    renderWithProviders(
      <NormalizedPayloadView
        payload={{ ...base, redactedReason: 'operator-drop' }}
        direction="request"
      />,
    );
    expect(screen.getByText('Content dropped per storage policy.')).toBeInTheDocument();
    expect(
      screen.getByText(/Operator set storageAction=drop-content/),
    ).toBeInTheDocument();
    expect(screen.getByText(/email, phone/)).toBeInTheDocument();
    // The degradation story must NOT appear.
    expect(
      screen.queryByText(/could not be safely applied/),
    ).not.toBeInTheDocument();
  });

  it('stays neutral for rows without a recorded reason', () => {
    renderWithProviders(<NormalizedPayloadView payload={base} direction="request" />);
    expect(screen.getByText('Content dropped per storage policy.')).toBeInTheDocument();
    expect(
      screen.getByText(/Content not stored per the storage policy/),
    ).toBeInTheDocument();
    // Neither operator intent nor a degradation may be asserted.
    expect(
      screen.queryByText(/Operator set storageAction=drop-content/),
    ).not.toBeInTheDocument();
    expect(
      screen.queryByText(/could not be safely applied/),
    ).not.toBeInTheDocument();
  });

  it('renders the degradation story with a localized cause and failed addresses', () => {
    renderWithProviders(
      <NormalizedPayloadView
        payload={{
          ...base,
          redactedReason: 'redact-degraded',
          redactedDetail: {
            cause: 'spans-unresolved',
            failedAddresses: ['messages.2.content.0', 'messages.3.content.1'],
          },
        }}
        direction="request"
      />,
    );
    expect(
      screen.getByText(
        'Content dropped — the redaction policy could not be safely applied to the stored copy.',
      ),
    ).toBeInTheDocument();
    // The cause renders as a readable phrase carrying the machine token,
    // the hint does not claim the redaction was applied to the live
    // request, and the operator is NOT blamed.
    expect(
      screen.getByText(
        /Cause: the redaction positions could not be resolved on the stored copy \(spans-unresolved\)/,
      ),
    ).toBeInTheDocument();
    expect(screen.queryByText(/applied to the live request/)).not.toBeInTheDocument();
    expect(
      screen.queryByText(/Operator set storageAction=drop-content/),
    ).not.toBeInTheDocument();
    // Failed content addresses render as a monospace list.
    expect(screen.getByText('messages.2.content.0')).toBeInTheDocument();
    expect(screen.getByText('messages.3.content.1')).toBeInTheDocument();
    // Rule attribution stays.
    expect(screen.getByText(/email, phone/)).toBeInTheDocument();
  });

  it('omits the address list when degradation carries no addresses', () => {
    renderWithProviders(
      <NormalizedPayloadView
        payload={{
          ...base,
          redactedReason: 'redact-degraded',
          redactedDetail: { cause: 'no-spans' },
        }}
        direction="request"
      />,
    );
    expect(
      screen.getByText(/Cause: the policy produced no redactable positions \(no-spans\)/),
    ).toBeInTheDocument();
    expect(screen.queryByText('Unresolved content addresses:')).not.toBeInTheDocument();
  });

  it('falls back to the raw cause token when no localization exists', () => {
    renderWithProviders(
      <NormalizedPayloadView
        payload={{
          ...base,
          redactedReason: 'redact-degraded',
          redactedDetail: { cause: 'some-future-cause' },
        }}
        direction="request"
      />,
    );
    expect(screen.getByText(/Cause: some-future-cause/)).toBeInTheDocument();
  });
});
