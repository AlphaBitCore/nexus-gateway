/**
 * AlertDetailDrawer — tests for open/close lifecycle, detail rendering,
 * and Ack/Resolve action wiring.
 */
import { describe, it, expect, vi, beforeEach, afterEach } from 'vitest';
import { screen, waitFor } from '@testing-library/react';
import userEvent from '@testing-library/user-event';
import '@testing-library/jest-dom/vitest';

import { renderWithProviders } from '@/test/test-utils';
import { alertsApi } from '@/api/services/alerts/alerts';
import type { AlertDetailResponse } from '@/api/services/alerts/alerts';
import { AlertDetailDrawer } from './AlertDetailDrawer';

function sampleDetail(
  overrides: Partial<AlertDetailResponse> = {},
): AlertDetailResponse {
  return {
    id: 'alert-42',
    ruleId: 'thing.offline',
    sourceType: 'thing',
    targetKey: 'thing:node-gw-1',
    targetLabel: 'node-gw-1',
    severity: 'critical',
    state: 'firing',
    message: 'Thing node-gw-1 has not reported in 10 minutes',
    details: { lastSeenAt: '2026-04-22T09:50:00Z' },
    firedAt: '2026-04-22T10:00:00Z',
    lastSeenAt: '2026-04-22T10:05:00Z',
    duplicateCount: 2,
    acknowledgedBy: null,
    acknowledgedAt: null,
    resolvedAt: null,
    resolvedBy: null,
    resolvedReason: null,
    dispatches: [
      {
        id: 'disp-1',
        channelId: 'ch-webhook-1',
        channelName: 'Primary webhook',
        success: true,
        statusCode: 200,
        errorMsg: null,
        attemptedAt: '2026-04-22T10:00:05Z',
      },
    ],
    ...overrides,
  };
}

describe('AlertDetailDrawer', () => {
  beforeEach(() => {
    vi.spyOn(alertsApi, 'detail').mockResolvedValue(sampleDetail());
  });

  afterEach(() => {
    vi.restoreAllMocks();
  });

  it('renders nothing when alertId is null', () => {
    renderWithProviders(
      <AlertDetailDrawer alertId={null} visible={false} onClose={() => {}} />,
    );
    expect(screen.queryByRole('dialog')).not.toBeInTheDocument();
  });

  it('fetches and renders detail when opened', async () => {
    renderWithProviders(
      <AlertDetailDrawer
        alertId="alert-42"
        visible
        onClose={() => {}}
      />,
    );

    await waitFor(() => {
      expect(alertsApi.detail).toHaveBeenCalledWith('alert-42');
    });

    expect(await screen.findByText(/thing\.offline/)).toBeInTheDocument();
    expect(screen.getAllByText(/node-gw-1/).length).toBeGreaterThan(0);
    expect(screen.getByText(/Primary webhook/)).toBeInTheDocument();
    // JSON details block contains the `lastSeenAt` field
    expect(screen.getByText(/"lastSeenAt":/)).toBeInTheDocument();
  });

  it('calls onClose when the close button is clicked', async () => {
    const onClose = vi.fn();
    const user = userEvent.setup();

    renderWithProviders(
      <AlertDetailDrawer alertId="alert-42" visible onClose={onClose} />,
    );

    // Wait for the drawer to render
    await waitFor(() => {
      expect(screen.getByRole('dialog')).toBeInTheDocument();
    });

    const closeBtn = screen.getAllByRole('button').find((b) => b.textContent === '×');
    expect(closeBtn).toBeDefined();
    await user.click(closeBtn!);

    expect(onClose).toHaveBeenCalled();
  });

  it('calls ack API when Ack button is clicked on a firing alert', async () => {
    const ackSpy = vi
      .spyOn(alertsApi, 'ack')
      .mockResolvedValue(sampleDetail({ state: 'acknowledged' }));
    const user = userEvent.setup();

    renderWithProviders(
      <AlertDetailDrawer alertId="alert-42" visible onClose={() => {}} />,
    );

    const ackButton = await screen.findByRole('button', { name: /acknowledge/i });
    await user.click(ackButton);

    await waitFor(() => {
      expect(ackSpy).toHaveBeenCalledWith('alert-42');
    });
  });

  it('calls resolve API when Resolve button is clicked', async () => {
    const resolveSpy = vi
      .spyOn(alertsApi, 'resolve')
      .mockResolvedValue(sampleDetail({ state: 'resolved' }));
    const user = userEvent.setup();

    renderWithProviders(
      <AlertDetailDrawer alertId="alert-42" visible onClose={() => {}} />,
    );

    const resolveButton = await screen.findByRole('button', { name: /resolve/i });
    await user.click(resolveButton);

    await waitFor(() => {
      expect(resolveSpy).toHaveBeenCalledWith('alert-42');
    });
  });

  it('does not fetch when visible is false', () => {
    renderWithProviders(
      <AlertDetailDrawer
        alertId="alert-42"
        visible={false}
        onClose={() => {}}
      />,
    );
    // useApi skips when not visible
    expect(alertsApi.detail).not.toHaveBeenCalled();
  });
});
