/**
 * Integration test — DashboardPage renders with mock API data.
 */
import { describe, it, expect } from 'vitest';
import { screen, waitFor } from '@testing-library/react';

import { renderWithRouter, server, http, HttpResponse } from '@/test/test-utils';
import { DashboardPage } from './DashboardPage';

function renderDashboard() {
  return renderWithRouter(
    
      <DashboardPage />
    ,
  );
}

describe('DashboardPage', () => {
  it('renders page title', async () => {
    renderDashboard();
    await waitFor(() => {
      expect(screen.getByText('Dashboard')).toBeDefined();
    });
  });

  it('displays primary metrics after loading', async () => {
    renderDashboard();
    await waitFor(() => {
      expect(screen.getByRole('heading', { level: 1 })).toBeDefined();
    });
  });

  it('shows error banner when API fails', async () => {
    server.use(
      http.get('/api/admin/analytics/summary', () =>
        HttpResponse.json({ message: 'Server error' }, { status: 500 }),
      ),
    );

    renderDashboard();
    await waitFor(() => {
      expect(screen.getByText(/error/i)).toBeDefined();
    }, { timeout: 5000 });
  });

  it('hides top providers section when no provider data', async () => {
    server.use(
      http.get('/api/admin/analytics/by-provider', () =>
        HttpResponse.json({ data: [] }),
      ),
    );

    renderDashboard();
    await waitFor(() => {
      expect(screen.getByRole('heading', { level: 1 })).toBeDefined();
    });
    expect(screen.queryByText('Top Providers')).toBeNull();
  });

  it('prefers providerLabel over provider id in top providers table', async () => {
    server.use(
      http.get('/api/admin/analytics/by-provider', () =>
        HttpResponse.json({
          data: [{
            provider: 'prov_123',
            providerLabel: 'OpenAI',
            requestCount: 120,
            avgLatencyMs: 280,
            totalTokens: 54000,
            totalEstimatedCostUsd: 12.34,
          }],
        }),
      ),
    );

    renderDashboard();
    await waitFor(() => {
      expect(screen.getByText('Top Providers')).toBeDefined();
    });

    expect(screen.getByText('OpenAI')).toBeDefined();
    expect(screen.queryByText('prov_123')).toBeNull();
  });
});
