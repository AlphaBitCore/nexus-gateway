/**
 * Unit tests for the catalog-sync surface: the button's permission gate, the
 * per-bucket checkbox defaults, and what an apply actually writes.
 */
import { describe, it, expect, vi, beforeEach } from 'vitest';
import { render, screen, waitFor } from '@testing-library/react';
import userEvent from '@testing-library/user-event';
import type { ApiProviderTemplate, Model, Provider } from '../../../../../src/api/types';
import type { ProviderDetailState } from '../../../../../src/pages/ai-gateway/providers/detail/useProviderDetail';

const addModel = vi.fn();
const updateModel = vi.fn();
const getTemplates = vi.fn();
const getTemplateDetail = vi.fn();

vi.mock('../../../../../src/api/services', () => ({
  providerApi: {
    getTemplates: (...a: unknown[]) => getTemplates(...a),
    getTemplateDetail: (...a: unknown[]) => getTemplateDetail(...a),
    addModel: (...a: unknown[]) => addModel(...a),
  },
  systemApi: {
    updateModel: (...a: unknown[]) => updateModel(...a),
  },
  credentialApi: {},
}));

// t() returns the key so queries can match on it, but it is a spy: the
// partial-failure copy carries its facts in the interpolation params, which is
// what the toast tells the admin.
const { tSpy } = vi.hoisted(() => ({ tSpy: vi.fn((k: string) => k) }));
vi.mock('react-i18next', () => ({
  useTranslation: () => ({ t: tSpy }),
  I18nextProvider: ({ children }: { children: React.ReactNode }) => children,
  initReactI18next: { type: '3rdParty', init: () => {} },
}));

vi.mock('../../../../../src/i18n', () => ({
  default: { t: (k: string) => k, language: 'en', changeLanguage: () => Promise.resolve() },
  SUPPORTED_LANGUAGES: [{ code: 'en', name: 'English' }],
  LANGUAGE_STORAGE_KEY: 'nexus-language',
}));

const addToast = vi.fn();
vi.mock('../../../../../src/context/ToastContext', () => ({
  useToast: () => ({ addToast }),
}));

vi.mock('../../../../../src/pages/ai-gateway/providers/detail/ModelFormDrawer', () => ({
  ModelFormDrawer: () => null,
}));

import { QueryClient, QueryClientProvider } from '@tanstack/react-query';
import { CatalogSyncDialog } from '../../../../../src/pages/ai-gateway/providers/detail/CatalogSyncDialog';
import { ProviderModelsTab } from '../../../../../src/pages/ai-gateway/providers/detail/ProviderModelsTab';

const PROVIDER: Provider = {
  id: 'provider-1',
  name: 'anthropic',
  displayName: 'Anthropic',
  baseUrl: 'https://api.anthropic.com',
  adapterType: 'anthropic',
  enabled: true,
} as Provider;

const TEMPLATES: ApiProviderTemplate[] = [
  { name: 'anthropic', displayName: 'Anthropic', description: '', baseUrl: 'https://api.anthropic.com', adapterType: 'anthropic' },
];

function makeModel(o: Partial<Model> = {}): Model {
  return {
    id: 'm-1',
    code: 'claude-haiku-4-5',
    name: 'Claude Haiku 4.5',
    providerId: 'provider-1',
    providerModelId: 'claude-haiku-4-5',
    type: 'chat',
    features: [],
    enabled: true,
    ...o,
  };
}

function withClient(ui: React.ReactNode) {
  const client = new QueryClient({ defaultOptions: { mutations: { retry: false }, queries: { retry: false } } });
  return <QueryClientProvider client={client}>{ui}</QueryClientProvider>;
}

function renderDialog(
  models: Model[],
  spies: { onApplied?: () => void; onClose?: () => void } = {},
) {
  return render(
    withClient(
      <CatalogSyncDialog
        open
        onClose={spies.onClose ?? vi.fn()}
        provider={PROVIDER}
        models={models}
        onApplied={spies.onApplied ?? vi.fn()}
      />,
    ),
  );
}

