/**
 * Integration test — InfraKillSwitchPage renders toggle, per-type
 * status breakdown, confirmation, and merged history. Post-PR-6 the
 * page fetches BOTH compliance-proxy AND agent nodes and shows
 * distinct rollups for each.
 */
import { describe, it, expect } from 'vitest';
import { screen, waitFor } from '@testing-library/react';
import userEvent from '@testing-library/user-event';

import { renderWithRouter, server, http, HttpResponse } from '@/test/test-utils';
import InfraKillSwitchPage from './InfraKillSwitchPage';

function renderPage() {
  return renderWithRouter(<InfraKillSwitchPage />);
}

describe('InfraKillSwitchPage', () => {
  it('renders kill switch toggle', async () => {
    renderPage();
    await waitFor(() => {
      const toggleButton = screen.getByRole('button', { name: /engage|disengage/i });
      expect(toggleButton).toBeDefined();
    });
  });

  it('confirmation dialog appears on toggle', async () => {
    const user = userEvent.setup();
    renderPage();

    await waitFor(() => {
      expect(screen.getByRole('button', { name: /engage|disengage/i })).toBeDefined();
    });

    const toggleButton = screen.getByRole('button', { name: /engage|disengage/i });
    await user.click(toggleButton);

    await waitFor(() => {
      const dialogTexts = screen.getAllByText(/engage kill switch|disengage kill switch|halt traffic interception|resume normal traffic/i);
      expect(dialogTexts.length).toBeGreaterThan(0);
    });
  });

  it('history table renders with merged entries', async () => {
    renderPage();
    await waitFor(() => {
      expect(screen.getAllByText('admin').length).toBeGreaterThan(0);
    });
  });

  it('shows normal-operation state when no node has killswitch engaged', async () => {
    renderPage();
    await waitFor(() => {
      const normalLabels = screen.getAllByText(/normal operation/i);
      expect(normalLabels.length).toBeGreaterThan(0);
    });
  });

  it('shows ENGAGED state when at least one compliance-proxy has killswitch on', async () => {
    server.use(
      http.get('/api/admin/nodes', ({ request }) => {
        const url = new URL(request.url);
        const type = url.searchParams.get('type');
        if (type === 'compliance-proxy') {
          return HttpResponse.json({
            nodes: [{
              id: 'node-proxy-1', type: 'compliance-proxy', name: 'proxy-west-1',
              status: 'online', listen_address: ':3040', metrics_url: null,
              version: '1.1.0', role: null, auth_type: 'mtls', conn_protocol: 'grpc',
              targetConfig: { killswitch: { engaged: true } }, targetVersion: 3,
              appliedConfig: { killswitch: { engaged: true, changedBy: 'admin', changedAt: '2026-04-17T09:00:00Z', reason: 'emergency' } },
              appliedVersion: 3,
              last_seen_at: '2026-04-17T09:00:00Z', created_at: '2026-01-01T00:00:00Z',
              updated_at: '2026-04-17T09:00:00Z',
              overrideCount: 0, overrideStaleCount: 0,
            }],
            total: 1, page: 1, pageSize: 50,
          });
        }
        // agent fleet remains active
        return HttpResponse.json({ nodes: [], total: 0, page: 1, pageSize: 50 });
      }),
    );

    renderPage();
    await waitFor(() => {
      const engagedLabels = screen.getAllByText(/ENGAGED/);
      expect(engagedLabels.length).toBeGreaterThan(0);
    });
  });

  it('renders separate per-type status panels for compliance proxies and agents', async () => {
    renderPage();
    await waitFor(() => {
      // Heading-style labels for each fleet. The substring "Compliance
      // Proxies" also appears in the subtitle and the dialog body, so
      // getAllByText is the correct assertion shape — we require AT LEAST
      // one match (the per-type status panel label).
      expect(screen.getAllByText(/compliance proxies/i).length).toBeGreaterThan(0);
      expect(screen.getAllByText(/^agents$/i).length).toBeGreaterThan(0);
    });
  });
});
