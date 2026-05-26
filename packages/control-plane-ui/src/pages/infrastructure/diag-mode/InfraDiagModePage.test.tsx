/**
 * Integration tests — InfraDiagModePage.
 *
 * Covers active-windows rendering, the per-row disable confirm + dispatch
 * flow, the bulk-enable happy path, the bulk 207 partial-success result
 * panel, and the >500 disable-on-overflow guard.
 *
 * Preview is implemented client-side: tests stub `/api/admin/nodes` so the
 * filter resolution returns a known agent list, then assert the count.
 */
import { describe, it, expect } from 'vitest';
import { screen, waitFor, within } from '@testing-library/react';
import userEvent from '@testing-library/user-event';

import { renderWithRouter, server, http, HttpResponse } from '@/test/test-utils';
import InfraDiagModePage from './InfraDiagModePage';

function renderPage() {
  return renderWithRouter(<InfraDiagModePage />);
}

const mockWindow = {
  id: 'win-1',
  nodeId: 'agent-pod-1',
  nodeType: 'agent',
  startedAt: '2026-04-27T14:32:01Z',
  endedAt: new Date(Date.now() + 32 * 60 * 1000).toISOString(),
  setBy: 'super-admin',
  reason: 'investigation',
  createdAt: '2026-04-27T14:32:01Z',
};

