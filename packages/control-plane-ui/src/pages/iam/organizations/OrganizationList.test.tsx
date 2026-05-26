/**
 * Integration test — OrganizationList renders org data and supports interactions.
 */
import { describe, it, expect } from 'vitest';
import { screen, waitFor } from '@testing-library/react';

import { renderWithRouter, server, http, HttpResponse } from '@/test/test-utils';
import { OrganizationList } from './OrganizationList';
import { mockOrganization } from '@/test/msw-handlers';

function renderOrgList() {
  return renderWithRouter(<OrganizationList />);
}

describe('OrganizationList', () => {
  it('renders page heading', async () => {
    renderOrgList();
    await waitFor(() => {
      expect(screen.getByRole('heading', { level: 1 })).toBeDefined();
    });
  });

  it('displays organization data in table', async () => {
    renderOrgList();
    await waitFor(() => {
      expect(screen.getByText(mockOrganization.name)).toBeDefined();
    });
  });

  it('shows empty state when no organizations', async () => {
    server.use(
      http.get('/api/admin/organizations/tree', () =>
        HttpResponse.json({ data: [] }),
      ),
    );

    renderOrgList();
    await waitFor(() => {
      expect(screen.getByText(/no organizations/i)).toBeDefined();
    });
  });
});
