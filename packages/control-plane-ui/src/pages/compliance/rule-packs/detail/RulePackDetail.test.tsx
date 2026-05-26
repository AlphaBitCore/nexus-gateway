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
      createdAt: '2026-04-22T00:00:00Z',
      rules: [{ id: 'r1', ruleId: 'r1', category: 'c', severity: 'hard', pattern: 'foo' }],
    }),
    dryRun: vi.fn().mockResolvedValue({
      matches: [{ pack: 'nexus/prompt-injection', packVersion: 'v1.0.0', ruleId: 'r1', category: 'c', severity: 'hard' }],
    }),
  },
}));

vi.mock('react-router-dom', async () => {
  const actual = await vi.importActual<any>('react-router-dom');
  return { ...actual, useParams: () => ({ id: 'p1' }) };
});

import { RulePackDetail } from './RulePackDetail';

describe('RulePackDetail', () => {
  it('renders metadata and rules', async () => {
    renderWithRouter(<RulePackDetail />);

    await waitFor(() => expect(screen.getByText('nexus/prompt-injection')).toBeDefined());
    expect(screen.getAllByText('r1').length).toBeGreaterThan(0);
  });

  it('runs dry-run and shows matches', async () => {
    const user = userEvent.setup();

    renderWithRouter(<RulePackDetail />);

    await waitFor(() => expect(screen.getByText('nexus/prompt-injection')).toBeDefined());
    await user.type(screen.getByLabelText(/content/i), 'trigger');
    await user.click(screen.getByRole('button', { name: /try|run/i }));

    await waitFor(() => expect(screen.getAllByText(/r1/).length).toBeGreaterThan(0));
  });
});

