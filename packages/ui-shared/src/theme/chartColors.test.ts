import { describe, expect, it } from 'vitest';
import {
  getAxisTickStyle,
  getGridStroke,
  getPieColors,
  getSemanticColor,
  getSeriesColors,
  getTooltipStyle,
} from './chartColors';

describe('prime-aligned chart colors', () => {
  it('uses the prime brand blue as the lead light-mode series color', () => {
    expect(getSeriesColors('light')[0]).toBe('#3b518a');
    expect(getSemanticColor('light', 'requests')).toBe('#3b518a');
    expect(getSemanticColor('light', 'promptCache')).toBe('#3b518a');
  });

  it('keeps dark charts readable on the dark prime shell', () => {
    expect(getSeriesColors('dark')[0]).toBe('#8ea4da');
    expect(getSemanticColor('dark', 'requests')).toBe('#8ea4da');
    expect(getGridStroke('dark')).toBe('var(--color-border)');
  });

  it('returns token-backed tooltip and axis styles for both modes', () => {
    expect(getPieColors('light')[0]).toBe('#3b518a');
    expect(getTooltipStyle('light')).toMatchObject({
      background: 'var(--color-surface-overlay)',
      color: 'var(--color-text)',
    });
    expect(getAxisTickStyle('dark')).toMatchObject({
      fill: 'var(--color-text-tertiary)',
    });
  });
});
