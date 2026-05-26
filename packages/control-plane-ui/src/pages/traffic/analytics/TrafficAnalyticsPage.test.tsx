/**
 * Integration test — TrafficAnalyticsPage renders heading with tabs
 * and shows Live Traffic tab content.
 */
import { describe, it, expect } from 'vitest';
import { screen, waitFor } from '@testing-library/react';
import userEvent from '@testing-library/user-event';
import { http, HttpResponse } from 'msw';

import { renderWithRouter, server } from '@/test/test-utils';
import { TrafficAnalyticsPage } from './TrafficAnalyticsPage';
import { getColumnsForSource } from '../list/TrafficTab';

function renderTrafficPage() {
  return renderWithRouter(<TrafficAnalyticsPage />);
}

describe('TrafficAnalyticsPage', () => {
  it('renders page heading with tabs', async () => {
    renderTrafficPage();
    await waitFor(() => {
      expect(screen.getByRole('heading', { level: 1 })).toBeDefined();
    });
    // Verify source-filter tab triggers are present
    expect(screen.getByRole('tab', { name: 'All' })).toBeDefined();
    expect(screen.getByRole('tab', { name: 'Virtual Key Traffic' })).toBeDefined();
    expect(screen.getByRole('tab', { name: 'Proxy' })).toBeDefined();
    expect(screen.getByRole('tab', { name: 'Agent' })).toBeDefined();
  });

  it('shows Live Traffic tab content', async () => {
    renderTrafficPage();
    // The traffic tab is selected by default and should render audit log content
    await waitFor(() => {
      // The traffic tab shows either audit data or helper text
      expect(screen.getByText(/live traffic/i)).toBeDefined();
    });
  });

  it('shows Path column in proxy list', async () => {
    const columns = getColumnsForSource('proxy', (k) => (k === 'pages:traffic.colPath' ? 'Path' : k));
    expect(columns.some((c) => c.key === 'path' && c.label === 'Path')).toBe(true);
  });

  it('re-fetches when Apply filters is clicked without changing filters', async () => {
    let hitCount = 0;
    server.use(
      http.get('/api/admin/traffic', () => {
        hitCount += 1;
        return HttpResponse.json({ data: [], total: 0 });
      }),
    );

    renderTrafficPage();
    const user = userEvent.setup();

    await waitFor(() => {
      expect(hitCount).toBeGreaterThanOrEqual(1);
    });

    await user.click(screen.getByRole('button', { name: /apply filters/i }));

    await waitFor(() => {
      expect(hitCount).toBeGreaterThanOrEqual(2);
    });
  });
});
