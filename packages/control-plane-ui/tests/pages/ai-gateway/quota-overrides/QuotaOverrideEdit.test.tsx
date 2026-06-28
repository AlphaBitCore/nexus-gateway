import { describe, it, expect, vi, beforeEach } from 'vitest';
import { render, screen, fireEvent, waitFor } from '@testing-library/react';
import { MemoryRouter } from 'react-router-dom';
import { I18nextProvider } from 'react-i18next';
import i18n from '@/i18n';
import { QuotaOverrideEdit } from '@/pages/ai-gateway/quota-overrides/QuotaOverrideEdit';

const svc = vi.hoisted(() => {
  const empty = () => Promise.resolve({ data: [], total: 0 });
  return {
    quotaOverrideApi: { get: vi.fn(), update: vi.fn() },
    iamApi: { listUsers: empty },
    virtualKeyApi: { list: empty },
    projectApi: { list: empty },
    organizationApi: { list: empty },
  };
});
vi.mock('@/api/services', () => svc);

const navigate = vi.fn();
vi.mock('react-router-dom', async (orig) => ({
  ...(await orig<typeof import('react-router-dom')>()),
  useParams: () => ({ id: 'o1' }),
  useNavigate: () => navigate,
}));
vi.mock('@/hooks/useMutation', () => ({
  useMutation: (fn: (a: unknown) => Promise<unknown>, opts?: { onSuccess?: (r: unknown) => void }) => ({
    mutate: async (arg: unknown) => { const r = await fn(arg); opts?.onSuccess?.(r); return r; },
    loading: false,
  }),
}));
const apiState = vi.hoisted(() => ({ value: { data: undefined as unknown, loading: false, error: null as Error | null } }));
vi.mock('@/hooks/useApi', () => ({ useApi: () => apiState.value }));

const override = { id: 'o1', targetType: 'user', targetId: 'u1', targetName: 'Alice', reason: 'temp bump', costLimitUsd: 100, tokenLimit: 5000, enforcementMode: '_inherit', periodType: '_inherit' };
function wrap() {
  return render(<I18nextProvider i18n={i18n}><MemoryRouter><QuotaOverrideEdit /></MemoryRouter></I18nextProvider>);
}

describe('QuotaOverrideEdit', () => {
  beforeEach(() => {
    vi.clearAllMocks();
    apiState.value = { data: override, loading: false, error: null } as never;
    svc.quotaOverrideApi.update.mockResolvedValue({});
  });

  it('renders the skeleton while fetching the override', () => {
    apiState.value = { data: undefined, loading: true, error: null } as never;
    const { container } = wrap();
    expect(container.firstChild).toBeTruthy();
  });

  it('hydrates the form from the loaded override', () => {
    wrap();
    expect(screen.getByDisplayValue('temp bump')).toBeInTheDocument();
    expect(screen.getByDisplayValue('100')).toBeInTheDocument();
  });

  it('submitting updates the override (mapping _inherit → undefined) then navigates', async () => {
    wrap();
    fireEvent.click(screen.getByRole('button', { name: i18n.t('common:save') }));
    await waitFor(() => expect(svc.quotaOverrideApi.update).toHaveBeenCalledWith('o1', expect.objectContaining({
      targetType: 'user', targetId: 'u1', reason: 'temp bump', costLimitUsd: 100, tokenLimit: 5000,
      enforcementMode: undefined, periodType: undefined,
    })));
    // on success the edit form returns to the override's detail page
    await waitFor(() => expect(navigate).toHaveBeenCalledWith('/ai-gateway/quota-overrides/o1'));
  });

  it('cancel navigates back to the list', () => {
    wrap();
    fireEvent.click(screen.getByRole('button', { name: i18n.t('common:cancel') }));
    expect(navigate).toHaveBeenCalledWith('/ai-gateway/quota-overrides');
  });
});
