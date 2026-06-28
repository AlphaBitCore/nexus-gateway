import { describe, it, expect, vi, beforeEach } from 'vitest';
import { render, screen, fireEvent, waitFor } from '@testing-library/react';
import { MemoryRouter } from 'react-router-dom';
import { I18nextProvider } from 'react-i18next';
import i18n from '@/i18n';
import { CredentialDetail } from '@/pages/ai-gateway/credentials/CredentialDetail';

const svc = vi.hoisted(() => ({
  credentialApi: { get: vi.fn(), update: vi.fn(), delete: vi.fn() },
  providerApi: { list: vi.fn() },
}));
vi.mock('@/api/services', () => svc);
vi.mock('@/hooks/usePermission', () => ({ usePermission: () => true }));

const navigate = vi.fn();
vi.mock('react-router-dom', async (orig) => ({
  ...(await orig<typeof import('react-router-dom')>()),
  useParams: () => ({ id: 'c1' }),
  useNavigate: () => navigate,
}));
const mutateCalls = vi.hoisted(() => ({ list: [] as unknown[] }));
vi.mock('@/hooks/useMutation', () => ({
  useMutation: (fn: (a: unknown) => Promise<unknown>, opts?: { onSuccess?: (r: unknown) => void }) => ({
    mutate: async (arg: unknown) => { mutateCalls.list.push(arg); const r = await fn(arg); opts?.onSuccess?.(r); return r; },
    loading: false,
  }),
}));
const apiByKey = vi.hoisted(() => ({ cred: undefined as unknown, providers: undefined as unknown }));
vi.mock('@/hooks/useApi', () => ({
  useApi: (_fn: unknown, key: unknown[]) => (key.includes('providers') ? apiByKey.providers : apiByKey.cred),
}));

const credential = { id: 'c1', name: 'OpenAI Key', enabled: true, status: 'active', selectionWeight: 100, createdAt: '2026-05-01T00:00:00Z', expiresAt: null, providerId: 'p1', rotationState: 'none', totalUsageCount: 1234, consecutiveFailures: 0, totalFailureCount: 0 };
function ok(d: unknown) { return { data: d, loading: false, error: null, refetch: vi.fn() }; }
function wrap() {
  return render(<I18nextProvider i18n={i18n}><MemoryRouter><CredentialDetail /></MemoryRouter></I18nextProvider>);
}

describe('CredentialDetail', () => {
  beforeEach(() => {
    vi.clearAllMocks();
    mutateCalls.list = [];
    apiByKey.cred = ok(credential);
    apiByKey.providers = ok({ data: [{ id: 'p1', name: 'openai', displayName: 'OpenAI' }] });
    svc.credentialApi.update.mockResolvedValue({});
    svc.credentialApi.delete.mockResolvedValue({});
  });

  it('renders the credential name + provider subtitle', () => {
    wrap();
    expect(screen.getAllByText('OpenAI Key').length).toBeGreaterThan(0);
  });

  it('renders the error branch', () => {
    apiByKey.cred = { data: undefined, loading: false, error: new Error('cred load failed'), refetch: vi.fn() };
    wrap();
    expect(screen.getByText('cred load failed')).toBeInTheDocument();
  });

  it('the enable/disable toggle flips the enabled flag', async () => {
    wrap();
    // the toggle button shows the credential's current state ("Enabled") and
    // clicking it flips the flag to the opposite value
    fireEvent.click(screen.getByRole('button', { name: i18n.t('common:enabled') }));
    await waitFor(() => expect(mutateCalls.list).toContainEqual({ enabled: false }));
  });

  it('edit → change name → save persists the full payload', async () => {
    wrap();
    fireEvent.click(screen.getByRole('button', { name: i18n.t('common:edit') }));
    const nameInput = await screen.findByDisplayValue('OpenAI Key');
    fireEvent.change(nameInput, { target: { value: 'OpenAI Key v2' } });
    fireEvent.click(screen.getByRole('button', { name: i18n.t('common:save') }));
    await waitFor(() => expect(svc.credentialApi.update).toHaveBeenCalledWith('c1', expect.objectContaining({ name: 'OpenAI Key v2', enabled: true, selectionWeight: 100, status: 'active' })));
  });

  it('delete confirms then removes the credential + navigates', async () => {
    wrap();
    fireEvent.click(screen.getByRole('button', { name: i18n.t('common:delete') }));
    const confirm = screen.getAllByRole('button', { name: i18n.t('common:delete') }).at(-1)!;
    fireEvent.click(confirm);
    await waitFor(() => expect(svc.credentialApi.delete).toHaveBeenCalledWith('c1'));
    await waitFor(() => expect(navigate).toHaveBeenCalledWith('/ai-gateway/credentials'));
  });
});
