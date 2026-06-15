import { describe, it, expect, vi } from 'vitest';
import { render, screen, fireEvent } from '@testing-library/react';
import { IconButton } from '../../../src/components/IconButton/IconButton';

describe('IconButton', () => {
  it('exposes the required aria-label so screen readers can announce it', () => {
    render(
      <IconButton aria-label="Dismiss">
        <svg data-testid="icon" />
      </IconButton>,
    );
    const btn = screen.getByRole('button', { name: 'Dismiss' });
    expect(btn).toBeInTheDocument();
    expect(screen.getByTestId('icon')).toBeInTheDocument();
  });

  it('fires onClick', () => {
    const handler = vi.fn();
    render(
      <IconButton aria-label="x" onClick={handler}>
        <span />
      </IconButton>,
    );
    fireEvent.click(screen.getByRole('button', { name: 'x' }));
    expect(handler).toHaveBeenCalledOnce();
  });

  it('applies the size class', () => {
    const { container } = render(
      <IconButton aria-label="x" size="lg">
        <span />
      </IconButton>,
    );
    expect(container.firstElementChild!.className).toContain('lg');
  });

  it('is disabled when disabled prop is true', () => {
    render(
      <IconButton aria-label="x" disabled>
        <span />
      </IconButton>,
    );
    expect(screen.getByRole('button', { name: 'x' })).toBeDisabled();
  });
});
