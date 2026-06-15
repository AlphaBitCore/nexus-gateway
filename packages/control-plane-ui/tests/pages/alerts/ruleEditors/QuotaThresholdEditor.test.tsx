import { describe, it, expect, vi } from 'vitest';
import { screen } from '@testing-library/react';
import userEvent from '@testing-library/user-event';

import { renderWithProviders } from '@/test/test-utils';
import { QuotaThresholdEditor } from '../../../../src/pages/alerts/ruleEditors/QuotaThresholdEditor';

describe('QuotaThresholdEditor', () => {
  it('renders with current thresholds formatted as comma list', () => {
    const onChange = vi.fn();
    renderWithProviders(
      <QuotaThresholdEditor
        value={{ thresholds: [80, 95] }}
        schema={{}}
        onChange={onChange}
      />,
    );
    const input = screen.getByDisplayValue('80, 95') as HTMLInputElement;
    expect(input).toBeInTheDocument();
  });

  it('parses comma list and calls onChange with valid integers in [1,100]', async () => {
    const user = userEvent.setup();
    const onChange = vi.fn();
    renderWithProviders(
      <QuotaThresholdEditor value={{ thresholds: [] }} schema={{}} onChange={onChange} />,
    );
    const input = screen.getByRole('textbox');
    await user.clear(input);
    await user.type(input, '50, 80, 120, abc, 0, 100');
    input.blur();
    expect(onChange).toHaveBeenLastCalledWith({ thresholds: [50, 80, 100] });
  });
});
