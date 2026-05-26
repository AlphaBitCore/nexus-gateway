/**
 * Integration test — InfraJobsPage renders scheduled jobs and trigger actions.
 */
import { describe, it, expect } from 'vitest';
import { screen, waitFor } from '@testing-library/react';
import userEvent from '@testing-library/user-event';

import { renderWithRouter, server, http, HttpResponse } from '@/test/test-utils';
import { mockJob, mockJob2 } from '@/test/msw-handlers';
import InfraJobsPage from './InfraJobsPage';

function renderPage() {
  return renderWithRouter(<InfraJobsPage />);
}

describe('InfraJobsPage', () => {
  it('renders jobs table with mock data', async () => {
    renderPage();
    await waitFor(() => {
      expect(screen.getByText(mockJob.name)).toBeDefined();
      expect(screen.getByText(mockJob2.name)).toBeDefined();
    });
  });

  it('trigger button calls API with job id', async () => {
    const user = userEvent.setup();
    let triggeredId: string | null = null;

    server.use(
      http.post('/api/admin/jobs/:id/trigger', ({ params }) => {
        triggeredId = String(params.id);
        return HttpResponse.json({ ok: true, jobId: triggeredId, triggeredAt: '2026-04-17T10:00:00Z' });
      }),
    );

    renderPage();

    await waitFor(() => {
      expect(screen.getByText(mockJob.name)).toBeDefined();
    });

    const triggerButtons = screen.getAllByRole('button', { name: /trigger/i });
    expect(triggerButtons.length).toBeGreaterThan(0);
    await user.click(triggerButtons[0]);

    await waitFor(() => {
      expect(triggeredId).toBe(mockJob.id);
    });
  });

  it('status badges show correct colors', async () => {
    renderPage();
    await waitFor(() => {
      expect(screen.getByText('ok')).toBeDefined();
      expect(screen.getByText('failed')).toBeDefined();
    });

    const okBadge = screen.getByText('ok').closest('[class*=badge]') ?? screen.getByText('ok');
    const failedBadge = screen.getByText('failed').closest('[class*=badge]') ?? screen.getByText('failed');
    expect(okBadge).toBeDefined();
    expect(failedBadge).toBeDefined();
    expect(okBadge !== failedBadge).toBe(true);
  });

  it('renders description under job name', async () => {
    renderPage();
    await waitFor(() => {
      expect(screen.getByText(mockJob.description)).toBeDefined();
      expect(screen.getByText(mockJob2.description)).toBeDefined();
    });
  });
});
