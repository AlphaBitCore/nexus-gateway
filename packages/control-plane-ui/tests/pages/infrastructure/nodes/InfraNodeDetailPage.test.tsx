/**
 * Integration test — InfraNodeDetailPage renders the 5-tab IA introduced in
 * Node detail page (Overview · Configuration · Runtime · Metrics · Logs). The
 * legacy "Config Sync" + "Applied Config" tabs are merged into the single
 * Configuration tab, which now renders the merged template/override/applied
 * view fed by GET /api/admin/nodes/:id/applied-config.
 */
import { describe, it, expect, vi } from 'vitest';
import { screen, waitFor } from '@testing-library/react';
import userEvent from '@testing-library/user-event';
import { Routes, Route } from 'react-router-dom';

// Grant the per-tab IAM gates so the full node-detail IA renders; the gating
// logic itself is covered by the usePermission unit tests.
vi.mock('@/hooks/usePermission', () => ({ usePermission: () => true }));

import { renderWithRouter, server, http, HttpResponse } from '@/test/test-utils';
import { mockNode } from '@/test/msw-handlers';
import InfraNodeDetailPage from '../../../../src/pages/infrastructure/nodes/InfraNodeDetailPage';

function renderPage() {
  return renderWithRouter(
    <Routes>
      <Route path="/infrastructure/nodes" element={<div>Nodes list</div>} />
      <Route path="/infrastructure/nodes/:id" element={<InfraNodeDetailPage />} />
    </Routes>,
    { route: '/infrastructure/nodes/node-gw-1' },
  );
}

describe('InfraNodeDetailPage', () => {
  it('renders node detail with mock data', async () => {
    renderPage();
    await waitFor(() => {
      expect(screen.getAllByText(mockNode.name).length).toBeGreaterThan(0);
      expect(screen.getAllByText(mockNode.type).length).toBeGreaterThan(0);
    });
  });

  it('renders the IA for an ai-gateway node: 6 tabs (Login History is agent-only)', async () => {
    renderPage();
    await waitFor(() => {
      expect(screen.getByRole('tab', { name: /overview/i })).toBeInTheDocument();
    });

    // The Tab IA grew from the original 5 to a 7/8-shape: the old
    // Logs tab split into a dedicated Diagnostics tab plus a
    // Traffic tab; a Login History tab is rendered conditionally
    // when `node.type === 'agent'`; a Stats tab (per-Thing rollup
    // dashboard) is rendered for agent / ai-gateway / compliance-proxy.
    // mockNode is an ai-gateway so we expect 7 tabs (including Stats)
    // and Login History absent.
    expect(screen.getAllByRole('tab')).toHaveLength(7);
    expect(screen.getByRole('tab', { name: /^configuration$/i })).toBeInTheDocument();
    expect(screen.getByRole('tab', { name: /runtime/i })).toBeInTheDocument();
    expect(screen.getByRole('tab', { name: /metrics/i })).toBeInTheDocument();
    expect(screen.getByRole('tab', { name: /diagnostics/i })).toBeInTheDocument();
    expect(screen.getByRole('tab', { name: /traffic/i })).toBeInTheDocument();
    expect(screen.getByRole('tab', { name: /^stats$/i })).toBeInTheDocument();
    expect(screen.queryByRole('tab', { name: /login history/i })).toBeNull();

    // The merged tab list must NOT expose the legacy labels.
    expect(screen.queryByRole('tab', { name: /^config sync$/i })).toBeNull();
    expect(screen.queryByRole('tab', { name: /^applied config$/i })).toBeNull();
    expect(screen.queryByRole('tab', { name: /^logs$/i })).toBeNull();
  });

  it('renders Configuration tab content sourced from applied-config endpoint', async () => {
    const user = userEvent.setup();

    server.use(
      http.get('/api/admin/nodes/:id/applied-config', () =>
        HttpResponse.json({
          nodeId: mockNode.id,
          nodeType: mockNode.type,
          targetVersion: 12,
          appliedVersion: 12,
          configs: {
            routing: {
              targetConfig: { version: 3 },
              targetVersion: 12,
              appliedConfig: { version: 3 },
              appliedVersion: 12,
              inSync: true,
              templateState: { version: 3 },
              templateVer: 1,
            },
          },
        }),
      ),
    );

    renderPage();

    await waitFor(() => {
      expect(screen.getByRole('tab', { name: /^configuration$/i })).toBeInTheDocument();
    });
    await user.click(screen.getByRole('tab', { name: /^configuration$/i }));

    await waitFor(() => {
      expect(screen.getByText('routing')).toBeInTheDocument();
    });

    // Toolbar label confirms the new view (legacy tabs would surface "Re-sync
    // all", not "Force resync all").
    expect(screen.getByRole('button', { name: /Force resync all/i })).toBeInTheDocument();
  });
});
