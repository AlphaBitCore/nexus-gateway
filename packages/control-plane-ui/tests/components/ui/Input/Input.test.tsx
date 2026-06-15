import { describe, it, expect } from 'vitest';
import { render, screen } from '@testing-library/react';
import { createRef } from 'react';
import { Input } from '../../../../src/components/ui/Input/Input';

describe('Input', () => {
  it('renders with default attributes', () => {
    render(<Input placeholder="Name" />);
    const el = screen.getByPlaceholderText('Name');
    expect(el).toBeInTheDocument();
    expect(el.tagName).toBe('INPUT');
  });

  it('sets data-error and aria-invalid when error is true', () => {
    render(<Input error placeholder="Email" />);
    const el = screen.getByPlaceholderText('Email');
    expect(el).toHaveAttribute('data-error');
    expect(el).toHaveAttribute('aria-invalid', 'true');
  });

  it('applies size class', () => {
    const { container } = render(<Input inputSize="lg" />);
    const el = container.firstElementChild!;
    expect(el.className).toContain('lg');
  });

  it('forwards ref', () => {
    const ref = createRef<HTMLInputElement>();
    render(<Input ref={ref} />);
    expect(ref.current).toBeInstanceOf(HTMLInputElement);
  });
});
