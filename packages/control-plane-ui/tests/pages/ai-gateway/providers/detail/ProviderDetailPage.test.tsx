/**
 * ProviderDetailPage — mock the useProviderDetail hook + stub the child tabs:
 * assert header, tab switching, the enable/disable toggle, and loading/error.
 * Replaces the smoke test.
 */
import { describe, it, expect, vi, beforeEach } from 'vitest';
import { render, screen, fireEvent } from '@testing-library/react';
import { MemoryRouter } from 'react-router-dom';
import { I18nextProvider } from 'react-i18next';
import i18n from '@/i18n';
import { ProviderDetailPage } from '@/pages/ai-gateway/providers/detail/ProviderDetailPage';

vi.mock('react-router-dom', async (orig) => ({ ...(await orig<typeof import('react-router-dom')>()), useNavigate: () => vi.fn() }));
// stub the six child tabs so the test stays on the page-shell behaviour
vi.mock('@/pages/ai-gateway/providers/detail/ProviderInfoTab', () => ({ ProviderInfoTab: () => <div data-testid="info-tab" /> }));
vi.mock('@/pages/ai-gateway/providers/detail/ProviderCredentialsTab', () => ({ ProviderCredentialsTab: () => <div data-testid="cred-tab" /> }));
vi.mock('@/pages/ai-gateway/providers/detail/ProviderModelsTab', () => ({ ProviderModelsTab: () => <div data-testid="models-tab" /> }));
vi.mock('@/pages/ai-gateway/providers/detail/ProviderUsageTab', () => ({ ProviderUsageTab: () => <div data-testid="usage-tab" /> }));
vi.mock('@/pages/ai-gateway/providers/detail/ProviderHealthTab', () => ({ ProviderHealthTab: () => <div data-testid="health-tab" /> }));
vi.mock('@/pages/ai-gateway/providers/detail/ProviderCacheTab', () => ({ ProviderCacheTab: () => <div data-testid="cache-tab" /> }));

const spies = vi.hoisted(() => ({ setActiveTab: vi.fn(), toggleEnabled: vi.fn(), startEditing: vi.fn(), setDeleting: vi.fn(), refetch: vi.fn() }));
const state = vi.hoisted(() => ({ value: {} as Record<string, unknown> }));
vi.mock('@/pages/ai-gateway/providers/detail/useProviderDetail', () => ({ useProviderDetail: () => state.value }));

const provider = { id: 'p1', name: 'openai', displayName: 'OpenAI', description: '', enabled: true };
function base(over: Record<string, unknown> = {}) {
  return {
    provider, loading: false, error: null, refetch: spies.refetch,
    activeTab: 'info', setActiveTab: spies.setActiveTab,
    toggleEnabled: spies.toggleEnabled, toggleLoading: false,
    canUpdate: true, canDelete: true, isEditing: false, startEditing: spies.startEditing,
    setDeleting: spies.setDeleting, deleting: false, credentials: [], models: [],
    ...over,
  };
}
function wrap() { return render(<I18nextProvider i18n={i18n}><MemoryRouter><ProviderDetailPage /></MemoryRouter></I18nextProvider>); }

describe('ProviderDetailPage', () => {
  beforeEach(() => { vi.clearAllMocks(); state.value = base(); });

  it('renders the provider header + the default info tab', () => {
    wrap();
    expect(screen.getAllByText('openai').length).toBeGreaterThan(0);
    expect(screen.getByTestId('info-tab')).toBeInTheDocument();
  });

  it('clicking the Usage tab switches the active tab', () => {
    wrap();
    fireEvent.click(screen.getByText(i18n.t('pages:providers.usage')));
    expect(spies.setActiveTab).toHaveBeenCalledWith('usage');
  });

  it('the enabled provider shows Disable and toggles it off', () => {
    wrap();
    // The status button labels itself with the current state ("Enabled") and
    // toggles to the opposite when clicked.
    fireEvent.click(screen.getByRole('button', { name: i18n.t('common:enabled') }));
    expect(spies.toggleEnabled).toHaveBeenCalledWith(false);
  });

  it('renders the loading + error branches', () => {
    state.value = base({ loading: true });
    const { unmount } = wrap();
    expect(screen.queryByText('openai')).toBeNull();
    unmount();
    state.value = base({ error: new Error('provider load failed') });
    wrap();
    expect(screen.getByText('provider load failed')).toBeInTheDocument();
  });
});
