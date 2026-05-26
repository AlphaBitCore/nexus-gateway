/**
 * Integration test — InfraConfigSyncPage renders change history and out-of-sync monitor tabs.
 */
import { describe, it, expect } from 'vitest';
import { screen, waitFor } from '@testing-library/react';
import userEvent from '@testing-library/user-event';

import { renderWithRouter, server, http, HttpResponse } from '@/test/test-utils';
import InfraConfigSyncPage from './InfraConfigSyncPage';

function renderPage() {
  return renderWithRouter(<InfraConfigSyncPage />);
}

describe('InfraConfigSyncPage', () => {
  it('renders two tabs (Change History, Out-of-Sync Monitor)', async () => {
    renderPage();
    await waitFor(() => {
      expect(screen.getByRole('tab', { name: /change history/i })).toBeDefined();
      expect(screen.getByRole('tab', { name: /out-of-sync monitor/i })).toBeDefined();
    });
  });

  it('change history table renders with mock data', async () => {
    renderPage();
    await waitFor(() => {
      expect(screen.getByText('admin')).toBeDefined();
      expect(screen.getByText('killswitch')).toBeDefined();
      expect(screen.getByText('update')).toBeDefined();
    });
  });

  it('out-of-sync monitor shows "all in sync" when empty', async () => {
    const user = userEvent.setup();
    renderPage();

    await waitFor(() => {
      expect(screen.getByRole('tab', { name: /out-of-sync monitor/i })).toBeDefined();
    });

    await user.click(screen.getByRole('tab', { name: /out-of-sync monitor/i }));

    await waitFor(() => {
      const syncMessages = screen.getAllByText(/all.*in sync|in sync/i);
      expect(syncMessages.length).toBeGreaterThan(0);
    });
  });

  it('out-of-sync monitor shows items when they exist', async () => {
    const user = userEvent.setup();
    server.use(
      http.get('/api/admin/config-sync/out-of-sync', () =>
        HttpResponse.json({
          outOfSync: [{
            nodeId: 'node-proxy-1',
            nodeType: 'compliance-proxy',
            name: 'proxy-west-1',
            outOfSyncKeys: ['killswitch'],
            lastSeen: '2026-04-17T08:00:00Z',
          }],
          total: 1,
        }),
      ),
    );

    renderPage();

    await waitFor(() => {
      expect(screen.getByRole('tab', { name: /out-of-sync monitor/i })).toBeDefined();
    });

    await user.click(screen.getByRole('tab', { name: /out-of-sync monitor/i }));

    await waitFor(() => {
      expect(screen.getByText('proxy-west-1')).toBeDefined();
    });
  });
});
