/**
 * SettingsCacheTab — the provider prompt-cache config tree is GlobalPanel-free:
 * Adapter defaults, Normalisation rules, and Active overrides only. The Tier-1
 * "Global Defaults" panel (normaliser gate + cache master kill switch) is
 * retired — emergency cache-off lives on the StatusStrip / Emergency
 * Passthrough, and the upstream rewrite is demand-driven.
 */
import { describe, it, expect, vi, beforeEach } from 'vitest';
import { render, screen } from '@testing-library/react';
import { MemoryRouter } from 'react-router-dom';
import { I18nextProvider } from 'react-i18next';
import i18n from '@/i18n';
import { SettingsCacheTab } from '@/pages/ai-gateway/cache/settings/SettingsCacheTab';

// All panels get the empty/loading branch (undefined data) — this suite pins
// the TREE shape, not panel internals (each panel has its own suite).
vi.mock('@/hooks/useApi', () => ({
  useApi: () => ({ data: undefined, loading: false, error: null, refetch: vi.fn() }),
}));
vi.mock('@/context/ToastContext', () => ({ useToast: () => ({ addToast: vi.fn() }) }));
vi.mock('@/hooks/useMutation', () => ({
  useMutation: (fn: () => Promise<unknown>, opts?: { onSuccess?: () => void }) => ({
    mutate: async () => { await fn(); opts?.onSuccess?.(); },
    loading: false,
  }),
}));

function wrap() {
  return render(
    <I18nextProvider i18n={i18n}>
      <MemoryRouter>
        <SettingsCacheTab />
      </MemoryRouter>
    </I18nextProvider>,
  );
}

describe('SettingsCacheTab — GlobalPanel-free tree', () => {
  beforeEach(() => vi.clearAllMocks());

  it('renders the three surviving panels', () => {
    wrap();
    expect(screen.getByText(i18n.t('pages:settings.promptCache.adapterTitle'))).toBeInTheDocument();
    expect(screen.getByText(i18n.t('pages:settings.promptCache.rulesTitle'))).toBeInTheDocument();
    expect(screen.getByText(i18n.t('pages:settings.promptCache.overridesTitle'))).toBeInTheDocument();
  });

  it('does not render the retired Global Defaults panel or its switches', () => {
    wrap();
    // The i18n keys were removed with the panel; assert none of the retired
    // strings leak back in via any locale fallback.
    expect(screen.queryByText(/Global Defaults/i)).not.toBeInTheDocument();
    expect(screen.queryByText(/kill switch/i)).not.toBeInTheDocument();
    expect(screen.queryByText(/normaliser pipeline enabled/i)).not.toBeInTheDocument();
  });
});
