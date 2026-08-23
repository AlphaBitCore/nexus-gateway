/**
 * RoutingRuleEditForm — rendered through the real useRoutingRuleDetail hook so
 * the strategy-specific editor branches (single / fallback / loadbalance /
 * ab_split / conditional / smart) and the entry-row editing are exercised with
 * a genuine form state rather than a hand-built prop bag.
 */
import { describe, it, expect, vi, beforeEach } from 'vitest';
import { render, screen, fireEvent } from '@testing-library/react';
import { MemoryRouter } from 'react-router-dom';
import { I18nextProvider } from 'react-i18next';
import i18n from '@/i18n';
import { useRoutingRuleDetail } from '@/pages/ai-gateway/routing/detail/useRoutingRuleDetail';
import { RoutingRuleEditForm } from '@/pages/ai-gateway/routing/detail/RoutingRuleEditForm';

let rule: Record<string, unknown> | undefined;
const groups = [{
  provider: { id: 'p1', name: 'openai', displayName: 'OpenAI', enabled: true },
  models: [
    { id: 'm1', providerModelId: 'gpt-4o', name: 'GPT-4o', enabled: true },
    { id: 'm2', providerModelId: 'gpt-4o-mini', name: 'Mini', enabled: true },
  ],
}];

vi.mock('react-router-dom', async (o) => ({ ...(await o<typeof import('react-router-dom')>()), useParams: () => ({ id: 'r1' }), useNavigate: () => vi.fn() }));
vi.mock('@/context/ToastContext', () => ({ useToast: () => ({ addToast: vi.fn() }) }));
vi.mock('@/hooks/useSyncFeedback', () => ({ useSyncFeedback: () => vi.fn() }));
vi.mock('@/hooks/useUnsavedChangesWarning', () => ({ useUnsavedChangesWarning: () => {} }));
vi.mock('@/hooks/usePermission', () => ({ usePermission: () => true, ACTION_MAP: {} }));
vi.mock('@/hooks/useApi', () => ({
  useApi: (_fn: unknown, key: string[]) =>
    key.includes('detail') ? { data: rule, loading: false, error: null, refetch: vi.fn() } : { data: { data: groups } },
}));
vi.mock('@/hooks/useMutation', () => ({ useMutation: () => ({ mutate: vi.fn(), loading: false }) }));
vi.mock('@/api/services', () => ({ routingApi: { simulate: vi.fn() }, systemApi: {} }));

function Harness() {
  const detail = useRoutingRuleDetail();
  return (
    <>
      <button onClick={() => detail.startEditing()}>start</button>
      <RoutingRuleEditForm detail={detail} />
    </>
  );
}

function renderEditing() {
  render(<I18nextProvider i18n={i18n}><MemoryRouter><Harness /></MemoryRouter></I18nextProvider>);
  fireEvent.click(screen.getByText('start')); // hydrate + enter edit mode
}

describe('RoutingRuleEditForm', () => {
  beforeEach(() => {
    rule = { id: 'r1', name: 'Rule', description: 'd', strategyType: 'single', priority: 3, enabled: true, pipelineStage: 1, config: { type: 'single', providerId: 'p1', modelId: 'm1' }, matchConditions: {} };
  });

  it('single strategy renders the provider-configuration section', () => {
    renderEditing();
    expect(screen.getByText(i18n.t('pages:routing.providerConfiguration'))).toBeInTheDocument();
  });

  it('fallback strategy renders the fallback-chain section + an entry row', () => {
    rule = { ...rule, strategyType: 'fallback', config: { type: 'fallback', targets: [{ type: 'single', providerId: 'p1', modelId: 'm1' }] } };
    renderEditing();
    expect(screen.getAllByText(i18n.t('pages:routing.fallbackChainTitle')).length).toBeGreaterThan(0);
  });

  it('loadbalance strategy shows the weight column header', () => {
    rule = { ...rule, strategyType: 'loadbalance', config: { type: 'loadbalance', weightedTargets: [{ providerId: 'p1', modelId: 'm1', weight: 50 }] } };
    renderEditing();
    expect(screen.getByText(i18n.t('pages:routing.loadBalanceTargets'))).toBeInTheDocument();
    expect(screen.getAllByText(i18n.t('pages:routing.weight')).length).toBeGreaterThan(0);
  });

  it('ab_split strategy shows the split-percent weight label', () => {
    rule = { ...rule, strategyType: 'ab_split', config: { type: 'ab_split', targets: [{ providerId: 'p1', modelId: 'm1', weight: 30 }] } };
    renderEditing();
    expect(screen.getByText(i18n.t('pages:routing.abSplitTargets'))).toBeInTheDocument();
    expect(screen.getAllByText(i18n.t('pages:routing.splitPercent')).length).toBeGreaterThan(0);
  });

  it('conditional strategy renders the conditional-routing editor', () => {
    rule = { ...rule, strategyType: 'conditional', config: { type: 'conditional', conditions: [], default: { type: 'single', providerId: 'p1', modelId: 'm1' } } };
    renderEditing();
    expect(screen.getByText(i18n.t('pages:routing.conditionalRouting'))).toBeInTheDocument();
  });

  it('smart strategy renders the intelligent-routing config section', () => {
    rule = { ...rule, strategyType: 'smart', config: { type: 'smart', routerProviderId: 'p1', routerModelId: 'm1', systemPrompt: 'route' } };
    renderEditing();
    expect(screen.getByText(i18n.t('pages:routing.intelligentRoutingConfig'))).toBeInTheDocument();
  });

  it('loadbalance: add/remove targets + edit a weight; add a fallback row', () => {
    rule = { ...rule, strategyType: 'loadbalance', config: { type: 'loadbalance', weightedTargets: [{ providerId: 'p1', modelId: 'm1', weight: 50 }] } };
    renderEditing();
    const removeName = new RegExp(`^${i18n.t('pages:routing.remove')}$`, 'i');
    expect(screen.getAllByRole('button', { name: removeName })).toHaveLength(1);
    // add a second weighted target
    fireEvent.click(screen.getByRole('button', { name: i18n.t('pages:routing.addTarget') }));
    expect(screen.getAllByRole('button', { name: removeName })).toHaveLength(2);
    // edit a weight (loadbalance shows the weight column)
    const weights = screen.getAllByPlaceholderText(i18n.t('pages:routing.placeholderWeight')) as HTMLInputElement[];
    fireEvent.change(weights[0], { target: { value: '70' } });
    expect(weights[0].value).toBe('70');
    // remove the added target → back to one
    fireEvent.click(screen.getAllByRole('button', { name: removeName })[1]);
    expect(screen.getAllByRole('button', { name: removeName })).toHaveLength(1);
    // add a fallback chain row
    expect(screen.getByText(i18n.t('pages:routing.noFallbackModels'))).toBeInTheDocument();
    fireEvent.click(screen.getByRole('button', { name: i18n.t('pages:routing.addFallback') }));
    expect(screen.queryByText(i18n.t('pages:routing.noFallbackModels'))).toBeNull();
  });

});
