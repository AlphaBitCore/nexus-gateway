/**
 * Smoke tests for InterceptionDomainsPage — list load, empty state, and
 * delete confirm flow. The full create modal interaction is covered by
 * detail-page tests (form is shared across both pages).
 */
import { describe, it, expect, vi } from 'vitest';
import { screen, waitFor } from '@testing-library/react';
import userEvent from '@testing-library/user-event';

import { renderWithRouter, server, http, HttpResponse } from '@/test/test-utils';
import { InterceptionDomainsPage } from './InterceptionDomainsPage';
import { mockInterceptionDomain } from '@/test/msw-handlers';

describe('InterceptionDomainsPage', () => {
  it('renders the page heading', async () => {
    renderWithRouter(<InterceptionDomainsPage />);
    await waitFor(() => {
      expect(screen.getByRole('heading', { level: 1 })).toBeDefined();
    });
  });

  it('displays domain data from the default MSW handler', async () => {
    renderWithRouter(<InterceptionDomainsPage />);
    await waitFor(() => {
      expect(screen.getByText(mockInterceptionDomain.name)).toBeDefined();
    });
    expect(screen.getByText(mockInterceptionDomain.hostPattern)).toBeDefined();
  });

  it('shows the empty state when the API returns zero rows', async () => {
    server.use(
      http.get('/api/admin/interception-domains', () =>
        HttpResponse.json({ data: [], total: 0 }),
      ),
    );
    renderWithRouter(<InterceptionDomainsPage />);
    await waitFor(() => {
      expect(screen.getAllByText(/no interception domains/i).length).toBeGreaterThan(0);
    });
  });

  it('opens the create dialog when the Create button is clicked', async () => {
    const user = userEvent.setup();
    renderWithRouter(<InterceptionDomainsPage />);
    await waitFor(() => {
      expect(screen.getByText(mockInterceptionDomain.name)).toBeDefined();
    });

    const createButtons = screen.getAllByRole('button', { name: /create/i });
    await user.click(createButtons[0]);

    await waitFor(() => {
      expect(screen.getAllByText(/create interception domain/i).length).toBeGreaterThan(0);
    });
  });

  it('submits enabled=false when the list row switch is turned off', async () => {
    const user = userEvent.setup();
    const putSpy = vi.fn();
    server.use(
      http.put('/api/admin/interception-domains/:id', async ({ request }) => {
        putSpy(await request.json());
        return HttpResponse.json({ ...mockInterceptionDomain, enabled: false });
      }),
    );
    renderWithRouter(<InterceptionDomainsPage />);
    await waitFor(() => {
      expect(screen.getByText(mockInterceptionDomain.name)).toBeDefined();
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

  it('opens the delete confirm dialog when Delete is clicked', async () => {
    const user = userEvent.setup();
    renderWithRouter(<InterceptionDomainsPage />);
    await waitFor(() => {
      expect(screen.getByText(mockInterceptionDomain.name)).toBeDefined();
    });

    const deleteButtons = screen.getAllByRole('button', { name: /delete/i });
    await user.click(deleteButtons[0]);

    await waitFor(() => {
      expect(
        screen.getAllByText(/delete interception domain\?/i).length,
      ).toBeGreaterThan(0);
    });
  });
});
