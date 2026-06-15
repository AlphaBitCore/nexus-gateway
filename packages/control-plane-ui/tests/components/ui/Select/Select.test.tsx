import { describe, it, expect, vi } from 'vitest';
import { render, screen } from '@testing-library/react';
import { Select } from '../../../../src/components/ui/Select/Select';

const options = [
  { value: 'a', label: 'Alpha' },
  { value: 'b', label: 'Beta' },
  { value: 'c', label: 'Gamma', disabled: true },
];

describe('Select', () => {
  it('renders trigger with placeholder', () => {
    render(
      <Select value="" onValueChange={vi.fn()} options={options} placeholder="Pick one" />,
    );
    expect(screen.getByRole('combobox')).toBeInTheDocument();
    expect(screen.getByText('Pick one')).toBeInTheDocument();
  });

  it('shows selected value', () => {
    render(
      <Select value="b" onValueChange={vi.fn()} options={options} placeholder="Pick one" />,
    );
    expect(screen.getByText('Beta')).toBeInTheDocument();
  });
});
