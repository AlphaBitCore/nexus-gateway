/**
 * Integration test — InfraJobsPage renders scheduled jobs and trigger actions.
 */
import { describe, it, expect } from 'vitest';
import { screen, waitFor, within } from '@testing-library/react';
import userEvent from '@testing-library/user-event';

import { renderWithRouter, server, http, HttpResponse } from '@/test/test-utils';
import { mockJob, mockJob2 } from '@/test/msw-handlers';
import InfraJobsPage from '../../../../src/pages/infrastructure/jobs/InfraJobsPage';

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

  // A row this Hub never registered is still Enabled in the table and will
  // still never run: SyncDefinitions upserts only what the running build
  // registered and never deletes, so a row seeded for another mode outlives
  // it. Its Trigger / Enable answer 404, which reads as a missing record
  // rather than the deployment fact.
  it('marks a job this Hub did not register and does not offer its actions', async () => {
    server.use(
      http.get('/api/admin/jobs', () =>
        HttpResponse.json({
          jobs: [
            { ...mockJob, id: 'stale-for-another-mode', name: 'Stale Job', enabled: true, registered: false },
          ],
          total: 1, limit: 20, offset: 0,
        }),
      ),
    );

    renderPage();
    await waitFor(() => expect(screen.getByText('Stale Job')).toBeDefined());

    // Scope to the job's own row: "Enabled" also names the column header and
    // a filter option, so an unscoped query proves nothing about the badge.
    const row = screen.getByText('Stale Job').closest('tr');
    expect(row).not.toBeNull();
    expect(within(row!).getByText('Not scheduled')).toBeDefined();
    expect(within(row!).queryByText(/^Enabled$/)).toBeNull();

    for (const name of [/trigger/i, /enable|disable/i]) {
      const btns = screen.queryAllByRole('button', { name });
      for (const b of btns) {
        expect(b.hasAttribute('disabled')).toBe(true);
      }
    }
  });

  // `=== false`, not falsy: a Hub older than the field omits it, and treating
  // `undefined` as unregistered would make every job unactionable against it.
  it('leaves actions enabled when the Hub omits the registered field', async () => {
    server.use(
      http.get('/api/admin/jobs', () => {
        const { ...noField } = mockJob;
        return HttpResponse.json({ jobs: [noField], total: 1, limit: 20, offset: 0 });
      }),
    );

    renderPage();
    await waitFor(() => expect(screen.getByText(mockJob.name)).toBeDefined());

    expect(screen.queryByText('Not scheduled')).toBeNull();
    const triggerButtons = screen.getAllByRole('button', { name: /trigger/i });
    expect(triggerButtons.length).toBeGreaterThan(0);
    expect(triggerButtons.some((b) => !b.hasAttribute('disabled'))).toBe(true);
  });

  // The API COALESCEs last_status to '' for a job that has never run, and
  // `?? '—'` does not catch an empty string, so those rows rendered an empty
  // badge — indistinguishable from a rendering failure.
  it('says "Never run" instead of a blank badge for a job with no runs', async () => {
    server.use(
      http.get('/api/admin/jobs', () =>
        HttpResponse.json({
          jobs: [{ ...mockJob, id: 'fresh', name: 'Fresh Job', lastStatus: '', lastRun: null, runCount: 0 }],
          total: 1, limit: 20, offset: 0,
        }),
      ),
    );

    renderPage();
    await waitFor(() => expect(screen.getByText('Fresh Job')).toBeDefined());
    expect(screen.getByText('Never run')).toBeDefined();
  });
});
