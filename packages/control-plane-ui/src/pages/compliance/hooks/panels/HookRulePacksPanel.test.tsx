import { screen, waitFor } from '@testing-library/react';
import userEvent from '@testing-library/user-event';
import { describe, expect, it, vi } from 'vitest';

import { renderWithProviders } from '@/test/test-utils';

vi.mock('@/api/services', () => ({
  rulePacksApi: {
    listInstallsForHook: vi.fn().mockResolvedValue([
      {
        id: 'i1',
        packId: 'p1',
        packName: 'nexus/prompt-injection',
        pinVersion: 'v1.0.0',
        boundHookId: 'hook-x',
        enabled: true,
        installedAt: '2026-04-22T00:00:00Z',
      },
    ]),
    patchInstall: vi.fn().mockResolvedValue({ installId: 'i1', enabled: false }),
    uninstall: vi.fn().mockResolvedValue(undefined),
    list: vi.fn().mockResolvedValue([]),
    effectiveRules: vi.fn().mockResolvedValue({
      install: {
        id: 'i1',
        packId: 'p1',
        packName: 'nexus/prompt-injection',
        pinVersion: 'v1.0.0',
        boundHookId: 'hook-x',
        enabled: true,
        installedAt: '',
      },
      pack: {
        id: 'p1',
        name: 'nexus/prompt-injection',
        version: 'v1.0.0',
        maintainer: 'nexus',
        createdAt: '',
        rules: [{ id: 'r1', ruleId: 'r1', category: 'c', severity: 'hard', pattern: 'foo' }],
      },
    }),
  },
}));

import { HookRulePacksPanel } from './HookRulePacksPanel';
import { rulePacksApi } from '@/api/services';

describe('HookRulePacksPanel', () => {
  it('lists installed rule packs for the hook', async () => {
    renderWithProviders(<HookRulePacksPanel hookId="hook-x" />);

    await waitFor(() => expect(screen.getByText('nexus/prompt-injection')).toBeDefined());
    expect(rulePacksApi.listInstallsForHook).toHaveBeenCalledWith('hook-x');
    expect(screen.getByText('v1.0.0')).toBeDefined();
  });

  it('toggles override panel visibility', async () => {
    const user = userEvent.setup();

    renderWithProviders(<HookRulePacksPanel hookId="hook-x" />);

    await waitFor(() => expect(screen.getByText('nexus/prompt-injection')).toBeDefined());

    await user.click(screen.getByRole('button', { name: /manage overrides/i }));

    await waitFor(() =>
      expect(screen.getByRole('button', { name: /hide overrides/i })).toBeDefined(),
    );
  });
});
