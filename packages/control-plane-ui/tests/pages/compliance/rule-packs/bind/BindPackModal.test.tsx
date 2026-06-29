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

import { BindPackModal } from '../../../../../src/pages/compliance/rule-packs/bind/BindPackModal';

describe('BindPackModal', () => {
  it('lists packs and submits', async () => {
    const onBound = vi.fn();
    const user = userEvent.setup();

    renderWithProviders(
      <BindPackModal open hookId="hook-x" onClose={() => {}} onBound={onBound} />,
    );

    // The pack family is offered as one option in the multi-select dropdown;
    // its two versions are grouped under the family name (latest first).
    const trigger = await screen.findByRole('button', { name: /rule packs/i });
    await user.click(trigger);

    const option = await screen.findByRole('option', { name: /nexus\/prompt-injection/i });
    await user.click(option);

    // Submitting binds each selected family at its latest version and reports
    // the resulting install(s) via onBound.
    await user.click(screen.getByRole('button', { name: /bind \d+ pack/i }));

    await waitFor(() => expect(onBound).toHaveBeenCalled());
  });
});

