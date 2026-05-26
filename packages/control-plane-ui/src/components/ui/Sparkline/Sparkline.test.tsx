import { describe, it, expect } from 'vitest';
import { render } from '@testing-library/react';
import { Sparkline } from './Sparkline';

describe('Sparkline', () => {
  it('renders SVG with valid data', () => {
    const { container } = render(<Sparkline data={[10, 20, 15, 25, 18]} />);
    const svg = container.querySelector('svg');
    expect(svg).not.toBeNull();
    expect(svg?.getAttribute('role')).toBe('img');
  });

  it('returns null for empty data', () => {
    const { container } = render(<Sparkline data={[]} />);
    expect(container.innerHTML).toBe('');
  });

  it('returns null for single point', () => {
    const { container } = render(<Sparkline data={[5]} />);
    expect(container.innerHTML).toBe('');
  });

  it('renders with 2 data points (minimum)', () => {
    const { container } = render(<Sparkline data={[10, 20]} />);
    expect(container.querySelector('svg')).not.toBeNull();
  });

  it('handles all-zero data', () => {
    const { container } = render(<Sparkline data={[0, 0, 0, 0]} />);
    expect(container.querySelector('svg')).not.toBeNull();
  });

  it('handles all-same data', () => {
    const { container } = render(<Sparkline data={[5, 5, 5, 5]} />);
    expect(container.querySelector('svg')).not.toBeNull();
  });

  it('renders end dot', () => {
    const { container } = render(<Sparkline data={[10, 20, 15]} />);
    expect(container.querySelector('circle')).not.toBeNull();
  });

  it('sets aria-label with trend direction', () => {
    const { container: up } = render(<Sparkline data={[10, 20]} />);
    expect(up.querySelector('svg')?.getAttribute('aria-label')).toBe('Trend: up');

    const { container: down } = render(<Sparkline data={[20, 10]} />);
    expect(down.querySelector('svg')?.getAttribute('aria-label')).toBe('Trend: down');

    const { container: flat } = render(<Sparkline data={[10, 10]} />);
    expect(flat.querySelector('svg')?.getAttribute('aria-label')).toBe('Trend: flat');
  });

  it('respects custom color', () => {
    const { container } = render(<Sparkline data={[10, 20]} color="red" />);
    const path = container.querySelector('path[stroke]');
    expect(path?.getAttribute('stroke')).toBe('red');
  });
});
