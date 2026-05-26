/**
 * AlertListPage — integration tests.
 *
 * Stubs `alertsApi` at the module boundary so we can assert the page calls
 * list/ack/resolve with the right arguments and rerenders on filter changes.
 */
import { describe, it, expect, vi, beforeEach, afterEach } from 'vitest';
import { screen, waitFor } from '@testing-library/react';
import userEvent from '@testing-library/user-event';
import '@testing-library/jest-dom/vitest';

import { renderWithRouter } from '@/test/test-utils';
import { alertsApi } from '@/api/services/alerts/alerts';
import type { Alert, AlertListResponse } from '@/api/services/alerts/alerts';
import { AlertListPage } from './AlertListPage';

function sampleAlert(overrides: Partial<Alert> = {}): Alert {
  return {
    id: 'alert-1',
    ruleId: 'quota.exceeded',
    sourceType: 'quota',
    targetKey: 'user:user-1',
    targetLabel: 'Alice Admin',
    severity: 'high',
    state: 'firing',
    message: 'Quota exceeded for Alice Admin',
    details: { percent: 105 },
    firedAt: '2026-04-22T10:00:00Z',
    lastSeenAt: '2026-04-22T10:05:00Z',
    duplicateCount: 1,
    acknowledgedBy: null,
    acknowledgedAt: null,
    resolvedAt: null,
    resolvedBy: null,
    resolvedReason: null,
    ...overrides,
  };
}

describe('AlertListPage', () => {
  beforeEach(() => {
    vi.spyOn(alertsApi, 'list').mockResolvedValue({
      alerts: [sampleAlert()],
      total: 1,
    } satisfies AlertListResponse);
  });

  afterEach(() => {
    vi.restoreAllMocks();
  });

  it('renders page heading and alert row', async () => {
    renderWithRouter(<AlertListPage />);

    await waitFor(() => {
      expect(screen.getByRole('heading', { level: 1 })).toBeInTheDocument();
    });
    expect(await screen.findByText('Alice Admin')).toBeInTheDocument();
    expect(screen.getByText('quota.exceeded')).toBeInTheDocument();
  });

  it('shows empty state when no alerts match', async () => {
    vi.mocked(alertsApi.list).mockResolvedValue({
      alerts: [],
      total: 0,
    });

    renderWithRouter(<AlertListPage />);

    await waitFor(() => {
      expect(screen.getByText(/no alerts/i)).toBeInTheDocument();
    });
  });

  it('refetches when the ruleId filter changes', async () => {
    const listSpy = vi.mocked(alertsApi.list);
    const user = userEvent.setup();

    renderWithRouter(<AlertListPage />);

    await waitFor(() => {
      expect(screen.getByText('Alice Admin')).toBeInTheDocument();
    });

    const callsBefore = listSpy.mock.calls.length;

    const ruleIdInput = screen.getByLabelText(/rule id/i);
    await user.type(ruleIdInput, 'quota.exceeded');

    // useDebouncedValue(300ms) → new key → list called with ruleId.
    await waitFor(
      () => {
        expect(listSpy.mock.calls.length).toBeGreaterThan(callsBefore);
        const lastCall = listSpy.mock.calls.at(-1);
        expect(lastCall?.[0]?.ruleId).toBe('quota.exceeded');
      },
      { timeout: 1500 },
    );
  });

  it('calls alertsApi.ack when the Ack button is clicked for a firing alert', async () => {
    const ackSpy = vi
      .spyOn(alertsApi, 'ack')
      .mockResolvedValue(sampleAlert({ state: 'acknowledged' }));
    const user = userEvent.setup();

    renderWithRouter(<AlertListPage />);

    const ackButton = await screen.findByRole('button', { name: /^ack$/i });
    await user.click(ackButton);

    await waitFor(() => {
      expect(ackSpy).toHaveBeenCalledWith('alert-1');
    });
  });

  it('hides the Ack button for a non-firing alert', async () => {
    vi.mocked(alertsApi.list).mockResolvedValue({
      alerts: [
        sampleAlert({
          id: 'alert-2',
          state: 'resolved',
          resolvedAt: '2026-04-22T11:00:00Z',
        }),
      ],
      total: 1,
    });

    renderWithRouter(<AlertListPage />);

    await waitFor(() => {
      expect(screen.getByText('Alice Admin')).toBeInTheDocument();
    });
    expect(screen.queryByRole('button', { name: /^ack$/i })).not.toBeInTheDocument();
    // Resolved alert hides the Resolve button too (state === 'resolved').
    expect(
      screen.queryByRole('button', { name: /^resolve$/i }),
    ).not.toBeInTheDocument();
  });
});
