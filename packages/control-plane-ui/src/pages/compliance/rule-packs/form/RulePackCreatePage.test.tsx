import { screen, waitFor } from '@testing-library/react';
import userEvent from '@testing-library/user-event';
import { describe, expect, it, vi } from 'vitest';

import { renderWithRouter } from '@/test/test-utils';

vi.mock('@/api/services', () => ({
  rulePacksApi: {
    create: vi.fn().mockResolvedValue({
      id: 'p-new',
      name: 'acme/rules',
      version: 'v1.0.0',
      maintainer: 'customer',
      createdAt: '',
      rules: [],
    }),
  },
}));

import { rulePacksApi } from '@/api/services';
import { RulePackCreatePage } from './RulePackCreatePage';

describe('RulePackCreatePage', () => {
  it('supports form mode and submits create request', async () => {
    const user = userEvent.setup();

    renderWithRouter(<RulePackCreatePage />, { route: '/compliance/rule-packs/create' });

    await user.type(screen.getByLabelText(/name/i), 'acme/rules');
    await user.type(screen.getByLabelText(/version/i), 'v1.0.0');
    await user.type(screen.getByLabelText(/maintainer/i), 'customer');
    await user.click(screen.getByRole('button', { name: /form mode/i }));

    await user.clear(screen.getByLabelText(/rule id/i));
    await user.type(screen.getByLabelText(/rule id/i), 'rule-1');

    await user.click(screen.getByRole('button', { name: /create pack/i }));

    await waitFor(() =>
      expect(rulePacksApi.create).toHaveBeenCalledWith(
        expect.objectContaining({
          name: 'acme/rules',
          version: 'v1.0.0',
          maintainer: 'customer',
        }),
      ),
    );
  });
});
