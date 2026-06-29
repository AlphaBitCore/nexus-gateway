/**
 * Unit tests for StepModels — the "Fetch from /v1/models" feature.
 *
 * Covers:
 *  (a) Static hint always renders for custom providers; hidden for template providers.
 *  (b) Fetch button enabled when baseUrl + credential apiKey are present;
 *      disabled otherwise (missing baseUrl, or missing apiKey without skipCredential).
 *  (c) After fetch succeeds: 2 pre-filled rows rendered with checkboxes UNCHECKED
 *      (opt-in: selected:false).
 *  (d) Count note renders after fetch (fetchModelsCount set).
 *  (e) Type-guess hint renders when fetchModelsCount is set and models exist.
 *  (f) Duplicate deduplication: wizard holds only the deduped set.
 *  (g) success:false with code discovery_unsupported → OpenAI-only alert renders.
 *  (h) Generic error shows inline error alert.
 */

import { describe, it, expect, vi, beforeEach } from 'vitest';
import { render, screen, fireEvent } from '@testing-library/react';
import { StepModels } from './StepModels';
import type { ProviderWizardHook } from './useProviderWizard';
import type { WizardModel } from './types';

// Stub out the i18n barrel so Vite doesn't try to resolve i18next-http-backend
// (not installed in this worktree). The components under test use t() via the
// react-i18next mock below — they don't need the real i18n instance.
vi.mock('@/i18n', () => ({
  default: { t: (k: string) => k, language: 'en', changeLanguage: () => Promise.resolve() },
  SUPPORTED_LANGUAGES: [{ code: 'en', name: 'English' }],
  LANGUAGE_STORAGE_KEY: 'nexus-language',
}));

// Mock react-i18next completely so no real i18n bundle is loaded.
// t(key) returns the key string; I18nextProvider is a passthrough wrapper.
vi.mock('react-i18next', () => ({
  useTranslation: () => ({ t: (k: string) => k }),
  I18nextProvider: ({ children }: { children: React.ReactNode }) => children,
  initReactI18next: { type: '3rdParty', init: () => {} },
}));

// ── Helpers ────────────────────────────────────────────────────────────────

type WizardOverrides = Partial<ProviderWizardHook>;

function makeWizard(overrides: WizardOverrides = {}): ProviderWizardHook {
  const defaults: ProviderWizardHook = {
    t: ((k: string) => k) as unknown as ProviderWizardHook['t'],
    navigate: vi.fn() as any,
    // template data
    templates: [],
    templatesLoading: false,
    templatesError: null,
    refetchTemplates: vi.fn(),
    templateQuery: '',
    handleTemplateQueryChange: vi.fn(),
    filteredTemplates: [],
    browseAllTemplates: false,
    setBrowseAllTemplates: vi.fn(),
    templatesForGrid: [],
    collapsedHiddenCount: 0,
    // wizard step
    step: 3,
    submitting: false,
    error: null,
    clearError: vi.fn(),
    // step 0
    selectedTemplate: null,
    isCustom: true,
    selectFromApiTemplate: vi.fn(),
    selectCustom: vi.fn(),
    // step 1
    name: 'my-provider',
    setName: vi.fn(),
    nameError: null,
    nameChecking: false,
    displayName: 'My Provider',
    setDisplayName: vi.fn(),
    baseUrl: 'https://api.example.com',
    setBaseUrl: vi.fn(),
    adapterType: 'openai',
    setAdapterType: vi.fn(),
    description: '',
    setDescription: vi.fn(),
    // step 2
    credName: 'my-cred',
    setCredName: vi.fn(),
    credNameError: null,
    credNameChecking: false,
    apiKey: 'sk-test-key',
    setApiKey: vi.fn(),
    skipCredential: false,
    setSkipCredential: vi.fn(),
    // step 3
    models: [],
    manualMode: false,
    setManualMode: vi.fn(),
    newModelId: '',
    setNewModelId: vi.fn(),
    newModelName: '',
    setNewModelName: vi.fn(),
    newModelDescription: '',
    setNewModelDescription: vi.fn(),
    newModelType: 'chat',
    setNewModelType: vi.fn(),
    newModelInputPrice: '',
    setNewModelInputPrice: vi.fn(),
    newModelOutputPrice: '',
    setNewModelOutputPrice: vi.fn(),
    newModelCachedInputReadPrice: '',
    setNewModelCachedInputReadPrice: vi.fn(),
    newModelCachedInputWritePrice: '',
    setNewModelCachedInputWritePrice: vi.fn(),
    newModelMaxContext: '',
    setNewModelMaxContext: vi.fn(),
    newModelMaxOutput: '',
    setNewModelMaxOutput: vi.fn(),
    newModelFeatures: [],
    setNewModelFeatures: vi.fn(),
    resetManualModelForm: vi.fn(),
    addManualModel: vi.fn(),
    toggleModel: vi.fn(),
    removeModel: vi.fn(),
    existingModelCodes: new Set<string>(),
    modelCodeConflicts: [],
    updateModelId: vi.fn(),
    fetchModels: vi.fn(),
    fetchingModels: false,
    fetchModelsError: null,
    fetchModelsUnsupported: false,
    fetchModelsCount: null,
    // navigation
    canNext: () => true,
    goBack: vi.fn(),
    goNext: vi.fn(),
    handleSubmit: vi.fn(),
  };
  return { ...defaults, ...overrides };
}