beforeEach(() => {
  vi.clearAllMocks();
  getTemplates.mockResolvedValue({ data: TEMPLATES });
  addModel.mockResolvedValue({});
  updateModel.mockResolvedValue({});
});

describe('ProviderModelsTab — sync button permission gate', () => {
  function makeDetail(o: Partial<ProviderDetailState>): ProviderDetailState {
    return {
      provider: PROVIDER,
      models: [],
      refetchModels: vi.fn(),
      canUpdate: false,
      canDelete: false,
      canCreateModel: false,
      canUpdateModel: false,
      canDeleteModel: false,
      showModelForm: false,
      setShowModelForm: vi.fn(),
      resetModelForm: vi.fn(),
      editingModelId: null,
      setEditingModelId: vi.fn(),
      startEditingModel: vi.fn(),
      setEditingCapabilityJson: vi.fn(),
      toggleModelEnabled: vi.fn(),
      setDeletingModel: vi.fn(),
      ...o,
    } as unknown as ProviderDetailState;
  }

  const KEY = 'pages:providers.catalogSyncButton';

  it('shows the button when the admin may both create and update models', () => {
    render(withClient(<ProviderModelsTab detail={makeDetail({ canCreateModel: true, canUpdateModel: true })} />));
    expect(screen.getByText(KEY)).toBeInTheDocument();
  });

  it('hides the button without the model update permission — syncing rewrites existing rows', () => {
    render(withClient(<ProviderModelsTab detail={makeDetail({ canCreateModel: true, canUpdateModel: false })} />));
    expect(screen.queryByText(KEY)).not.toBeInTheDocument();
  });

  it('hides the button without the create permission — syncing adds rows', () => {
    render(withClient(<ProviderModelsTab detail={makeDetail({ canCreateModel: false, canUpdateModel: true })} />));
    expect(screen.queryByText(KEY)).not.toBeInTheDocument();
  });

  it('hides the button when the admin may do neither', () => {
    render(withClient(<ProviderModelsTab detail={makeDetail({})} />));
    expect(screen.queryByText(KEY)).not.toBeInTheDocument();
  });

  // A custom policy can grant every provider action plus model.create while
  // withholding model.update. Offering sync to that admin lets the apply commit
  // its new rows and then be rejected on the first changed row — a write the
  // backend was always going to refuse. The provider permission must not stand
  // in for the model one.
  it('hides the button from an admin who may update the provider but not models', () => {
    render(
      withClient(
        <ProviderModelsTab
          detail={makeDetail({
            canCreateModel: true,
            canUpdate: true,
            canDelete: true,
            canUpdateModel: false,
          })}
        />,
      ),
    );
    expect(screen.queryByText(KEY)).not.toBeInTheDocument();
  });

  it('shows the button to an admin holding only the model actions, without provider writes', () => {
    render(
      withClient(
        <ProviderModelsTab
          detail={makeDetail({
            canCreateModel: true,
            canUpdateModel: true,
            canUpdate: false,
            canDelete: false,
          })}
        />,
      ),
    );
    expect(screen.getByText(KEY)).toBeInTheDocument();
  });
});

