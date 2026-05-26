import { describe, it, expect } from 'vitest';
import { render, screen } from '@testing-library/react';
import '@testing-library/jest-dom/vitest';
import { Grid } from './Grid';

describe('Grid', () => {
  it('renders children', () => {
    render(
      <Grid>
        <div>Cell 1</div>
        <div>Cell 2</div>
      </Grid>,
    );
    expect(screen.getByText('Cell 1')).toBeInTheDocument();
    expect(screen.getByText('Cell 2')).toBeInTheDocument();
  });

  it('applies column class for cols-2', () => {
    const { container } = render(<Grid columns={2}>content</Grid>);
    const el = container.firstElementChild!;
    expect(el.className).toContain('cols-2');
  });

  it('applies column class for cols-3', () => {
    const { container } = render(<Grid columns={3}>content</Grid>);
    const el = container.firstElementChild!;
    expect(el.className).toContain('cols-3');
  });

  it('applies container responsive mode class', () => {
    const { container } = render(<Grid responsive="container">content</Grid>);
    const el = container.firstElementChild!;
    expect(el.className).toContain('container');
  });
});
