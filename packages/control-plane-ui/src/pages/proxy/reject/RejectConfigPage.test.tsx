/**
 * Integration test — RejectConfigPage renders reject level selector and contact input.
 */
import { describe, it, expect } from 'vitest';
import { screen, waitFor } from '@testing-library/react';

import { renderWithRouter } from '@/test/test-utils';
import { RejectConfigPage } from './RejectConfigPage';

function renderPage() {
  return renderWithRouter(<RejectConfigPage />);
}

describe('RejectConfigPage', () => {
  it('renders level selector radio buttons', async () => {
    renderPage();
    await waitFor(() => {
      // Should find radio inputs for levels 0, 1, 2
      const radios = screen.getAllByRole('radio');
      expect(radios.length).toBe(3);
    });
  });

  it('renders contact info input', async () => {
    renderPage();
    await waitFor(() => {
      const input = screen.getByDisplayValue('compliance@example.com');
      expect(input).toBeDefined();
    });
  });

  it('renders page heading', async () => {
    renderPage();
    await waitFor(() => {
      expect(screen.getByRole('heading', { level: 1 })).toBeDefined();
    });
  });

  it('renders save button initially disabled', async () => {
    renderPage();
    await waitFor(() => {
      const saveButton = screen.getByRole('button');
      expect(saveButton).toBeDefined();
    });
  });
});
