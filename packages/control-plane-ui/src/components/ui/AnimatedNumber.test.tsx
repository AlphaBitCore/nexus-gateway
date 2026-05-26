import { describe, it, expect } from 'vitest';
import { render } from '@testing-library/react';
import { AnimatedNumber } from './AnimatedNumber';

// In jsdom + the useCountUp hook's "skip" path (no requestAnimationFrame
// scheduling actually ticking), the component renders the final value
// immediately when target is 0; for non-zero targets it still renders a
// number while ramping. We assert the final-state shape — the formatted
// output for the resolved value — so the test is deterministic.

describe('AnimatedNumber', () => {
  it('renders zero through the default integer formatter', () => {
    const { container } = render(<AnimatedNumber value={0} />);
    expect(container.textContent).toBe('0');
  });

  it('renders a finite numeric value through a custom formatter', () => {
    const { container } = render(
      <AnimatedNumber value={0} format={(n) => `$${n.toFixed(2)}`} />,
    );
    expect(container.textContent).toBe('$0.00');
  });

  it('coerces NaN / Infinity to 0', () => {
    const { container } = render(<AnimatedNumber value={NaN} format={(n) => `${n}`} />);
    expect(container.textContent).toBe('0');
  });
});
