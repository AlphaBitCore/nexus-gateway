/**
 * Integration test — HookList renders hook data and supports interactions.
 */
import { describe, it, expect } from 'vitest';
import { screen, waitFor } from '@testing-library/react';

import { renderWithRouter, server, http, HttpResponse } from '@/test/test-utils';
import { ConfigHooksPage } from './HookList';
import { mockHook } from '@/test/msw-handlers';

function renderHookList() {
  return renderWithRouter(<ConfigHooksPage />);
}

describe('HookList', () => {
  it('renders page heading', async () => {
    renderHookList();
    await waitFor(() => {
      expect(screen.getByRole('heading', { level: 1 })).toBeDefined();
    });
  });

  it('displays hook data in table', async () => {
    renderHookList();
    await waitFor(() => {
      expect(screen.getByText(mockHook.name)).toBeDefined();
    });
  });

  it('shows empty state when no hooks', async () => {
    server.use(
      http.get('/api/admin/hooks', () =>
        HttpResponse.json({ data: [], total: 0 }),
      ),
    );

    renderHookList();
    await waitFor(() => {
      expect(screen.getAllByText(/no hooks/i).length).toBeGreaterThan(0);
    });
  });
});
