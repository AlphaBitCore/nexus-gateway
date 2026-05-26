/**
 * Integration test — StatusPage renders heading and status information.
 */
import { describe, it, expect } from 'vitest';
import { screen, waitFor } from '@testing-library/react';

import { renderWithRouter } from '@/test/test-utils';
import { StatusPage } from './StatusPage';

function renderStatusPage() {
  return renderWithRouter(<StatusPage />);
}

describe('StatusPage', () => {
  it('renders page heading', async () => {
    renderStatusPage();
    await waitFor(() => {
      expect(screen.getByRole('heading', { level: 1 })).toBeDefined();
    });
  });

  it('shows status information', async () => {
    renderStatusPage();
    await waitFor(() => {
      // Should display infrastructure section with database/redis status
      expect(screen.getByText(/overview/i)).toBeDefined();
    });
  });
});
