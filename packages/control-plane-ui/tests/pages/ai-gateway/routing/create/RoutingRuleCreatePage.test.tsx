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

// The wizard carries its own copy of the smart-rule guard so it can block on the
// step where the operator can still fix it. The predicate is pinned in
// routing-rule-config-safety.test.ts; what these two pin is the WIRING — that a
// multi-keyword rule reaches the end of the wizard, and that the one shape which
// would quietly claim every request in the fleet still does not.
describe('RoutingRuleCreate — smart-rule keyword guard', () => {
  beforeEach(() => { vi.clearAllMocks(); });

  // Walk to the Match-conditions step, typing keywords into the literals input.
  function toMatchStep(keywords: string[]) {
    wrap();
    fireEvent.change(screen.getByTestId('routing-rule-name'), { target: { value: 'kw-rule' } });
    setStrategy('smart');
    const cont = () => screen.getByRole('button', { name: i18n.t('pages:routing.wizardContinue', 'Continue') }) as HTMLButtonElement;
    for (let i = 0; i < 3; i++) {
      if (i === 1) {
        // Step 1 for `smart` needs a router provider + model before it will
        // advance. ProviderModelSelect renders bare <select>s, so address them
        // by position: router pair first, default pair second.
        const sels = [...document.querySelectorAll('select')].filter((s) => s.id !== 'strategyType');
        fireEvent.change(sels[0], { target: { value: 'openai' } });
        const after = [...document.querySelectorAll('select')].filter((s) => s.id !== 'strategyType');
        fireEvent.change(after[1], { target: { value: 'gpt-4o' } });
      }
      if (cont().disabled) {
        throw new Error(`wizard blocked leaving step ${i}: ${screen.queryByRole('alert')?.textContent}`);
      }
      fireEvent.click(cont());
    }
    const input = screen.getByRole('textbox', { name: i18n.t('pages:routing.matchRequestedModelLiteralsLabel') });
    for (const kw of keywords) {
      fireEvent.change(input, { target: { value: kw } });
      fireEvent.keyDown(input, { key: 'Enter' });
    }
    return input;
  }

  it('several non-auto keywords leave the wizard unblocked', () => {
    toMatchStep(['fast', 'cheap']);
    expect(screen.queryByRole('alert')).toBeNull();
  });

  it('an everything-glob still blocks, and says which entry did it', () => {
    toMatchStep(['auto', '*']);
    const alert = screen.getByRole('alert');
    expect(alert.textContent).toContain('*');
    expect(alert.textContent).toContain('matches every request');
  });

  it('pinning no keyword at all blocks a smart rule', () => {
    toMatchStep([]);
    expect(screen.getByRole('alert').textContent).toContain('at least one request keyword');
  });
});
