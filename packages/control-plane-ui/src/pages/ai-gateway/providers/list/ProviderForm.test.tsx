import { describe, it, expect, vi, beforeEach } from 'vitest';
import { render, screen, fireEvent, waitFor } from '@testing-library/react';
import { QueryClient, QueryClientProvider } from '@tanstack/react-query';
import { ToastProvider } from '@/context/ToastContext';
import type { Provider } from '@/api/types';
import { ProviderForm } from './ProviderForm';

// t returns the key so assertions don't depend on i18n initialization; the
// English fallback (2nd arg) is ignored.
vi.mock('react-i18next', () => ({
  useTranslation: () => ({ t: (k: string) => k }),
  initReactI18next: { type: '3rdParty', init: () => {} },
  // ProviderConnectivityTestButton renders <Trans>; render its i18nKey as text.
  Trans: ({ i18nKey }: { i18nKey: string }) => i18nKey,
}));

// Capture the payload the form submits to the admin API. Both create() and
// update() are mocked so the form's useMutation resolves cleanly.
const create = vi.fn(async (data: unknown) => ({ id: 'p_new', ...(data as object) }));
const update = vi.fn(async (_id: string, data: unknown) => ({ id: _id, ...(data as object) }));
vi.mock('@/api/services', () => ({
  providerApi: {
    create: (data: unknown) => create(data),
    update: (id: string, data: unknown) => update(id, data),
    // ProviderConnectivityTestButton references these; unused in these tests.
    testExisting: vi.fn(),
    testConnection: vi.fn(),
  },
}));

function renderForm(provider?: Provider) {
  const qc = new QueryClient({ defaultOptions: { queries: { retry: false } } });
  render(
    <QueryClientProvider client={qc}>
      <ToastProvider>
        <ProviderForm provider={provider} onClose={vi.fn()} onSaved={vi.fn()} />
      </ToastProvider>
    </QueryClientProvider>,
  );
}

const baseProvider: Provider = {
  id: 'p_1',
  name: 'openai-prod',
  adapterType: 'openai',
  baseUrl: 'https://api.openai.com',
  pathPrefix: '/v1',
  enabled: true,
  createdAt: '2026-06-29T00:00:00Z',
};

describe('ProviderForm — servesResponsesApi round-trip', () => {
  beforeEach(() => {
    create.mockClear();
    update.mockClear();
  });

  it('sends servesResponsesApi: null on create when left at the adapter default', async () => {
    renderForm();
    // Fill the required fields (name + baseUrl) so the Save button enables.
    fireEvent.change(document.querySelector('input[name="name"]') as HTMLInputElement, {
      target: { value: 'my-provider' },
    });
    fireEvent.change(document.querySelector('input[name="baseUrl"]') as HTMLInputElement, {
      target: { value: 'https://api.example.com' },
    });

    fireEvent.click(screen.getByRole('button', { name: 'common:save' }));

    await waitFor(() => expect(create).toHaveBeenCalledTimes(1));
    expect(create.mock.calls[0][0]).toMatchObject({ servesResponsesApi: null });
  });

  it('initialises the override to false and round-trips false on update', async () => {
    renderForm({ ...baseProvider, servesResponsesApi: false });

    fireEvent.click(screen.getByRole('button', { name: 'common:save' }));

    await waitFor(() => expect(update).toHaveBeenCalledTimes(1));
    expect(update.mock.calls[0][0]).toBe('p_1');
    expect(update.mock.calls[0][1]).toMatchObject({ servesResponsesApi: false });
  });

  it('initialises the override to true and round-trips true on update', async () => {
    renderForm({ ...baseProvider, servesResponsesApi: true });

    fireEvent.click(screen.getByRole('button', { name: 'common:save' }));

    await waitFor(() => expect(update).toHaveBeenCalledTimes(1));
    expect(update.mock.calls[0][1]).toMatchObject({ servesResponsesApi: true });
  });

  it('keeps a null/absent capability as the adapter default (null) on update', async () => {
    renderForm({ ...baseProvider, servesResponsesApi: null });

    fireEvent.click(screen.getByRole('button', { name: 'common:save' }));

    await waitFor(() => expect(update).toHaveBeenCalledTimes(1));
    expect(update.mock.calls[0][1]).toMatchObject({ servesResponsesApi: null });
  });
});
