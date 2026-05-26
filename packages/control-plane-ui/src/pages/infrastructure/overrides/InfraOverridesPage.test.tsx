/**
 * Integration test — InfraOverridesPage renders the global override registry,
 * the four summary tiles, type/TTL/stale/recent filters, free-text search,
 * and per-row View / Force resync / Clear actions.
 */
import { describe, it, expect, vi, afterEach } from 'vitest';
import { screen, waitFor, within } from '@testing-library/react';
import userEvent from '@testing-library/user-event';
import { Routes, Route } from 'react-router-dom';
import '@testing-library/jest-dom/vitest';

import { renderWithRouter, server, http, HttpResponse } from '@/test/test-utils';
import { hubApi } from '@/api/services/infrastructure/nodes/hub';
import InfraOverridesPage from './InfraOverridesPage';

// Computed against the real wall clock so userEvent's internal timers
// (used by `await user.type(...)`) keep advancing normally.
const NOW = Date.now();
const MIN_AGO = (mins: number) => new Date(NOW - mins * 60 * 1000).toISOString();
const MIN_AHEAD = (mins: number) => new Date(NOW + mins * 60 * 1000).toISOString();

const mockRows = [
  // Killswitch / break-glass row.
  {
    nodeId: 'node-proxy-1',
    nodeName: 'proxy-west-1',
    nodeType: 'compliance-proxy',
    configKey: 'killswitch',
    state: { engaged: true },
    templateVerAtSet: 5,
    currentTemplateVer: 5,
    stale: false,
    setBy: 'alice@nexus.ai',
    setAt: MIN_AGO(30),
    reason: 'break-glass: incident #421',
    expiresAt: MIN_AHEAD(45), // expires within 1h
    emergencyOverride: true,
  },
  // Stale routing override.
  {
    nodeId: 'node-gw-1',
    nodeName: 'gateway-east-1',
    nodeType: 'ai-gateway',
    configKey: 'routing_rules',
    state: { fallback: 'gpt-4o' },
    templateVerAtSet: 3,
    currentTemplateVer: 4,
    stale: true,
    setBy: 'carol@nexus.ai',
    setAt: MIN_AGO(60 * 48), // 2 days ago — not "recent"
    emergencyOverride: false,
  },
  // Permanent (no TTL) override.
  {
    nodeId: 'node-gw-2',
    nodeName: 'gateway-west-1',
    nodeType: 'ai-gateway',
    configKey: 'observability',
    state: { sample_rate: 0.5 },
    templateVerAtSet: 2,
    currentTemplateVer: 2,
    stale: false,
    setBy: 'bob@nexus.ai',
    setAt: MIN_AGO(2),
    emergencyOverride: false,
  },
  // Permanent override (older).
  {
    nodeId: 'node-agent-1',
    nodeName: 'agent-laptop-alice',
    nodeType: 'agent',
    configKey: 'hooks',
    state: {},
    templateVerAtSet: 1,
    currentTemplateVer: 1,
    stale: false,
    setBy: 'admin@nexus.ai',
    setAt: MIN_AGO(60 * 6),
    emergencyOverride: false,
  },
  // TTL'd override (still > 1h away).
  {
    nodeId: 'node-gw-3',
    nodeName: 'gateway-spare',
    nodeType: 'ai-gateway',
    configKey: 'credentials',
    state: { upstream: 'fallback' },
    templateVerAtSet: 7,
    currentTemplateVer: 7,
    stale: false,
    setBy: 'carol@nexus.ai',
    setAt: MIN_AGO(15),
    expiresAt: MIN_AHEAD(60 * 12),
    emergencyOverride: false,
  },
];

const SUMMARY = {
  totalNodes: 5,
  totalOverrides: 5,
  staleCount: 1,
  expiringSoonCount: 1,
};

function defaultListHandler() {
  server.use(
    http.get('/api/admin/nodes/overrides', () =>
      HttpResponse.json({ overrides: mockRows, total: mockRows.length, summary: SUMMARY }),
    ),
  );
}

function emptyListHandler() {
  server.use(
    http.get('/api/admin/nodes/overrides', () =>
      HttpResponse.json({
        overrides: [],
        total: 0,
        summary: { totalNodes: 0, totalOverrides: 0, staleCount: 0, expiringSoonCount: 0 },
      }),
    ),
  );
}

function renderPage() {
  return renderWithRouter(
    <Routes>
      <Route path="/infrastructure/overrides" element={<InfraOverridesPage />} />
      <Route path="/infrastructure/nodes/:id" element={<div data-testid="detail-page">Detail</div>} />
    </Routes>,
    { route: '/infrastructure/overrides' },
  );
}

afterEach(() => {
  vi.restoreAllMocks();
});

