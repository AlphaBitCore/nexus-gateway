/**
 * Smoke tests for InterceptionDomainDetailPage — loads the domain, renders
 * the allowlist note + paths sub-table, and exposes Add / Edit / Delete
 * controls on each row. The MSW handler layer returns the canned domain for
 * the :id captured by the test route.
 */
import { describe, it, expect, vi } from 'vitest';
import { screen, waitFor } from '@testing-library/react';
import userEvent from '@testing-library/user-event';
import { MemoryRouter, Routes, Route } from 'react-router-dom';

import { renderWithProviders, server, http, HttpResponse } from '@/test/test-utils';
import { InterceptionDomainDetailPage } from './InterceptionDomainDetailPage';
import { mockInterceptionDomain, mockInterceptionPath } from '@/test/msw-handlers';

function renderDetail(id = 'id-1') {
  return renderWithProviders(
    <MemoryRouter initialEntries={[`/compliance/interception-domains/${id}`]}>
      <Routes>
        <Route
          path="/compliance/interception-domains/:id"
          element={<InterceptionDomainDetailPage />}
        />
      </Routes>
    </MemoryRouter>,
  );
}

describe('InterceptionDomainDetailPage', () => {
  it('renders the domain name, allowlist note, and nested path row', async () => {
    renderDetail();
    await waitFor(() => {
      expect(screen.getByRole('heading', { level: 1, name: mockInterceptionDomain.name }))
        .toBeDefined();
    });
    expect(screen.getAllByText(/allowlist/i).length).toBeGreaterThan(0);
    expect(screen.getByText(mockInterceptionPath.pathPattern[0])).toBeDefined();
  });

  it('opens the path edit dialog when Edit is clicked on a path row', async () => {
    const user = userEvent.setup();
    renderDetail();
    await waitFor(() => {
      expect(screen.getByText(mockInterceptionPath.pathPattern[0])).toBeDefined();
    });

    const editButtons = screen.getAllByRole('button', { name: /^edit$/i });
    await user.click(editButtons[0]);

    await waitFor(() => {
      expect(screen.getAllByText(/edit path/i).length).toBeGreaterThan(0);
    });
  });

  it('submits enabled=false when the summary switch is turned off', async () => {
    const user = userEvent.setup();
    const putSpy = vi.fn();
    server.use(
      http.put('/api/admin/interception-domains/:id', async ({ request }) => {
        putSpy(await request.json());
        return HttpResponse.json({ ...mockInterceptionDomain, enabled: false });
      }),
    );
    renderDetail();
    await waitFor(() => {
      expect(
        screen.getByRole('heading', { level: 1, name: mockInterceptionDomain.name }),
      ).toBeDefined();
    });

    const toggle = screen.getByRole('switch', {
      name: new RegExp(mockInterceptionDomain.name, 'i'),
    });
    expect(toggle.getAttribute('data-state')).toBe('checked');
    await user.click(toggle);

    await waitFor(() => {
      expect(putSpy).toHaveBeenCalledWith({ enabled: false });
    });
  });

  it('opens the add path dialog when Add path is clicked', async () => {
    const user = userEvent.setup();
    renderDetail();
    await waitFor(() => {
      expect(screen.getByText(mockInterceptionPath.pathPattern[0])).toBeDefined();
    });

    const addButtons = screen.getAllByRole('button', { name: /add path/i });
    await user.click(addButtons[0]);

    await waitFor(() => {
      expect(screen.getAllByText(/path patterns/i).length).toBeGreaterThan(0);
    });
  });
});
