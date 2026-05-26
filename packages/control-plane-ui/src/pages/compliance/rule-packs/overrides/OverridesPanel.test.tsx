import { screen, waitFor } from '@testing-library/react';
import userEvent from '@testing-library/user-event';
import { describe, expect, it, vi } from 'vitest';

import { renderWithProviders } from '@/test/test-utils';

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

import { OverridesPanel } from './OverridesPanel';

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
});

