/**
 * Integration tests — InfraRecentErrorsPage.
 *
 * The page is now driven by a single endpoint (`/api/admin/diag-events/groups`)
 * augmented with `buckets` (5-min sparkline) and `silenced` flag. Hero tiles
 * are derived client-side from the same response. Silence/unsilence wires
 * through `/api/admin/diag-silences`.
 */
import { describe, it, expect, vi } from 'vitest';
import { screen, waitFor } from '@testing-library/react';
import userEvent from '@testing-library/user-event';

import { renderWithRouter, server, http, HttpResponse } from '@/test/test-utils';
import InfraRecentErrorsPage from '../../../../src/pages/infrastructure/recent-errors/InfraRecentErrorsPage';

function renderPage() {
  return renderWithRouter(<InfraRecentErrorsPage />);
}

const nowIso = new Date().toISOString();
const fiveMinAgoIso = new Date(Date.now() - 5 * 60_000).toISOString();
const halfHourAgoIso = new Date(Date.now() - 30 * 60_000).toISOString();

const errorGroup = {
  messageHash: 'hash-error-1',
  sampleMessage: 'panic: nil ptr in router',
  source: 'ai-gateway',
  affectedNodes: 1,
  totalOccurrences: 12,
  firstSeen: halfHourAgoIso,
  lastSeen: fiveMinAgoIso,
  maxLevel: 'error',
  buckets: [
    { ts: halfHourAgoIso, count: 4 },
    { ts: fiveMinAgoIso, count: 8 },
  ],
  silenced: false,
};

const silencedGroup = {
  messageHash: 'hash-silenced-1',
  sampleMessage: 'auto-updater disabled: no Ed25519 key',
  source: 'agent',
  affectedNodes: 2,
  totalOccurrences: 31,
  firstSeen: '2026-05-12T00:00:00Z',
  lastSeen: nowIso,
  maxLevel: 'error',
  buckets: [
    { ts: halfHourAgoIso, count: 5 },
    { ts: fiveMinAgoIso, count: 5 },
  ],
  silenced: true,
};

describe('InfraRecentErrorsPage', () => {
  it('renders hero tiles + issue list from the groups endpoint', async () => {
    server.use(
      http.get('/api/admin/diag-events/groups', () =>
        HttpResponse.json({ data: [errorGroup, silencedGroup] }),
      ),
      http.get('/api/admin/diag-silences', () => HttpResponse.json({ data: [] })),
    );

    renderPage();

    await waitFor(() => {
      expect(screen.getAllByText(errorGroup.sampleMessage).length).toBeGreaterThan(0);
    });
    // Silenced rows are hidden by default — only the active error renders.
    expect(screen.queryByText(silencedGroup.sampleMessage)).toBeNull();
  });

  it('shows silenced rows when the user toggles "Show silenced"', async () => {
    server.use(
      http.get('/api/admin/diag-events/groups', () =>
        HttpResponse.json({ data: [errorGroup, silencedGroup] }),
      ),
      http.get('/api/admin/diag-silences', () => HttpResponse.json({ data: [] })),
    );

    renderPage();
    const user = userEvent.setup();

    await waitFor(() => screen.getAllByText(errorGroup.sampleMessage)[0]);

    // The "Show silenced" toggle lives inline on the issue-list header; clicking
    // it flips hideSilenced=false so the silenced group becomes visible.
    await user.click(screen.getByText(/Show silenced|显示 silenced|Mostrar silenciados/));

    await waitFor(() => {
      expect(screen.getByText(silencedGroup.sampleMessage)).toBeDefined();
    });
  });

  it('posts to /diag-silences when an operator clicks "Silence 1h"', async () => {
    const createSpy = vi.fn(() => HttpResponse.json({
      silence: {
        id: 'sil-1',
        messageHash: errorGroup.messageHash,
        level: errorGroup.maxLevel,
        silencedBy: 'admin',
        silencedAt: nowIso,
        expiresAt: new Date(Date.now() + 3600_000).toISOString(),
      },
    }));

    server.use(
      http.get('/api/admin/diag-events/groups', () =>
        HttpResponse.json({ data: [errorGroup] }),
      ),
      http.get('/api/admin/diag-silences', () => HttpResponse.json({ data: [] })),
      http.post('/api/admin/diag-silences', createSpy),
    );

    renderPage();
    const user = userEvent.setup();

    await waitFor(() => screen.getAllByText(errorGroup.sampleMessage)[0]);
    await user.click(screen.getByText(/Silence 1h|Silenciar 1h/));

    await waitFor(() => {
      expect(createSpy).toHaveBeenCalled();
    });
  });

  it('clicking an issue opens the detail drawer with its metadata', async () => {
    server.use(
      http.get('/api/admin/diag-events/groups', () => HttpResponse.json({ data: [errorGroup] })),
      http.get('/api/admin/diag-silences', () => HttpResponse.json({ data: [] })),
      http.get('/api/admin/diag-events', () => HttpResponse.json({ data: [], nextCursor: null })),
    );

    renderPage();
    const user = userEvent.setup();

    await waitFor(() => screen.getAllByText(errorGroup.sampleMessage)[0]);
    // the sampleMessage appears in the hero tile AND the issue row; the row
    // (last occurrence) is the clickable one that opens the drawer.
    const occurrences = screen.getAllByText(errorGroup.sampleMessage);
    await user.click(occurrences[occurrences.length - 1]);

    // the drawer surfaces the group's messageHash in its metadata list
    await waitFor(() => expect(screen.getByText('hash-error-1')).toBeInTheDocument());
  });
});
