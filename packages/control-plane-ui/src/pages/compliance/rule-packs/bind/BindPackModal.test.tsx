import { screen, waitFor } from '@testing-library/react';
import userEvent from '@testing-library/user-event';
import { describe, expect, it, vi } from 'vitest';

import { renderWithProviders } from '@/test/test-utils';

vi.mock('@/api/services', () => ({
  rulePacksApi: {
    list: vi.fn().mockResolvedValue([
      { id: 'p1', name: 'nexus/prompt-injection', version: 'v1.0.0', maintainer: 'nexus', createdAt: '' },
      { id: 'p1b', name: 'nexus/prompt-injection', version: 'v1.1.0', maintainer: 'nexus', createdAt: '' },
    ]),
    install: vi.fn().mockResolvedValue({
      id: 'i1',
      packId: 'p1',
      pinVersion: 'v1.0.0',
      boundHookId: 'hook-x',
      enabled: true,
      installedAt: '',
      packName: 'nexus/prompt-injection',
    }),
  },
}));

import { BindPackModal } from './BindPackModal';

describe('BindPackModal', () => {
  it('lists packs and submits', async () => {
    const onBound = vi.fn();
    const user = userEvent.setup();

    renderWithProviders(
      <BindPackModal open hookId="hook-x" onClose={() => {}} onBound={onBound} />,
    );

    await waitFor(() => expect(screen.getByText('nexus/prompt-injection')).toBeDefined());
    await user.click(screen.getByText('nexus/prompt-injection'));

    const versionSelect = screen.getByLabelText(/version/i);
    await user.selectOptions(versionSelect, 'v1.0.0');
    await user.click(screen.getByRole('button', { name: /bind|install/i }));

    await waitFor(() => expect(onBound).toHaveBeenCalled());
  });
});

