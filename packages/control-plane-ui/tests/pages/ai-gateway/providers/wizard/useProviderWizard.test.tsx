import { describe, it, expect, vi, beforeEach } from 'vitest';
import { renderHook, act, waitFor } from '@testing-library/react';
import { useProviderWizard } from '@/pages/ai-gateway/providers/wizard/useProviderWizard';

const navigate = vi.fn();
const addToast = vi.fn();
const apiMock = vi.hoisted(() => ({
  providerApi: {
    list: vi.fn(),
    create: vi.fn(),
    getTemplateDetail: vi.fn(),
    discoverModels: vi.fn(),
  },
}));

vi.mock('react-router-dom', () => ({ useNavigate: () => navigate }));
// `t` MUST be a stable reference — the name-check effect depends on it, so a new
// closure each render would re-arm the debounce forever and never settle nameChecking.
const stableT = (k: string, d?: string) => d ?? k;
const i18nReturn = { t: stableT };
vi.mock('react-i18next', async (o) => ({ ...(await o<typeof import('react-i18next')>()), useTranslation: () => i18nReturn as never }));
vi.mock('@/context/ToastContext', () => ({ useToast: () => ({ addToast }) }));
vi.mock('@/hooks/useSyncFeedback', () => ({ useSyncFeedback: () => vi.fn() }));
vi.mock('@/hooks/useApi', () => ({ useApi: () => ({ data: { data: [] } }) }));
vi.mock('@/api/services', () => apiMock);