describe('ProviderModelsTab — per-model action gates', () => {
  function makeDetail(o: Partial<ProviderDetailState>): ProviderDetailState {
    return {
      provider: PROVIDER,
      models: [makeModel()],
      refetchModels: vi.fn(),
      canUpdate: false,
      canDelete: false,
      canCreateModel: false,
      canUpdateModel: false,
      canDeleteModel: false,
      showModelForm: false,
      setShowModelForm: vi.fn(),
      resetModelForm: vi.fn(),
      editingModelId: null,
      setEditingModelId: vi.fn(),
      startEditingModel: vi.fn(),
      setEditingCapabilityJson: vi.fn(),
      toggleModelEnabled: vi.fn(),
      setDeletingModel: vi.fn(),
      ...o,
    } as unknown as ProviderDetailState;
  }

  // Edit and enable/disable both write PUT /models/:id; delete writes
  // DELETE /models/:id. Each is guarded on the model resource, so the provider
  // permissions must not reveal them.
  it('hides Edit and Disable from an admin who may update the provider but not models', () => {
    render(withClient(<ProviderModelsTab detail={makeDetail({ canUpdate: true })} />));
    expect(screen.queryByText('common:edit')).not.toBeInTheDocument();
    expect(screen.queryByText('pages:providers.disable')).not.toBeInTheDocument();
  });

  it('shows Edit and Disable to an admin holding model.update alone', () => {
    render(withClient(<ProviderModelsTab detail={makeDetail({ canUpdateModel: true })} />));
    expect(screen.getByText('common:edit')).toBeInTheDocument();
    expect(screen.getByText('pages:providers.disable')).toBeInTheDocument();
  });

  it('hides Delete from an admin who may delete the provider but not models', () => {
    render(withClient(<ProviderModelsTab detail={makeDetail({ canDelete: true })} />));
    expect(screen.queryByText('common:delete')).not.toBeInTheDocument();
  });

  it('shows Delete to an admin holding model.delete alone', () => {
    render(withClient(<ProviderModelsTab detail={makeDetail({ canDeleteModel: true })} />));
    expect(screen.getByText('common:delete')).toBeInTheDocument();
  });
});

describe('CatalogSyncDialog — checkbox defaults', () => {
  it('checks a NEW model by default and unchecks a CHANGED one', async () => {
    getTemplateDetail.mockResolvedValue({
      ...TEMPLATES[0],
      models: [
        { code: 'claude-haiku-4-5', name: 'Claude Haiku 4.5', description: '', providerModelId: 'claude-haiku-4-5', type: 'chat', features: [], maxOutputTokens: 64000 },
        { code: 'claude-opus-4-5', name: 'Claude Opus 4.5', description: '', providerModelId: 'claude-opus-4-5', type: 'chat', features: [] },
      ],
    });

    renderDialog([makeModel({ maxOutputTokens: 65536 })]);

    await waitFor(() => expect(screen.getByText('pages:providers.catalogSyncSectionNew')).toBeInTheDocument());
    expect(screen.getByText('pages:providers.catalogSyncSectionChanged')).toBeInTheDocument();

    const boxes = screen.getAllByRole('checkbox');
    expect(boxes).toHaveLength(2);
    // New model (rendered first) starts accepted; the changed one does not,
    // because the admin may have overridden the catalog on purpose.
    expect(boxes[0]).toHaveAttribute('data-state', 'checked');
    expect(boxes[1]).toHaveAttribute('data-state', 'unchecked');
  });

  it('shows the drifted field as a yours-to-catalog move so the admin sees what changes', async () => {
    getTemplateDetail.mockResolvedValue({
      ...TEMPLATES[0],
      models: [
        { code: 'claude-haiku-4-5', name: 'Claude Haiku 4.5', description: '', providerModelId: 'claude-haiku-4-5', type: 'chat', features: [], maxOutputTokens: 64000 },
      ],
    });

    renderDialog([makeModel({ maxOutputTokens: 65536 })]);

    await waitFor(() => expect(screen.getByText('pages:providers.catalogSyncSectionChanged')).toBeInTheDocument());
    expect(screen.getByText('pages:providers.modelTableOutput')).toBeInTheDocument();
    expect(screen.getByText('65536')).toBeInTheDocument();
    expect(screen.getByText('64000')).toBeInTheDocument();
  });

  it('reports an in-sync provider rather than an empty dialog', async () => {
    getTemplateDetail.mockResolvedValue({
      ...TEMPLATES[0],
      models: [
        { code: 'claude-haiku-4-5', name: 'Claude Haiku 4.5', description: '', providerModelId: 'claude-haiku-4-5', type: 'chat', features: [], maxOutputTokens: 64000 },
      ],
    });

    renderDialog([makeModel({ maxOutputTokens: 64000 })]);

    await waitFor(() => expect(screen.getByText('pages:providers.catalogSyncInSync')).toBeInTheDocument());
    expect(screen.getByText('pages:providers.catalogSyncApply').closest('button')).toBeDisabled();
  });

  it('surfaces a failed catalog load instead of showing an empty diff', async () => {
    getTemplateDetail.mockRejectedValue(new Error('network down'));

    renderDialog([makeModel()]);

    await waitFor(() => expect(screen.getByText('pages:providers.catalogSyncLoadFailed')).toBeInTheDocument());
    // A failed load must never be mistaken for "nothing to do".
    expect(screen.getByText('pages:providers.catalogSyncApply').closest('button')).toBeDisabled();
    expect(addModel).not.toHaveBeenCalled();
  });

  it('renders a PROVIDER-ONLY row with no checkbox, so it can never be written', async () => {
    getTemplateDetail.mockResolvedValue({ ...TEMPLATES[0], models: [] });

    renderDialog([makeModel({ id: 'm-custom', providerModelId: 'our-private-finetune' })]);

    await waitFor(() => expect(screen.getByText('pages:providers.catalogSyncSectionProviderOnly')).toBeInTheDocument());
    expect(screen.queryAllByRole('checkbox')).toHaveLength(0);
    // Nothing is selectable, so apply is inert.
    expect(screen.getByText('pages:providers.catalogSyncApply').closest('button')).toBeDisabled();
  });
});

