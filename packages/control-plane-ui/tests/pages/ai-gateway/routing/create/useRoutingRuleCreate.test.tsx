import { describe, it, expect, vi, beforeEach } from 'vitest';
import { renderHook, act } from '@testing-library/react';
import { useRoutingRuleCreate } from '@/pages/ai-gateway/routing/create/useRoutingRuleCreate';

const mutateCalls: unknown[] = [];
const addToast = vi.fn();
const groups = [
  { provider: { id: 'p1', name: 'openai', enabled: true }, models: [{ id: 'm1', providerModelId: 'gpt-4o', enabled: true }] },
];

vi.mock('react-router-dom', () => ({ useNavigate: () => vi.fn() }));
vi.mock('react-i18next', async (o) => ({ ...(await o<typeof import('react-i18next')>()), useTranslation: () => ({ t: (k: string) => k }) as never }));
vi.mock('@/context/ToastContext', () => ({ useToast: () => ({ addToast }) }));
vi.mock('@/hooks/useSyncFeedback', () => ({ useSyncFeedback: () => vi.fn() }));
vi.mock('@/hooks/useApi', () => ({ useApi: () => ({ data: { data: groups } }) }));
vi.mock('@/hooks/useMutation', () => ({ useMutation: () => ({ mutate: (a: unknown) => mutateCalls.push(a), loading: false }) }));
vi.mock('@/api/services', () => ({ routingApi: {}, systemApi: {} }));

describe('useRoutingRuleCreate', () => {
  beforeEach(() => { mutateCalls.length = 0; addToast.mockClear(); });

  it('entry + fallback helpers add/update/remove rows', () => {
    const { result } = renderHook(() => useRoutingRuleCreate());
    act(() => result.current.addEntry());
    expect(result.current.entries).toHaveLength(2);
    act(() => result.current.updateEntry(1, 'provider', 'openai'));
    expect(result.current.entries[1].provider).toBe('openai');
    act(() => result.current.removeEntry(1));
    expect(result.current.entries).toHaveLength(1);
    act(() => result.current.addFallback());
    act(() => result.current.updateFallback(0, 'model', 'gpt-4o'));
    expect(result.current.fallbackEntries[0].model).toBe('gpt-4o');
    act(() => result.current.removeFallback(0));
    expect(result.current.fallbackEntries).toHaveLength(0);
  });

  it('showWeightColumn reflects weighted strategies; handleStrategyChange hydrates conditional', () => {
    const { result } = renderHook(() => useRoutingRuleCreate());
    expect(result.current.showWeightColumn).toBe(false);
    act(() => result.current.handleStrategyChange('loadbalance'));
    expect(result.current.showWeightColumn).toBe(true);
    act(() => result.current.handleStrategyChange('conditional'));
    expect(result.current.conditionalUi.mode).toBeDefined();
  });

  it('handleSubmit (single) errors via toast when no provider/model selected', () => {
    const { result } = renderHook(() => useRoutingRuleCreate());
    act(() => { result.current.setName('r'); });
    act(() => result.current.handleSubmit());
    expect(addToast).toHaveBeenCalledWith(expect.any(String), 'error');
    expect(mutateCalls).toHaveLength(0);
  });

  it('handleSubmit (single) submits a built payload when a valid model is chosen', () => {
    const { result } = renderHook(() => useRoutingRuleCreate());
    act(() => {
      result.current.setName('my-rule');
      result.current.setSingleProvider('openai');
      result.current.setSingleModel('gpt-4o');
    });
    act(() => result.current.handleSubmit());
    expect(mutateCalls).toHaveLength(1);
    expect(mutateCalls[0]).toMatchObject({ name: 'my-rule', strategyType: 'single', pipelineStage: 1 });
  });

});
