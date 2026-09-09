import { describe, it, expect, vi, beforeEach } from 'vitest';
import { render, screen, fireEvent, waitFor } from '@testing-library/react';
import { I18nextProvider } from 'react-i18next';
import i18n from '@/i18n';
import { AccountApiKeysTab } from '@/pages/account/AccountApiKeysTab';

const pak = vi.hoisted(() => ({ personalApiKeyApi: { list: vi.fn(), create: vi.fn(), delete: vi.fn(), regenerate: vi.fn() } }));
vi.mock('@/api/services', () => pak);
vi.mock('@/hooks/useMutation', () => ({
  useMutation: (fn: (a: unknown) => Promise<unknown>, opts?: { onSuccess?: (r: unknown) => void }) => ({
    mutate: async (arg: unknown) => { const r = await fn(arg); opts?.onSuccess?.(r); return r; },
    loading: false,
  }),
}));
const apiState = vi.hoisted(() => ({ value: { data: undefined as unknown, loading: false, error: null as Error | null, refetch: vi.fn() } }));
vi.mock('@/hooks/useApi', () => ({ useApi: () => apiState.value }));
vi.mock('@/theme/useTheme', () => ({ useTheme: () => ({ brand: { productName: 'Nexus' } }) }));

const key = { id: 'k1', name: 'ci-key', keyPrefix: 'nx_ab', enabled: true, status: 'active', expiresAt: null, createdAt: '2026-05-01T00:00:00Z' };
function ok(d: unknown) { return { data: d, loading: false, error: null, refetch: vi.fn() }; }
function wrap() {
  return render(<I18nextProvider i18n={i18n}><AccountApiKeysTab /></I18nextProvider>);
}
const createLabel = () => i18n.t('pages:account.createKey');

describe('AccountApiKeysTab', () => {
  beforeEach(() => {
    vi.clearAllMocks();
    apiState.value = ok({ data: [key] });
    pak.personalApiKeyApi.create.mockResolvedValue({ key: 'sk-created' });
    pak.personalApiKeyApi.regenerate.mockResolvedValue({ key: 'sk-regen' });
    pak.personalApiKeyApi.delete.mockResolvedValue({});
  });

  it('renders the key rows', () => {
    wrap();
    expect(screen.getByText('ci-key')).toBeInTheDocument();
    expect(screen.getByText(/nx_ab/)).toBeInTheDocument();
  });

  it('renders the error branch', () => {
    apiState.value = { data: undefined, loading: false, error: new Error('keys load failed'), refetch: vi.fn() };
    wrap();
    expect(screen.getByText('keys load failed')).toBeInTheDocument();
  });

  it('regenerate rotates the key and reveals the new secret', async () => {
    wrap();
    fireEvent.click(screen.getByRole('button', { name: i18n.t('pages:account.keyRegenerate') }));
    await waitFor(() => expect(pak.personalApiKeyApi.regenerate).toHaveBeenCalledWith('k1'));
    await waitFor(() => expect(screen.getByText('sk-regen')).toBeInTheDocument());
  });

  it('create opens the dialog, submits the name, and shows the new secret', async () => {
    wrap();
    fireEvent.click(screen.getAllByRole('button', { name: createLabel() })[0]); // header opens dialog
    fireEvent.change(screen.getByPlaceholderText(i18n.t('pages:account.keyNamePlaceholder')), { target: { value: 'laptop' } });
    fireEvent.click(screen.getAllByRole('button', { name: createLabel() }).at(-1)!); // dialog submit
    await waitFor(() => expect(pak.personalApiKeyApi.create).toHaveBeenCalledWith({ name: 'laptop' }));
    await waitFor(() => expect(screen.getByText('sk-created')).toBeInTheDocument());
  });

  it('delete confirms then revokes the key', async () => {
    wrap();
    fireEvent.click(screen.getByRole('button', { name: i18n.t('common:delete') }));
    const confirm = screen.getAllByRole('button').filter((b) => /confirm|delete/i.test(b.textContent ?? '')).at(-1)!;
    fireEvent.click(confirm);
    await waitFor(() => expect(pak.personalApiKeyApi.delete).toHaveBeenCalledWith('k1'));
  });

  // Retiring a key writes `status` and never touches `enabled`; only the auth
  // lookup folds the lifecycle back into the boolean, and that fold is local
  // to the auth path. Revocation genuinely works -- the key stops
  // authenticating -- but the list returns the raw row, so a key revoked
  // after a compromise read "Enabled" to the person who owns it.
  it('shows a revoked key as revoked, not as Enabled', () => {
    apiState.value = ok({ data: [{ ...key, status: 'unavailable', enabled: true }] });
    wrap();
    expect(screen.getByText(i18n.t('pages:account.keyStatusUnavailable'))).toBeInTheDocument();
    expect(screen.queryByText(i18n.t('common:enabled'))).toBeNull();
  });

  // A key mid-rotation was invisible for the same reason: `enabled` stays
  // true through a rotation, so the row said nothing was happening.
  it('shows a key mid-rotation as rotating', () => {
    apiState.value = ok({ data: [{ ...key, status: 'rotating', enabled: true }] });
    wrap();
    expect(screen.getByText(i18n.t('pages:account.keyStatusRotating'))).toBeInTheDocument();
  });
});
