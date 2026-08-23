import { describe, it, expect, vi, beforeEach } from 'vitest';
import { renderHook, act, waitFor } from '@testing-library/react';
import { useRoutingRuleDetail } from '@/pages/ai-gateway/routing/detail/useRoutingRuleDetail';

const mutateCalls: unknown[] = [];
const simulate = vi.fn();
const addToast = vi.fn();
const navigate = vi.fn();
const updateApi = vi.fn();
const deleteApi = vi.fn();
let rule: Record<string, unknown> | undefined;
const groups = [{ provider: { id: 'p1', name: 'openai', enabled: true }, models: [{ id: 'm1', providerModelId: 'gpt-4o', enabled: true }] }];

vi.mock('react-router-dom', () => ({ useParams: () => ({ id: 'r1' }), useNavigate: () => navigate }));
vi.mock('react-i18next', async (o) => ({ ...(await o<typeof import('react-i18next')>()), useTranslation: () => ({ t: (k: string) => k }) as never }));
vi.mock('@/context/ToastContext', () => ({ useToast: () => ({ addToast }) }));
vi.mock('@/hooks/useSyncFeedback', () => ({ useSyncFeedback: () => vi.fn() }));
vi.mock('@/hooks/useUnsavedChangesWarning', () => ({ useUnsavedChangesWarning: () => {} }));
vi.mock('@/hooks/usePermission', () => ({ usePermission: () => true }));
vi.mock('@/hooks/useApi', () => ({
  useApi: (_fn: unknown, key: string[]) =>
    key.includes('detail') ? { data: rule, loading: false, error: null, refetch: vi.fn() } : { data: { data: groups } },
}));
// run the real mutation fn then onSuccess, so save→setIsEditing(false) and delete→navigate are exercised
vi.mock('@/hooks/useMutation', () => ({
  useMutation: (fn: (a: unknown) => Promise<unknown>, opts?: { onSuccess?: () => void }) => ({
    mutate: async (a: unknown) => { mutateCalls.push(a); await fn(a); opts?.onSuccess?.(); },
    loading: false,
  }),
}));
vi.mock('@/api/services', () => ({
  routingApi: { simulate: (b: unknown) => simulate(b), update: (...a: unknown[]) => updateApi(...a), delete: (...a: unknown[]) => deleteApi(...a) },
  systemApi: {},
}));