describe('CatalogSyncDialog — apply', () => {
  it('creates the accepted new model and never touches the provider-only row', async () => {
    getTemplateDetail.mockResolvedValue({
      ...TEMPLATES[0],
      models: [
        { code: 'claude-opus-4-5', name: 'Claude Opus 4.5', description: '', providerModelId: 'claude-opus-4-5', type: 'chat', features: [], maxOutputTokens: 64000 },
      ],
    });

    renderDialog([makeModel({ id: 'm-custom', providerModelId: 'our-private-finetune' })]);

    await waitFor(() => expect(screen.getByText('pages:providers.catalogSyncSectionNew')).toBeInTheDocument());
    await userEvent.click(screen.getByText('pages:providers.catalogSyncApply'));

    await waitFor(() => expect(addModel).toHaveBeenCalledTimes(1));
    expect(addModel).toHaveBeenCalledWith('provider-1', expect.objectContaining({
      code: 'claude-opus-4-5',
      providerModelId: 'claude-opus-4-5',
      maxOutputTokens: 64000,
    }));
    // The provider-only row is informational — never created, never deleted.
    expect(updateModel).not.toHaveBeenCalled();
  });

  it('writes only the differing fields of a CHANGED row the admin accepted', async () => {
    getTemplateDetail.mockResolvedValue({
      ...TEMPLATES[0],
      models: [
        { code: 'claude-haiku-4-5', name: 'Claude Haiku 4.5', description: '', providerModelId: 'claude-haiku-4-5', type: 'chat', features: [], maxOutputTokens: 64000 },
      ],
    });

    renderDialog([makeModel({ name: 'Claude Haiku 4.5', maxOutputTokens: 65536 })]);

    await waitFor(() => expect(screen.getByText('pages:providers.catalogSyncSectionChanged')).toBeInTheDocument());
    await userEvent.click(screen.getAllByRole('checkbox')[0]);
    await userEvent.click(screen.getByText('pages:providers.catalogSyncApply'));

    await waitFor(() => expect(updateModel).toHaveBeenCalledTimes(1));
    expect(updateModel).toHaveBeenCalledWith('m-1', { maxOutputTokens: 64000 });
    expect(addModel).not.toHaveBeenCalled();
  });

  it('writes nothing when the admin unchecks the default-accepted new model', async () => {
    getTemplateDetail.mockResolvedValue({
      ...TEMPLATES[0],
      models: [
        { code: 'claude-opus-4-5', name: 'Claude Opus 4.5', description: '', providerModelId: 'claude-opus-4-5', type: 'chat', features: [] },
      ],
    });

    renderDialog([]);

    await waitFor(() => expect(screen.getByText('pages:providers.catalogSyncSectionNew')).toBeInTheDocument());
    await userEvent.click(screen.getAllByRole('checkbox')[0]);

    expect(screen.getByText('pages:providers.catalogSyncApply').closest('button')).toBeDisabled();
    expect(addModel).not.toHaveBeenCalled();
  });

  it('tells the admin the apply succeeded and refreshes the tab when every row commits', async () => {
    const onApplied = vi.fn();
    const onClose = vi.fn();
    getTemplateDetail.mockResolvedValue({
      ...TEMPLATES[0],
      models: [
        { code: 'claude-opus-4-5', name: 'Claude Opus 4.5', description: '', providerModelId: 'claude-opus-4-5', type: 'chat', features: [] },
      ],
    });

    renderDialog([], { onApplied, onClose });

    await waitFor(() => expect(screen.getByText('pages:providers.catalogSyncSectionNew')).toBeInTheDocument());
    await userEvent.click(screen.getByText('pages:providers.catalogSyncApply'));

    await waitFor(() => expect(onApplied).toHaveBeenCalledTimes(1));
    expect(onClose).toHaveBeenCalledTimes(1);
    expect(addToast).toHaveBeenCalledWith('pages:providers.catalogSyncApplied', 'success');
  });

  it('reports a provider with no matching catalog instead of guessing one', async () => {
    render(
      withClient(
        <CatalogSyncDialog
          open
          onClose={vi.fn()}
          provider={{ ...PROVIDER, name: 'local-vllm', adapterType: 'openai', baseUrl: 'http://10.0.0.5:8000' }}
          models={[]}
          onApplied={vi.fn()}
        />,
      ),
    );

    await waitFor(() => expect(screen.getByText('pages:providers.catalogSyncNoTemplate')).toBeInTheDocument());
    expect(getTemplateDetail).not.toHaveBeenCalled();
  });
});

