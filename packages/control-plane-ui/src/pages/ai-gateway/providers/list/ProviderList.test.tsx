/**
 * Integration test — ProviderList renders provider data and supports interactions.
 */
import { describe, it, expect } from 'vitest';
import { screen, waitFor } from '@testing-library/react';

import { renderWithRouter, server, http, HttpResponse } from '@/test/test-utils';
import { ConfigProvidersPage } from './ProviderList';
import { mockProvider } from '@/test/msw-handlers';

function renderProviderList() {
  return renderWithRouter(
    
      <ConfigProvidersPage />
    ,
  );
}

describe('ProviderList', () => {
  it('renders page title', async () => {
    renderProviderList();
    await waitFor(() => {
      expect(screen.getByRole("heading", { level: 1 })).toBeDefined();
    });
  });

  it('displays provider data in table', async () => {
    renderProviderList();
    await waitFor(() => {
      // The Type column also displays "OpenAI" because the i18n key
      // `pages:providers.adapterOption_openai` translates the raw
      // `adapterType: 'openai'` to its pretty label "OpenAI" — so the
      // string appears in both the Display Name and Type cells.
      // Asserting *some* match is enough to prove the table rendered.
      expect(screen.getAllByText(mockProvider.displayName).length).toBeGreaterThan(0);
    });
  });

  it('shows empty state when no providers', async () => {
    server.use(
      http.get('/api/admin/providers', () =>
        HttpResponse.json({ data: [], total: 0 }),
      ),
    );

    renderProviderList();
    await waitFor(() => {
      expect(screen.getAllByText(/no providers/i).length).toBeGreaterThan(0);
    });
  });

  // Regression: the Type column must render the `adapterType` field from
  // the API response, not the legacy `type` field. If the column key ever
  // drifts back to `type`, these cells render empty and this test fails.
  it('renders adapterType values in the Type column', async () => {
    server.use(
      http.get('/api/admin/providers', () =>
        HttpResponse.json({
          data: [
            {
              id: 'p1',
              name: 'alpha',
              displayName: 'Alpha',
              adapterType: 'anthropic',
              baseUrl: 'https://example.com/alpha',
              enabled: true,
              createdAt: '2026-04-01T00:00:00Z',
            },
            {
              id: 'p2',
              name: 'beta',
              displayName: 'Beta',
              adapterType: 'gemini',
              baseUrl: 'https://example.com/beta',
              enabled: true,
              createdAt: '2026-04-01T00:00:00Z',
            },
          ],
          total: 2,
        }),
      ),
    );

    renderProviderList();
    await waitFor(() => {
      // The renderer translates raw adapterType values through i18n
      // (`adapterOption_<value>` → pretty label). The Type column
      // therefore shows the pretty labels, not the raw strings.
      expect(screen.getByText('Anthropic (Messages)')).toBeDefined();
      expect(screen.getByText('Google Gemini')).toBeDefined();
    });
  });
});