describe('useProviderWizard', () => {
  beforeEach(() => {
    navigate.mockClear(); addToast.mockClear();
    apiMock.providerApi.list.mockResolvedValue({ data: [] });
    apiMock.providerApi.create.mockReset();
  });

  it('canNext gates step 0 on a template/custom choice', () => {
    const { result } = renderHook(() => useProviderWizard());
    expect(result.current.canNext()).toBe(false);
    act(() => result.current.selectCustom());
    expect(result.current.isCustom).toBe(true);
  });

  it('addManualModel appends a selected model + resets the form; toggle/remove work', () => {
    const { result } = renderHook(() => useProviderWizard());
    act(() => result.current.setNewModelId('gpt-4o'));
    act(() => result.current.addManualModel());
    expect(result.current.models).toHaveLength(1);
    expect(result.current.models[0].selected).toBe(true);
    expect(result.current.newModelId).toBe(''); // reset
    act(() => result.current.toggleModel(0));
    expect(result.current.models[0].selected).toBe(false);
    act(() => result.current.removeModel(0));
    expect(result.current.models).toHaveLength(0);
  });

  it('addManualModel no-ops without a model id', () => {
    const { result } = renderHook(() => useProviderWizard());
    act(() => result.current.addManualModel());
    expect(result.current.models).toHaveLength(0);
  });

  it('handleSubmit posts provider + selected models + credential, then navigates', async () => {
    apiMock.providerApi.create.mockResolvedValue({ id: 'p-new' });
    const { result } = renderHook(() => useProviderWizard());
    act(() => {
      result.current.setName('openai');
      result.current.setBaseUrl('https://api');
      result.current.setCredName('default');
      result.current.setApiKey('sk-123');
      result.current.setNewModelId('gpt-4o');
    });
    act(() => result.current.addManualModel());
    await act(async () => { await result.current.handleSubmit(); });
    expect(apiMock.providerApi.create).toHaveBeenCalledWith(
      expect.objectContaining({
        name: 'openai', baseUrl: 'https://api',
        models: expect.arrayContaining([expect.objectContaining({ providerModelId: 'gpt-4o' })]),
        credential: { name: 'default', apiKey: 'sk-123' },
      }),
    );
    expect(navigate).toHaveBeenCalledWith('/ai-gateway/providers/p-new');
  });

  it('handleSubmit surfaces an error toast on failure', async () => {
    apiMock.providerApi.create.mockRejectedValue(new Error('dup name'));
    const { result } = renderHook(() => useProviderWizard());
    act(() => { result.current.setName('x'); result.current.setBaseUrl('u'); result.current.setSkipCredential(true); });
    await act(async () => { await result.current.handleSubmit(); });
    expect(addToast).toHaveBeenCalledWith('dup name', 'error');
    expect(result.current.error).toBe('dup name');
  });

  it('goNext/goBack/clearError + template query update step/state', () => {
    const { result } = renderHook(() => useProviderWizard());
    act(() => result.current.goNext());
    expect(result.current.step).toBe(1);
    act(() => result.current.goBack());
    expect(result.current.step).toBe(0);
    act(() => result.current.goBack()); // step 0 → navigate away
    expect(navigate).toHaveBeenCalledWith('/ai-gateway/providers');
    act(() => result.current.handleTemplateQueryChange('anthr'));
    expect(result.current.templateQuery).toBe('anthr');
  });

  it('canNext gates each wizard step on its own preconditions', async () => {
    const { result } = renderHook(() => useProviderWizard());
    // step 0: template/custom required
    act(() => result.current.selectCustom());
    expect(result.current.canNext()).toBe(true);
    // step 1: name + baseUrl required
    act(() => result.current.goNext());
    expect(result.current.canNext()).toBe(false);
    act(() => { result.current.setName('openai'); result.current.setBaseUrl('https://api'); });
    // the name-uniqueness debounce sets nameChecking; wait for it to settle (no conflict)
    await waitFor(() => expect(result.current.canNext()).toBe(true));
    // step 2: credential required unless skipped
    act(() => result.current.goNext());
    expect(result.current.canNext()).toBe(false);
    act(() => result.current.setSkipCredential(true));
    expect(result.current.canNext()).toBe(true);
    // step 3: no model-code conflicts → allowed
    act(() => result.current.goNext());
    expect(result.current.canNext()).toBe(true);
  });

  it('selectFromApiTemplate hydrates provider fields + the catalog models', async () => {
    apiMock.providerApi.getTemplateDetail.mockResolvedValue({
      models: [{ providerModelId: 'gpt-4o', name: 'GPT-4o', type: 'chat', features: ['vision'] }],
    });
    const { result } = renderHook(() => useProviderWizard());
    await act(async () => {
      await result.current.selectFromApiTemplate({ name: 'openai', displayName: 'OpenAI', baseUrl: 'https://api.openai.com', adapterType: 'openai', description: 'd' } as never);
    });
    expect(result.current.isCustom).toBe(false);
    expect(result.current.name).toBe('openai');
    expect(result.current.baseUrl).toBe('https://api.openai.com');
    expect(result.current.credName).toBe('openai-default');
    expect(result.current.models).toHaveLength(1);
    expect(result.current.models[0]).toMatchObject({ modelId: 'gpt-4o', name: 'GPT-4o', selected: true });
  });

  it('selectFromApiTemplate surfaces a non-fatal toast when the catalog fails to load', async () => {
    apiMock.providerApi.getTemplateDetail.mockRejectedValue(new Error('404'));
    const { result } = renderHook(() => useProviderWizard());
    await act(async () => {
      await result.current.selectFromApiTemplate({ name: 'broken', displayName: 'B', baseUrl: 'u', adapterType: 'openai', description: '' } as never);
    });
    // provider fields still hydrated; catalog left empty; error toasted (non-fatal)
    expect(result.current.name).toBe('broken');
    expect(result.current.models).toHaveLength(0);
    expect(addToast).toHaveBeenCalledWith(expect.any(String), 'error');
  });

  it('fetchModels appends discovered rows (chat + embedding) and deduplicates existing ids', async () => {
    apiMock.providerApi.discoverModels.mockResolvedValue({
      success: true,
      models: [
        { id: 'gpt-4o', suggestedType: 'chat' },
        { id: 'text-embedding-3-small', suggestedType: 'embedding' },
        { id: 'existing-model', suggestedType: 'chat' }, // already in the list
      ],
    });
    const { result } = renderHook(() => useProviderWizard());

    // Add an existing model manually first
    act(() => {
      result.current.setBaseUrl('https://api.openai.com');
      result.current.setApiKey('sk-test');
      result.current.selectCustom();
      result.current.setNewModelId('existing-model');
    });
    act(() => result.current.addManualModel());
    expect(result.current.models).toHaveLength(1);

    // Fetch — should add 2 new rows, skip the duplicate
    await act(async () => { await result.current.fetchModels(); });

    expect(result.current.models).toHaveLength(3); // 1 existing + 2 new
    const ids = result.current.models.map((m) => m.modelId);
    expect(ids).toContain('gpt-4o');
    expect(ids).toContain('text-embedding-3-small');
    expect(ids).toContain('existing-model');
    // Fetched rows carry the correct type but are opt-in (unselected by default)
    // so a provider returning dozens of models doesn't pre-check them all.
    const embedding = result.current.models.find((m) => m.modelId === 'text-embedding-3-small');
    expect(embedding?.type).toBe('embedding');
    expect(embedding?.selected).toBe(false);
    expect(result.current.fetchModelsUnsupported).toBe(false);
    expect(result.current.fetchModelsError).toBeNull();
  });

  it('fetchModels sets fetchModelsUnsupported when code is discovery_unsupported', async () => {
    apiMock.providerApi.discoverModels.mockResolvedValue({
      success: false,
      error: 'not supported',
      code: 'discovery_unsupported',
    });
    const { result } = renderHook(() => useProviderWizard());
    act(() => { result.current.setBaseUrl('https://x'); result.current.setApiKey('k'); });
    await act(async () => { await result.current.fetchModels(); });
    expect(result.current.fetchModelsUnsupported).toBe(true);
    expect(result.current.fetchModelsError).toBeNull();
    expect(result.current.models).toHaveLength(0);
  });

  it('fetchModels sets fetchModelsError on a generic failure response', async () => {
    apiMock.providerApi.discoverModels.mockResolvedValue({
      success: false,
      error: 'connection refused',
    });
    const { result } = renderHook(() => useProviderWizard());
    act(() => { result.current.setBaseUrl('https://x'); result.current.setApiKey('k'); });
    await act(async () => { await result.current.fetchModels(); });
    expect(result.current.fetchModelsError).toBe('connection refused');
    expect(result.current.fetchModelsUnsupported).toBe(false);
  });

  it('fetchModels sets fetchModelsError when discoverModels throws', async () => {
    apiMock.providerApi.discoverModels.mockRejectedValue(new Error('network timeout'));
    const { result } = renderHook(() => useProviderWizard());
    act(() => { result.current.setBaseUrl('https://x'); result.current.setApiKey('k'); });
    await act(async () => { await result.current.fetchModels(); });
    expect(result.current.fetchModelsError).toBe('network timeout');
    expect(result.current.fetchModelsUnsupported).toBe(false);
  });
});