/**
 * A model code is globally unique, so two providers for the same vendor
 * (openai-prod / openai-dev) resolve to the same template and the second one's
 * apply is rejected with MODEL_CODE_EXISTS on rows the first already owns. The
 * rows are written one per request, so that rejection lands mid-apply, with
 * earlier rows already committed.
 */
describe('CatalogSyncDialog — a row that fails mid-apply', () => {
  const TWO_NEW = {
    ...TEMPLATES[0],
    models: [
      { code: 'claude-opus-4-5', name: 'Claude Opus 4.5', description: '', providerModelId: 'claude-opus-4-5', type: 'chat', features: [] },
      { code: 'claude-sonnet-4-5', name: 'Claude Sonnet 4.5', description: '', providerModelId: 'claude-sonnet-4-5', type: 'chat', features: [] },
    ],
  };

  /** Rejects the named code the way the backend rejects a duplicate. */
  function rejectCode(code: string) {
    addModel.mockImplementation((_providerId: string, input: { code: string }) =>
      input.code === code
        ? Promise.reject(new Error('MODEL_CODE_EXISTS'))
        : Promise.resolve({}),
    );
  }

  it('still writes the rows behind the rejected one', async () => {
    getTemplateDetail.mockResolvedValue(TWO_NEW);
    rejectCode('claude-opus-4-5');

    renderDialog([]);

    await waitFor(() => expect(screen.getByText('pages:providers.catalogSyncSectionNew')).toBeInTheDocument());
    await userEvent.click(screen.getByText('pages:providers.catalogSyncApply'));

    // Both rows are attempted: one rejection must not silently drop the rest of
    // the diff the admin accepted.
    await waitFor(() => expect(addModel).toHaveBeenCalledTimes(2));
    expect(addModel.mock.calls.map((c) => (c[1] as { code: string }).code).sort())
      .toEqual(['claude-opus-4-5', 'claude-sonnet-4-5']);
  });

  it('carries on into the changed rows when a new row is rejected', async () => {
    getTemplateDetail.mockResolvedValue({
      ...TEMPLATES[0],
      models: [
        { code: 'claude-opus-4-5', name: 'Claude Opus 4.5', description: '', providerModelId: 'claude-opus-4-5', type: 'chat', features: [] },
        { code: 'claude-haiku-4-5', name: 'Claude Haiku 4.5', description: '', providerModelId: 'claude-haiku-4-5', type: 'chat', features: [], maxOutputTokens: 64000 },
      ],
    });
    rejectCode('claude-opus-4-5');

    renderDialog([makeModel({ name: 'Claude Haiku 4.5', maxOutputTokens: 65536 })]);

    await waitFor(() => expect(screen.getByText('pages:providers.catalogSyncSectionChanged')).toBeInTheDocument());
    // Accept the changed row too; the new row is accepted by default.
    await userEvent.click(screen.getAllByRole('checkbox')[1]);
    await userEvent.click(screen.getByText('pages:providers.catalogSyncApply'));

    // The rejected create must not abandon the accepted update.
    await waitFor(() => expect(updateModel).toHaveBeenCalledTimes(1));
    expect(updateModel).toHaveBeenCalledWith('m-1', { maxOutputTokens: 64000 });
  });

  it('refetches the tab so the committed rows are on screen', async () => {
    const onApplied = vi.fn();
    getTemplateDetail.mockResolvedValue(TWO_NEW);
    rejectCode('claude-opus-4-5');

    renderDialog([], { onApplied });

    await waitFor(() => expect(screen.getByText('pages:providers.catalogSyncSectionNew')).toBeInTheDocument());
    await userEvent.click(screen.getByText('pages:providers.catalogSyncApply'));

    // The row that committed exists in the database; the tab has to show it.
    await waitFor(() => expect(onApplied).toHaveBeenCalledTimes(1));
  });

  it('closes the dialog rather than leaving the now-stale diff on screen', async () => {
    const onClose = vi.fn();
    getTemplateDetail.mockResolvedValue(TWO_NEW);
    rejectCode('claude-opus-4-5');

    renderDialog([], { onClose });

    await waitFor(() => expect(screen.getByText('pages:providers.catalogSyncSectionNew')).toBeInTheDocument());
    await userEvent.click(screen.getByText('pages:providers.catalogSyncApply'));

    // The diff was a snapshot; re-applying it would re-send the committed row
    // and be rejected as a duplicate. Reopening recomputes it.
    await waitFor(() => expect(onClose).toHaveBeenCalledTimes(1));
  });

  it('never reports success, and names the row that failed', async () => {
    getTemplateDetail.mockResolvedValue(TWO_NEW);
    rejectCode('claude-opus-4-5');

    renderDialog([]);

    await waitFor(() => expect(screen.getByText('pages:providers.catalogSyncSectionNew')).toBeInTheDocument());
    await userEvent.click(screen.getByText('pages:providers.catalogSyncApply'));

    await waitFor(() =>
      expect(addToast).toHaveBeenCalledWith('pages:providers.catalogSyncPartialFailure', 'error'),
    );
    // A partial apply is never a success.
    expect(addToast).not.toHaveBeenCalledWith('pages:providers.catalogSyncApplied', 'success');
    // The admin is told what committed and exactly which row did not.
    expect(tSpy).toHaveBeenCalledWith('pages:providers.catalogSyncPartialFailure', {
      applied: 1,
      total: 2,
      failed: 'Claude Opus 4.5',
    });
  });

  it('names every failed row when the whole apply fails', async () => {
    getTemplateDetail.mockResolvedValue(TWO_NEW);
    addModel.mockRejectedValue(new Error('network down'));

    renderDialog([]);

    await waitFor(() => expect(screen.getByText('pages:providers.catalogSyncSectionNew')).toBeInTheDocument());
    await userEvent.click(screen.getByText('pages:providers.catalogSyncApply'));

    await waitFor(() =>
      expect(tSpy).toHaveBeenCalledWith('pages:providers.catalogSyncPartialFailure', {
        applied: 0,
        total: 2,
        failed: 'Claude Opus 4.5, Claude Sonnet 4.5',
      }),
    );
    expect(addToast).not.toHaveBeenCalledWith('pages:providers.catalogSyncApplied', 'success');
  });
});
