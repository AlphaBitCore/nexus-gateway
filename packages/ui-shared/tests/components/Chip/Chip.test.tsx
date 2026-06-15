import { describe, it, expect, vi } from 'vitest';
import { render, screen, fireEvent } from '@testing-library/react';
import { Chip } from '../../../src/components/Chip/Chip';

describe('Chip', () => {
  it('renders children and exposes aria-pressed=false when inactive', () => {
    render(<Chip>1 hour</Chip>);
    const chip = screen.getByRole('button', { name: '1 hour' });
    expect(chip).toBeInTheDocument();
    expect(chip).toHaveAttribute('aria-pressed', 'false');
  });

  it('exposes aria-pressed=true and the active class when active', () => {
    const { container } = render(<Chip active>24 hours</Chip>);
    const chip = screen.getByRole('button', { name: '24 hours' });
    expect(chip).toHaveAttribute('aria-pressed', 'true');
    expect(container.firstElementChild!.className).toContain('active');
  });

  it('fires onClick for selection', () => {
    const handler = vi.fn();
    render(<Chip onClick={handler}>7 days</Chip>);
    fireEvent.click(screen.getByRole('button'));
    expect(handler).toHaveBeenCalledOnce();
  });

  it('does not invoke onClick when disabled', () => {
    const handler = vi.fn();
    render(<Chip disabled onClick={handler}>x</Chip>);
    fireEvent.click(screen.getByRole('button'));
    expect(handler).not.toHaveBeenCalled();
  });
});
