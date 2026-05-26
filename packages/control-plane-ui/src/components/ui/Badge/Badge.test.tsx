import { describe, it, expect } from 'vitest';
import { render, screen } from '@testing-library/react';
import '@testing-library/jest-dom/vitest';
import { Badge, statusToVariant } from './Badge';

describe('Badge', () => {
  it('renders children', () => {
    render(<Badge>Active</Badge>);
    expect(screen.getByText('Active')).toBeInTheDocument();
  });

  it('applies variant class', () => {
    const { container } = render(<Badge variant="success">OK</Badge>);
    const el = container.firstElementChild!;
    expect(el.className).toContain('success');
  });
});

describe('statusToVariant', () => {
  it('maps "active" to "success"', () => {
    expect(statusToVariant('active')).toBe('success');
  });

  it('returns "default" for unknown status', () => {
    expect(statusToVariant('something-random')).toBe('default');
  });
});
