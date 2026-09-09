import { describe, it, expect, vi } from 'vitest';
import { screen } from '@testing-library/react';
import userEvent from '@testing-library/user-event';
import { renderWithProviders } from '@/test/test-utils';
import { CorrelationSection } from './CorrelationSection';
import type { TrafficEvent } from '@/api/types';

// Three ids, three owners. The drawer once labelled the PRIMARY KEY "Request
// ID" and pivoted on it, which worked only while the gateway happened to set
// traffic_event.id from the caller's X-Nexus-Request-Id. It no longer does:
// the id is minted per row, so pivoting on it searches a value that exists in
// no other row and returns an empty list — silently, with no error to tell the
// operator their click was meaningless.
//
// The list filter keys on external_request_id, so the pivot must carry the
// REQUEST ID. The W3C trace id is a different value with a different owner —
// it names a slice that lives in the caller's tracing system, not here — so it
// gets no pivot at all.
const event = {
  id: 'evt-minted-uuid',
  externalRequestId: 'request-id-from-the-response-header',
  traceId: '4bf92f3577b34da6a3ce929d0e0e4736',
} as unknown as TrafficEvent;

describe('CorrelationSection pivot', () => {
  it('pivots on the request id, never on the row primary key', async () => {
    const onPivot = vi.fn();
    renderWithProviders(
      <CorrelationSection e={event} isGatewayTraffic onPivot={onPivot} />,
    );

    await userEvent.click(screen.getByTestId('corr-pivot-request-id'));

    expect(onPivot).toHaveBeenCalledTimes(1);
    expect(onPivot.mock.calls[0][0]).toMatchObject({
      requestId: 'request-id-from-the-response-header',
    });
    expect(JSON.stringify(onPivot.mock.calls[0][0])).not.toContain('evt-minted-uuid');
  });

  it('pivots the W3C trace id onto the traceId filter, not the request one', async () => {
    const onPivot = vi.fn();
    renderWithProviders(
      <CorrelationSection e={event} isGatewayTraffic onPivot={onPivot} />,
    );

    await userEvent.click(screen.getByTestId('corr-pivot-trace-id'));

    // The two ids answer different questions and land on different columns.
    // Sending the trace to the requestId filter would return nothing and look
    // like "this trace has no traffic" rather than "wrong filter".
    expect(onPivot).toHaveBeenCalledTimes(1);
    expect(onPivot.mock.calls[0][0]).toMatchObject({
      traceId: '4bf92f3577b34da6a3ce929d0e0e4736',
      requestId: '',
    });
  });

  it('shows the row key as Event ID and gives it no pivot', () => {
    const onPivot = vi.fn();
    renderWithProviders(
      <CorrelationSection e={event} isGatewayTraffic onPivot={onPivot} />,
    );

    // The row key is copyable but not pivotable: it matches exactly one row
    // and nothing else, so offering "show me this id's slice" would be a lie.
    expect(screen.getByTestId('corr-copy-event-id')).toBeInTheDocument();
    expect(screen.queryByTestId('corr-pivot-event-id')).toBeNull();
    expect(screen.getByText('evt-minted-uuid')).toBeInTheDocument();
  });
});
