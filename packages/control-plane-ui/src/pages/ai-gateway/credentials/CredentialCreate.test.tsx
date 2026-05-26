import { describe, it, expect } from 'vitest';
import { screen, waitFor } from '@testing-library/react';
import { renderWithRouter } from '@/test/test-utils';
import { CredentialCreate } from './CredentialCreate';

describe('CredentialCreate', () => {
  it('renders the form heading', async () => {
    renderWithRouter(<CredentialCreate />);
    await waitFor(() => {
      expect(screen.getByRole('heading', { level: 1 })).toBeDefined();
    });
  });

  it('renders form inputs', async () => {
    renderWithRouter(<CredentialCreate />);
    await waitFor(() => {
      const inputs = screen.getAllByRole('textbox');
      expect(inputs.length).toBeGreaterThan(0);
    });
  });

  // Regression: the standalone Create page must expose an expiration-date
  // input. Previously only the Provider Detail inline form had it, leaving
  // users unable to set credential expiry from this page.
  it('exposes an Expiration Date input', async () => {
    const { container } = renderWithRouter(<CredentialCreate />);
    await waitFor(() => {
      const dateInputs = container.querySelectorAll('input[type="date"]');
      expect(dateInputs.length).toBeGreaterThan(0);
    });
  });
});
