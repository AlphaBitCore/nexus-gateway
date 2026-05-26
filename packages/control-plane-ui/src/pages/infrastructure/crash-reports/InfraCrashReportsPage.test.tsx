/**
 * Integration tests — InfraCrashReportsPage.
 */
import { describe, it, expect } from 'vitest';
import { screen, waitFor, within } from '@testing-library/react';
import userEvent from '@testing-library/user-event';

import { renderWithRouter, server, http, HttpResponse } from '@/test/test-utils';
import { mockCrashCohort1, mockCrashCohort2 } from '@/test/msw-handlers';
import InfraCrashReportsPage from './InfraCrashReportsPage';

function renderPage() {
  return renderWithRouter(<InfraCrashReportsPage />);
}

const cohortFatalEvent = {
  id: 'crash-1',
  nodeId: 'agent-darwin-1',
  nodeType: 'agent',
  occurredAt: '2026-04-27T09:30:00Z',
  receivedAt: '2026-04-27T09:30:01Z',
  level: 'fatal',
  eventType: 'agent.crash',
  source: 'agent',
  message: 'panic: nil deref in relay loop',
  messageHash: 'b1c4cafe1234',
  stackTrace: 'goroutine 1 [running]:\nmain.run()\n\t/x.go:42 +0x1c',
  attrs: { goroutines: 23 },
  repeatCount: 1,
  agentVersion: mockCrashCohort1.agentVersion,
  osInfo: { os: mockCrashCohort1.os, version: mockCrashCohort1.osVersion },
};

const otherCohortEvent = {
  ...cohortFatalEvent,
  id: 'crash-2',
  nodeId: 'agent-linux-1',
  message: 'panic: linux only',
  agentVersion: mockCrashCohort2.agentVersion,
  osInfo: { os: mockCrashCohort2.os, version: mockCrashCohort2.osVersion },
};

describe('InfraCrashReportsPage', () => {
  it('TestCrashReportsPage_RendersCohorts', async () => {
    renderPage();
    await waitFor(() => {
      expect(screen.getByText(mockCrashCohort1.agentVersion)).toBeDefined();
      expect(screen.getByText(mockCrashCohort2.agentVersion)).toBeDefined();
      expect(screen.getByText(mockCrashCohort1.os)).toBeDefined();
      expect(screen.getByText(mockCrashCohort2.os)).toBeDefined();
    });
  });

  it('TestCrashReportsPage_ExpandCohortShowsEvents', async () => {
    server.use(
      http.get('/api/admin/diag-events', () =>
        HttpResponse.json({ data: [cohortFatalEvent, otherCohortEvent], nextCursor: '' }),
      ),
    );

    const user = userEvent.setup();
    renderPage();

    await waitFor(() => {
      expect(screen.getByText(mockCrashCohort1.agentVersion)).toBeDefined();
    });

    const cohortRow = screen.getByText(mockCrashCohort1.agentVersion).closest('tr');
    expect(cohortRow).not.toBeNull();
    if (cohortRow) await user.click(cohortRow);

    await waitFor(() => {
      // Only the matching cohort's event surfaces in the drilldown — the
      // page filters the FATAL stream client-side by agentVersion + osInfo.
      expect(screen.getByText(cohortFatalEvent.message)).toBeDefined();
      expect(screen.queryByText(otherCohortEvent.message)).toBeNull();
    });
  });

  it('TestCrashReportsPage_StackTraceModal', async () => {
    server.use(
      http.get('/api/admin/diag-events', () =>
        HttpResponse.json({ data: [cohortFatalEvent], nextCursor: '' }),
      ),
    );

    const user = userEvent.setup();
    renderPage();

    await waitFor(() => {
      expect(screen.getByText(mockCrashCohort1.agentVersion)).toBeDefined();
    });

    const cohortRow = screen.getByText(mockCrashCohort1.agentVersion).closest('tr');
    if (cohortRow) await user.click(cohortRow);

    await waitFor(() => {
      expect(screen.getByText(cohortFatalEvent.message)).toBeDefined();
    });

    await user.click(screen.getByText(cohortFatalEvent.message));

    await waitFor(() => {
      const dialog = screen.getByRole('dialog');
      expect(within(dialog).getByText(/crash detail/i)).toBeDefined();
      expect(within(dialog).getByText(/main\.run/)).toBeDefined();
    });
  });
});
