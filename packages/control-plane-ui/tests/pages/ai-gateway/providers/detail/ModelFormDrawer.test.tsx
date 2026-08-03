import { describe, it, expect, vi, beforeEach } from 'vitest';
import { render, screen, fireEvent, waitFor } from '@testing-library/react';
import { useForm } from 'react-hook-form';
import { I18nextProvider } from 'react-i18next';
import i18n from '@/i18n';
import { ModelFormDrawer } from '@/pages/ai-gateway/providers/detail/ModelFormDrawer';

const sys = vi.hoisted(() => ({ systemApi: { listModelsFlat: vi.fn() } }));
vi.mock('@/api/services', () => sys);

const createDefaults = {
  modelName: 'GPT-X', modelCode: 'gpt-x', modelProviderModelId: 'gpt-x', modelType: 'audio',
  modelDescription: '', modelInputPrice: '', modelOutputPrice: '',
  modelCachedInputReadPrice: '', modelCachedInputWritePrice: '',
  modelMaxContext: '', modelMaxOutput: '', modelSelectedFeatures: [], modelAliases: '',
};
const editDefaults = {
  editModelName: 'GPT-Y', editModelCode: 'gpt-y', editModelProviderModelId: 'gpt-y', editModelType: 'audio',
  editModelStatus: 'active', editModelDescription: '', editModelInputPrice: '', editModelOutputPrice: '',
  editModelCachedInputReadPrice: '', editModelCachedInputWritePrice: '',
  editModelMaxContext: '', editModelMaxOutput: '', editModelFeatures: [], editModelAliases: '',
  editModelEnabled: true, editModelDeprecationDate: '', editModelReplacedBy: '',
};

const handlers = { createModel: vi.fn(), handleModelUpdate: vi.fn(), onClose: vi.fn(), setEditingCapabilityJson: vi.fn() };

function Harness({
  mode,
  canUpdateModel = true,
  editType,
}: {
  mode: 'create' | 'edit';
  canUpdateModel?: boolean;
  editType?: string;
}) {
  const newModelForm = useForm({ defaultValues: createDefaults });
  const editModelForm = useForm({
    defaultValues: editType ? { ...editDefaults, editModelType: editType } : editDefaults,
  });
  const detail = {
    newModelForm, editModelForm,
    editingModelId: 'm-edit',
    createModel: handlers.createModel, handleModelUpdate: handlers.handleModelUpdate,
    modelCreating: false, modelUpdating: false,
    canCreateModel: true, canUpdateModel,
    editingCapabilityJson: undefined, setEditingCapabilityJson: handlers.setEditingCapabilityJson,
  } as never;
  return <ModelFormDrawer detail={detail} mode={mode} open onClose={handlers.onClose} />;
}

function wrap(mode: 'create' | 'edit') {
  return render(<I18nextProvider i18n={i18n}><Harness mode={mode} /></I18nextProvider>);
}

describe('ModelFormDrawer — create mode', () => {
  beforeEach(() => {
    vi.clearAllMocks();
    sys.systemApi.listModelsFlat.mockResolvedValue({ data: [] });
  });

  it('renders the add-model drawer with the identity fields', () => {
    wrap('create');
    expect(screen.getByText(i18n.t('pages:providers.addModel'))).toBeInTheDocument();
    expect(screen.getByDisplayValue('GPT-X')).toBeInTheDocument();
  });

  it('runs the debounced uniqueness check against listModelsFlat', async () => {
    wrap('create');
    await waitFor(() => expect(sys.systemApi.listModelsFlat).toHaveBeenCalledWith({ q: 'gpt-x', limit: '50' }));
  });

  it('flags a duplicate model code returned by the uniqueness check', async () => {
    sys.systemApi.listModelsFlat.mockResolvedValue({ data: [{ id: 'other', code: 'gpt-x' }] });
    wrap('create');
    await waitFor(() =>
      expect(screen.getByText(/model with this code already exists/i)).toBeInTheDocument(),
    );
  });

  it('Create builds the model payload from the form values', async () => {
    wrap('create');
    // wait for the uniqueness check to clear so the Create button enables
    await waitFor(() => expect(screen.getByRole('button', { name: i18n.t('common:create') })).toBeEnabled());
    fireEvent.click(screen.getByRole('button', { name: i18n.t('common:create') }));
    expect(handlers.createModel).toHaveBeenCalledWith(
      expect.objectContaining({ name: 'GPT-X', code: 'gpt-x', providerModelId: 'gpt-x', type: 'audio' }),
    );
  });

  it('Cancel closes the drawer', () => {
    wrap('create');
    fireEvent.click(screen.getByRole('button', { name: i18n.t('common:cancel') }));
    expect(handlers.onClose).toHaveBeenCalled();
  });
});

describe('ModelFormDrawer — edit mode', () => {
  beforeEach(() => {
    vi.clearAllMocks();
    sys.systemApi.listModelsFlat.mockResolvedValue({ data: [] });
  });

  it('renders the edit-model drawer and Save fires handleModelUpdate', async () => {
    wrap('edit');
    expect(screen.getByText(i18n.t('pages:providers.editModel', 'Edit model'))).toBeInTheDocument();
    await waitFor(() => expect(screen.getByRole('button', { name: i18n.t('common:save') })).toBeEnabled());
    fireEvent.click(screen.getByRole('button', { name: i18n.t('common:save') }));
    expect(handlers.handleModelUpdate).toHaveBeenCalled();
  });

  it('expands the lifecycle section on demand', () => {
    wrap('edit');
    const toggle = screen.getByRole('button', { name: /lifecycle/i });
    expect(toggle).toHaveAttribute('aria-expanded', 'false');
    fireEvent.click(toggle);
    expect(toggle).toHaveAttribute('aria-expanded', 'true');
  });
});

describe('ModelFormDrawer — edit-mode capability panel permission gate', () => {
  beforeEach(() => {
    vi.clearAllMocks();
    sys.systemApi.listModelsFlat.mockResolvedValue({ data: [] });
  });

  function wrapEdit(canUpdateModel: boolean) {
    return render(
      <I18nextProvider i18n={i18n}>
        <Harness mode="edit" canUpdateModel={canUpdateModel} editType="embedding" />
      </I18nextProvider>,
    );
  }

  // Saving the panel issues PUT /models/:id, guarded on model.update. An admin
  // without it must not be given editable capability controls; the provider
  // permission does not stand in for the model one.
  it('offers the capability editors to an admin holding model.update', () => {
    wrapEdit(true);
    expect(screen.getByText(i18n.t('pages:providers.capabilities.addDimensionButton'))).toBeInTheDocument();
  });

  it('withholds the capability editors from an admin without model.update', () => {
    wrapEdit(false);
    expect(screen.queryByText(i18n.t('pages:providers.capabilities.addDimensionButton'))).not.toBeInTheDocument();
  });
});