describe('useRoutingRuleDetail', () => {
  beforeEach(() => {
    mutateCalls.length = 0; simulate.mockReset();
    addToast.mockClear(); navigate.mockClear();
    updateApi.mockReset().mockResolvedValue({}); deleteApi.mockReset().mockResolvedValue({});
    rule = { id: 'r1', name: 'Rule', description: 'd', strategyType: 'single', priority: 3, enabled: true, pipelineStage: 1, config: { type: 'single' }, matchConditions: {} };
  });

  it('exposes the rule + permissions + derived view config', () => {
    const { result } = renderHook(() => useRoutingRuleDetail());
    expect(result.current.rule?.name).toBe('Rule');
    expect(result.current.canUpdate).toBe(true);
    expect(result.current.viewConfig).toBeTruthy();
    expect(result.current.viewSmartParsed).toBeNull(); // single → not smart
  });

  it('parses smart config for a smart-strategy rule', () => {
    rule = { ...rule, strategyType: 'smart', config: { type: 'smart', routerProviderId: 'p1', routerModelId: 'm1' } };
    const { result } = renderHook(() => useRoutingRuleDetail());
    expect(result.current.viewSmartParsed).not.toBeNull();
  });

  it('startEditing hydrates the edit form + enters edit mode', () => {
    const { result } = renderHook(() => useRoutingRuleDetail());
    act(() => result.current.startEditing());
    expect(result.current.isEditing).toBe(true);
    expect(result.current.editForm.getValues().editName).toBe('Rule');
    expect(result.current.editForm.getValues().editPriority).toBe(3);
  });

  it('runSimulation stores the payload on success', async () => {
    simulate.mockResolvedValue({ winner: { providerId: 'p1' } });
    const { result } = renderHook(() => useRoutingRuleDetail());
    act(() => result.current.setSimModelId('gpt-4o'));
    await act(async () => { await result.current.runSimulation(); });
    expect(simulate).toHaveBeenCalledWith(expect.objectContaining({ modelId: 'gpt-4o', endpointType: 'chat' }));
    await waitFor(() => expect(result.current.simData).toMatchObject({ winner: { providerId: 'p1' } }));
  });

  it('runSimulation records an error payload on failure + adds messages for auto', async () => {
    simulate.mockRejectedValue(new Error('down'));
    const { result } = renderHook(() => useRoutingRuleDetail());
    act(() => result.current.setSimModelId('auto'));
    await act(async () => { await result.current.runSimulation(); });
    expect(simulate).toHaveBeenCalledWith(expect.objectContaining({ modelId: 'auto', messages: expect.any(Array) }));
    await waitFor(() => expect(result.current.simData).toMatchObject({ error: expect.any(String) }));
  });

  it('handleSave (single) builds the stage-1 payload, calls update, and leaves edit mode', async () => {
    const { result } = renderHook(() => useRoutingRuleDetail());
    act(() => result.current.startEditing());
    act(() => {
      result.current.editForm.setValue('singleProvider', 'openai');
      result.current.editForm.setValue('singleModel', 'gpt-4o');
    });
    await act(async () => { result.current.handleSave(); });
    expect(mutateCalls[0]).toMatchObject({
      name: 'Rule', strategyType: 'single', pipelineStage: 1,
      config: { type: 'single', providerId: 'p1', modelId: 'm1' },
    });
    expect(updateApi).toHaveBeenCalledWith('r1', expect.objectContaining({ strategyType: 'single' }));
    await waitFor(() => expect(result.current.isEditing).toBe(false)); // onSuccess fired
  });

  it('handleSave (single) aborts with an error toast when no provider/model is chosen', () => {
    const { result } = renderHook(() => useRoutingRuleDetail());
    act(() => result.current.startEditing());
    act(() => result.current.handleSave());
    expect(addToast).toHaveBeenCalledWith(expect.any(String), 'error');
    expect(mutateCalls).toHaveLength(0); // build failed → never calls update
  });

  it('handleSave short-circuits on an invalid retry policy before touching update', () => {
    const { result } = renderHook(() => useRoutingRuleDetail());
    act(() => result.current.startEditing());
    act(() => {
      result.current.editForm.setValue('singleProvider', 'openai');
      result.current.editForm.setValue('singleModel', 'gpt-4o');
      result.current.setRetryPolicyMode('custom');
      result.current.setRetryMaxAttempts('not-a-number');
    });
    act(() => result.current.handleSave());
    expect(addToast).toHaveBeenCalledWith(expect.any(String), 'error');
    expect(mutateCalls).toHaveLength(0);
  });

  it('deleteRule calls delete then navigates back to the routing list', async () => {
    const { result } = renderHook(() => useRoutingRuleDetail());
    await act(async () => { result.current.deleteRule(undefined); });
    expect(deleteApi).toHaveBeenCalled();
    expect(navigate).toHaveBeenCalledWith('/ai-gateway/routing');
  });

  it('entry editing helpers add/update/remove fallback-style rows', () => {
    const { result } = renderHook(() => useRoutingRuleDetail());
    act(() => result.current.startEditing()); // seeds one empty entry
    act(() => result.current.addEntry());
    expect(result.current.entries).toHaveLength(2);
    act(() => result.current.updateEntry(0, 'provider', 'openai'));
    expect(result.current.entries[0].provider).toBe('openai');
    act(() => result.current.removeEntry(1));
    expect(result.current.entries).toHaveLength(1);
  });
});
