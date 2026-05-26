import { describe, it, expect } from 'vitest';
import { render, screen } from '@testing-library/react';
import '@testing-library/jest-dom/vitest';
import { Stack } from './Stack';

describe('Stack', () => {
  it('renders children', () => {
    render(
      <Stack>
        <span>A</span>
        <span>B</span>
      </Stack>,
    );
    expect(screen.getByText('A')).toBeInTheDocument();
    expect(screen.getByText('B')).toBeInTheDocument();
  });

  it('defaults to vertical direction', () => {
    const { container } = render(<Stack>content</Stack>);
    const el = container.firstElementChild!;
    expect(el.className).toContain('vertical');
    expect(el.className).not.toContain('horizontal');
  });

  it('applies direction class for horizontal', () => {
    const { container } = render(<Stack direction="horizontal">content</Stack>);
    const el = container.firstElementChild!;
    expect(el.className).toContain('horizontal');
  });

  it('accepts className override', () => {
    const { container } = render(<Stack className="custom">content</Stack>);
    const el = container.firstElementChild!;
    expect(el.className).toContain('custom');
  });
});