describe('InfraOverridesPage', () => {
  it('renders empty state when there are no overrides', async () => {
    emptyListHandler();
    renderPage();

    await waitFor(() => {
      expect(screen.getByText(/no active overrides/i)).toBeInTheDocument();
    });
  });

  it('renders all rows with the expected names', async () => {
    defaultListHandler();
    renderPage();

    await waitFor(() => {
      expect(screen.getByText('proxy-west-1')).toBeInTheDocument();
      expect(screen.getByText('gateway-east-1')).toBeInTheDocument();
      expect(screen.getByText('gateway-west-1')).toBeInTheDocument();
      expect(screen.getByText('agent-laptop-alice')).toBeInTheDocument();
      expect(screen.getByText('gateway-spare')).toBeInTheDocument();
    });
  });

  it('renders summary tiles with counters from the response', async () => {
    defaultListHandler();
    renderPage();

    await waitFor(() => {
      expect(screen.getByText(/5 node\(s\)/)).toBeInTheDocument();
      expect(screen.getByText(/5 key\(s\)/)).toBeInTheDocument();
      expect(screen.getByText(/1 stale/)).toBeInTheDocument();
      expect(screen.getByText(/1 expiring within 1h/)).toBeInTheDocument();
    });
  });

  it('type filter chip refetches with the chosen type', async () => {
    const calls: Array<string | null> = [];
    server.use(
      http.get('/api/admin/nodes/overrides', ({ request }) => {
        const url = new URL(request.url);
        calls.push(url.searchParams.get('type'));
        return HttpResponse.json({
          overrides: mockRows,
          total: mockRows.length,
          summary: SUMMARY,
        });
      }),
    );
    const user = userEvent.setup();
    renderPage();

    await waitFor(() => expect(screen.getByText('proxy-west-1')).toBeInTheDocument());

    const aiGwChip = screen.getByRole('button', { name: 'ai-gateway' });
    await user.click(aiGwChip);

    await waitFor(() => expect(calls).toContain('ai-gateway'));
  });

  it('stale filter chip refetches with stale=true', async () => {
    const calls: Array<string | null> = [];
    server.use(
      http.get('/api/admin/nodes/overrides', ({ request }) => {
        const url = new URL(request.url);
        calls.push(url.searchParams.get('stale'));
        return HttpResponse.json({
          overrides: mockRows,
          total: mockRows.length,
          summary: SUMMARY,
        });
      }),
    );
    const user = userEvent.setup();
    renderPage();

    await waitFor(() => expect(screen.getByText('proxy-west-1')).toBeInTheDocument());

    const staleChip = screen.getByRole('button', { name: 'Stale' });
    await user.click(staleChip);

    await waitFor(() => expect(calls).toContain('true'));
  });

  it('search input filters rows by node name client-side', async () => {
    defaultListHandler();
    const user = userEvent.setup();
    renderPage();

    await waitFor(() => expect(screen.getByText('proxy-west-1')).toBeInTheDocument());

    const search = screen.getByRole('searchbox');
    await user.type(search, 'proxy');

    await waitFor(() => {
      expect(screen.getByText('proxy-west-1')).toBeInTheDocument();
      expect(screen.queryByText('gateway-east-1')).not.toBeInTheDocument();
      expect(screen.queryByText('agent-laptop-alice')).not.toBeInTheDocument();
    });
  });

  it('clicking View navigates to /infrastructure/nodes/<id>#configuration', async () => {
    defaultListHandler();
    const user = userEvent.setup();
    renderPage();

    await waitFor(() => expect(screen.getByText('gateway-east-1')).toBeInTheDocument());

    const row = screen.getByText('gateway-east-1').closest('tr')!;
    const viewBtn = within(row).getByRole('button', { name: /^view$/i });
    await user.click(viewBtn);

    await waitFor(() => expect(screen.getByTestId('detail-page')).toBeInTheDocument());
  });

  it('clicking Clear calls hubApi.clearOverride and refetches', async () => {
    defaultListHandler();
    const clearSpy = vi.spyOn(hubApi, 'clearOverride').mockResolvedValue(undefined);
    const user = userEvent.setup();
    renderPage();

    await waitFor(() => expect(screen.getByText('gateway-east-1')).toBeInTheDocument());

    const row = screen.getByText('gateway-east-1').closest('tr')!;
    const clearBtn = within(row).getByRole('button', { name: /^clear$/i });
    await user.click(clearBtn);

    await waitFor(() => {
      expect(clearSpy).toHaveBeenCalledWith('node-gw-1', 'routing_rules');
    });
  });

  it('clicking Force resync calls hubApi.resyncNodeAll with configKey', async () => {
    defaultListHandler();
    const resyncSpy = vi.spyOn(hubApi, 'resyncNodeAll').mockResolvedValue({
      ok: true as const,
      nodeId: 'node-gw-1',
      configKey: 'routing_rules',
    });
    const user = userEvent.setup();
    renderPage();

    await waitFor(() => expect(screen.getByText('gateway-east-1')).toBeInTheDocument());

    const row = screen.getByText('gateway-east-1').closest('tr')!;
    const resyncBtn = within(row).getByRole('button', { name: /force resync/i });
    await user.click(resyncBtn);

    await waitFor(() => {
      expect(resyncSpy).toHaveBeenCalledWith('node-gw-1', { configKey: 'routing_rules' });
    });
  });

  it('killswitch row shows a break-glass status badge', async () => {
    defaultListHandler();
    renderPage();

    await waitFor(() => expect(screen.getByText('proxy-west-1')).toBeInTheDocument());

    const row = screen.getByText('proxy-west-1').closest('tr')!;
    expect(within(row).getByText(/break-glass/i)).toBeInTheDocument();
  });
});
