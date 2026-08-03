import { describe, it, expect, vi, beforeEach } from 'vitest';
import { renderHook, act } from '@testing-library/react';
import { useProviderDetail } from '@/pages/ai-gateway/providers/detail/useProviderDetail';

const mutateCalls: unknown[] = [];
const navigate = vi.fn();
let provider: Record<string, unknown> | undefined;
const creds = [
  { id: 'c1', name: 'cred-a', enabled: true, providerId: 'prov-1', expiresAt: '2026-12-31T00:00:00Z' },
  { id: 'c2', name: 'other', enabled: true, providerId: 'other-prov', expiresAt: null },
];
const models = [{ id: 'm1', code: 'gpt', providerModelId: 'gpt-4o', name: 'GPT', type: 'chat', enabled: true }];

vi.mock('react-router-dom', () => ({
  useParams: () => ({ id: 'prov-1' }),
  useNavigate: () => navigate,
}));
vi.mock('react-i18next', async (orig) => ({
  ...(await orig<typeof import('react-i18next')>()),
  useTranslation: () => ({ t: (k: string) => k }) as never,
}));
vi.mock('@/hooks/useApi', () => ({
  useApi: (_fn: unknown, key: string[]) => {
    if (key.includes('detail')) return { data: provider, loading: false, error: null, refetch: vi.fn() };
    if (key.includes('models')) return { data: { data: models }, refetch: vi.fn() };
    if (key.includes('list')) return { data: { data: creds }, refetch: vi.fn() };
    return { data: undefined, refetch: vi.fn() };
  },
}));
vi.mock('@/hooks/useMutation', () => ({
  useMutation: () => ({ mutate: (arg: unknown) => mutateCalls.push(arg), loading: false }),
}));
vi.mock('@tanstack/react-query', () => ({
  useQueryClient: () => ({ setQueryData: vi.fn() }),
}));
// Key-aware permission double: every action is granted unless a test denies it,
// so a test can model the custom policy that separates provider access from
// model access.
const { deniedPermissions } = vi.hoisted(() => ({ deniedPermissions: new Set<string>() }));
vi.mock('@/hooks/usePermission', () => ({
  usePermission: (key: string) => !deniedPermissions.has(key),
}));
vi.mock('@/hooks/useSyncFeedback', () => ({ useSyncFeedback: () => vi.fn() }));
vi.mock('@/hooks/useUnsavedChangesWarning', () => ({ useUnsavedChangesWarning: () => {} }));
vi.mock('@/api/services', () => ({ providerApi: {}, credentialApi: {}, systemApi: {} }));

describe('useProviderDetail', () => {
  beforeEach(() => {
    mutateCalls.length = 0;
    navigate.mockClear();
    deniedPermissions.clear();
    provider = {
      id: 'prov-1', name: 'openai', displayName: 'OpenAI', description: 'd',
      baseUrl: 'https://api', adapterType: 'openai', region: '', apiVersion: '', enabled: true,
    };
  });

  it('filters credentials to this provider only', () => {
    const { result } = renderHook(() => useProviderDetail());
    expect(result.current.credentials.map((c) => c.id)).toEqual(['c1']);
    expect(result.current.models).toHaveLength(1);
    expect(result.current.canUpdate).toBe(true);
  });

  // The model endpoints (PUT/DELETE /models/:id) are guarded on the model
  // resource, so a principal granted every provider action but denied
  // model.update must not be told it may write a model row. Granting
  // provider.update must not imply it.
  it('denies model updates to a principal that holds provider.update but not model.update', () => {
    deniedPermissions.add('model:update');
    const { result } = renderHook(() => useProviderDetail());
    expect(result.current.canUpdate).toBe(true);
    expect(result.current.canUpdateModel).toBe(false);
  });

  it('denies model deletes to a principal that holds provider.delete but not model.delete', () => {
    deniedPermissions.add('model:delete');
    const { result } = renderHook(() => useProviderDetail());
    expect(result.current.canDelete).toBe(true);
    expect(result.current.canDeleteModel).toBe(false);
  });

  it('grants model writes on the model actions alone, without the provider write actions', () => {
    deniedPermissions.add('provider:update');
    deniedPermissions.add('provider:delete');
    const { result } = renderHook(() => useProviderDetail());
    expect(result.current.canUpdateModel).toBe(true);
    expect(result.current.canDeleteModel).toBe(true);
  });

  it('startEditing seeds the provider form; handleSave maps empties → undefined', () => {
    const { result } = renderHook(() => useProviderDetail());
    act(() => result.current.startEditing());
    expect(result.current.isEditing).toBe(true);
    act(() => result.current.handleSave());
    expect(mutateCalls).toContainEqual(
      expect.objectContaining({ name: 'openai', baseUrl: 'https://api', region: undefined, apiVersion: undefined, enabled: true }),
    );
  });

  it('startEditingModel + handleModelUpdate transform the model payload', () => {
    const { result } = renderHook(() => useProviderDetail());
    const m = {
      id: 'm1', code: 'gpt', providerModelId: 'gpt-4o', name: 'GPT', description: 'x',
      inputPricePerMillion: 2.5, outputPricePerMillion: 10, type: 'chat', status: 'active',
      features: ['vision'], aliases: ['g4', 'gpt4'], enabled: true, capabilityJson: null,
    } as never;
    act(() => result.current.startEditingModel(m));
    expect(result.current.editingModelId).toBe('m1');
    act(() => result.current.handleModelUpdate());
    const call = mutateCalls.find((c) => (c as { id?: string }).id === 'm1') as { payload: Record<string, unknown> };
    expect(call.payload).toMatchObject({
      code: 'gpt', providerModelId: 'gpt-4o', name: 'GPT',
      inputPricePerMillion: 2.5, outputPricePerMillion: 10,
      type: 'chat', enabled: true, aliases: ['g4', 'gpt4'],
      capabilityJson: null, // editingCapabilityJson initialised to null → included
    });
  });

  it('startEditingCred + handleCredUpdate format expiresAt and omit blank apiKey', () => {
    const { result } = renderHook(() => useProviderDetail());
    const c = { id: 'c1', name: 'cred-a', enabled: true, expiresAt: '2026-12-31T00:00:00Z' } as never;
    act(() => result.current.startEditingCred(c));
    act(() => result.current.handleCredUpdate());
    const call = mutateCalls.find((x) => (x as { id?: string }).id === 'c1') as { payload: Record<string, unknown> };
    expect(call.payload).toMatchObject({ name: 'cred-a', enabled: true, expiresAt: '2026-12-31T00:00:00Z' });
    expect('apiKey' in call.payload).toBe(false); // blank apiKey not sent
  });

  it('tab + dialog setters update state', () => {
    const { result } = renderHook(() => useProviderDetail());
    act(() => result.current.setActiveTab('models'));
    expect(result.current.activeTab).toBe('models');
    act(() => result.current.setShowCredForm(true));
    expect(result.current.showCredForm).toBe(true);
    act(() => result.current.toggleEnabled(false));
    expect(mutateCalls).toContain(false);
  });
});
