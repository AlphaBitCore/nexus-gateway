import { describe, it, expect } from 'vitest';
import { render, screen } from '@testing-library/react';
import { Card } from '../../../../src/components/ui/Card/Card';

describe('Card', () => {
  it('renders children with default padding', () => {
    render(<Card>Card content</Card>);
    const el = screen.getByText('Card content');
    expect(el).toBeInTheDocument();
    expect(el.className).toContain('pad-md');
    expect(el).not.toHaveAttribute('data-interactive');
  });

  it('marks clickable cards as interactive', () => {
    render(<Card onClick={() => {}}>Open detail</Card>);
    expect(screen.getByText('Open detail')).toHaveAttribute('data-interactive', 'true');
  });
});
