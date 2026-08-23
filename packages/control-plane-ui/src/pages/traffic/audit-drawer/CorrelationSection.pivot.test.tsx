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
// no trace_id anywhere and returns an empty list — silently, with no error to
// tell the operator their click was meaningless.
//
// The list filter keys on trace_id, so the pivot must carry the TRACE.
const event = {
  id: 'evt-minted-uuid',
  traceId: 'trace-from-the-response-header',
  externalRequestId: 'client-own-id',
} as unknown as TrafficEvent;

describe('CorrelationSection pivot', () => {
  it('pivots on the trace, never on the row primary key', async () => {
    const onPivot = vi.fn();
    renderWithProviders(
      <CorrelationSection e={event} isGatewayTraffic onPivot={onPivot} />,
    );

    await userEvent.click(screen.getByTestId('corr-pivot-trace-id'));

    expect(onPivot).toHaveBeenCalledTimes(1);
    expect(onPivot.mock.calls[0][0]).toMatchObject({ requestId: 'trace-from-the-response-header' });
    expect(JSON.stringify(onPivot.mock.calls[0][0])).not.toContain('evt-minted-uuid');
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
