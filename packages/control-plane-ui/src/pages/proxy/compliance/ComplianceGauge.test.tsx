/**
 * Unit test — ComplianceGauge renders correctly at various coverage levels.
 */
import { describe, it, expect } from 'vitest';
import { screen } from '@testing-library/react';

import { renderWithProviders } from '@/test/test-utils';
import { ComplianceGauge } from './ComplianceGauge';

function renderGauge(percent: number) {
  return renderWithProviders(<ComplianceGauge percent={percent} />);
}

describe('ComplianceGauge', () => {
  it('renders with 95% (green / success color)', () => {
    renderGauge(95);
    expect(screen.getByText('95.0%')).toBeDefined();
    const bar = screen.getByRole('progressbar');
    expect(bar.getAttribute('aria-valuenow')).toBe('95');
    // Green = var(--color-success)
    expect(bar.style.backgroundColor).toBe('var(--color-success)');
  });

  it('renders with 80% (yellow / warning color)', () => {
    renderGauge(80);
    expect(screen.getByText('80.0%')).toBeDefined();
    const bar = screen.getByRole('progressbar');
    expect(bar.getAttribute('aria-valuenow')).toBe('80');
    // Yellow = var(--color-warning)
    expect(bar.style.backgroundColor).toBe('var(--color-warning)');
  });

  it('renders with 50% (red / danger color)', () => {
    renderGauge(50);
    expect(screen.getByText('50.0%')).toBeDefined();
    const bar = screen.getByRole('progressbar');
    expect(bar.getAttribute('aria-valuenow')).toBe('50');
    // Red = var(--color-danger)
    expect(bar.style.backgroundColor).toBe('var(--color-danger)');
  });

  it('renders 0%', () => {
    renderGauge(0);
    expect(screen.getByText('0.0%')).toBeDefined();
    const bar = screen.getByRole('progressbar');
    expect(bar.getAttribute('aria-valuenow')).toBe('0');
    expect(bar.style.width).toBe('0%');
  });

  it('clamps values above 100', () => {
    renderGauge(150);
    expect(screen.getByText('100.0%')).toBeDefined();
    const bar = screen.getByRole('progressbar');
    expect(bar.getAttribute('aria-valuenow')).toBe('100');
  });

  it('renders compliance coverage label (i18n)', () => {
    renderGauge(90);
    expect(screen.getByText('Compliance coverage')).toBeDefined();
  });
});
