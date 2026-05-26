/**
 * Integration test — CredentialList: compact 8-column main row. Rows are
 * clickable and navigate to the credential detail page; the diagnostic
 * fields (rotation detail, weight, usage count, last success/failure,
 * created) live on the detail page only.
 */
import { describe, it, expect } from 'vitest';
import { screen, waitFor } from '@testing-library/react';
import userEvent from '@testing-library/user-event';
import { Routes, Route } from 'react-router-dom';

import { renderWithRouter, server, http, HttpResponse } from '@/test/test-utils';
import { CredentialListPage } from './CredentialList';

function seedCredentials(rows: Array<Record<string, unknown>>) {
  server.use(
    http.get('/api/admin/credentials', () =>
      HttpResponse.json({ data: rows, total: rows.length }),
    ),
  );
}

function renderList() {
  return renderWithRouter(
    <Routes>
      <Route path="/ai-gateway/credentials" element={<CredentialListPage />} />
      <Route path="/ai-gateway/credentials/:id" element={<div data-testid="detail-page">Detail</div>} />
    </Routes>,
    { route: '/ai-gateway/credentials' },
  );
}

describe('CredentialList — main row signals', () => {
  it('renders Overdue badge for expired credentials', async () => {
    seedCredentials([
      {
        id: 'c1',
        name: 'expired-key',
        providerId: 'p1',
        enabled: true,
        rotationState: 'none',
        expiresAt: '2020-01-01T00:00:00Z',
        totalUsageCount: 0,
        createdAt: '2024-01-01T00:00:00Z',
      },
    ]);

    renderList();
    await waitFor(() => {
      expect(screen.getByText('expired-key')).toBeDefined();
      expect(screen.getByText(/overdue/i)).toBeDefined();
    });
  });

  it('renders Expiring badge when rotation is pending and expiry is in the future', async () => {
    const futureDate = new Date(Date.now() + 7 * 24 * 60 * 60 * 1000).toISOString();
    seedCredentials([
      {
        id: 'c2',
        name: 'rotating-key',
        providerId: 'p1',
        enabled: true,
        rotationState: 'pending_rotation',
        expiresAt: futureDate,
        totalUsageCount: 0,
        createdAt: '2024-01-01T00:00:00Z',
      },
    ]);

    renderList();
    await waitFor(() => {
      expect(screen.getByText('rotating-key')).toBeDefined();
      expect(screen.getByText(/expiring/i)).toBeDefined();
    });
  });
});

describe('CredentialList — column reduction + row navigation', () => {
  const fullRow = {
    id: 'c3',
    name: 'sample-key',
    providerId: 'p1',
    enabled: true,
    rotationState: 'none',
    selectionWeight: 100,
    status: 'active',
    lastSuccessAt: '2026-05-10T00:00:00Z',
    lastFailureAt: '2026-05-09T00:00:00Z',
    lastFailureReason: 'auth_fail',
    totalUsageCount: 42,
    createdAt: '2024-01-01T00:00:00Z',
  };

  it('hides diagnostic fields from main row column headers', async () => {
    seedCredentials([fullRow]);
    renderList();
    await waitFor(() => {
      // Core columns that MUST be present
      expect(screen.getByRole('columnheader', { name: /^expires$/i })).toBeDefined();
      expect(screen.getByRole('columnheader', { name: /last used/i })).toBeDefined();
      expect(screen.getByRole('columnheader', { name: /pool status/i })).toBeDefined();
      // Separate Circuit + Health columns were unified into a
      // single "Reliability" column with hover-card popover.
      expect(screen.getByRole('columnheader', { name: /reliability/i })).toBeDefined();
    });
    // These six fields live on the detail page only; they MUST NOT be
    // column headers in the list.
    expect(screen.queryByRole('columnheader', { name: /rotation/i })).toBeNull();
    expect(screen.queryByRole('columnheader', { name: /^weight$/i })).toBeNull();
    expect(screen.queryByRole('columnheader', { name: /last success/i })).toBeNull();
    expect(screen.queryByRole('columnheader', { name: /last failure/i })).toBeNull();
    expect(screen.queryByRole('columnheader', { name: /usage count/i })).toBeNull();
    expect(screen.queryByRole('columnheader', { name: /^created$/i })).toBeNull();
  });

  it('clicking a row navigates to the credential detail page', async () => {
    const user = userEvent.setup();
    seedCredentials([fullRow]);
    renderList();

    await waitFor(() => expect(screen.getByText('sample-key')).toBeDefined());

    const row = screen.getByText('sample-key').closest('tr');
    expect(row).not.toBeNull();
    await user.click(row!);

    await waitFor(() => {
      expect(screen.getByTestId('detail-page')).toBeDefined();
    });
  });
});