describe('InfraDiagModePage', () => {
  it('TestAgentDiagModePage_RendersActiveWindows', async () => {
    server.use(
      http.get('/api/admin/agents/diagnostic-mode', () =>
        HttpResponse.json({ data: [mockWindow] }),
      ),
    );

    renderPage();

    await waitFor(() => {
      expect(screen.getByText('agent-pod-1')).toBeDefined();
      expect(screen.getByText('super-admin')).toBeDefined();
      expect(screen.getByText('investigation')).toBeDefined();
    });
  });

  it('TestAgentDiagModePage_DisableConfirmsAndCalls', async () => {
    server.use(
      http.get('/api/admin/agents/diagnostic-mode', () =>
        HttpResponse.json({ data: [mockWindow] }),
      ),
    );

    const calls: string[] = [];
    server.use(
      http.delete('/api/admin/agents/:nodeId/diagnostic-mode', ({ params }) => {
        calls.push(String(params.nodeId));
        return HttpResponse.json({ ok: true });
      }),
    );

    const user = userEvent.setup();
    renderPage();

    await screen.findByText('agent-pod-1');

    await user.click(screen.getByRole('button', { name: /^disable$/i }));

    // Confirm dialog
    const dialog = await screen.findByRole('alertdialog');
    await user.click(within(dialog).getByRole('button', { name: /^disable$/i }));

    await waitFor(() => {
      expect(calls).toContain('agent-pod-1');
    });
  });

  it('TestAgentDiagModePage_BulkEnableHappyPath', async () => {
    server.use(
      http.get('/api/admin/agents/diagnostic-mode', () =>
        HttpResponse.json({ data: [] }),
      ),
      http.get('/api/admin/nodes', () =>
        HttpResponse.json({
          nodes: [
            { id: 'agent-1', type: 'agent', version: '1.4.2', status: 'online' },
            { id: 'agent-2', type: 'agent', version: '1.4.2', status: 'online' },
            { id: 'agent-3', type: 'agent', version: '1.3.9', status: 'online' },
          ],
          total: 3,
          page: 1,
          pageSize: 1000,
        }),
      ),
    );

    let bulkBody: unknown = null;
    server.use(
      http.post('/api/admin/agents/diagnostic-mode/bulk', async ({ request }) => {
        bulkBody = await request.json();
        const body = bulkBody as { filter?: { nodeIds?: string[] } };
        const ids = body.filter?.nodeIds ?? ['agent-1', 'agent-2'];
        return HttpResponse.json({
          ok: true,
          total: ids.length,
          failed: 0,
          items: ids.map((id) => ({ nodeId: id, ok: true })),
        });
      }),
    );

    const user = userEvent.setup();
    renderPage();

    // Filter by version 1.4.2 → should match 2 agents.
    const versionInput = await screen.findByPlaceholderText(/e\.g\./i);
    await user.type(versionInput, '1.4.2');

    // Reason is required.
    const reasonInput = screen.getByPlaceholderText(/why/i);
    await user.type(reasonInput, 'rollout-debug');

    // Resolve preview.
    await user.click(screen.getByRole('button', { name: /resolve preview/i }));

    await waitFor(() => {
      expect(screen.getByText(/2\b/)).toBeDefined();
    });

    // Enable diag mode.
    await user.click(screen.getByRole('button', { name: /enable diag mode/i }));

    await waitFor(() => {
      expect(bulkBody).not.toBeNull();
      const body = bulkBody as {
        filter: { agentVersion?: string };
        reason: string;
      };
      expect(body.filter.agentVersion).toBe('1.4.2');
      expect(body.reason).toBe('rollout-debug');
    });

    // Success panel surfaces.
    await waitFor(() => {
      expect(screen.getByText(/diagnostic mode enabled/i)).toBeDefined();
    });
  });

  it('TestAgentDiagModePage_BulkEnablePartialSuccessShowsPanel', async () => {
    server.use(
      http.get('/api/admin/agents/diagnostic-mode', () =>
        HttpResponse.json({ data: [] }),
      ),
      http.get('/api/admin/nodes', () =>
        HttpResponse.json({
          nodes: [
            { id: 'agent-1', type: 'agent', version: '1.4.2', status: 'online' },
            { id: 'agent-2', type: 'agent', version: '1.4.2', status: 'online' },
            { id: 'agent-3', type: 'agent', version: '1.4.2', status: 'online' },
          ],
          total: 3,
          page: 1,
          pageSize: 1000,
        }),
      ),
    );

    server.use(
      http.post('/api/admin/agents/diagnostic-mode/bulk', () =>
        HttpResponse.json(
          {
            ok: false,
            total: 3,
            failed: 1,
            items: [
              { nodeId: 'agent-1', ok: true },
              { nodeId: 'agent-2', ok: true },
              { nodeId: 'agent-3', ok: false, error: 'thing offline' },
            ],
          },
          { status: 207 },
        ),
      ),
    );

    const user = userEvent.setup();
    renderPage();

    const versionInput = await screen.findByPlaceholderText(/e\.g\./i);
    await user.type(versionInput, '1.4.2');
    await user.type(screen.getByPlaceholderText(/why/i), 'investigation');
    await user.click(screen.getByRole('button', { name: /resolve preview/i }));

    await waitFor(() => {
      expect(screen.getByText(/3\b/)).toBeDefined();
    });

    await user.click(screen.getByRole('button', { name: /enable diag mode/i }));

    // Partial-success panel shows succeeded + failed counts and the failed
    // thingId / error pair.
    await waitFor(() => {
      expect(screen.getByText(/partial/i)).toBeDefined();
      expect(screen.getByText('agent-3')).toBeDefined();
      expect(screen.getByText(/thing offline/i)).toBeDefined();
    });
  });

  it('TestAgentDiagModePage_BulkButtonDisabledOverFiveHundred', async () => {
    server.use(
      http.get('/api/admin/agents/diagnostic-mode', () =>
        HttpResponse.json({ data: [] }),
      ),
      http.get('/api/admin/nodes', () => {
        const nodes = Array.from({ length: 600 }, (_, i) => ({
          id: `agent-${i + 1}`,
          type: 'agent',
          version: '2.0.0',
          status: 'online',
        }));
        return HttpResponse.json({ nodes, total: 600, page: 1, pageSize: 1000 });
      }),
    );

    const user = userEvent.setup();
    renderPage();

    const versionInput = await screen.findByPlaceholderText(/e\.g\./i);
    await user.type(versionInput, '2.0.0');
    await user.type(screen.getByPlaceholderText(/why/i), 'too-many');
    await user.click(screen.getByRole('button', { name: /resolve preview/i }));

    await waitFor(() => {
      // Over-limit warning surfaces.
      expect(screen.getByText(/exceeds.*500|over.*500|narrow/i)).toBeDefined();
    });

    const submit = screen.getByRole('button', { name: /enable diag mode/i });
    expect((submit as HTMLButtonElement).disabled).toBe(true);
  });
});
