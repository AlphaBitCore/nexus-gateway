/**
 * Integration test — InfraNodesPage renders node list with type filter tabs,
 * the per-row Overrides column, and the Has-overrides toolbar toggle.
 */
import { describe, it, expect } from 'vitest';
import { screen, waitFor } from '@testing-library/react';
import userEvent from '@testing-library/user-event';
import { Routes, Route } from 'react-router-dom';

import { renderWithRouter, server, http, HttpResponse } from '@/test/test-utils';
import InfraNodesPage from '../../../../src/pages/infrastructure/nodes/InfraNodesPage';

function renderPage() {
  return renderWithRouter(
    <Routes>
      <Route path="/infrastructure/nodes" element={<InfraNodesPage />} />
      <Route path="/infrastructure/nodes/:id" element={<div data-testid="detail-page">Detail</div>} />
    </Routes>,
    { route: '/infrastructure/nodes' },
  );
}

describe('InfraNodesPage', () => {
  it('renders loading state initially', async () => {
    server.use(
      http.get('/api/admin/nodes', async () => {
        await new Promise((r) => setTimeout(r, 2000));
        return HttpResponse.json({ nodes: [], total: 0, page: 1, pageSize: 50 });
      }),
    );
    renderPage();
    await waitFor(() => {
      const heading = screen.queryByText(/nodes/i);
      expect(heading === null || document.querySelector('[class*=skeleton]') !== null).toBe(true);
    });
  });

  it('renders table with mock nodes data', async () => {
    renderPage();
    await waitFor(() => {
      expect(screen.getByText('gateway-east-1')).toBeDefined();
      expect(screen.getByText('proxy-west-1')).toBeDefined();
      expect(screen.getByText('agent-laptop-alice')).toBeDefined();
    });
  });

  it('type filter tabs work', async () => {
    const user = userEvent.setup();
    renderPage();

    await waitFor(() => {
      expect(screen.getByText('gateway-east-1')).toBeDefined();
    });

    const aiGatewayTab = screen.getByRole('tab', { name: 'ai-gateway' });
    expect(aiGatewayTab).toBeDefined();

    server.use(
      http.get('/api/admin/nodes', ({ request }) => {
        const url = new URL(request.url);
        const type = url.searchParams.get('type');
        if (type === 'ai-gateway') {
          return HttpResponse.json({
            nodes: [{
              id: 'node-gw-1', type: 'ai-gateway', name: 'gateway-east-1',
              status: 'online', listen_address: ':3050', metrics_url: null,
              version: '1.2.0', role: 'primary', auth_type: 'mtls', conn_protocol: 'grpc',
              targetConfig: {}, targetVersion: 3, appliedConfig: {}, appliedVersion: 3,
              last_seen_at: '2026-04-17T08:00:00Z', created_at: '2026-01-01T00:00:00Z',
              updated_at: '2026-04-17T08:00:00Z',
              overrideCount: 0, overrideStaleCount: 0,
            }],
            total: 1, page: 1, pageSize: 50,
          });
        }
        return HttpResponse.json({ nodes: [], total: 0, page: 1, pageSize: 50 });
      }),
    );

    await user.click(aiGatewayTab);

    await waitFor(() => {
      expect(screen.getByText('gateway-east-1')).toBeDefined();
    });
  });

  it('clicking a row navigates to detail page', async () => {
    const user = userEvent.setup();
    renderPage();

    await waitFor(() => {
      expect(screen.getByText('gateway-east-1')).toBeDefined();
    });

    const row = screen.getByText('gateway-east-1').closest('tr');
    expect(row).not.toBeNull();
    await user.click(row!);

    await waitFor(() => {
      expect(screen.getByTestId('detail-page')).toBeDefined();
    });
  });

  // (Overrides column tests removed — the column was dropped in the
  // thing-identity refactor; the Has-overrides toggle
  // and detail-page Overrides view still cover the underlying feature.)

  it('Has-overrides toggle sends hasOverrides=true and refetches', async () => {
    const user = userEvent.setup();
    const calls: Array<{ hasOverrides: string | null; type: string | null }> = [];

    server.use(
      http.get('/api/admin/nodes', ({ request }) => {
        const url = new URL(request.url);
        calls.push({
          hasOverrides: url.searchParams.get('hasOverrides'),
          type: url.searchParams.get('type'),
        });
        const filter = url.searchParams.get('hasOverrides') === 'true';
        return HttpResponse.json({
          nodes: filter ? [{
            id: 'node-gw-1', type: 'ai-gateway', name: 'gateway-east-1',
            status: 'online', listen_address: ':3050', metrics_url: null,
            version: '1.2.0', role: 'primary', auth_type: 'mtls', conn_protocol: 'grpc',
            targetConfig: {}, targetVersion: 3, appliedConfig: {}, appliedVersion: 3,
            last_seen_at: '2026-04-17T08:00:00Z', created_at: '2026-01-01T00:00:00Z',
            updated_at: '2026-04-17T08:00:00Z',
            overrideCount: 5, overrideStaleCount: 0,
          }] : [
            { id: 'a', type: 'ai-gateway', name: 'a-node', status: 'online',
              listen_address: ':3050', metrics_url: null, version: '1', role: null,
              auth_type: 'mtls', conn_protocol: 'grpc',
              targetConfig: {}, targetVersion: 1, appliedConfig: {}, appliedVersion: 1,
              last_seen_at: null, created_at: '2026-01-01T00:00:00Z',
              updated_at: '2026-01-01T00:00:00Z',
              overrideCount: 0, overrideStaleCount: 0 },
            { id: 'b', type: 'compliance-proxy', name: 'b-node', status: 'online',
              listen_address: ':3040', metrics_url: null, version: '1', role: null,
              auth_type: 'mtls', conn_protocol: 'grpc',
              targetConfig: {}, targetVersion: 1, appliedConfig: {}, appliedVersion: 1,
              last_seen_at: null, created_at: '2026-01-01T00:00:00Z',
              updated_at: '2026-01-01T00:00:00Z',
              overrideCount: 0, overrideStaleCount: 0 },
          ],
          total: filter ? 1 : 2,
          page: 1,
          pageSize: 50,
        });
      }),
    );

    renderPage();

    // Initial fetch: no hasOverrides param.
    await waitFor(() => {
      expect(screen.getByText('a-node')).toBeDefined();
    });
    const initialCall = calls[0];
    expect(initialCall.hasOverrides).toBeNull();

    const toggle = screen.getByRole('button', { name: /has overrides/i });
    expect(toggle).toBeDefined();
    expect(toggle.getAttribute('aria-pressed')).toBe('false');

    await user.click(toggle);

    // After toggle: refetch with hasOverrides=true.
    await waitFor(() => {
      expect(screen.getByText('gateway-east-1')).toBeDefined();
    });
    // Re-query the toggle in case React replaced the node.
    const toggleAfter = screen.getByRole('button', { name: /has overrides/i });
    expect(toggleAfter.getAttribute('aria-pressed')).toBe('true');
    const filteredCall = calls.at(-1)!;
    expect(filteredCall.hasOverrides).toBe('true');
  });

  it('rows with hasKillswitchBypass=true carry the data-killswitch-bypass attribute', async () => {
    server.use(
      http.get('/api/admin/nodes', () =>
        HttpResponse.json({
          nodes: [
            {
              id: 'node-gw-bypass', type: 'ai-gateway', name: 'gateway-bypass',
              status: 'online', listen_address: ':3050', metrics_url: null,
              version: '1.2.0', role: 'primary', auth_type: 'mtls', conn_protocol: 'grpc',
              targetConfig: {}, targetVersion: 3, appliedConfig: {}, appliedVersion: 3,
              last_seen_at: '2026-04-17T08:00:00Z', created_at: '2026-01-01T00:00:00Z',
              updated_at: '2026-04-17T08:00:00Z',
              overrideCount: 1, overrideStaleCount: 0,
              hasKillswitchBypass: true,
            },
            {
              id: 'node-gw-clean', type: 'ai-gateway', name: 'gateway-clean',
              status: 'online', listen_address: ':3050', metrics_url: null,
              version: '1.2.0', role: 'primary', auth_type: 'mtls', conn_protocol: 'grpc',
              targetConfig: {}, targetVersion: 3, appliedConfig: {}, appliedVersion: 3,
              last_seen_at: '2026-04-17T08:00:00Z', created_at: '2026-01-01T00:00:00Z',
              updated_at: '2026-04-17T08:00:00Z',
              overrideCount: 0, overrideStaleCount: 0,
              hasKillswitchBypass: false,
            },
          ],
          total: 2, page: 1, pageSize: 50,
        }),
      ),
    );

    renderPage();

    await waitFor(() => {
      expect(screen.getByText('gateway-bypass')).toBeDefined();
      expect(screen.getByText('gateway-clean')).toBeDefined();
    });

    const bypassRow = screen.getByText('gateway-bypass').closest('tr')!;
    expect(bypassRow).not.toBeNull();
    expect(bypassRow.getAttribute('data-killswitch-bypass')).toBe('true');

    // Sibling row without the flag must not carry the attribute (so CSS
    // matchers keyed on `[data-killswitch-bypass="true"]` only paint the
    // intended row).
    const cleanRow = screen.getByText('gateway-clean').closest('tr')!;
    expect(cleanRow.getAttribute('data-killswitch-bypass')).toBeNull();
  });
});
