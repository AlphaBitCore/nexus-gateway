/**
 * RoutingRuleCreate — wizard page driven through the real useRoutingRuleCreate
 * hook (deps mocked) with real i18n, so the per-strategy step-1 sections, the
 * fallback warning banner, and the wizard next/back/cancel footer are
 * exercised. Replaces the previous render-without-crashing smoke test, which
 * asserted nothing observable.
 */
import { describe, it, expect, vi, beforeEach } from 'vitest';
import { render, screen, fireEvent } from '@testing-library/react';
import { MemoryRouter } from 'react-router-dom';
import { I18nextProvider } from 'react-i18next';
import i18n from '@/i18n';
import { RoutingRuleCreate } from '@/pages/ai-gateway/routing/create/RoutingRuleCreatePage';

const navigate = vi.fn();
const mutate = vi.fn();
const addToast = vi.fn();
const groups = [{
  provider: { id: 'p1', name: 'openai', displayName: 'OpenAI', enabled: true },
  models: [{ id: 'm1', providerModelId: 'gpt-4o', name: 'GPT-4o', enabled: true }],
}];

vi.mock('react-router-dom', async (o) => ({ ...(await o<typeof import('react-router-dom')>()), useNavigate: () => navigate }));
vi.mock('@/context/ToastContext', () => ({ useToast: () => ({ addToast }) }));
vi.mock('@/hooks/useSyncFeedback', () => ({ useSyncFeedback: () => vi.fn() }));
vi.mock('@/hooks/usePermission', () => ({ usePermission: () => true, ACTION_MAP: {} }));
vi.mock('@/hooks/useApi', () => ({ useApi: () => ({ data: { data: groups }, loading: false, error: null, refetch: vi.fn() }) }));
vi.mock('@/hooks/useMutation', () => ({ useMutation: () => ({ mutate, loading: false }) }));
vi.mock('@/api/services', () => ({ routingApi: {}, systemApi: {} }));

function wrap() {
  return render(<I18nextProvider i18n={i18n}><MemoryRouter><RoutingRuleCreate /></MemoryRouter></I18nextProvider>);
}
const setStrategy = (v: string) => {
  const sel = document.getElementById('strategyType') as HTMLSelectElement;
  fireEvent.change(sel, { target: { value: v } });
};

describe('RoutingRuleCreate', () => {
  beforeEach(() => { vi.clearAllMocks(); });

  it('step 0 captures the name and defaults to the single strategy section', () => {
    wrap();
    const name = screen.getByTestId('routing-rule-name') as HTMLInputElement;
    fireEvent.change(name, { target: { value: 'my-rule' } });
    expect(name.value).toBe('my-rule');
    expect(screen.getAllByText(i18n.t('pages:routing.providerConfiguration')).length).toBeGreaterThan(0);
  });

  it('selecting fallback shows the fallback section + a recovery-only warning', () => {
    wrap();
    setStrategy('fallback');
    expect(screen.getAllByText(i18n.t('pages:routing.fallbackChainTitle')).length).toBeGreaterThan(0);
    expect(screen.getByRole('status')).toBeInTheDocument();
  });

  it('selecting each weighted/advanced strategy renders its step-1 section', () => {
    wrap();
    setStrategy('loadbalance');
    expect(screen.getAllByText(i18n.t('pages:routing.loadBalanceTargets')).length).toBeGreaterThan(0);
    setStrategy('ab_split');
    expect(screen.getAllByText(i18n.t('pages:routing.abSplitTargets')).length).toBeGreaterThan(0);
    setStrategy('conditional');
    expect(screen.getAllByText(i18n.t('pages:routing.conditionalRouting')).length).toBeGreaterThan(0);
    setStrategy('smart');
    expect(screen.getAllByText(i18n.t('pages:routing.intelligentRoutingConfig')).length).toBeGreaterThan(0);
  });

  it('smart strategy: editing the system prompt updates the smart-config state', () => {
    const { container } = render(<I18nextProvider i18n={i18n}><MemoryRouter><RoutingRuleCreate /></MemoryRouter></I18nextProvider>);
    fireEvent.change(container.querySelector('#strategyType')!, { target: { value: 'smart' } });
    // the smart section renders exactly one textarea — the router system prompt,
    // a controlled field backed by useRoutingRuleCreate.smartState (updateSmart)
    const promptArea = container.querySelector('textarea') as HTMLTextAreaElement;
    expect(promptArea).toBeTruthy();
    fireEvent.change(promptArea, { target: { value: 'Route cheap models for short prompts.' } });
    expect(promptArea.value).toBe('Route cheap models for short prompts.');
  });

  it('loadbalance strategy: Add target grows the weighted-target rows', () => {
    const { container } = render(<I18nextProvider i18n={i18n}><MemoryRouter><RoutingRuleCreate /></MemoryRouter></I18nextProvider>);
    fireEvent.change(container.querySelector('#strategyType')!, { target: { value: 'loadbalance' } });
    const removeName = new RegExp(`^${i18n.t('pages:routing.remove')}$`, 'i');
    const before = screen.getAllByRole('button', { name: removeName }).length;
    fireEvent.click(screen.getByRole('button', { name: i18n.t('pages:routing.addTarget') }));
    expect(screen.getAllByRole('button', { name: removeName }).length).toBe(before + 1);
  });

  it('Cancel on step 0 navigates back to the routing list', () => {
    wrap();
    fireEvent.click(screen.getByRole('button', { name: i18n.t('common:cancel') }));
    expect(navigate).toHaveBeenCalledWith('/ai-gateway/routing');
  });

  it('blocks Continue on step 0 until Name is filled', () => {
    wrap();
    const next = () => screen.getByRole('button', { name: i18n.t('pages:routing.wizardContinue', 'Continue') });
    expect(next()).toBeDisabled();
    fireEvent.change(screen.getByTestId('routing-rule-name'), { target: { value: 'r' } });
    expect(next()).toBeEnabled();
  });

  it('blocks Continue on the Configuration step when single strategy has no provider/model', () => {
    wrap();
    fireEvent.change(screen.getByTestId('routing-rule-name'), { target: { value: 'r' } });
    // step 0 → Configuration
    fireEvent.click(screen.getByRole('button', { name: i18n.t('pages:routing.wizardContinue', 'Continue') }));
    // single strategy with no provider/model resolved → Continue gated, never submits
    expect(screen.getByRole('button', { name: i18n.t('pages:routing.wizardContinue', 'Continue') })).toBeDisabled();
    expect(mutate).not.toHaveBeenCalled();
  });
});
