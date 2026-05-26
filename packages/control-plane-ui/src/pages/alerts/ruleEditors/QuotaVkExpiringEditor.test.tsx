import { describe, it, expect, vi } from 'vitest';
import { screen } from '@testing-library/react';
import userEvent from '@testing-library/user-event';
import '@testing-library/jest-dom/vitest';

import { renderWithProviders } from '@/test/test-utils';
import { QuotaVkExpiringEditor } from './QuotaVkExpiringEditor';

describe('QuotaVkExpiringEditor', () => {
  it('renders current warnDays list', () => {
    renderWithProviders(
      <QuotaVkExpiringEditor
        value={{ warnDays: [30, 15, 7, 1] }}
        schema={{}}
        onChange={vi.fn()}
      />,
    );
    expect(screen.getByDisplayValue('30, 15, 7, 1')).toBeInTheDocument();
  });

  it('emits onChange with parsed integers >= 1 on blur', async () => {
    const user = userEvent.setup();
    const onChange = vi.fn();
    renderWithProviders(
      <QuotaVkExpiringEditor
        value={{ warnDays: [] }}
        schema={{}}
        onChange={onChange}
      />,
    );
    const input = screen.getByRole('textbox');
    await user.clear(input);
    await user.type(input, '7, 0, -3, 30, abc');
    input.blur();
    expect(onChange).toHaveBeenLastCalledWith({ warnDays: [7, 30] });
  });
});
