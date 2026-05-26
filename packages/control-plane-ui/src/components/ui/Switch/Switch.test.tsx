import { describe, it, expect, vi } from 'vitest';
import { render, screen, fireEvent } from '@testing-library/react';
import '@testing-library/jest-dom/vitest';
import { Switch } from './Switch';

describe('Switch', () => {
  it('renders unchecked state', () => {
    render(<Switch checked={false} onCheckedChange={vi.fn()} />);
    const el = screen.getByRole('switch');
    expect(el).toBeInTheDocument();
    expect(el).toHaveAttribute('data-state', 'unchecked');
  });

  it('calls onCheckedChange when clicked', () => {
    const handler = vi.fn();
    render(<Switch checked={false} onCheckedChange={handler} />);
    fireEvent.click(screen.getByRole('switch'));
    expect(handler).toHaveBeenCalledWith(true);
  });

  it('exposes aria-label on the switch root', () => {
    render(
      <Switch
        checked
        onCheckedChange={vi.fn()}
        aria-label="Test switch label"
      />,
    );
    expect(screen.getByRole('switch', { name: 'Test switch label' })).toBeInTheDocument();
  });
});
