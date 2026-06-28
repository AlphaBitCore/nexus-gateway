/**
 * Unit tests for ProviderModelsTab — pricing-not-set reminder badge.
 *
 * Covers:
 *  (a) A model with null inputPricePerMillion renders the "Pricing not set" badge.
 *  (b) A model with undefined inputPricePerMillion renders the "Pricing not set" badge.
 *  (c) A model with a numeric inputPricePerMillion (including 0) does NOT render the badge.
 *  (d) Multiple models: badge shows for those missing pricing, absent for those with pricing.
 */

import { describe, it, expect, vi } from 'vitest';
import { render, screen } from '@testing-library/react';
import { ProviderModelsTab } from './ProviderModelsTab';
import type { ProviderDetailState } from './useProviderDetail';
import type { Model } from '@/api/types';

// Stub i18n — no real bundle needed; t(key) returns the key.
vi.mock('@/i18n', () => ({
  default: { t: (k: string) => k, language: 'en', changeLanguage: () => Promise.resolve() },
  SUPPORTED_LANGUAGES: [{ code: 'en', name: 'English' }],
  LANGUAGE_STORAGE_KEY: 'nexus-language',
}));

vi.mock('react-i18next', () => ({
  useTranslation: () => ({ t: (k: string) => k }),
  I18nextProvider: ({ children }: { children: React.ReactNode }) => children,
  initReactI18next: { type: '3rdParty', init: () => {} },
}));

// ModelFormDrawer pulls in many dependencies — mock it to a no-op.
vi.mock('./ModelFormDrawer', () => ({
  ModelFormDrawer: () => null,
}));

// ── Helpers ────────────────────────────────────────────────────────────────

function makeModel(overrides: Partial<Model> = {}): Model {
  return {
    id: 'model-1',
    code: 'gpt-4o',
    name: 'GPT-4o',
    description: '',
    providerId: 'provider-1',
    providerModelId: 'gpt-4o',
    type: 'chat',
    features: [],
    enabled: true,
    ...overrides,
  };
}

function makeDetail(models: Model[]): ProviderDetailState {
  return {
    models,
    canUpdate: false,
    canDelete: false,
    canCreateModel: false,
    showModelForm: false,
    setShowModelForm: vi.fn(),
    resetModelForm: vi.fn(),
    editingModelId: null,
    setEditingModelId: vi.fn(),
    startEditingModel: vi.fn(),
    setEditingCapabilityJson: vi.fn(),
    toggleModelEnabled: vi.fn(),
    setDeletingModel: vi.fn(),
  } as unknown as ProviderDetailState;
}

// ── Tests ──────────────────────────────────────────────────────────────────

const BADGE_KEY = 'pages:providers.pricingNotSet';

describe('ProviderModelsTab — pricing-not-set badge', () => {
  it('shows badge when inputPricePerMillion is undefined', () => {
    const model = makeModel({ inputPricePerMillion: undefined });
    render(<ProviderModelsTab detail={makeDetail([model])} />);
    expect(screen.getByText(BADGE_KEY)).toBeInTheDocument();
  });

  it('shows badge when inputPricePerMillion is null (API omits the field)', () => {
    // Type assertion: the API may return null even though the TS type says number|undefined.
    const model = makeModel({ inputPricePerMillion: null as unknown as undefined });
    render(<ProviderModelsTab detail={makeDetail([model])} />);
    expect(screen.getByText(BADGE_KEY)).toBeInTheDocument();
  });

  it('does NOT show badge when inputPricePerMillion is a positive number', () => {
    const model = makeModel({ inputPricePerMillion: 2.5 });
    render(<ProviderModelsTab detail={makeDetail([model])} />);
    expect(screen.queryByText(BADGE_KEY)).not.toBeInTheDocument();
  });

  it('does NOT show badge when inputPricePerMillion is 0 (explicitly set to zero-cost)', () => {
    const model = makeModel({ inputPricePerMillion: 0 });
    render(<ProviderModelsTab detail={makeDetail([model])} />);
    expect(screen.queryByText(BADGE_KEY)).not.toBeInTheDocument();
  });

  it('shows badge for the un-priced model but not for the priced one', () => {
    const models = [
      makeModel({ id: 'a', code: 'no-price', name: 'No Price', inputPricePerMillion: undefined }),
      makeModel({ id: 'b', code: 'has-price', name: 'Has Price', inputPricePerMillion: 1.0 }),
    ];
    render(<ProviderModelsTab detail={makeDetail(models)} />);
    // Exactly one badge rendered
    const badges = screen.getAllByText(BADGE_KEY);
    expect(badges).toHaveLength(1);
  });
});
