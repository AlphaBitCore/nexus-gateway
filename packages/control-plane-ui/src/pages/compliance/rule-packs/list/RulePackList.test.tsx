import { screen, waitFor } from '@testing-library/react';
import userEvent from '@testing-library/user-event';
import { MemoryRouter } from 'react-router-dom';
import { describe, expect, it, vi } from 'vitest';

import { renderWithProviders } from '@/test/test-utils';

import { RulePackList } from './RulePackList';

vi.mock('@/api/services', () => ({
  rulePacksApi: {
    list: vi.fn().mockResolvedValue([
      {
        id: 'p1',
        name: 'nexus/prompt-injection',
        version: 'v1.0.0',
        maintainer: 'nexus',
        createdAt: '2026-04-22T00:00:00Z',
      },
      {
        id: 'p2',
        name: 'acme/custom',
        version: 'v1.0.0',
        maintainer: 'customer',
        createdAt: '2026-04-22T00:00:00Z',
      },
    ]),
  },
}));

describe('RulePackList', () => {
  it('renders packs in a table', async () => {
    renderWithProviders(
      <MemoryRouter>
        <RulePackList />
      </MemoryRouter>,
    );

    await waitFor(() => expect(screen.getByText('nexus/prompt-injection')).toBeDefined());
    expect(screen.getByText('acme/custom')).toBeDefined();
  });

  it('filters by maintainer', async () => {
    const user = userEvent.setup();

    renderWithProviders(
      <MemoryRouter>
        <RulePackList />
      </MemoryRouter>,
    );

    await waitFor(() => expect(screen.getByText('nexus/prompt-injection')).toBeDefined());

    await user.selectOptions(screen.getByLabelText(/maintainer/i), 'customer');

    expect(screen.queryByText('nexus/prompt-injection')).toBeNull();
    expect(screen.getByText('acme/custom')).toBeDefined();
  });
});
