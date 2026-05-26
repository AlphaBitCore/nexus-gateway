import { screen, waitFor } from '@testing-library/react';
import userEvent from '@testing-library/user-event';
import { describe, expect, it, vi } from 'vitest';

import { renderWithRouter } from '@/test/test-utils';

vi.mock('@/api/services', () => ({
  rulePacksApi: {
    get: vi.fn().mockResolvedValue({
      id: 'p1',
      name: 'nexus/prompt-injection',
      version: 'v1.0.0',
      maintainer: 'nexus',
      description: 'desc',
      signature: '',
      createdAt: '2026-04-22T00:00:00Z',
      rules: [{ id: 'r1', ruleId: 'r1', category: 'c', severity: 'hard', pattern: 'foo' }],
    }),
    update: vi.fn().mockResolvedValue({
      id: 'p1',
      name: 'nexus/prompt-injection',
      version: 'v1.0.0',
      maintainer: 'nexus-team',
      createdAt: '2026-04-22T00:00:00Z',
      rules: [{ id: 'r1', ruleId: 'r1', category: 'compliance', severity: 'hard', pattern: 'foo' }],
    }),
  },
}));

vi.mock('react-router-dom', async () => {
  const actual = await vi.importActual<any>('react-router-dom');
  return { ...actual, useParams: () => ({ id: 'p1' }) };
});

import { rulePacksApi } from '@/api/services';
import { RulePackEditPage } from './RulePackEditPage';

describe('RulePackEditPage', () => {
  it('supports form mode and submits update request', async () => {
    const user = userEvent.setup();

    renderWithRouter(<RulePackEditPage />, { route: '/compliance/rule-packs/p1/edit' });

    await waitFor(() => expect(screen.getByText(/edit rule pack/i)).toBeDefined());
    await user.click(screen.getByRole('button', { name: /form mode/i }));

    const categoryInput = screen.getByLabelText(/category/i);
    await user.clear(categoryInput);
    await user.type(categoryInput, 'compliance');

    await user.click(screen.getByRole('button', { name: /save/i }));

    await waitFor(() =>
      expect(rulePacksApi.update).toHaveBeenCalledWith(
        'p1',
        expect.objectContaining({
          rules: [
            expect.objectContaining({
              category: 'compliance',
            }),
          ],
        }),
      ),
    );
  });
});
