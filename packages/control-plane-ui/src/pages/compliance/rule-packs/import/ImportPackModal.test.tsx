import { fireEvent, screen, waitFor } from '@testing-library/react';
import userEvent from '@testing-library/user-event';
import { describe, expect, it, vi } from 'vitest';

import { renderWithProviders } from '@/test/test-utils';

vi.mock('@/api/services', () => ({
  rulePacksApi: {
    preview: vi.fn().mockResolvedValue({
      pack: { name: 'acme/test', version: 'v1.0.0' },
      warnings: [],
      errors: [],
    }),
    import: vi.fn().mockResolvedValue({
      packId: 'new-id',
      ruleCount: 3,
      warnings: [],
    }),
  },
}));

import { ImportPackModal } from './ImportPackModal';

describe('ImportPackModal', () => {
  it('previews and imports', async () => {
    const onClose = vi.fn();
    const user = userEvent.setup();

    renderWithProviders(<ImportPackModal open onClose={onClose} />);

    const textarea = screen.getByLabelText(/yaml/i);
    fireEvent.change(textarea, {
      target: {
        value: 'name: acme/test\nversion: v1.0.0\nmaintainer: customer\nrules: []\n',
      },
    });
    await user.click(screen.getByRole('button', { name: /preview/i }));

    await waitFor(() => expect(screen.getAllByText(/acme\/test/).length).toBeGreaterThan(0));

    await user.click(screen.getByRole('button', { name: /import/i }));

    await waitFor(() => expect(onClose).toHaveBeenCalled());
  });
});
