import { describe, it, expect, vi } from 'vitest';
import { render, screen, fireEvent } from '@testing-library/react';
import { LinkButton } from '../../../src/components/LinkButton/LinkButton';

describe('LinkButton', () => {
  it('renders text children inside a button (not an anchor)', () => {
    render(<LinkButton>Skip this step</LinkButton>);
    const btn = screen.getByRole('button', { name: 'Skip this step' });
    expect(btn).toBeInTheDocument();
    expect(btn.tagName).toBe('BUTTON');
  });

  it('fires onClick', () => {
    const handler = vi.fn();
    render(<LinkButton onClick={handler}>Add another</LinkButton>);
    fireEvent.click(screen.getByRole('button'));
    expect(handler).toHaveBeenCalledOnce();
  });

  it('is disabled when disabled prop is set', () => {
    render(<LinkButton disabled>Browse all</LinkButton>);
    expect(screen.getByRole('button', { name: 'Browse all' })).toBeDisabled();
  });
});