function renderStep(wizard: ProviderWizardHook) {
  return render(<StepModels wizard={wizard} />);
}

// ── Tests ──────────────────────────────────────────────────────────────────

describe('StepModels — fetch from /v1/models', () => {
  beforeEach(() => {
    vi.clearAllMocks();
  });

  describe('static hint', () => {
    it('renders the OpenAI-compatible hint for custom providers', () => {
      renderStep(makeWizard());
      expect(
        screen.getByText('pages:providers.wizardModelsFetchHint'),
      ).toBeInTheDocument();
    });

    it('does NOT render the fetch section for template (non-custom) providers', () => {
      renderStep(makeWizard({ isCustom: false, selectedTemplate: 'openai' }));
      expect(
        screen.queryByText('pages:providers.wizardModelsFetchHint'),
      ).not.toBeInTheDocument();
    });
  });

  describe('Fetch button enabled / disabled', () => {
    it('is enabled when baseUrl and apiKey are both present', () => {
      renderStep(makeWizard({ baseUrl: 'https://api.example.com', apiKey: 'sk-key' }));
      const btn = screen.getByRole('button', { name: 'pages:providers.wizardModelsFetchButton' });
      expect(btn).not.toBeDisabled();
    });

    it('is disabled when baseUrl is empty', () => {
      renderStep(makeWizard({ baseUrl: '', apiKey: 'sk-key' }));
      const btn = screen.getByRole('button', { name: 'pages:providers.wizardModelsFetchButton' });
      expect(btn).toBeDisabled();
    });

    it('is disabled when apiKey is empty and skipCredential is false', () => {
      renderStep(makeWizard({ baseUrl: 'https://api.example.com', apiKey: '', skipCredential: false }));
      const btn = screen.getByRole('button', { name: 'pages:providers.wizardModelsFetchButton' });
      expect(btn).toBeDisabled();
    });

    it('is enabled when apiKey is empty but skipCredential is true', () => {
      renderStep(makeWizard({ baseUrl: 'https://api.example.com', apiKey: '', skipCredential: true }));
      const btn = screen.getByRole('button', { name: 'pages:providers.wizardModelsFetchButton' });
      expect(btn).not.toBeDisabled();
    });

    it('shows "Fetching…" label and is disabled while fetchingModels is true', () => {
      renderStep(makeWizard({ fetchingModels: true }));
      const btn = screen.getByRole('button', { name: 'pages:providers.wizardModelsFetching' });
      expect(btn).toBeDisabled();
    });
  });

  describe('Fetch click', () => {
    it('calls fetchModels on click', () => {
      const fetchModels = vi.fn();
      renderStep(makeWizard({ fetchModels }));
      const btn = screen.getByRole('button', { name: 'pages:providers.wizardModelsFetchButton' });
      fireEvent.click(btn);
      expect(fetchModels).toHaveBeenCalledTimes(1);
    });
  });

  describe('Rendered rows after successful fetch', () => {
    it('renders 2 pre-filled rows with UNCHECKED checkboxes (opt-in: selected:false)', () => {
      // Fetched models must default to selected:false so admins opt-in rather
      // than opt-out.
      const fetchedModels: WizardModel[] = [
        {
          modelId: 'gpt-4o',
          name: 'gpt-4o',
          description: '',
          type: 'chat',
          inputPrice: '',
          outputPrice: '',
          cachedInputReadPrice: '',
          cachedInputWritePrice: '',
          maxContextTokens: '',
          maxOutputTokens: '',
          features: [],
          selected: false,
        },
        {
          modelId: 'text-embedding-3-small',
          name: 'text-embedding-3-small',
          description: '',
          type: 'embedding',
          inputPrice: '',
          outputPrice: '',
          cachedInputReadPrice: '',
          cachedInputWritePrice: '',
          maxContextTokens: '',
          maxOutputTokens: '',
          features: [],
          selected: false,
        },
      ];

      renderStep(makeWizard({ models: fetchedModels }));

      // Both model-id inputs render
      const idInputs = screen.getAllByRole('textbox', { name: 'pages:providers.modelId' });
      expect(idInputs).toHaveLength(2);

      // Both rows are NOT selected (opt-in default)
      const checkboxes = screen.getAllByRole('checkbox');
      expect(checkboxes[0]).not.toBeChecked();
      expect(checkboxes[1]).not.toBeChecked();
    });

    it('renders the count note when fetchModelsCount is set and no error', () => {
      renderStep(makeWizard({ fetchModelsCount: 3, fetchModelsError: null, fetchModelsUnsupported: false }));
      expect(screen.getByText('pages:providers.wizardModelsFetchCount')).toBeInTheDocument();
    });

    it('does NOT render the count note when fetchModelsCount is null', () => {
      renderStep(makeWizard({ fetchModelsCount: null }));
      expect(screen.queryByText('pages:providers.wizardModelsFetchCount')).not.toBeInTheDocument();
    });

    it('does NOT render the count note when there is an unsupported error', () => {
      renderStep(makeWizard({ fetchModelsCount: 2, fetchModelsUnsupported: true }));
      expect(screen.queryByText('pages:providers.wizardModelsFetchCount')).not.toBeInTheDocument();
    });

    it('renders the type-guess hint when fetchModelsCount is set and models exist', () => {
      const fetchedModels: WizardModel[] = [
        {
          modelId: 'gpt-4o',
          name: 'gpt-4o',
          description: '',
          type: 'chat',
          inputPrice: '',
          outputPrice: '',
          cachedInputReadPrice: '',
          cachedInputWritePrice: '',
          maxContextTokens: '',
          maxOutputTokens: '',
          features: [],
          selected: false,
        },
      ];
      renderStep(makeWizard({ models: fetchedModels, fetchModelsCount: 1 }));
      expect(screen.getByText('pages:providers.wizardModelsTypeGuessHint')).toBeInTheDocument();
    });

    it('does NOT render the type-guess hint when fetchModelsCount is null', () => {
      const fetchedModels: WizardModel[] = [
        {
          modelId: 'gpt-4o',
          name: 'gpt-4o',
          description: '',
          type: 'chat',
          inputPrice: '',
          outputPrice: '',
          cachedInputReadPrice: '',
          cachedInputWritePrice: '',
          maxContextTokens: '',
          maxOutputTokens: '',
          features: [],
          selected: false,
        },
      ];
      renderStep(makeWizard({ models: fetchedModels, fetchModelsCount: null }));
      expect(screen.queryByText('pages:providers.wizardModelsTypeGuessHint')).not.toBeInTheDocument();
    });

    it('renders the embedding badge for embedding-type model', () => {
      const embeddingModel: WizardModel = {
        modelId: 'text-embedding-3-small',
        name: 'text-embedding-3-small',
        description: '',
        type: 'embedding',
        inputPrice: '',
        outputPrice: '',
        cachedInputReadPrice: '',
        cachedInputWritePrice: '',
        maxContextTokens: '',
        maxOutputTokens: '',
        features: [],
        selected: false,
      };
      renderStep(makeWizard({ models: [embeddingModel] }));
      expect(screen.getByText('embedding')).toBeInTheDocument();
    });
  });

  describe('Duplicate deduplication', () => {
    it('shows exactly 2 rows when wizard received the already-deduped list', () => {
      // Simulate the state after fetchModels deduplicated 'gpt-4o' (already present)
      // and appended only 'gpt-4-turbo'. Fetched rows default to selected:false.
      const deduped: WizardModel[] = [
        {
          modelId: 'gpt-4o',
          name: 'gpt-4o',
          description: '',
          type: 'chat',
          inputPrice: '',
          outputPrice: '',
          cachedInputReadPrice: '',
          cachedInputWritePrice: '',
          maxContextTokens: '',
          maxOutputTokens: '',
          features: [],
          selected: false,
        },
        {
          modelId: 'gpt-4-turbo',
          name: 'gpt-4-turbo',
          description: '',
          type: 'chat',
          inputPrice: '',
          outputPrice: '',
          cachedInputReadPrice: '',
          cachedInputWritePrice: '',
          maxContextTokens: '',
          maxOutputTokens: '',
          features: [],
          selected: false,
        },
      ];

      renderStep(makeWizard({ models: deduped }));

      const idInputs = screen.getAllByRole('textbox', { name: 'pages:providers.modelId' });
      expect(idInputs).toHaveLength(2);
    });
  });

  describe('discovery_unsupported response', () => {
    it('shows the OpenAI-only note when fetchModelsUnsupported is true', () => {
      renderStep(makeWizard({ fetchModelsUnsupported: true }));
      const alert = screen.getByRole('alert');
      expect(alert).toHaveTextContent('pages:providers.wizardModelsFetchOpenAIOnly');
    });

    it('does not show the unsupported note when fetchModelsUnsupported is false', () => {
      renderStep(makeWizard({ fetchModelsUnsupported: false }));
      expect(
        screen.queryByText('pages:providers.wizardModelsFetchOpenAIOnly'),
      ).not.toBeInTheDocument();
    });
  });

  describe('generic error response', () => {
    it('shows inline error message when fetchModelsError is set', () => {
      renderStep(makeWizard({ fetchModelsError: 'connection refused', fetchModelsUnsupported: false }));
      expect(screen.getByRole('alert')).toHaveTextContent('connection refused');
    });

    it('shows the unsupported note (not the generic error) when both are set', () => {
      // The component hides fetchModelsError when fetchModelsUnsupported is true.
      renderStep(makeWizard({ fetchModelsError: 'some error', fetchModelsUnsupported: true }));
      const alert = screen.getByRole('alert');
      expect(alert).toHaveTextContent('pages:providers.wizardModelsFetchOpenAIOnly');
      expect(alert).not.toHaveTextContent('some error');
    });
  });
});
