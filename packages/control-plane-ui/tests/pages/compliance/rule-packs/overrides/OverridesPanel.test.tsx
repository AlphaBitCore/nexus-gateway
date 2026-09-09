import { screen, waitFor } from '@testing-library/react';
import userEvent from '@testing-library/user-event';
import { describe, expect, it, vi } from 'vitest';

import { renderWithProviders } from '@/test/test-utils';

// Both panels write through hook:update. The set is mutable so one arm can
// take the grant away and prove the affordance is actually gated — a blanket
// `() => true` would leave the gate itself untested.
const denied = new Set<string>();
vi.mock('@/hooks/usePermission', () => ({
  usePermission: (key: string) => !denied.has(key),
}));

vi.mock('@/api/services', () => ({
  rulePacksApi: {
    effectiveRules: vi.fn().mockResolvedValue({
      install: { id: 'i1', packName: 'nexus/prompt-injection', pinVersion: 'v1.0.0', boundHookId: 'hook-x', enabled: true, installedAt: '' },
      pack: {
        id: 'p1',
        name: 'nexus/prompt-injection',
        version: 'v1.0.0',
        maintainer: 'nexus',
        createdAt: '',
        rules: [
          { id: 'pi-io-001', ruleId: 'pi-io-001', category: 'c', severity: 'hard', pattern: 'foo' },
          { id: 'pi-io-002', ruleId: 'pi-io-002', category: 'c', severity: 'soft', pattern: 'bar' },
        ],
      },
    }),
    upsertOverrides: vi.fn().mockResolvedValue({ installId: 'i1', overridesSaved: 1 }),
  },
}));

import { OverridesPanel } from '../../../../../src/pages/compliance/rule-packs/overrides/OverridesPanel';

describe('OverridesPanel', () => {
  it('renders rules and submits overrides', async () => {
    const user = userEvent.setup();

    renderWithProviders(<OverridesPanel installId="i1" />);

    await waitFor(() => expect(screen.getByText('pi-io-001')).toBeDefined());
    const toggles = screen.getAllByRole('checkbox');
    await user.click(toggles[0]);
    await user.click(screen.getByRole('button', { name: /save/i }));

    const { rulePacksApi } = await import('@/api/services');
    await waitFor(() => expect(rulePacksApi.upsertOverrides).toHaveBeenCalled());
  });

  // The server refuses this write without hook:update, so offering an enabled
  // Save button only produces a 403 the operator cannot act on.
  it('disables Save and writes nothing without hook:update', async () => {
    denied.add('hook:update');
    try {
      const { rulePacksApi } = await import('@/api/services');
      vi.mocked(rulePacksApi.upsertOverrides).mockClear();
      const user = userEvent.setup();

      renderWithProviders(<OverridesPanel installId="i1" />);

      await waitFor(() => expect(screen.getByText('pi-io-001')).toBeDefined());
      // Make a real change first. Without it Save is disabled because nothing
      // is pending, and the assertion below would hold for the wrong reason.
      await user.click(screen.getAllByRole('checkbox')[0]);
      const save = screen.getByRole('button', { name: /save/i });
      expect(save.hasAttribute('disabled')).toBe(true);

      await user.click(save);
      expect(rulePacksApi.upsertOverrides).not.toHaveBeenCalled();
    } finally {
      denied.delete('hook:update');
    }
  });
});

